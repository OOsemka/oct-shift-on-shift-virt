package crd

import (
	"strings"
	"testing"
)

func pool(addrs ...string) IPPoolSpec {
	return IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Addresses: addrs, Gateway: "192.0.2.1"}
}

func TestPoolExpand(t *testing.T) {
	got, err := pool("192.0.2.12", "192.0.2.10-192.0.2.12", " 192.0.2.20 ").Expand()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "192.0.2.10,192.0.2.11,192.0.2.12,192.0.2.20" {
		t.Fatalf("got %v", got)
	}
	for _, bad := range []string{"192.0.2.12-192.0.2.10", "192.0.2.x", "10.0.0.0-10.0.255.255"} {
		if _, err := pool(bad).Expand(); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestPoolValidate(t *testing.T) {
	if err := pool("192.0.2.10-192.0.2.40").Validate(); err != nil {
		t.Fatalf("valid pool: %v", err)
	}
	cases := map[string]struct {
		p    IPPoolSpec
		want string
	}{
		"no NAD":          {IPPoolSpec{PrefixLength: 24, Addresses: []string{"192.0.2.10"}}, "network (NAD) is required"},
		"project NAD":     {IPPoolSpec{NADName: "team-a/vlan-a", PrefixLength: 24, Addresses: []string{"192.0.2.10"}}, `must be a NAD in the "default" namespace`},
		"bare NAD name":   {IPPoolSpec{NADName: "vlan-a", PrefixLength: 24, Addresses: []string{"192.0.2.10"}}, "not a valid default/<name> reference"},
		"no addresses":    {pool(), "at least one address"},
		"two subnets":     {pool("192.0.2.10", "192.0.3.10"), "not in the same /24 subnet"},
		"broadcast":       {pool("192.0.2.250-192.0.2.255"), "network or broadcast"},
		"gateway in pool": {pool("192.0.2.1-192.0.2.5"), "gateway 192.0.2.1 must not be one of"},
		"gateway outside": {IPPoolSpec{NADName: "default/x", PrefixLength: 24, Addresses: []string{"192.0.2.10"}, Gateway: "192.0.3.1"}, "outside the pool subnet"},
		"bad prefix":      {IPPoolSpec{NADName: "default/x", PrefixLength: 31, Addresses: []string{"192.0.2.10"}}, "between 8 and 30"},
	}
	for name, tc := range cases {
		err := tc.p.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

func TestAddressConflictsAndPool(t *testing.T) {
	existing := spec("192.0.2.10", "192.0.2.11", "default/vlan-a")
	existing.ClusterName = "first"
	other := spec("192.0.2.10", "192.0.2.11", "default/vlan-b") // same IPs, different network
	other.ClusterName = "elsewhere"

	s := spec("192.0.2.11", "192.0.2.12", "default/vlan-a")
	err := s.CheckAddressesFree([]NestedClusterSpec{existing, other})
	if err == nil || !strings.Contains(err.Error(), "192.0.2.11 is already used by cluster first (Ingress)") {
		t.Fatalf("want conflict with first, got %v", err)
	}
	s2 := spec("192.0.2.12", "192.0.2.13", "default/vlan-a")
	if err := s2.CheckAddressesFree([]NestedClusterSpec{existing, other}); err != nil {
		t.Fatalf("free addresses: %v", err)
	}

	p := &IPPool{Name: "lab", Spec: pool("192.0.2.10-192.0.2.20")}
	s2.ExternalAccess.IPPool = "lab"
	if err := s2.CheckPool(p); err != nil {
		t.Fatalf("matching pool: %v", err)
	}
	s3 := spec("192.0.2.12", "192.0.2.30", "default/vlan-b")
	err = s3.CheckPool(p)
	if err == nil || !strings.Contains(err.Error(), "not the pool's network") || !strings.Contains(err.Error(), "192.0.2.30 is not in pool") {
		t.Fatalf("want network and membership errors, got %v", err)
	}
	if err := s3.CheckPool(nil); err == nil {
		t.Fatal("missing pool must fail")
	}
}
