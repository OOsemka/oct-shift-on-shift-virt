package orchestrator

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
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
