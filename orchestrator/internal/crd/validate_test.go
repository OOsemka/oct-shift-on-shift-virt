package crd

import (
	"strings"
	"testing"
)

func spec(apiIP, ingressIP, nad string) NestedClusterSpec {
	return NestedClusterSpec{
		Network: NetworkConfig{
			CUDNSubnet:         "10.100.0.0/16",
			APIVIP:             "10.100.0.10",
			IngressVIP:         "10.100.0.11",
			ClusterNetworkCIDR: "10.128.0.0/14",
			ServiceNetworkCIDR: "172.30.0.0/16",
		},
		ExternalAccess: ExternalAccessConfig{NADName: nad, APIIP: apiIP, IngressIP: ingressIP},
	}
}

func TestValidateNetwork(t *testing.T) {
	cases := []struct {
		name  string
		spec  NestedClusterSpec
		wants []string // substrings of the error; none means valid
	}{
		{"valid ", spec("192.0.2.10", "192.0.2.11", "default/vlan-a"), nil},
		{"blank ingress IP", spec("198.51.100.10", "", "default/vlan-b"),
			[]string{"external Ingress IP (externalAccess.ingressIP) is empty", "not assigned automatically"}},
		{"internal VIPs as external IPs", spec("10.100.0.10", "10.100.0.11", "default/vlan-c"),
			[]string{"external API IP 10.100.0.10 is the internal API VIP", "external Ingress IP 10.100.0.11 is the internal Ingress VIP"}},
		{"no NAD", spec("192.0.2.10", "192.0.2.11", ""), []string{"network (NAD) is not selected"}},
		{"same external IP twice", spec("192.0.2.10", "192.0.2.10", "default/x"), []string{"both 192.0.2.10"}},
		{"different VLAN subnets", spec("192.0.2.10", "192.0.3.11", "default/x"), []string{"not in the same /24 VLAN subnet"}},
		{"inside the service network", spec("172.30.0.50", "172.30.0.51", "default/x"), []string{"pod or service network 172.30.0.0/16"}},
		{"broadcast address", spec("192.0.2.10", "192.0.2.255", "default/x"), []string{"network or broadcast address"}},
		{"internal network too small", func() NestedClusterSpec {
			x := spec("192.0.2.10", "192.0.2.11", "default/x")
			x.Network.CUDNSubnet = "10.100.0.0/26"
			return x
		}(), []string{"must be a /24 or larger"}},
		{"internal network overlaps pods", func() NestedClusterSpec {
			x := spec("192.0.2.10", "192.0.2.11", "default/x")
			x.Network.CUDNSubnet = "10.128.0.0/16"
			x.Network.APIVIP = "10.128.0.10"
			x.Network.IngressVIP = "10.128.0.11"
			return x
		}(), []string{"overlaps the cluster (pod) network"}},
		{"not an IP", spec("api.example.com", "192.0.2.11", "default/x"), []string{`"api.example.com" is not an IPv4 address`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.ValidateNetwork()
			if len(tc.wants) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wants)
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestValidateNetworkPrefix(t *testing.T) {
	s := spec("198.51.100.6", "198.51.101.7", "default/x")
	if err := s.ValidateNetwork(); err == nil {
		t.Fatal("two addresses in different /24s must fail with the default prefix")
	}
	s.ExternalAccess.PrefixLength = 22
	if err := s.ValidateNetwork(); err != nil {
		t.Fatalf("same /22 must pass: %v", err)
	}
	s.ExternalAccess.PrefixLength = 31
	if err := s.ValidateNetwork(); err == nil || !strings.Contains(err.Error(), "between 8 and 30") {
		t.Fatalf("prefix 31 must fail, got %v", err)
	}
}

func TestValidateNetworkInternalVIPOutsideSubnet(t *testing.T) {
	s := spec("192.0.2.10", "192.0.2.11", "default/x")
	s.Network.CUDNSubnet = "10.200.0.0/16"
	if err := s.ValidateNetwork(); err == nil || !strings.Contains(err.Error(), "outside the internal network 10.200.0.0/16") {
		t.Fatalf("want internal VIP outside subnet error, got %v", err)
	}
}
