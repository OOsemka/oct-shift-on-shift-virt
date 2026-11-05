package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

const quickTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGdWqeGJ8m8v3eWJbXrDqgXSz9wjxL3Kd0a9vmyxUAbC own@example"

// The quick-cluster endpoint takes the name, the SSH key override and the
// desktop choice, and nothing else.
func TestQuickClusterOverrides(t *testing.T) {
	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "octools.io/v1alpha1", "kind": "IPPool",
		"metadata": map[string]interface{}{"name": "lab", "namespace": clusterNamespace},
		"spec": map[string]interface{}{"nadName": "default/vlan-a", "prefixLength": int64(24),
			"gateway": "192.0.2.1", "addresses": []interface{}{"192.0.2.10-192.0.2.20"}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nestedClusterGVR: "NestedClusterList", ipPoolGVR: "IPPoolList"}, pool)
	settings, _ := json.Marshal(crd.LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab", BastionGraphical: true})
	kube := kubefake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: crd.LabDefaultsSecret, Namespace: clusterNamespace},
		Data: map[string][]byte{
			crd.LabDefaultsPullSecret:  []byte(`{"auths":{"example.invalid":{"auth":"eA=="}}}`),
			crd.LabDefaultsSettingsKey: settings,
		},
	})
	srv := NewServer(&k8s.Client{DynamicClient: dyn, Clientset: kube}, logging.New())

	body := `{"clusterName":"quick-a","graphical":false,"sshPublicKey":"` + quickTestKey + `","pullSecret":"smuggled","baseDomain":"evil.test"}`
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/quick-clusters", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("quick create returned %d: %s", rec.Code, rec.Body.String())
	}
	obj, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "quick-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := obj.Object["spec"].(map[string]interface{})
	bastion, _ := spec["bastion"].(map[string]interface{})
	if g, _ := bastion["graphical"].(bool); g {
		t.Error("graphical=false in the request must override the lab default (true)")
	}
	if spec["sshKey"] != quickTestKey {
		t.Errorf("sshPublicKey override not applied: %v", spec["sshKey"])
	}
	if spec["baseDomain"] != "example.test" || spec["pullSecret"] != nil {
		t.Errorf("smuggled fields must be ignored: baseDomain=%v pullSecret=%v", spec["baseDomain"], spec["pullSecret"])
	}

	// No overrides: the lab default desktop setting applies.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/quick-clusters", strings.NewReader(`{"clusterName":"quick-b"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("second quick create returned %d: %s", rec.Code, rec.Body.String())
	}
	obj, _ = dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "quick-b", metav1.GetOptions{})
	b2, _ := obj.Object["spec"].(map[string]interface{})["bastion"].(map[string]interface{})
	if g, _ := b2["graphical"].(bool); !g {
		t.Error("without an override the lab default desktop setting (true) must apply")
	}

	// A bad key is refused, nothing is created.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/quick-clusters", strings.NewReader(`{"clusterName":"quick-c","sshPublicKey":"nope"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "SSH public key") {
		t.Fatalf("bad key must be refused with a reason, got %d %s", rec.Code, rec.Body.String())
	}
}
