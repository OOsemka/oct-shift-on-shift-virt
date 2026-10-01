package k8s

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (c *Client) CreateJob(ctx context.Context, job *batchv1.Job) error {
	_, err := c.Clientset.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

func (c *Client) WaitForJobCompletion(ctx context.Context, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s/%s timed out after %v", namespace, name, timeout)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		job, err := c.Clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting job %s/%s: %w", namespace, name, err)
		}

		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return nil
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				logs := c.getJobPodLogs(ctx, namespace, name)
				return fmt.Errorf("job %s/%s failed: %s\nlogs: %s", namespace, name, cond.Message, logs)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func (c *Client) getJobPodLogs(ctx context.Context, namespace, jobName string) string {
	pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return "(no logs available)"
	}
	tailLines := int64(50)
	logs, err := c.Clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		TailLines: &tailLines,
	}).Do(ctx).Raw()
	if err != nil {
		return "(failed to get logs: " + err.Error() + ")"
	}
	return string(logs)
}

func (c *Client) DeleteJob(ctx context.Context, namespace, name string) error {
	propagation := metav1.DeletePropagationBackground
	err := c.Clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting job %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *Client) CreatePVC(ctx context.Context, namespace, name, storageClass, size string) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
			StorageClassName: &storageClass,
		},
	}
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating PVC %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *Client) CreateBlockPVC(ctx context.Context, namespace, name, storageClass, size string) error {
	blockMode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  &blockMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
			StorageClassName: &storageClass,
		},
	}
	_, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating block PVC %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ClonePVCViaDV creates a CDI DataVolume that clones a PVC (supports cross-namespace).
// The CDI controller handles the clone at CSI level — no pod network needed in the target namespace.
func (c *Client) ClonePVCViaDV(ctx context.Context, srcNS, srcPVC, dstNS, dstName, storageClass, size string) error {
	dv := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]interface{}{
				"name":      dstName,
				"namespace": dstNS,
				"labels": map[string]interface{}{
					"oct-shift-on-shift-virt/component": "iso-clone",
				},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"pvc": map[string]interface{}{
						"namespace": srcNS,
						"name":      srcPVC,
					},
				},
				"storage": map[string]interface{}{
					"accessModes":      []interface{}{"ReadWriteOnce"},
					"storageClassName": storageClass,
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": size,
						},
					},
				},
			},
		},
	}

	dvGVR := schema.GroupVersionResource{
		Group:    "cdi.kubevirt.io",
		Version:  "v1beta1",
		Resource: "datavolumes",
	}
	_, err := c.DynamicClient.Resource(dvGVR).Namespace(dstNS).Create(ctx, dv, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating clone DV %s/%s: %w", dstNS, dstName, err)
	}
	return nil
}

// WaitForDVReady waits until a CDI DataVolume reaches Succeeded phase.
func (c *Client) WaitForDVReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	dvGVR := schema.GroupVersionResource{
		Group:    "cdi.kubevirt.io",
		Version:  "v1beta1",
		Resource: "datavolumes",
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		dv, err := c.DynamicClient.Resource(dvGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
		if phase == "Succeeded" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("DataVolume %s/%s did not reach Succeeded within %v", namespace, name, timeout)
}
