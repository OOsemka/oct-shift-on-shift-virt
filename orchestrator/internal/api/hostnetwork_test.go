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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// hostNode is a host node like the live case: the host is itself a nested
// cluster whose nodes sit on 10.100.0.0/16.
func hostNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			"k8s.ovn.org/node-primary-ifaddr": `{"ipv4":"` + ip + `/16"}`}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}},
	}
}

func hostNetworkServer(t *testing.T) (*Server, *kubefake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "octools.io/v1alpha1", "kind": "IPPool",
		"metadata": map[string]interface{}{"name": "lab", "namespace": clusterNamespace},
		"spec": map[string]interface{}{"nadName": "default/vlan-a", "prefixLength": int64(24),
			"gateway": "192.0.2.1", "addresses": []interface{}{"192.0.2.10-192.0.2.20"}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nestedClusterGVR: "NestedClusterList", ipPoolGVR: "IPPoolList"}, pool)
	settings, _ := json.Marshal(crd.LabSettings{BaseDomain: "example.test", OCPVersion: "4.22", IPPool: "lab"})
	kube := kubefake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: crd.LabDefaultsSecret, Namespace: clusterNamespace},
			Data: map[string][]byte{
				crd.LabDefaultsPullSecret:  []byte(`{"auths":{"example.invalid":{"auth":"eA=="}}}`),
				crd.LabDefaultsSettingsKey: settings,
			},
		},
		hostNode("cluster-master-0", "10.100.0.100"),
		hostNode("cluster-master-1", "10.100.0.101"),
	)
	return NewServer(&k8s.Client{DynamicClient: dyn, Clientset: kube}, logging.New()), kube, dyn
}

func manualSpec(name, cudn string) crd.NestedClusterSpec {
	api, ingress := crd.InternalVIPs(cudn)
	return crd.NestedClusterSpec{
		ClusterName: name, Namespace: name + "-ns", BaseDomain: "example.test", OCPVersion: "4.22",
		Network: crd.NetworkConfig{CUDNSubnet: cudn, APIVIP: api, IngressVIP: ingress,
			ClusterNetworkCIDR: crd.DefaultClusterNetworkCIDR, ServiceNetworkCIDR: crd.DefaultServiceNetworkCIDR},
		ExternalAccess: crd.ExternalAccessConfig{NADName: "default/vlan-a", PrefixLength: 24,
			APIIP: "192.0.2.30", IngressIP: "192.0.2.31", Gateway: "192.0.2.1"},
	}
}

func post(srv *Server, path string, body interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b))))
	return rec
}

func TestHostNetworkEndpoint(t *testing.T) {
	srv, _, _ := hostNetworkServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/host-network", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /host-network returned %d: %s", rec.Code, rec.Body.String())
	}
	var got hostNetwork
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.NodeAddresses) != 2 || got.NodeAddresses[0].Address != "10.100.0.100" || got.NodeAddresses[0].Subnet != "10.100.0.0/16" {
		t.Errorf("node addresses: %+v", got.NodeAddresses)
	}
	if got.SuggestedCUDNSubnet != "10.200.0.0/16" || got.SuggestedAPIVIP != "10.200.0.10" || got.SuggestedIngressVIP != "10.200.0.11" {
		t.Errorf("suggestion: %+v", got)
	}
}

// The create API refuses an internal network on the host's node network and
// accepts a free one; the quick cluster picks a free one by itself.
func TestCreateRefusesHostNetworkOverlap(t *testing.T) {
	srv, _, dyn := hostNetworkServer(t)

	rec := post(srv, "/api/v1/clusters", manualSpec("overlap", "10.100.0.0/16"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlapping cUDN must be refused with 400, got %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"10.100.0.100", "node network", "10.200.0.0/16"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("refusal lacks %q: %s", want, rec.Body.String())
		}
	}
	if _, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "overlap", metav1.GetOptions{}); err == nil {
		t.Error("nothing may be created for a refused cluster")
	}

	if rec := post(srv, "/api/v1/clusters", manualSpec("free", "10.200.0.0/16")); rec.Code != http.StatusCreated {
		t.Fatalf("free cUDN must be accepted, got %d %s", rec.Code, rec.Body.String())
	}

	// Quick preview and create choose the first free candidate.
	prev := httptest.NewRecorder()
	srv.Handler().ServeHTTP(prev, httptest.NewRequest(http.MethodGet, "/api/v1/quick-clusters/preview?name=quick-a", nil))
	var plan quickPlan
	if err := json.Unmarshal(prev.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Spec.Network.CUDNSubnet != "10.200.0.0/16" || len(plan.Problems) != 0 {
		t.Errorf("quick preview: %+v %v", plan.Spec.Network, plan.Problems)
	}
	if rec := post(srv, "/api/v1/quick-clusters", map[string]string{"clusterName": "quick-a"}); rec.Code != http.StatusCreated {
		t.Fatalf("quick create returned %d: %s", rec.Code, rec.Body.String())
	}
	obj, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "quick-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "network", "cudnSubnet"); got != "10.200.0.0/16" {
		t.Errorf("quick cluster cUDN = %s", got)
	}
}

// Without permission to list nodes (RBAC not converged yet) the check is
// skipped rather than blocking every create; the endpoint reports 503.
func TestHostNetworkForbiddenSkipsCheck(t *testing.T) {
	srv, kube, _ := hostNetworkServer(t)
	kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/host-network", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("forbidden node list: want 503, got %d", rec.Code)
	}
	if rec := post(srv, "/api/v1/clusters", manualSpec("unchecked", "10.100.0.0/16")); rec.Code != http.StatusCreated {
		t.Errorf("forbidden node list must not block creates, got %d %s", rec.Code, rec.Body.String())
	}
}
