package orchestrator

import (
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func vmWithRole(name, role string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": name}}}
	u.SetLabels(map[string]string{"oct-shift-on-shift-virt/role": role})
	return u
}

// Workers and infra restart before the masters; the bastion is never touched.
func TestNodeVMsOrder(t *testing.T) {
	got := nodeVMs([]unstructured.Unstructured{
		vmWithRole("cluster-master-1", "master"), vmWithRole("bastion", "bastion"), vmWithRole("cluster-worker-0", "worker"),
		vmWithRole("cluster-master-0", "master"), vmWithRole("cluster-infra-0", "infra"),
	})
	var names []string
	for _, vm := range got {
		names = append(names, vm.GetName())
	}
	if want := "cluster-worker-0 cluster-infra-0 cluster-master-0 cluster-master-1"; strings.Join(names, " ") != want {
		t.Fatalf("order = %v, want %s", names, want)
	}
}

func TestKeyValue(t *testing.T) {
	logs := "OK nested API reachable\nOK cluster-master-0 drained\nBOOTID=abc-123\n"
	if v := keyValue(logs, "BOOTID"); v != "abc-123" {
		t.Fatalf("got %q", v)
	}
	if v := keyValue("no id here", "BOOTID"); v != "" {
		t.Fatalf("got %q", v)
	}
}

func TestEnableStorageSummary(t *testing.T) {
	nc := &crd.NestedCluster{}
	nc.Spec.Virtualization.Enabled = true
	nc.Status.Conditions = []metav1.Condition{
		{Type: conditionStorageReady, Status: metav1.ConditionTrue},
		{Type: conditionVirtualizationReady, Status: metav1.ConditionFalse, Message: "no /dev/kvm"},
	}
	got := enableStorageSummary(nc)
	if !strings.Contains(got, "nested storage ready") || !strings.Contains(got, "OpenShift Virtualization failed: no /dev/kvm") {
		t.Fatalf("summary = %q", got)
	}
}

func TestBootIDs(t *testing.T) {
	got := bootIDs("noise\nBOOTID cluster-master-0 aaa\nBOOTID cluster-master-1 bbb\nBOOTID broken\n")
	if len(got) != 2 || got["cluster-master-0"] != "aaa" || got["cluster-master-1"] != "bbb" {
		t.Errorf("bootIDs = %v", got)
	}
}
