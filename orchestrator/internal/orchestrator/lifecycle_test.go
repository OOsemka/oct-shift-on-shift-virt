package orchestrator

import (
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

// The CRD constrains these fields with enums. A value outside them makes the
// ENTIRE status update invalid, which silently freezes the CR mid-install.
func TestNodeLifecycleValuesAreInCRDEnums(t *testing.T) {
	validBoot := map[string]bool{"ISO": true, "Disk": true, "Unknown": true}
	validAgent := map[string]bool{
		"Pending": true, "Discovering": true, "Installing": true,
		"Installed": true, "Error": true, "Unknown": true,
	}
	// every pair the orchestrator actually sets
	for _, tc := range [][2]string{
		{"ISO", "Discovering"},
		{"Disk", "Installing"},
		{"Disk", "Installed"},
		{"Unknown", "Pending"},
	} {
		nc := &crd.NestedCluster{}
		nc.Status.Nodes = []crd.NodeStatus{{Name: "n"}}
		setNodeLifecycle(nc, tc[0], tc[1])
		got := nc.Status.Nodes[0]
		if !validBoot[got.BootPhase] {
			t.Fatalf("bootPhase %q is not in the CRD enum", got.BootPhase)
		}
		if !validAgent[got.AgentStatus] {
			t.Fatalf("agentStatus %q is not in the CRD enum", got.AgentStatus)
		}
	}
	// empty leaves values untouched
	nc := &crd.NestedCluster{}
	nc.Status.Nodes = []crd.NodeStatus{{BootPhase: "ISO", AgentStatus: "Installing"}}
	setNodeLifecycle(nc, "", "")
	if nc.Status.Nodes[0].BootPhase != "ISO" || nc.Status.Nodes[0].AgentStatus != "Installing" {
		t.Fatal("empty values must not overwrite")
	}
}

// Guard the source itself: a future edit that introduces a prose value would
// reproduce the freeze.
func TestNoProseLifecycleValuesInSource(t *testing.T) {
	for _, bad := range []string{`"Booting (agent ISO)"`, `"Installing (pivoted)"`, `"Discovered"`, `"Done"`} {
		if strings.Contains(lifecycleCallSites, bad) {
			t.Fatalf("lifecycle call sites must use CRD enum values, found %s", bad)
		}
	}
}
