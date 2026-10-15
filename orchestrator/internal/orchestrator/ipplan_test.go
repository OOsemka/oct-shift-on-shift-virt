package orchestrator

import "testing"

func TestComputeIPPlanSmoke(t *testing.T) {
	p, err := computeIPPlan("sos-nightly", "10.101.0.0/16", 3, "192.0.2.106", "192.0.2.108")
	if err != nil {
		t.Fatal(err)
	}
	if p.NetworkName != "cluster_udn_sos-nightly" || p.Prefix != 16 {
		t.Fatalf("network/prefix: %s/%d", p.NetworkName, p.Prefix)
	}
	// One bastion VM now carries DNS + MCS relay + jumpbox; it keeps the old
	// mcs-relay offset so existing clusters' IPAMClaims stay valid.
	if p.Bastion.String() != "10.101.0.12" {
		t.Fatalf("bastion offset: %s", p.Bastion)
	}
	want := []string{"10.101.0.100", "10.101.0.101", "10.101.0.102"}
	for i, w := range want {
		if p.Masters[i].String() != w {
			t.Fatalf("master-%d = %s want %s", i, p.Masters[i], w)
		}
	}

	// gateway collision must be impossible to express via a too-small subnet
	if _, err := computeIPPlan("x", "10.101.0.0/26", 3, "", ""); err == nil {
		t.Fatal("expected size guard failure on /26 (masters offset outside)")
	}
	// apiVIP inside the cUDN subnet must be rejected
	if _, err := computeIPPlan("x", "10.101.0.0/16", 3, "10.101.0.101", ""); err == nil {
		t.Fatal("expected VIP/plan collision failure")
	}
	// a CR VIP landing on the bastion address must also be rejected
	if _, err := computeIPPlan("x", "10.101.0.0/16", 3, "10.101.0.12", ""); err == nil {
		t.Fatal("expected VIP/bastion collision failure")
	}
	// /29 too small even for infra offsets
	if _, err := computeIPPlan("x", "10.101.0.0/29", 1, "", ""); err == nil {
		t.Fatal("expected size guard failure on /29")
	}
	// gateway itself: a /16 at base .0 never assigns +1 to anything we create
	if p.Bastion.String() == "10.101.0.1" {
		t.Fatal("bastion sits on the gateway")
	}
}
