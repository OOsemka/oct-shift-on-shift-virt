package crd

import (
	"strings"
	"testing"
)

func TestBuildQuickSpec(t *testing.T) {
	pool := &IPPool{Name: "lab", Spec: IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Gateway: "192.0.2.1",
		Addresses: []string{"192.0.2.10-192.0.2.13"}}}
	used := spec("192.0.2.10", "192.0.2.11", "default/vlan-a")
	free := FreePoolAddresses(pool, []NestedClusterSpec{used})
	if strings.Join(free, ",") != "192.0.2.12,192.0.2.13" {
		t.Fatalf("free = %v", free)
	}
	s, problems := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab"}, true, pool, free, nil)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if s.Masters.Count != 3 || s.Masters.CPUCores != 8 || s.Topology != "compact" || s.Namespace != "quick-1-ns" {
		t.Errorf("sizing/topology: %+v", s.Masters)
	}
	ea := s.ExternalAccess
	if ea.APIIP != "192.0.2.12" || ea.IngressIP != "192.0.2.13" || ea.Gateway != "192.0.2.1" || ea.IPPool != "lab" || ea.NADName != "default/vlan-a" {
		t.Errorf("external access: %+v", ea)
	}
	for _, p := range []NodeSizing{s.Workers, s.Infra} {
		if p.Count != 0 || p.CPUCores < 1 || p.MemoryGi < 1 || p.DiskGi < 1 {
			t.Errorf("zero-count pools still need valid sizes (CRD minimums): %+v", p)
		}
	}
	if ea.HostRoutes {
		t.Error("host routes are opt-in (publishHostRoutes)")
	}
	if s2, _ := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab", PublishHostRoutes: true}, true, pool, free, nil); !s2.ExternalAccess.HostRoutes {
		t.Error("publishHostRoutes must set externalAccess.hostRoutes")
	}
	if s.PullSecret != "" || s.SSHKey != "" {
		t.Error("credentials must come from the lab defaults server-side, never in the spec")
	}
	// A quick spec must pass the same checks as the wizard.
	if err := s.ValidateNetwork(); err != nil {
		t.Errorf("quick spec fails ValidateNetwork: %v", err)
	}
	if err := s.CheckPool(pool); err != nil {
		t.Errorf("quick spec fails CheckPool: %v", err)
	}
}

func TestBuildQuickSpecProblems(t *testing.T) {
	pool := &IPPool{Name: "lab", Spec: IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Gateway: "192.0.2.1",
		Addresses: []string{"192.0.2.10"}}}
	_, problems := BuildQuickSpec("Bad_Name", LabSettings{IPPool: "lab"}, false, pool, []string{"192.0.2.10"}, nil)
	joined := strings.Join(problems, "; ")
	for _, want := range []string{"cluster name", "no pull secret", "no base domain", "no OpenShift version", "1 free address"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q: %s", want, joined)
		}
	}
	if _, p := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "gone"}, true, nil, nil, nil); !strings.Contains(strings.Join(p, ";"), `"gone" does not exist`) {
		t.Errorf("missing pool not reported: %v", p)
	}
	if _, p := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", MasterDiskGi: 100}, true, nil, nil, nil); !strings.Contains(strings.Join(p, ";"), "below the minimum") {
		t.Errorf("undersized nodes not reported: %v", p)
	}
	// Lab defaults saved before only 5.0 and 4.22 were offered.
	if _, p := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.20"}, true, nil, nil, nil); !strings.Contains(strings.Join(p, ";"), "not supported") {
		t.Errorf("unsupported lab default version not reported: %v", p)
	}
	if SuggestQuickName([]string{"quick-1", "quick-3"}) != "quick-2" {
		t.Error("SuggestQuickName must take the lowest free number")
	}
}

func TestQuickOverrides(t *testing.T) {
	pool := &IPPool{Name: "lab", Spec: IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Gateway: "192.0.2.1",
		Addresses: []string{"192.0.2.10-192.0.2.13"}}}
	s, _ := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "lab", BastionGraphical: true}, true, pool, []string{"192.0.2.10", "192.0.2.11"}, nil)
	if !s.Bastion.Graphical {
		t.Fatal("lab default desktop setting must apply")
	}
	no := false
	if p := ApplyQuickOverrides(&s, QuickOverrides{SSHPublicKey: testKey, Graphical: &no}); len(p) != 0 {
		t.Fatalf("valid overrides: %v", p)
	}
	if s.Bastion.Graphical || s.SSHKey != testKey {
		t.Fatalf("overrides not applied: graphical=%v key=%q", s.Bastion.Graphical, s.SSHKey)
	}
	if p := ApplyQuickOverrides(&s, QuickOverrides{SSHPublicKey: "not a key"}); len(p) != 1 {
		t.Fatalf("bad key must be a problem: %v", p)
	}
	yes := true
	if ApplyQuickOverrides(&s, QuickOverrides{HostRoutes: &yes}); !s.ExternalAccess.HostRoutes {
		t.Fatal("hostRoutes override not applied")
	}
	if ApplyQuickOverrides(&s, QuickOverrides{HostRoutes: &no}); s.ExternalAccess.HostRoutes {
		t.Fatal("hostRoutes=false must turn the lab default off")
	}
	if ApplyQuickOverrides(&s, QuickOverrides{HostRoutes: &yes, PublishAppRoutes: &yes}); !s.ExternalAccess.PublishAppRoutes {
		t.Fatal("publishAppRoutes override not applied")
	}
	if ApplyQuickOverrides(&s, QuickOverrides{HostRoutes: &no}); s.ExternalAccess.PublishAppRoutes {
		t.Fatal("publishing application routes needs host routes")
	}
	s2, _ := BuildQuickSpec("quick-2", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "lab"}, true, pool, []string{"192.0.2.12", "192.0.2.13"}, nil)
	if p := ApplyQuickOverrides(&s2, QuickOverrides{Graphical: &yes, DesktopUsername: "chris", DesktopPassword: "correct-horse"}); len(p) != 0 {
		t.Fatalf("valid desktop overrides: %v", p)
	}
	if !s2.Bastion.Graphical || s2.Bastion.Username != "chris" || s2.Bastion.Password != "correct-horse" {
		t.Fatalf("desktop overrides not applied: %+v", s2.Bastion)
	}
	if p := ApplyQuickOverrides(&s2, QuickOverrides{DesktopUsername: "root", DesktopPassword: "short"}); len(p) != 2 {
		t.Fatalf("root user and short password must both be problems: %v", p)
	}
	s3, _ := BuildQuickSpec("quick-3", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "lab"}, true, pool, []string{"192.0.2.12", "192.0.2.13"}, nil)
	ApplyQuickOverrides(&s3, QuickOverrides{DesktopUsername: "chris", DesktopPassword: "correct-horse"})
	if s3.Bastion.Password != "" || s3.Bastion.Username != "core" {
		t.Fatal("desktop credentials must be ignored without a desktop")
	}
}

func TestQuickNestedStorageAndVirt(t *testing.T) {
	pool := &IPPool{Name: "lab", Spec: IPPoolSpec{NADName: "default/vlan-a", PrefixLength: 24, Gateway: "192.0.2.1",
		Addresses: []string{"192.0.2.10-192.0.2.13"}}}
	base := LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "lab"}
	s, _ := BuildQuickSpec("q", base, true, pool, []string{"192.0.2.10", "192.0.2.11"}, nil)
	if s.Storage.Enabled || s.Virtualization.Enabled {
		t.Fatal("nested storage/virtualization must be off unless the profile turns them on")
	}
	v := base
	v.NestedVirtualization = true
	v.NestedStorageClass = "fast-rbd"
	v.NestedStorageAllowanceGi = 250
	s, _ = BuildQuickSpec("q", v, true, pool, []string{"192.0.2.10", "192.0.2.11"}, nil)
	if !s.Virtualization.Enabled || !s.Storage.Enabled || s.Storage.InfraStorageClassName != "fast-rbd" || s.Storage.AllowanceGi != 250 {
		t.Fatalf("virtualization must imply storage with the profile class/allowance: %+v %+v", s.Storage, s.Virtualization)
	}
}

func TestValidateClusterNamespace(t *testing.T) {
	for _, ok := range []string{"demo-ns", "quick-1-ns"} {
		if err := ValidateClusterNamespace(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "default", "oct-shift-on-shift-virt", "openshift-storage", "openshift", "kube-system", "Bad_NS"} {
		if ValidateClusterNamespace(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
