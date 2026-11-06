package crd

import (
	"fmt"
	"net"
	"strings"
)

// HostNodeAddress is one address of a host cluster node, read at runtime
// from the Kubernetes API (InternalIP / ExternalIP). Subnet is the node's
// primary interface network when OVN-Kubernetes publishes it
// (k8s.ovn.org/node-primary-ifaddr), for example 10.100.0.100/16.
type HostNodeAddress struct {
	Node    string `json:"node"`
	Address string `json:"address"`
	Subnet  string `json:"subnet,omitempty"`
}

// DefaultCUDNSubnet is the wizard's and the quick cluster's internal network
// on hosts where it is free.
const DefaultCUDNSubnet = "10.100.0.0/16"

// DefaultServiceNetworkCIDR is the nested service network of the wizard and
// the quick cluster (the pod network is DefaultClusterNetworkCIDR).
const DefaultServiceNetworkCIDR = "172.30.0.0/16"

// cudnCandidates are tried in order when the default internal network
// overlaps the host's node network. The default comes first so nothing
// changes on hosts where it is free. Deterministic on purpose: the wizard's
// suggestion, the quick preview and the quick create agree.
var cudnCandidates = []string{
	DefaultCUDNSubnet,
	"10.200.0.0/16",
	"10.210.0.0/16",
	"10.220.0.0/16",
	"10.230.0.0/16",
	"10.240.0.0/16",
	"10.250.0.0/16",
	"172.20.0.0/16",
	"172.21.0.0/16",
	"172.22.0.0/16",
	"192.168.0.0/16",
}

// InternalVIPs returns the internal API and Ingress VIPs of an internal
// network: base+10 and base+11, as in the wizard.
func InternalVIPs(cudnSubnet string) (apiVIP, ingressVIP string) {
	_, n, err := net.ParseCIDR(strings.TrimSpace(cudnSubnet))
	if err != nil || n.IP.To4() == nil {
		return "", ""
	}
	at := func(off byte) string {
		ip := make(net.IP, 4)
		copy(ip, n.IP.To4())
		ip[3] += off
		return ip.String()
	}
	return at(10), at(11)
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// hostOverlap describes the first way the internal network collides with the
// host's node network, or "" if it does not.
func hostOverlap(cudn *net.IPNet, host []HostNodeAddress) string {
	for _, h := range host {
		if ip := parseIPv4(h.Address); ip != nil && cudn.Contains(ip) {
			return fmt.Sprintf("it contains host node %s's address %s", h.Node, ip)
		}
	}
	for _, h := range host {
		if _, sub, err := net.ParseCIDR(strings.TrimSpace(h.Subnet)); err == nil && sub.IP.To4() != nil && cidrsOverlap(cudn, sub) {
			return fmt.Sprintf("it overlaps host node %s's network %s", h.Node, sub)
		}
	}
	return ""
}

// FreeCUDNSubnet returns the first candidate internal network that does not
// collide with the host's node network and the nested pod/service networks,
// or "" if none is free.
func FreeCUDNSubnet(host []HostNodeAddress, clusterNetworkCIDR, serviceNetworkCIDR string) string {
	var others []*net.IPNet
	for _, c := range []string{clusterNetworkCIDR, serviceNetworkCIDR} {
		if _, o, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			others = append(others, o)
		}
	}
next:
	for _, c := range cudnCandidates {
		_, cand, _ := net.ParseCIDR(c)
		for _, o := range others {
			if cidrsOverlap(cand, o) {
				continue next
			}
		}
		if hostOverlap(cand, host) == "" {
			return c
		}
	}
	return ""
}

// CheckHostNetwork refuses an internal network (cUDN) that overlaps the host
// cluster's node network. OVN-Kubernetes requires UDN subnets not to overlap
// the node network; when they do, DNS through the bastion still works but
// pods and VMs on the UDN get no egress, so the agent installer's
// connectivity check fails and the install never starts (seen on a host that
// is itself a nested cluster on 10.100.0.0/16).
func (s *NestedClusterSpec) CheckHostNetwork(host []HostNodeAddress) error {
	_, cudn, err := net.ParseCIDR(strings.TrimSpace(s.Network.CUDNSubnet))
	if err != nil || cudn.IP.To4() == nil {
		return nil // ValidateNetwork reports this
	}
	why := hostOverlap(cudn, host)
	if why == "" {
		return nil
	}
	msg := fmt.Sprintf("internal network %s overlaps the host cluster's node network (%s); OVN-Kubernetes needs the cluster's network to stay clear of the host nodes, otherwise its pods and VMs have no internet access", cudn, why)
	if free := FreeCUDNSubnet(host, s.Network.ClusterNetworkCIDR, s.Network.ServiceNetworkCIDR); free != "" {
		msg += fmt.Sprintf(": use a free range such as %s", free)
	} else {
		msg += ": choose a range the host nodes do not use"
	}
	return fmt.Errorf("invalid network settings: %s", msg)
}
