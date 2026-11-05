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
	s, problems := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab"}, true, pool, free)
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
	_, problems := BuildQuickSpec("Bad_Name", LabSettings{IPPool: "lab"}, false, pool, []string{"192.0.2.10"})
	joined := strings.Join(problems, "; ")
	for _, want := range []string{"cluster name", "no pull secret", "no base domain", "no OpenShift version", "1 free address"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q: %s", want, joined)
		}
	}
	if _, p := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", IPPool: "gone"}, true, nil, nil); !strings.Contains(strings.Join(p, ";"), `"gone" does not exist`) {
		t.Errorf("missing pool not reported: %v", p)
	}
	if _, p := BuildQuickSpec("quick-1", LabSettings{BaseDomain: "x.test", OCPVersion: "4.22", MasterDiskGi: 100}, true, nil, nil); !strings.Contains(strings.Join(p, ";"), "below the minimum") {
		t.Errorf("undersized nodes not reported: %v", p)
	}
	if SuggestQuickName([]string{"quick-1", "quick-3"}) != "quick-2" {
		t.Error("SuggestQuickName must take the lowest free number")
	}
}
