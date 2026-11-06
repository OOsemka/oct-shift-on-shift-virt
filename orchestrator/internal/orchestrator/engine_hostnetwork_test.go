package orchestrator

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"
)

// A new cluster applied with oc (no API check) whose internal network sits on
// the host's node network fails before anything is created.
func TestReconcileRefusesHostNetworkOverlap(t *testing.T) {
	kube := kubefake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-master-0"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.100.0.100"}}},
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nestedClusterGVR: "NestedClusterList"})
	e := &Engine{
		client:   &k8s.Client{Clientset: kube, DynamicClient: dyn},
		logger:   logging.New(),
		running:  map[string]*reconcileRun{},
		deleting: map[string]bool{},
	}
	nc := cluster(crd.PhaseEmpty, false)
	nc.Finalizers = []string{cleanupFinalizer}
	nc.Spec = crd.NestedClusterSpec{
		OCPVersion: "4.22", // required by the CRD
		Network: crd.NetworkConfig{CUDNSubnet: "10.100.0.0/16", APIVIP: "10.100.0.10", IngressVIP: "10.100.0.11",
			ClusterNetworkCIDR: crd.DefaultClusterNetworkCIDR, ServiceNetworkCIDR: crd.DefaultServiceNetworkCIDR},
		ExternalAccess: crd.ExternalAccessConfig{NADName: "default/vlan-a", PrefixLength: 24,
			APIIP: "192.0.2.30", IngressIP: "192.0.2.31", Gateway: "192.0.2.1"},
	}
	if err := nc.Spec.ValidateNetwork(); err != nil {
		t.Fatalf("fixture must pass ValidateNetwork: %v", err)
	}
	// steps is nil: reaching any phase would panic.
	e.reconcile(context.Background(), nc)
	if nc.Status.Phase != crd.PhaseFailed || !strings.Contains(nc.Status.Message, "10.100.0.100") {
		t.Errorf("want Failed naming the node address, got %s: %s", nc.Status.Phase, nc.Status.Message)
	}
}

// A new cluster applied with oc for an OpenShift minor the tool does not
// support fails before anything is created.
func TestReconcileRefusesUnsupportedVersion(t *testing.T) {
	kube := kubefake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nestedClusterGVR: "NestedClusterList"})
	e := &Engine{
		client:   &k8s.Client{Clientset: kube, DynamicClient: dyn},
		logger:   logging.New(),
		running:  map[string]*reconcileRun{},
		deleting: map[string]bool{},
	}
	nc := cluster(crd.PhaseEmpty, false)
	nc.Finalizers = []string{cleanupFinalizer}
	nc.Spec = crd.NestedClusterSpec{
		OCPVersion: "4.20",
		Network: crd.NetworkConfig{CUDNSubnet: "10.200.0.0/16", APIVIP: "10.200.0.10", IngressVIP: "10.200.0.11",
			ClusterNetworkCIDR: crd.DefaultClusterNetworkCIDR, ServiceNetworkCIDR: crd.DefaultServiceNetworkCIDR},
		ExternalAccess: crd.ExternalAccessConfig{NADName: "default/vlan-a", PrefixLength: 24,
			APIIP: "192.0.2.30", IngressIP: "192.0.2.31", Gateway: "192.0.2.1"},
	}
	// steps is nil: reaching any phase would panic.
	e.reconcile(context.Background(), nc)
	if nc.Status.Phase != crd.PhaseFailed || !strings.Contains(nc.Status.Message, "not supported") {
		t.Errorf("want Failed for 4.20, got %s: %s", nc.Status.Phase, nc.Status.Message)
	}
}
