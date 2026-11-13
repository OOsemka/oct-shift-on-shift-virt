package crd

import (
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyEnableStorage(t *testing.T) {
	var nc NestedCluster
	nc.ApplyEnableStorage(EnableStorageRequest{InfraStorageClassName: "sc", AllowanceGi: 200})
	if s := nc.Spec; !s.Storage.Enabled || s.Virtualization.Enabled || s.Storage.InfraStorageClassName != "sc" || s.Storage.AllowanceGi != 200 {
		t.Fatalf("storage only: %+v %+v", s.Storage, s.Virtualization)
	}
	// Storage that works keeps its settings.
	nc.Status.Conditions = []metav1.Condition{{Type: ConditionStorageReady, Status: metav1.ConditionTrue}}
	nc.ApplyEnableStorage(EnableStorageRequest{Virtualization: true, InfraStorageClassName: "other"})
	if !nc.Spec.Virtualization.Enabled || nc.Spec.Storage.InfraStorageClassName != "sc" {
		t.Fatalf("virtualization must keep working storage settings: %+v", nc.Spec.Storage)
	}
	var v NestedCluster
	v.ApplyEnableStorage(EnableStorageRequest{Virtualization: true})
	if !v.Spec.Storage.Enabled || !v.Spec.StorageWanted() {
		t.Fatal("virtualization must turn storage on")
	}
}

// A retry after a failed storage step uses the class and allowance the user
// picks now (the dialog shows the pickers again).
func TestApplyEnableStorageReplacesFailedStorage(t *testing.T) {
	nc := NestedCluster{Spec: NestedClusterSpec{Storage: StorageConfig{Enabled: true, InfraStorageClassName: "old", AllowanceGi: 100}}}
	nc.Status.Conditions = []metav1.Condition{{Type: ConditionStorageReady, Status: metav1.ConditionFalse}}
	if !nc.StorageSettingsApply() {
		t.Fatal("settings of storage that never became ready must apply")
	}
	nc.ApplyEnableStorage(EnableStorageRequest{InfraStorageClassName: "new", AllowanceGi: 300})
	if nc.Spec.Storage.InfraStorageClassName != "new" || nc.Spec.Storage.AllowanceGi != 300 {
		t.Fatalf("failed storage spec not replaced: %+v", nc.Spec.Storage)
	}
}

func TestDay2Pending(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	nc := NestedCluster{}
	if nc.Day2Pending(now) {
		t.Error("no annotation: not pending")
	}
	nc.Annotations = map[string]string{AnnotationDay2Pending: now.Add(-30 * time.Second).Format(time.RFC3339)}
	if !nc.Day2Pending(now) {
		t.Error("30s old: pending")
	}
	nc.Annotations[AnnotationDay2Pending] = now.Add(-3 * time.Minute).Format(time.RFC3339)
	if nc.Day2Pending(now) {
		t.Error("3m old: expired")
	}
	nc.Annotations[AnnotationDay2Pending] = "garbage"
	if nc.Day2Pending(now) {
		t.Error("unparsable: not pending")
	}
}

func TestEnableStorageRefusal(t *testing.T) {
	cond := func(typ string, st metav1.ConditionStatus) metav1.Condition {
		return metav1.Condition{Type: typ, Status: st}
	}
	nc := func(phase ClusterPhase, storage, virt bool, conds ...metav1.Condition) *NestedCluster {
		c := &NestedCluster{}
		c.Status.Phase, c.Spec.Storage.Enabled, c.Spec.Virtualization.Enabled, c.Status.Conditions = phase, storage, virt, conds
		return c
	}
	if nc(PhaseReady, false, false).EnableStorageRefusal(EnableStorageRequest{}) != "" {
		t.Error("a Ready cluster without storage must be allowed")
	}
	if nc(PhaseMaster, false, false).EnableStorageRefusal(EnableStorageRequest{}) == "" {
		t.Error("a master must be refused")
	}
	if nc(PhaseReady, true, false, cond(ConditionStorageReady, metav1.ConditionTrue)).EnableStorageRefusal(EnableStorageRequest{}) == "" {
		t.Error("working storage must be refused for a storage-only request")
	}
	if nc(PhaseReady, true, false, cond(ConditionStorageReady, metav1.ConditionTrue)).EnableStorageRefusal(EnableStorageRequest{Virtualization: true}) != "" {
		t.Error("virtualization on top of working storage must be allowed")
	}
	if nc(PhaseReady, true, false, cond(ConditionStorageReady, metav1.ConditionFalse)).EnableStorageRefusal(EnableStorageRequest{}) != "" {
		t.Error("a failed storage step must be repeatable")
	}
	if nc(PhaseReady, true, true, cond(ConditionStorageReady, metav1.ConditionTrue), cond(ConditionVirtualizationReady, metav1.ConditionTrue)).EnableStorageRefusal(EnableStorageRequest{Virtualization: true}) == "" {
		t.Error("working virtualization must be refused")
	}
}

// Every phase the code can write must be allowed by the CRD's status.phase
// enum, or the API server rejects the status update at run time.
func TestCRDPhaseEnumCoversAllPhases(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := string(b)
	phases := append(append([]ClusterPhase{}, PhaseOrder...), ClonePhaseOrder...)
	phases = append(phases, PhaseMakeMaster, PhaseMaster, PhaseRefreshMaster, PhaseEnableStorage, PhaseEnableVirtualization, PhaseFailed, PhaseDeleting)
	for _, p := range phases {
		if !strings.Contains(crd, "\n                    - "+string(p)+"\n") {
			t.Errorf("deploy/crd.yaml status.phase enum is missing %q", p)
		}
	}
}

func TestRestartModeOf(t *testing.T) {
	nc := &NestedCluster{}
	if m := nc.RestartModeOf(); m != RestartRolling {
		t.Errorf("no annotation: %q, want rolling", m)
	}
	nc.Annotations = map[string]string{AnnotationRestartMode: "garbage"}
	if m := nc.RestartModeOf(); m != RestartRolling {
		t.Errorf("unknown value: %q, want rolling", m)
	}
	nc.Annotations[AnnotationRestartMode] = RestartSimultaneous
	if m := nc.RestartModeOf(); m != RestartSimultaneous {
		t.Errorf("%q, want simultaneous", m)
	}
}
