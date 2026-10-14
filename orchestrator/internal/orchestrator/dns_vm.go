package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
)

// dnsBaseImage is the DataSource cloned for the DNS VM (same trick as the
// MCS relay: cluster-provided OS images are referenceable from any ns).
func dnsBaseImage() string {
	if v := os.Getenv("DNS_VM_BASE_IMAGE"); v != "" {
		return v
	}
	return "centos-stream9"
}

// dnsVMMode reports whether DNS runs as a KubeVirt VM (claim-persistent cUDN
// IP) instead of a Deployment pod. Pod DNS IPs are NOT claimed: tonight's
// sos-nightly outage was exactly a DNS pod rescheduling .18 -> .52 and
// stranding every pre-pivot guest pinned to the old IP. VM DNS is stable for
// the life of the cluster namespace, so status.dnsIPCUDN written once stays
// true — which is what the ISO nameserver= karg, node guards and wait jobs
// all consume.
func dnsVMMode() bool { return os.Getenv("DNS_MODE") == "vm" }

// deployDNSVM provisions <cluster>-dns-vm (python dns.py under systemd) and
// records its persistent cUDN IP as status.dnsIPCUDN.
func (s *StepExecutor) deployDNSVM(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("dns-vm")
	clusterNs := nc.Spec.Namespace

	clusterName := nc.Spec.ClusterName
	localZone := clusterName + "." + nc.Spec.BaseDomain

	// Zone content is identical to the pod path — record targets follow the
	// VM-first control VIPs below.
	apiVIP := s.resolveControlPlaneVIP(ctx, nc)
	ingressVIP := s.resolveIngressVIP(ctx, nc)
	if apiVIP == "" || ingressVIP == "" {
		return fmt.Errorf("control VIPs not ready for DNS zone (api=%q ingress=%q)", apiVIP, ingressVIP)
	}
	dnsScript := s.generateDNSScript(apiVIP, ingressVIP, localZone)

	pubKey := ""
	if b, err := s.client.GetSecretData(ctx, clusterNs, clusterName+"-ssh-key", "ssh-publickey"); err == nil {
		pubKey = strings.TrimSpace(string(b))
	}

	vmName := clusterName + "-dns-vm"
	if err := s.client.CreateInfraVM(ctx, k8s.InfraVMParams{
		Name:             vmName,
		Namespace:        clusterNs,
		ClusterName:      clusterName,
		Cores:            1,
		MemoryGi:         2,
		DiskGi:           32, // centos-stream9 DataSource is 30Gi; clone >= source
		StorageClassName: nc.Spec.StorageClassName,
		DataSourceNS:     "openshift-virtualization-os-images",
		DataSourceName:   dnsBaseImage(),
		UserData:         dnsVMUserData(vmName, dnsScript, pubKey),
	}); err != nil {
		return fmt.Errorf("creating dns VM: %w", err)
	}

	log.Info("waiting for DNS VM to boot", "vm", vmName)
	ip, err := s.client.WaitForVMIReady(ctx, clusterNs, vmName, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("dns VM never got an IP: %w", err)
	}
	// The nameserver= karg baked into the agent ISO points at the RESERVED
	// address (ipPlan.offsetDNSVM); a drifted VM would leave every pre-pivot
	// ignition fetch pointing at nothing. Fail loudly instead.
	if plan, perr := computeIPPlan(clusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP); perr == nil && plan.DNSVM.String() != ip {
		return fmt.Errorf("DNS VM got IP %s but the reserved allocation is %s — the ISO karg targets the reserved address (check IPAMClaim %s.default)",
			ip, plan.DNSVM.String(), vmName)
	}
	nc.Status.DNSIPCUDN = ip
	// The zone itself may have changed since (VM recreated): re-push via
	// configmap is not used in vm mode — userData embeds the script; the
	// sha-rollover concern does not apply because recreating the zone value
	// means recreating the VM.
	s.updateCRStatus(ctx, nc)
	log.Info("DNS VM up", "ip", ip)

	// NOTE: no active probe from here — the orchestrator pod lives in its
	// own namespace and cannot reach the cluster cUDN. dns.py is embedded
	// unchanged from the (already validated) pod path, runs as root on the
	// VM with Restart=always, and ConfigureNodes/wait-jobs exercise it.
	return nil
}

// dnsVMUserData embeds the generated dns.py and a systemd unit. The base
// image ships python3 (cloud images); binding :53 works because the VM runs
// as root — no SCC acrobatics at all (a pod-mode simplification we get for
// free).
func dnsVMUserData(hostname, script, pubKey string) string {
	authorized := ""
	if pubKey != "" {
		authorized = fmt.Sprintf("ssh_authorized_keys:\n  - %s\n", pubKey)
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%swrite_files:
  - path: /opt/sos-dns/dns.py
    permissions: '0755'
    content: |
%s  - path: /etc/systemd/system/sos-dns.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS nested DNS (authoritative for nested zone, forwards the rest)
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 /opt/sos-dns/dns.py
      Restart=always
      RestartSec=3
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl daemon-reload
  - systemctl enable --now sos-dns.service
`, hostname, authorized, indentBlock(script, "      "))
}

// indentBlock indents every line of script by prefix (cloud-config block
// scalar content).
func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = prefix + strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n") + "\n"
}

// resolveControlPlaneVIP returns the IP nested DNS must serve for
// api/api-int. HAProxy's cUDN lease is authoritative: VM->pod MCS traffic
// rides the OVN pipeline and is not filtered by the host's SYN-reject rules
// (master-0 -> haproxy:22623 == 200, zero exceptions). The relay VM is the
// fallback when haproxy is absent (its 22623 frontends are VM-socket legal).
func (s *StepExecutor) resolveControlPlaneVIP(ctx context.Context, nc *crd.NestedCluster) string {
	if nc.Status.APIVIPCUDN != "" {
		return nc.Status.APIVIPCUDN
	}
	if ip, err := s.client.WaitForDeploymentCUDNIP(ctx, nc.Spec.Namespace,
		nc.Spec.ClusterName+"-haproxy-api", 2*time.Minute); err == nil {
		nc.Status.APIVIPCUDN = ip
		return ip
	}
	if nc.Status.MCSRelayIP != "" {
		return nc.Status.MCSRelayIP
	}
	return nc.Spec.Network.APIVIP
}

// resolveIngressVIP returns the IP served for *.apps. Ingress fronts on
// unrestricted ports (80/443), so haproxy pods are fine there.
func (s *StepExecutor) resolveIngressVIP(ctx context.Context, nc *crd.NestedCluster) string {
	if nc.Status.IngressVIPCUDN != "" {
		return nc.Status.IngressVIPCUDN
	}
	if ip, err := s.client.WaitForDeploymentCUDNIP(ctx, nc.Spec.Namespace,
		nc.Spec.ClusterName+"-haproxy-ingress", 2*time.Minute); err == nil {
		nc.Status.IngressVIPCUDN = ip
		return ip
	}
	return nc.Spec.Network.IngressVIP
}
