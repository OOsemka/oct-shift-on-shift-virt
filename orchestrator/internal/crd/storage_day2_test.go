package crd

import (
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyEnableStorage(t *testing.T) {
	var s NestedClusterSpec
	s.ApplyEnableStorage(EnableStorageRequest{InfraStorageClassName: "sc", AllowanceGi: 200})
	if !s.Storage.Enabled || s.Virtualization.Enabled || s.Storage.InfraStorageClassName != "sc" || s.Storage.AllowanceGi != 200 {
		t.Fatalf("storage only: %+v %+v", s.Storage, s.Virtualization)
	}
	s.ApplyEnableStorage(EnableStorageRequest{Virtualization: true, InfraStorageClassName: "other"})
	if !s.Virtualization.Enabled || s.Storage.InfraStorageClassName != "sc" {
		t.Fatalf("virtualization must keep the existing storage settings: %+v", s.Storage)
	}
	var v NestedClusterSpec
	v.ApplyEnableStorage(EnableStorageRequest{Virtualization: true})
	if !v.Storage.Enabled || !v.StorageWanted() {
		t.Fatal("virtualization must turn storage on")
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
	phases = append(phases, PhaseMakeMaster, PhaseMaster, PhaseRefreshMaster, PhaseEnableStorage, PhaseFailed, PhaseDeleting)
	for _, p := range phases {
		if !strings.Contains(crd, "\n                    - "+string(p)+"\n") {
			t.Errorf("deploy/crd.yaml status.phase enum is missing %q", p)
		}
	}
}
