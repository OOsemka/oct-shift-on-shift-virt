package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// A nested cluster must never adopt (and later delete) a namespace it did not create.
func TestCreateNamespaceOwnership(t *testing.T) {
	kube := kubefake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-app"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo-ns", Labels: map[string]string{NamespaceOwnerLabel: "demo"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-ns", Labels: map[string]string{NamespaceOwnerLabel: "other"}}},
	)
	c := &Client{Clientset: kube}
	ctx := context.Background()

	if err := c.CreateNamespace(ctx, "team-app", CUDNNamespaceLabels("demo")); err == nil || !strings.Contains(err.Error(), "does not belong to this cluster") {
		t.Fatalf("existing unowned namespace must be refused, got %v", err)
	}
	if err := c.CreateNamespace(ctx, "other-ns", CUDNNamespaceLabels("demo")); err == nil {
		t.Fatal("another cluster's namespace must be refused")
	}
	if err := c.CreateNamespace(ctx, "demo-ns", CUDNNamespaceLabels("demo")); err != nil {
		t.Fatalf("resuming with our own namespace must work: %v", err)
	}
	if err := c.CreateNamespace(ctx, "fresh-ns", CUDNNamespaceLabels("demo")); err != nil {
		t.Fatalf("new namespace: %v", err)
	}
	ns, _ := kube.CoreV1().Namespaces().Get(ctx, "fresh-ns", metav1.GetOptions{})
	if !NamespaceOwnedBy(ns.Labels, "demo") || NamespaceOwnedBy(ns.Labels, "other") || NamespaceOwnedBy(nil, "") {
		t.Fatal("ownership label must identify exactly the creating cluster")
	}
}
