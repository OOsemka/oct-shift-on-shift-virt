package installer

import (
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

func TestInstallConfigNetworkType(t *testing.T) {
	for in, want := range map[string]string{"": "OVNKubernetes", "Calico": "Calico", "Cilium": "Cilium"} {
		spec := crd.NestedClusterSpec{ClusterName: "c", BaseDomain: "d", Masters: crd.NodeSizing{Count: 3},
			Network: crd.NetworkConfig{CUDNSubnet: "10.100.0.0/16", NetworkType: in}}
		cfg := GenerateInstallConfig(spec, nil, "{}", "ssh-ed25519 AAAA")
		if !strings.Contains(cfg, "  networkType: "+want+"\n") {
			t.Errorf("networkType %q: want %s in\n%s", in, want, cfg)
		}
	}
}
