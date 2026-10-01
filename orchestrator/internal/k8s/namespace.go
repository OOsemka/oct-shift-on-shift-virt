package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Client) CreateNamespace(ctx context.Context, name string, labels map[string]string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}

	_, err := c.Clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating namespace %s: %w", name, err)
	}
	return nil
}

func (c *Client) DeleteNamespace(ctx context.Context, name string) error {
	err := c.Clientset.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting namespace %s: %w", name, err)
	}
	return nil
}

func (c *Client) NamespaceExists(ctx context.Context, name string) (bool, error) {
	_, err := c.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking namespace %s: %w", name, err)
	}
	return true, nil
}

func CUDNNamespaceLabels(clusterName string) map[string]string {
	return map[string]string{
		"k8s.ovn.org/primary-user-defined-network": "",
		"cudn":                            "true",
		"oct-shift-on-shift-virt/cluster": clusterName,
		"app.kubernetes.io/managed-by":    "oct-shift-on-shift-virt",
	}
}
