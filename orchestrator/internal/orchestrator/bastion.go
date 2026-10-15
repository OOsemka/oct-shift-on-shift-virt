package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// bastionBaseImage is the DataSource cloned for the helper VM. Cluster
// templates in openshift-virtualization-os-images are always present with
// OpenShift Virtualization and are referenceable from any namespace.
func bastionBaseImage() string {
	if v := os.Getenv("BASTION_BASE_IMAGE"); v != "" {
		return v
	}
	return "centos-stream9"
}

// reserveBastionIP records the bastion's RESERVED address on status before the
// VM exists.
//
// This is what lets three helper VMs collapse into one. The old MCSRelay phase
// had to create its VM *before* DeployHAProxy, because HAProxy's generated
// config needs the relay address and a later rewrite rolls the HAProxy pod —
// which changes its cUDN lease and silently stales the DNS records. But DNS
// records need that same lease, so a single VM could never have both baked at
// cloud-init time.
//
// Since the deterministic ipPlan reserves the address up front (preseeded
// IPAMClaim, created in CreateVMs), HAProxy can be pointed at the bastion
// before it boots. The ordering constraint dissolves: HAProxy comes up first
// with a correct first config, and the bastion is then created once, with the
// MCS backends AND the DNS records both known.
func (s *StepExecutor) reserveBastionIP(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion")
	plan, err := computeIPPlan(nc.Spec.ClusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP)
	if err != nil {
		return fmt.Errorf("computing IP plan: %w", err)
	}
	nc.Status.MCSRelayIP = plan.Bastion.String()
	s.updateCRStatus(ctx, nc)
	log.Info("bastion address reserved; HAProxy MCS backends will point here", "ip", nc.Status.MCSRelayIP)

	// Retry/resume path only: if HAProxy's config secret already exists it was
	// generated before this reservation and must be refreshed.
	return s.rewireHAProxyMCSToRelay(ctx, nc, nc.Status.MCSRelayIP)
}

// deployBastion provisions the single <cluster>-bastion VM, replacing the
// three single-purpose helpers (dns-vm, mcs-relay, jumpbox):
//
//   - nested DNS on :53 — authoritative for the nested zone, forwards the
//     rest. Must be a claim-stable VM: GenerateISO bakes nameserver=<this ip>
//     into the agent ISO as a kernel argument, and an unclaimed pod would
//     drift its lease and strand every pre-pivot guest.
//   - MCS relay on :12262/:12264 -> masters 22623/22624. Must be a VM: the
//     hypervisors' nftables reject SYNs to 22623/22624 from host- and
//     pod-originated traffic, so HAProxy (a pod) cannot reach MCS directly,
//     while VM->VM traffic never traverses those hooks.
//   - sshd for operators (jumpbox duty) — diagnostics only, free to co-host.
//
// Nothing conflicts: three daemons, three port ranges, one 1-vCPU VM.
func (s *StepExecutor) deployBastion(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion")
	clusterNs := nc.Spec.Namespace

	var masterIPs []string
	for _, n := range nc.Status.Nodes {
		if n.IP != "" {
			masterIPs = append(masterIPs, n.IP)
		}
	}
	if len(masterIPs) == 0 {
		return fmt.Errorf("no node IPs in status for the bastion's MCS backends")
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}

	pubKey := ""
	if b, err := s.client.GetSecretData(ctx, clusterNs, nc.Spec.ClusterName+"-ssh-key", "ssh-publickey"); err == nil {
		pubKey = strings.TrimSpace(string(b))
	}

	// DNS records: HAProxy has already published its cUDN leases by now.
	apiVIP := s.resolveControlPlaneVIP(ctx, nc)
	ingressVIP := s.resolveIngressVIP(ctx, nc)
	if apiVIP == "" || ingressVIP == "" {
		return fmt.Errorf("cannot build nested DNS records: apiVIP=%q ingressVIP=%q (DeployHAProxy must run first)", apiVIP, ingressVIP)
	}
	localZone := nc.Spec.ClusterName + "." + nc.Spec.BaseDomain
	dnsScript := s.generateDNSScript(apiVIP, ingressVIP, localZone)

	name := bastionVMName(nc.Spec.ClusterName)
	if err := s.client.CreateInfraVM(ctx, k8s.InfraVMParams{
		Name:             name,
		Namespace:        clusterNs,
		ClusterName:      nc.Spec.ClusterName,
		Cores:            1,
		MemoryGi:         2,
		DiskGi:           32, // the centos-stream9 DataSource is 30Gi; a clone must be >= source
		StorageClassName: nc.Spec.StorageClassName,
		DataSourceNS:     "openshift-virtualization-os-images",
		DataSourceName:   bastionBaseImage(),
		UserData:         bastionUserData(name, pubKey, dnsScript, masterIPs),
	}); err != nil {
		return fmt.Errorf("creating bastion VM: %w", err)
	}

	log.Info("waiting for bastion VM to boot", "vm", name, "api_vip", apiVIP, "ingress_vip", ingressVIP)
	ip, err := s.client.WaitForVMIReady(ctx, clusterNs, name, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("bastion VM never got an IP: %w", err)
	}

	// The address is preseeded via IPAMClaim and is baked into the agent ISO
	// karg and the HAProxy MCS backends. A drift here means the claim was
	// ignored and every pre-pivot guest would resolve against nothing — fail
	// loudly rather than install a cluster that cannot boot.
	plan, perr := computeIPPlan(nc.Spec.ClusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP)
	if perr == nil && plan.Bastion.String() != ip {
		return fmt.Errorf("bastion got IP %s but the reserved allocation is %s (check IPAMClaim %s.default)",
			ip, plan.Bastion.String(), name)
	}

	nc.Status.DNSIPCUDN = ip
	nc.Status.MCSRelayIP = ip
	s.updateCRStatus(ctx, nc)
	log.Info("bastion up: nested DNS + MCS relay + jumpbox", "ip", ip)

	// Operators find the box without digging through logs.
	ann := map[string]string{
		"sos.octools.io/bastion":        "ssh core@" + ip,
		"sos.octools.io/bastion-access": "oc -n " + clusterNs + " run diag --rm -it --restart=Never --image=" + sshHelperImage() + " --command -- bash  # in-ns pods reach every cUDN IP",
	}
	if patchBody, merr := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"annotations": ann}}); merr != nil {
		log.Info("failed to encode bastion annotation patch", "error", merr.Error())
	} else if _, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Patch(ctx, nc.Name, types.MergePatchType, patchBody, metav1.PatchOptions{}); err != nil {
		log.Info("failed to annotate CR with the bastion address (non-fatal)", "error", err.Error())
	}
	return nil
}

// bastionUserData renders cloud-init for the combined helper VM: the nested
// DNS server, the MCS relay and the operator SSH key. python3 ships in the
// cloud image; the VM runs as root so binding :53 needs no privilege work.
func bastionUserData(hostname, pubKey, dnsScript string, masterIPs []string) string {
	masters := strings.Join(masterIPs, " ")
	authorized := ""
	if pubKey != "" {
		// Multiple keys (the user's plus the generated automation key) arrive
		// newline-separated; cloud-init wants a YAML list.
		var items []string
		for _, k := range strings.Split(pubKey, "\n") {
			if k = strings.TrimSpace(k); k != "" {
				items = append(items, "  - "+k)
			}
		}
		if len(items) > 0 {
			authorized = "ssh_authorized_keys:\n" + strings.Join(items, "\n") + "\n"
		}
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%swrite_files:
  - path: /opt/sos-dns/dns.py
    permissions: '0755'
    content: |
%s  - path: /opt/mcs-relay/relay.py
    permissions: '0755'
    content: |
%s  - path: /etc/systemd/system/sos-dns.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS nested DNS (authoritative for the nested zone, forwards the rest)
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 /opt/sos-dns/dns.py
      Restart=always
      RestartSec=3
      [Install]
      WantedBy=multi-user.target
  - path: /etc/systemd/system/mcs-relay.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS MCS relay (12262/12264 -> masters 22623/22624)
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 /opt/mcs-relay/relay.py %s
      Restart=always
      RestartSec=3
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl daemon-reload
  - systemctl enable --now sos-dns.service
  - systemctl enable --now mcs-relay.service
`, hostname, authorized, indentBlock(dnsScript, "      "), indentBlock(mcsRelayScript, "      "), masters)
}
