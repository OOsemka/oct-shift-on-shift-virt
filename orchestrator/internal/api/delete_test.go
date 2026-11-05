package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A delete clicked while the install is busy updating the status used to
// fail on the status-write conflict and delete nothing. It must delete anyway.
func TestDeleteSurvivesStatusConflict(t *testing.T) {
	nc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "octools.io/v1alpha1", "kind": "NestedCluster",
		"metadata": map[string]interface{}{"name": "busy", "namespace": clusterNamespace},
		"spec":     map[string]interface{}{"clusterName": "busy"},
		"status":   map[string]interface{}{"phase": "CollectIPs"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nestedClusterGVR: "NestedClusterList"}, nc)
	conflicts := 0
	dyn.PrependReactor("update", "nestedclusters", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "status" {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "octools.io", Resource: "nestedclusters"}, "busy", nil)
		}
		return false, nil, nil
	})

	srv := NewServer(&k8s.Client{DynamicClient: dyn}, logging.New())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/clusters/busy", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", rec.Code, rec.Body.String())
	}
	if conflicts < 2 {
		t.Errorf("status write should be retried on conflict, tried %d time(s)", conflicts)
	}
	if _, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(t.Context(), "busy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cluster was not deleted despite the status conflict (err=%v)", err)
	}
}
