package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// DeleteNamespaceForce deletes a namespace and, if the namespace controller
// stalls, applies a bounded final push. The stall pattern observed in
// production: a VolumeSnapshot in the namespace and its prebound
// cluster-scoped VolumeSnapshotContent hold each other's finalizers (the
// namespace controller will not force-delete the snapshot, the snapshot
// waits for the content, the content waits for the snapshot) and the
// namespace hangs in Terminating forever.
//
// Order of escalation:
//  1. normal namespace delete
//  2. wait up to `timeout`
//  3. sweep cluster-scoped VolumeSnapshotContents bound to resources in the
//     namespace (the only cluster-scoped binder this architecture creates,
//     besides PVs — and those use reclaim policy Delete)
//  4. wait one more minute
//  5. verify nothing is left inside the namespace via a full discovery
//     sweep, and only then remove the namespace's own finalizer.
//
// The final push refuses to act if the sweep cannot verify emptiness.
func (c *Client) DeleteNamespaceForce(ctx context.Context, name string, timeout time.Duration) error {
	if err := c.DeleteNamespace(ctx, name); err != nil {
		return err
	}
	if c.waitNamespaceGone(ctx, name, timeout) {
		return nil
	}

	// Escalation 1: break the snapshot↔content mutual-hold deadlock by
	// releasing the finalizers of VolumeSnapshots in the namespace that are
	// already being deleted. The bound VolumeSnapshotContent then finalizes
	// through its normal path (CSI DeleteSnapshot + PV cleanup), so no
	// storage is orphaned.
	if err := c.releaseSnapshotHolds(ctx, name); err != nil {
		return fmt.Errorf("releasing snapshot holds for stuck namespace %s: %w", name, err)
	}
	// Escalation 2: delete any remaining cluster-scoped contents pointing
	// into the namespace.
	if err := c.sweepContentsForNamespace(ctx, name); err != nil {
		return fmt.Errorf("sweeping snapshot contents for stuck namespace %s: %w", name, err)
	}
	if c.waitNamespaceGone(ctx, name, time.Minute) {
		return nil
	}

	// Final push — only on a VERIFIED-empty namespace.
	left, scannable, err := c.countNamespaceObjects(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot verify namespace %s empty, refusing final push: %w", name, err)
	}
	if !scannable {
		return fmt.Errorf("namespace %s still Terminating and discovery sweep failed, refusing final push", name)
	}
	if left > 0 {
		return fmt.Errorf("namespace %s stuck with %d objects remaining, refusing final push", name, left)
	}
	ns, err := c.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ns.Finalizers = nil
	if _, err := c.Clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("final push on namespace %s: %w", name, err)
	}
	if !c.waitNamespaceGone(ctx, name, 2*time.Minute) {
		return fmt.Errorf("namespace %s still present after final push", name)
	}
	return nil
}

func (c *Client) waitNamespaceGone(ctx context.Context, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := c.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); errors.IsNotFound(err) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
	return false
}

// releaseSnapshotHolds strips finalizers from VolumeSnapshots in the
// namespace that already carry a deletionTimestamp. Safe only for
// already-deletion-requested snapshots: it breaks the documented
// snapshot↔content bound-protection deadlock without touching live data.
func (c *Client) releaseSnapshotHolds(ctx context.Context, ns string) error {
	list, err := c.DynamicClient.Resource(vsGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // no snapshot CRDs usable
		}
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetDeletionTimestamp() == nil || len(item.GetFinalizers()) == 0 {
			continue
		}
		empty := []string{}
		item.SetFinalizers(empty)
		if _, err := c.DynamicClient.Resource(vsGVR).Namespace(ns).Update(ctx, item, metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("releasing finalizers on snapshot %s: %w", item.GetName(), err)
		}
	}
	return nil
}

// sweepContentsForNamespace deletes cluster-scoped VolumeSnapshotContents
// whose volumeSnapshotRef points into the given namespace.
func (c *Client) sweepContentsForNamespace(ctx context.Context, ns string) error {
	list, err := c.DynamicClient.Resource(vscGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		refNS, _, _ := unstructured.NestedString(item.Object, "spec", "volumeSnapshotRef", "namespace")
		if refNS != ns {
			continue
		}
		name := item.GetName()
		if err := c.DynamicClient.Resource(vscGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting orphan content %s: %w", name, err)
		}
	}
	return nil
}

// countNamespaceObjects best-effort-lists every namespaced resource in ns
// via discovery. scannable=false when any resource type could not be
// listed (RBAC/discovery gap), which callers must treat as "cannot verify".
func (c *Client) countNamespaceObjects(ctx context.Context, ns string) (objects int, scannable bool, err error) {
	groups, err := c.Discovery.ServerPreferredResources()
	if err != nil && groups == nil {
		return 0, false, err
	}
	scannable = true
	for _, g := range groups {
		for _, r := range g.APIResources {
			if !r.Namespaced || strings.Contains(r.Name, "/") {
				continue
			}
			gv := strings.SplitN(g.GroupVersion, "/", 2)
			group := ""
			version := gv[0]
			if len(gv) == 2 {
				group = gv[0]
				version = gv[1]
			}
			gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: r.Name}
			list, lerr := c.DynamicClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 200})
			if lerr != nil {
				scannable = false // cannot fully verify emptiness
				continue
			}
			objects += len(list.Items)
		}
	}
	return objects, scannable, nil
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
