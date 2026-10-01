package k8s

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	vsGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshots",
	}
	vscGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshotcontents",
	}
)

func (c *Client) CreateVolumeSnapshot(ctx context.Context, namespace, name, pvcName, snapshotClassName string) error {
	vs := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"volumeSnapshotClassName": snapshotClassName,
				"source": map[string]interface{}{
					"persistentVolumeClaimName": pvcName,
				},
			},
		},
	}

	_, err := c.DynamicClient.Resource(vsGVR).Namespace(namespace).Create(ctx, vs, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// WaitForVolumeSnapshot waits until the snapshot is readyToUse and returns
// the VolumeSnapshotContent name, the CSI driver name, and the snapshot handle.
func (c *Client) WaitForVolumeSnapshot(ctx context.Context, namespace, name string, timeout time.Duration) (contentName, driver, handle string, err error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", "", "", err
		}
		vs, getErr := c.DynamicClient.Resource(vsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return "", "", "", fmt.Errorf("getting snapshot %s: %w", name, getErr)
		}
		ready, _, _ := unstructured.NestedBool(vs.Object, "status", "readyToUse")
		if ready {
			cn, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
			if cn == "" {
				return "", "", "", fmt.Errorf("snapshot %s ready but no content name", name)
			}
			vsc, getErr := c.DynamicClient.Resource(vscGVR).Get(ctx, cn, metav1.GetOptions{})
			if getErr != nil {
				return "", "", "", fmt.Errorf("getting content %s: %w", cn, getErr)
			}
			d, _, _ := unstructured.NestedString(vsc.Object, "spec", "driver")
			h, _, _ := unstructured.NestedString(vsc.Object, "status", "snapshotHandle")
			return cn, d, h, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", "", "", fmt.Errorf("snapshot %s not ready within %v", name, timeout)
}

// CloneSnapshotToNamespace creates a static VolumeSnapshotContent and a
// pre-bound VolumeSnapshot in the destination namespace, pointing at the same
// underlying CSI snapshot handle. This is the standard cross-namespace snapshot
// restore pattern — no pods or network access required.
func (c *Client) CloneSnapshotToNamespace(ctx context.Context, dstNs, dstSnapshotName, driver, snapshotHandle, snapshotClassName string) error {
	contentName := dstSnapshotName + "-content"

	vsc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshotContent",
			"metadata": map[string]interface{}{
				"name": contentName,
			},
			"spec": map[string]interface{}{
				"deletionPolicy": "Retain",
				"driver":         driver,
				"source": map[string]interface{}{
					"snapshotHandle": snapshotHandle,
				},
				"volumeSnapshotClassName": snapshotClassName,
				"volumeSnapshotRef": map[string]interface{}{
					"name":      dstSnapshotName,
					"namespace": dstNs,
				},
			},
		},
	}

	_, err := c.DynamicClient.Resource(vscGVR).Create(ctx, vsc, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating VolumeSnapshotContent %s: %w", contentName, err)
	}

	vs := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]interface{}{
				"name":      dstSnapshotName,
				"namespace": dstNs,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"volumeSnapshotContentName": contentName,
				},
			},
		},
	}

	_, err = c.DynamicClient.Resource(vsGVR).Namespace(dstNs).Create(ctx, vs, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating VolumeSnapshot %s in %s: %w", dstSnapshotName, dstNs, err)
	}

	return nil
}

// CreatePVCFromSnapshot creates a Block-mode PVC restored from a VolumeSnapshot.
func (c *Client) CreatePVCFromSnapshot(ctx context.Context, namespace, name, snapshotName, storageClassName, size string) error {
	pvc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"accessModes":      []interface{}{"ReadWriteOnce"},
				"volumeMode":       "Block",
				"storageClassName": storageClassName,
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"storage": size,
					},
				},
				"dataSource": map[string]interface{}{
					"name":     snapshotName,
					"kind":     "VolumeSnapshot",
					"apiGroup": "snapshot.storage.k8s.io",
				},
			},
		},
	}

	pvcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	_, err := c.DynamicClient.Resource(pvcGVR).Namespace(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Client) DeleteVolumeSnapshot(ctx context.Context, namespace, name string) error {
	err := c.DynamicClient.Resource(vsGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) DeleteVolumeSnapshotContent(ctx context.Context, name string) error {
	err := c.DynamicClient.Resource(vscGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
