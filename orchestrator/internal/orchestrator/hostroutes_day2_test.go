package orchestrator

import (
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hostRoutes set on a cluster that is already Ready must publish the API and
// console once per spec generation (PublishRoutes only runs during the install).
func TestHostRoutesPending(t *testing.T) {
	ready := func() *crd.NestedCluster {
		nc := &crd.NestedCluster{}
		nc.Generation = 4
		nc.Status.Phase = crd.PhaseReady
		nc.Spec.ExternalAccess.HostRoutes = true
		return nc
	}
	withCond := func(nc *crd.NestedCluster, status metav1.ConditionStatus, gen int64) *crd.NestedCluster {
		nc.Status.Conditions = append(nc.Status.Conditions, metav1.Condition{
			Type: conditionHostRoutesReady, Status: status, Reason: "PublishFailed", ObservedGeneration: gen,
		})
		return nc
	}
	now := metav1.Now()
	cases := []struct {
		name string
		nc   *crd.NestedCluster
		want bool
	}{
		{"turned on after the install", ready(), true},
		{"off", func() *crd.NestedCluster { n := ready(); n.Spec.ExternalAccess.HostRoutes = false; return n }(), false},
		{"already published", func() *crd.NestedCluster { n := ready(); n.Status.HostAPIURL = "https://api.example"; return n }(), false},
		{"not Ready yet (install phase publishes)", func() *crd.NestedCluster { n := ready(); n.Status.Phase = crd.PhaseWaitInstall; return n }(), false},
		{"being deleted", func() *crd.NestedCluster { n := ready(); n.DeletionTimestamp = &now; return n }(), false},
		{"failed for this generation: no retry loop", withCond(ready(), metav1.ConditionFalse, 4), false},
		{"failed for an older generation: spec edited since", withCond(ready(), metav1.ConditionFalse, 3), true},
	}
	for _, c := range cases {
		if got := hostRoutesPending(c.nc); got != c.want {
			t.Errorf("%s: hostRoutesPending = %v, want %v", c.name, got, c.want)
		}
	}
}
