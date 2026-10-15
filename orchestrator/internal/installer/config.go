package installer

import (
	"fmt"
	"net"
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
%s`,
		spec.ClusterName,
		spec.BaseDomain,
		workerReplicas,
		spec.Masters.Count,
		clusterNetworkCIDR,
		spec.Network.CUDNSubnet,
		serviceNetworkCIDR,
		escapeSingleQuotes(pullSecret),
		SSHKeyYAML(sshKey),
	)

	return config
}

// ParseCIDRInfo extracts the gateway IP (first usable address) and prefix
// length from a CIDR string. OVN Layer2 uses the first IP in the subnet as
// the gateway (e.g. 10.100.0.0/16 → gateway 10.100.0.1, prefix 16).
func ParseCIDRInfo(cidr string) (gateway string, prefixLen int, err error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0, fmt.Errorf("parsing CIDR %q: %w", cidr, err)
	}

	networkIP := ipNet.IP.To4()
	if networkIP == nil {
		return "", 0, fmt.Errorf("only IPv4 CIDRs supported, got %q", cidr)
	}

	gw := make(net.IP, 4)
	copy(gw, networkIP)
	for i := 3; i >= 0; i-- {
		gw[i]++
		if gw[i] != 0 {
			break
		}
	}

	ones, _ := ipNet.Mask.Size()
	return gw.String(), ones, nil
}

// GenerateAgentConfig creates agent-config.yaml for the agent-based installer.
// NMState networkConfig with static IPs is NOT included because OVN-K DHCP on
// Primary cUDN networks always overrides NMState config. The DHCP lease from
// OVN wins regardless of what NMState specifies. We keep the interfaces block
// with MAC addresses so the agent can identify hosts.
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

// EscapeSingleQuotes is the exported version of escapeSingleQuotes for use
// when replacing <PULL_SECRET> placeholders in user-edited config overrides.
func EscapeSingleQuotes(s string) string {
	return escapeSingleQuotes(s)
}

// SSHKeyYAML renders the install-config sshKey entry. A single key stays a
// single-quoted scalar; MULTIPLE keys (user-supplied + the generated
// automation key) are emitted as a block scalar, because an embedded newline
// inside single quotes is invalid YAML.
//
// Multiple keys are safe downstream: the installer validates only the first
// key (validate.SSHPublicKey -> ssh.ParseAuthorizedKey ignores the rest) and
// passes the whole string through as ONE ignition SSHAuthorizedKey entry, so
// authorized_keys ends up with one key per line and sshd honours both.
func SSHKeyYAML(sshKey string) string {
	keys := []string{}
	for _, l := range strings.Split(sshKey, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keys = append(keys, l)
		}
	}
	if len(keys) <= 1 {
		k := ""
		if len(keys) == 1 {
			k = keys[0]
		}
		return "sshKey: '" + escapeSingleQuotes(k) + "'"
	}
	out := "sshKey: |-"
	for _, k := range keys {
		out += "\n  " + k
	}
	return out
}
