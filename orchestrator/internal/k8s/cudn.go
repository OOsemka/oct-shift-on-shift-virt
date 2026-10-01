package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var cudnGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1",
	Resource: "clusteruserdefinednetworks",
}

func (c *Client) CreateCUDN(ctx context.Context, name, namespace, subnet, apiVIP, ingressVIP string) error {
	// Exclude VIP IPs from IPAM so DHCP doesn't assign them to node pods
	excludeSubnets := []interface{}{}
	if apiVIP != "" {
		excludeSubnets = append(excludeSubnets, apiVIP+"/32")
	}
	if ingressVIP != "" {
		excludeSubnets = append(excludeSubnets, ingressVIP+"/32")
	}

	layer2Spec := map[string]interface{}{
		"role": "Primary",
		"ipam": map[string]interface{}{
			"lifecycle": "Persistent",
		},
		"subnets": []interface{}{subnet},
	}
	if len(excludeSubnets) > 0 {
		layer2Spec["excludeSubnets"] = excludeSubnets
	}

	cudn := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.ovn.org/v1",
			"kind":       "ClusterUserDefinedNetwork",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					"oct-shift-on-shift-virt/cluster": name,
				},
			},
			"spec": map[string]interface{}{
				"namespaceSelector": map[string]interface{}{
					"matchExpressions": []interface{}{
						map[string]interface{}{
							"key":      "kubernetes.io/metadata.name",
							"operator": "In",
							"values":   []interface{}{namespace},
						},
					},
				},
				"network": map[string]interface{}{
					"topology": "Layer2",
					"layer2":   layer2Spec,
				},
			},
		},
	}

	_, err := c.DynamicClient.Resource(cudnGVR).Create(ctx, cudn, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating CUDN %s: %w", name, err)
	}
	return nil
}

var nadGVR = schema.GroupVersionResource{
	Group:    "k8s.cni.cncf.io",
	Version:  "v1",
	Resource: "network-attachment-definitions",
}

func (c *Client) CopyNAD(ctx context.Context, srcNamespace, srcName, dstNamespace string) error {
	src, err := c.DynamicClient.Resource(nadGVR).Namespace(srcNamespace).Get(ctx, srcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting NAD %s/%s: %w", srcNamespace, srcName, err)
	}

	dst := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]interface{}{
				"name":      srcName,
				"namespace": dstNamespace,
			},
		},
	}
	if spec, found, _ := unstructured.NestedMap(src.Object, "spec"); found {
		// Rewrite netAttachDefName in the CNI config to reference the
		// destination namespace. OVN-Kubernetes uses this field to locate
		// the NAD; leaving the source namespace causes "failed to find NAD
		// key" errors on pods in the target namespace.
		if configStr, ok := spec["config"].(string); ok {
			oldRef := srcNamespace + "/" + srcName
			newRef := dstNamespace + "/" + srcName
			if strings.Contains(configStr, oldRef) {
				spec["config"] = strings.Replace(configStr, oldRef, newRef, 1)
			}
		}
		unstructured.SetNestedField(dst.Object, spec, "spec")
	}

	_, err = c.DynamicClient.Resource(nadGVR).Namespace(dstNamespace).Create(ctx, dst, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating NAD copy in %s: %w", dstNamespace, err)
	}
	return nil
}

func (c *Client) DeleteCUDN(ctx context.Context, name string) error {
	err := c.DynamicClient.Resource(cudnGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting CUDN %s: %w", name, err)
	}
	return nil
}

// WaitForDeploymentCUDNIP waits for a Deployment's pod to become Ready and
// returns its Primary UDN (cUDN) IP. Looks up the Deployment's label selector
// to find the correct pods.
func (c *Client) WaitForDeploymentCUDNIP(ctx context.Context, namespace, deployName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)

	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	deploy, err := c.DynamicClient.Resource(deployGVR).Namespace(namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting deployment %s: %w", deployName, err)
	}
	matchLabels, _, _ := unstructured.NestedStringMap(deploy.Object, "spec", "selector", "matchLabels")
	var parts []string
	for k, v := range matchLabels {
		parts = append(parts, k+"="+v)
	}
	selector := strings.Join(parts, ",")

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase != "Running" {
				continue
			}
			ready := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					ready = true
				}
			}
			if !ready {
				continue
			}

			// Parse the cUDN IP from the OVN network-status annotation.
			// The Primary UDN interface is marked with "default": true.
			netStatus, ok := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
			if !ok {
				continue
			}
			ip := parseCUDNIP(netStatus)
			if ip != "" {
				return ip, nil
			}
		}

		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("timeout waiting for deployment %s cUDN IP", deployName)
}

// parseCUDNIP extracts the Primary UDN IP from the Multus network-status
// annotation JSON. The Primary UDN interface has "default": true.
func parseCUDNIP(netStatusJSON string) string {
	var entries []struct {
		Name      string   `json:"name"`
		Interface string   `json:"interface"`
		IPs       []string `json:"ips"`
		Default   bool     `json:"default"`
	}
	if err := json.Unmarshal([]byte(netStatusJSON), &entries); err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Default && len(e.IPs) > 0 {
			return e.IPs[0]
		}
	}
	// Fallback: look for ovn-udn1 interface
	for _, e := range entries {
		if e.Interface == "ovn-udn1" && len(e.IPs) > 0 {
			return e.IPs[0]
		}
	}
	return ""
}
