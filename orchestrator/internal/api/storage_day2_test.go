package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func day2Cluster(phase string, spec map[string]interface{}, conds ...map[string]interface{}) *unstructured.Unstructured {
	s := map[string]interface{}{"clusterName": "c1"}
	for k, v := range spec {
		s[k] = v
	}
	status := map[string]interface{}{"phase": phase}
	if len(conds) > 0 {
		cs := make([]interface{}, len(conds))
		for i, c := range conds {
			cs[i] = c
		}
		status["conditions"] = cs
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "octools.io/v1alpha1", "kind": "NestedCluster",
		"metadata": map[string]interface{}{"name": "c1", "namespace": clusterNamespace},
		"spec":     s, "status": status,
	}}
}

func ready(t string) map[string]interface{} {
	return map[string]interface{}{"type": t, "status": "True", "reason": "x", "message": "ok", "lastTransitionTime": "2026-11-12T00:00:00Z"}
}

func postEnableStorage(t *testing.T, nc *unstructured.Unstructured, body string) (*httptest.ResponseRecorder, *unstructured.Unstructured) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nestedClusterGVR: "NestedClusterList"}, nc)
	kube := kubefake.NewSimpleClientset(&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "host-rbd"}})
	srv := NewServer(&k8s.Client{DynamicClient: dyn, Clientset: kube}, logging.New())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/clusters/c1/enable-storage", strings.NewReader(body)))
	after, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "c1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rec, after
}

func TestEnableVirtualizationImpliesStorage(t *testing.T) {
	rec, after := postEnableStorage(t, day2Cluster("Ready", nil), `{"virtualization": true, "infraStorageClassName": "host-rbd", "allowanceGi": 500}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := unstructured.NestedBool(after.Object, "spec", "storage", "enabled"); !v {
		t.Error("virtualization must turn nested storage on")
	}
	if v, _, _ := unstructured.NestedBool(after.Object, "spec", "virtualization", "enabled"); !v {
		t.Error("virtualization not enabled")
	}
	if sc, _, _ := unstructured.NestedString(after.Object, "spec", "storage", "infraStorageClassName"); sc != "host-rbd" {
		t.Errorf("infraStorageClassName = %q", sc)
	}
	if p, _, _ := unstructured.NestedString(after.Object, "status", "phase"); p != "EnableVirtualization" {
		t.Errorf("phase = %q, want EnableVirtualization", p)
	}
	if m, _, _ := unstructured.NestedString(after.Object, "status", "message"); m != "Adding nested storage and OpenShift Virtualization" {
		t.Errorf("message = %q", m)
	}
}

func TestEnableStorageOnlyKeepsVirtualizationOff(t *testing.T) {
	rec, after := postEnableStorage(t, day2Cluster("Ready", nil), `{"virtualization": false}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := unstructured.NestedBool(after.Object, "spec", "storage", "enabled"); !v {
		t.Error("storage not enabled")
	}
	if v, _, _ := unstructured.NestedBool(after.Object, "spec", "virtualization", "enabled"); v {
		t.Error("storage alone must not enable virtualization")
	}
	if p, _, _ := unstructured.NestedString(after.Object, "status", "phase"); p != "EnableStorage" {
		t.Errorf("phase = %q, want EnableStorage", p)
	}
}

func TestEnableStorageRefusals(t *testing.T) {
	storageOn := map[string]interface{}{"storage": map[string]interface{}{"enabled": true, "infraStorageClassName": "kept"}}
	cases := []struct {
		name string
		nc   *unstructured.Unstructured
		body string
		code int
	}{
		{"not Ready", day2Cluster("WaitInstall", nil), `{}`, http.StatusConflict},
		{"master", day2Cluster("Master", nil), `{}`, http.StatusConflict},
		{"storage already working", day2Cluster("Ready", storageOn, ready("StorageReady")), `{}`, http.StatusBadRequest},
		{"virtualization already working", day2Cluster("Ready", map[string]interface{}{"storage": map[string]interface{}{"enabled": true}, "virtualization": map[string]interface{}{"enabled": true}}, ready("StorageReady"), ready("VirtualizationReady")), `{"virtualization": true}`, http.StatusBadRequest},
		{"unknown host StorageClass", day2Cluster("Ready", nil), `{"infraStorageClassName": "nope"}`, http.StatusBadRequest},
		{"unknown field", day2Cluster("Ready", nil), `{"virtualisation": true}`, http.StatusBadRequest},
		{"negative allowance", day2Cluster("Ready", nil), `{"allowanceGi": -1}`, http.StatusBadRequest},
		{"unknown restart mode", day2Cluster("Ready", nil), `{"virtualization": true, "restartMode": "sometimes"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, after := postEnableStorage(t, c.nc, c.body)
			if rec.Code != c.code {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.code, rec.Body.String())
			}
			if p, _, _ := unstructured.NestedString(after.Object, "status", "phase"); p == "EnableStorage" || p == "EnableVirtualization" {
				t.Error("a refused request must not start the phase")
			}
		})
	}
}

// Adding virtualization to a cluster whose storage already works keeps the
// existing storage settings, and a failed earlier attempt can be repeated.
func TestEnableVirtualizationOnExistingStorage(t *testing.T) {
	nc := day2Cluster("Ready", map[string]interface{}{"storage": map[string]interface{}{"enabled": true, "infraStorageClassName": "kept", "allowanceGi": int64(300)}}, ready("StorageReady"))
	rec, after := postEnableStorage(t, nc, `{"virtualization": true, "infraStorageClassName": "host-rbd"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if sc, _, _ := unstructured.NestedString(after.Object, "spec", "storage", "infraStorageClassName"); sc != "kept" {
		t.Errorf("existing storage settings changed: infraStorageClassName = %q", sc)
	}
	if m, _, _ := unstructured.NestedString(after.Object, "status", "message"); m != "Adding OpenShift Virtualization (nested storage is already in place)" {
		t.Errorf("message = %q: storage that already works must not be announced as added", m)
	}
	failed := day2Cluster("Ready", map[string]interface{}{"storage": map[string]interface{}{"enabled": true}},
		map[string]interface{}{"type": "StorageReady", "status": "False", "reason": "ConfigureFailed", "message": "x", "lastTransitionTime": "2026-11-12T00:00:00Z"})
	if rec, _ := postEnableStorage(t, failed, `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("retry after a failed storage step: got %d: %s", rec.Code, rec.Body.String())
	}
}

// The chosen restart mode is recorded for the orchestrator; rolling unless the
// user asked for all nodes at once.
func TestEnableVirtualizationRestartMode(t *testing.T) {
	for body, want := range map[string]string{
		`{"virtualization": true}`:                                "rolling",
		`{"virtualization": true, "restartMode": "rolling"}`:      "rolling",
		`{"virtualization": true, "restartMode": "simultaneous"}`: "simultaneous",
	} {
		rec, after := postEnableStorage(t, day2Cluster("Ready", nil), body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: got %d: %s", body, rec.Code, rec.Body.String())
		}
		if got := after.GetAnnotations()["oct-shift-on-shift-virt/virtualization-restart"]; got != want {
			t.Errorf("%s: restart annotation = %q, want %q", body, got, want)
		}
	}
}

// Review: a second request while one is starting (spec updated, phase not
// yet) is refused instead of changing the running request.
func TestEnableStorageRefusesWhilePending(t *testing.T) {
	nc := day2Cluster("Ready", nil)
	nc.SetAnnotations(map[string]string{"oct-shift-on-shift-virt/day2-request-pending": time.Now().UTC().Format(time.RFC3339)})
	rec, after := postEnableStorage(t, nc, `{"virtualization": true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if v, _, _ := unstructured.NestedBool(after.Object, "spec", "virtualization", "enabled"); v {
		t.Error("a refused request must not change the spec")
	}
	// An old marker (a request that died between its two updates) expires.
	old := day2Cluster("Ready", nil)
	old.SetAnnotations(map[string]string{"oct-shift-on-shift-virt/day2-request-pending": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)})
	if rec, _ := postEnableStorage(t, old, `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("expired marker: got %d: %s", rec.Code, rec.Body.String())
	}
}

// A successful request leaves the marker, so a request racing it is refused.
func TestEnableStorageSetsPendingMarker(t *testing.T) {
	_, after := postEnableStorage(t, day2Cluster("Ready", nil), `{}`)
	if after.GetAnnotations()["oct-shift-on-shift-virt/day2-request-pending"] == "" {
		t.Error("pending marker not set")
	}
}

// Review: after a failed storage step the user's new StorageClass is used and
// validated (the dialog shows the pickers again).
func TestEnableStorageRetryUsesNewClass(t *testing.T) {
	failed := func() *unstructured.Unstructured {
		return day2Cluster("Ready", map[string]interface{}{"storage": map[string]interface{}{"enabled": true, "infraStorageClassName": "old"}},
			map[string]interface{}{"type": "StorageReady", "status": "False", "reason": "ConfigureFailed", "message": "x", "lastTransitionTime": "2026-11-12T00:00:00Z"})
	}
	rec, after := postEnableStorage(t, failed(), `{"infraStorageClassName": "host-rbd", "allowanceGi": 250}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if sc, _, _ := unstructured.NestedString(after.Object, "spec", "storage", "infraStorageClassName"); sc != "host-rbd" {
		t.Errorf("infraStorageClassName = %q, want the newly picked host-rbd", sc)
	}
	if rec, _ := postEnableStorage(t, failed(), `{"infraStorageClassName": "nope"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown class on retry: got %d, want 400", rec.Code)
	}
}
