package installer

import (
	"fmt"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

type HostConfig struct {
	Name string
	Role string
	MAC  string
	IP   string // DHCP-assigned IP read from VMI status (set after VM starts)
}

func GenerateInstallConfig(spec crd.NestedClusterSpec, hosts []HostConfig, pullSecret, sshKey string) string {
	clusterNetworkCIDR := spec.Network.ClusterNetworkCIDR
	if clusterNetworkCIDR == "" {
		clusterNetworkCIDR = "10.128.0.0/14"
	}
	serviceNetworkCIDR := spec.Network.ServiceNetworkCIDR
	if serviceNetworkCIDR == "" {
		serviceNetworkCIDR = "172.30.0.0/16"
	}

	workerReplicas := spec.Workers.Count
	if spec.Topology == "compact" {
		workerReplicas = 0
	}

	// Use platform: none with userManagedNetworking so the installer
	// does NOT validate VIPs on the L2 segment. Our HAProxy pods hold
	// the VIP IPs, and OVN port security means the agent ARP check
	// would see them as "already in use" and block installation.
	// After bootstrap, HAProxy provides the external LB.
	config := fmt.Sprintf(`apiVersion: v1
metadata:
  name: %s
baseDomain: %s
compute:
- architecture: amd64
  hyperthreading: Enabled
  name: worker
  replicas: %d
controlPlane:
  architecture: amd64
  hyperthreading: Enabled
  name: master
  replicas: %d
networking:
  clusterNetwork:
  - cidr: %s
    hostPrefix: 23
  machineNetwork:
  - cidr: %s
  serviceNetwork:
  - %s
  networkType: OVNKubernetes
platform:
  none: {}
pullSecret: '%s'
sshKey: '%s'`,
		spec.ClusterName,
		spec.BaseDomain,
		workerReplicas,
		spec.Masters.Count,
		clusterNetworkCIDR,
		spec.Network.CUDNSubnet,
		serviceNetworkCIDR,
		escapeSingleQuotes(pullSecret),
		sshKey,
	)

	return config
}

// GenerateAgentConfig creates agent-config.yaml for the agent-based installer.
// Hosts use DHCP from the cUDN (OVN IPAM) — no static networkConfig.
// The rendezvousIP is the DHCP-assigned IP of the first master, read from VMI status.
func GenerateAgentConfig(spec crd.NestedClusterSpec, hosts []HostConfig) string {
	rendezvousIP := ""
	if len(hosts) > 0 {
		rendezvousIP = hosts[0].IP
	}

	var hostEntries []string
	for _, h := range hosts {
		entry := fmt.Sprintf("  - hostname: %s\n    role: %s", h.Name, h.Role)
		if h.MAC != "" {
			entry += fmt.Sprintf(`
    interfaces:
    - name: enp1s0
      macAddress: %s`, h.MAC)
		}
		hostEntries = append(hostEntries, entry)
	}

	config := fmt.Sprintf(`apiVersion: v1alpha1
metadata:
  name: %s
rendezvousIP: %s
hosts:
%s`,
		spec.ClusterName,
		rendezvousIP,
		strings.Join(hostEntries, "\n"),
	)

	return config
}

func OcpMirrorChannel(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		return "stable-" + parts[0] + "." + parts[1]
	}
	return "stable-" + version
}

func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
