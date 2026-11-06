package crd

import (
	"strings"
	"testing"
)

// A host that is itself a nested cluster on 10.100.0.0/16 (seen live: its
// nodes at .100-.102, OVN reporting a /16 node network).
var nestedHost = []HostNodeAddress{
	{Node: "cluster-master-0", Address: "10.100.0.100", Subnet: "10.100.0.0/16"},
	{Node: "cluster-master-1", Address: "10.100.0.101", Subnet: "10.100.0.0/16"},
	{Node: "cluster-master-2", Address: "10.100.0.102", Subnet: "10.100.0.0/16"},
}

func TestCheckHostNetworkOverlap(t *testing.T) {
	s := spec("192.0.2.10", "192.0.2.11", "default/x")
	err := s.CheckHostNetwork(nestedHost)
	if err == nil {
		t.Fatal("a cUDN containing the host node addresses must be refused")
	}
	for _, want := range []string{"10.100.0.0/16", "cluster-master-0", "10.100.0.100", "such as 10.200.0.0/16"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}

	// A smaller cUDN without node addresses still overlaps the node network.
	s.Network.CUDNSubnet, s.Network.APIVIP, s.Network.IngressVIP = "10.100.5.0/24", "10.100.5.10", "10.100.5.11"
	if err := s.CheckHostNetwork(nestedHost); err == nil || !strings.Contains(err.Error(), "network 10.100.0.0/16") {
		t.Errorf("overlap with the node network must be refused, got %v", err)
	}
	// Without the OVN subnet only the addresses count.
	addrOnly := []HostNodeAddress{{Node: "n", Address: "10.100.0.100"}}
	if err := s.CheckHostNetwork(addrOnly); err != nil {
		t.Errorf("no node address inside 10.100.5.0/24, got %v", err)
	}

	// A free range passes.
	s.Network.CUDNSubnet, s.Network.APIVIP, s.Network.IngressVIP = "10.200.0.0/16", "10.200.0.10", "10.200.0.11"
	if err := s.CheckHostNetwork(nestedHost); err != nil {
		t.Errorf("10.200.0.0/16 is clear of the host, got %v", err)
	}
	// Usual hosts (nodes elsewhere, or none readable) keep the default.
	s = spec("192.0.2.10", "192.0.2.11", "default/x")
	if err := s.CheckHostNetwork([]HostNodeAddress{{Node: "w", Address: "192.0.2.50", Subnet: "192.0.2.0/24"}}); err != nil {
		t.Errorf("default subnet on a normal host must pass, got %v", err)
	}
	if err := s.CheckHostNetwork(nil); err != nil {
		t.Errorf("no host addresses: %v", err)
	}
}

func TestFreeCUDNSubnet(t *testing.T) {
	if got := FreeCUDNSubnet(nil, DefaultClusterNetworkCIDR, DefaultServiceNetworkCIDR); got != DefaultCUDNSubnet {
		t.Errorf("normal host must keep the default, got %s", got)
	}
	if got := FreeCUDNSubnet(nestedHost, DefaultClusterNetworkCIDR, DefaultServiceNetworkCIDR); got != "10.200.0.0/16" {
		t.Errorf("nested host: got %s, want 10.200.0.0/16", got)
	}
	// Candidates overlapping the nested pod/service networks are skipped.
	if got := FreeCUDNSubnet(nestedHost, "10.200.0.0/14", DefaultServiceNetworkCIDR); got != "10.210.0.0/16" {
		t.Errorf("pod network on 10.200/14: got %s", got)
	}
	// Nothing free.
	all := []HostNodeAddress{{Node: "n", Address: "10.0.0.1", Subnet: "0.0.0.0/0"}}
	if got := FreeCUDNSubnet(all, DefaultClusterNetworkCIDR, DefaultServiceNetworkCIDR); got != "" {
		t.Errorf("want none free, got %s", got)
	}
	s := spec("192.0.2.10", "192.0.2.11", "default/x")
	if err := s.CheckHostNetwork(all); err == nil || !strings.Contains(err.Error(), "choose a range the host nodes do not use") {
		t.Errorf("want a refusal without a suggestion, got %v", err)
	}
	// Every candidate is a valid internal network on its own.
	for _, c := range cudnCandidates {
		x := spec("192.0.2.10", "192.0.2.11", "default/x")
		x.Network.CUDNSubnet = c
		x.Network.APIVIP, x.Network.IngressVIP = InternalVIPs(c)
		if err := x.ValidateNetwork(); err != nil {
			t.Errorf("candidate %s: %v", c, err)
		}
	}
}

func TestInternalVIPs(t *testing.T) {
	if a, i := InternalVIPs("10.200.0.0/16"); a != "10.200.0.10" || i != "10.200.0.11" {
		t.Errorf("got %s %s", a, i)
	}
	if a, i := InternalVIPs("nope"); a != "" || i != "" {
		t.Errorf("invalid CIDR: got %s %s", a, i)
	}
}

func TestBuildQuickSpecAvoidsHostNetwork(t *testing.T) {
	pool := &IPPool{Name: "lab", Spec: IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Gateway: "192.0.2.1",
		Addresses: []string{"192.0.2.10-192.0.2.13"}}}
	settings := LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab"}
	free := []string{"192.0.2.10", "192.0.2.11"}

	s, problems := BuildQuickSpec("quick-1", settings, true, pool, free, nil)
	if len(problems) != 0 || s.Network.CUDNSubnet != DefaultCUDNSubnet || s.Network.APIVIP != "10.100.0.10" || s.Network.IngressVIP != "10.100.0.11" {
		t.Errorf("normal host must keep the default: %+v %v", s.Network, problems)
	}

	s, problems = BuildQuickSpec("quick-1", settings, true, pool, free, nestedHost)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if s.Network.CUDNSubnet != "10.200.0.0/16" || s.Network.APIVIP != "10.200.0.10" || s.Network.IngressVIP != "10.200.0.11" {
		t.Errorf("nested host: %+v", s.Network)
	}
	if err := s.ValidateNetwork(); err != nil {
		t.Errorf("ValidateNetwork: %v", err)
	}
	if err := s.CheckHostNetwork(nestedHost); err != nil {
		t.Errorf("CheckHostNetwork: %v", err)
	}

	all := []HostNodeAddress{{Node: "n", Address: "10.0.0.1", Subnet: "0.0.0.0/0"}}
	if _, p := BuildQuickSpec("quick-1", settings, true, pool, free, all); !strings.Contains(strings.Join(p, ";"), "no default internal network is free") {
		t.Errorf("want a problem when no candidate is free, got %v", p)
	}
}
