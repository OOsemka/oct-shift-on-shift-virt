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

// IPAMClaims are how OVN-K persistent IPs survive VM reschedules on a
// primary UDN namespace (NAD allowPersistentIPs=true). KubeVirt auto-creates
// one per VM interface named "<vm>.<interface>" (default interface →
// "<vm>.default"), and OVN-K's allocator registers the claim's status.ips so
// the dynamic pool avoids them. Proven live (skill sos-nested-ocp-ops):
// a PRESEEDED status.ips is honored — which is what lets us make the nested
// masters' IPs deterministic BEFORE the first discovery boot, killing the
// DHCP race that drifted master-0 off the rendezvousIP baked into the ISO.
var ipamClaimGVR = schema.GroupVersionResource{
	Group:    "k8s.cni.cncf.io",
	Version:  "v1alpha1",
	Resource: "ipamclaims",
}

// PreseedIPAMClaim creates (or verifies) the persistent-IP claim for a VM's
// default interface on the namespace's cluster UDN, presetting the
// allocation. It must run BEFORE the VM object is created (or at least
// before its first interface attach) so the allocation is registered before
// any dynamic assignment. Idempotent: an existing claim holding the wanted
// IP is a no-op; an existing claim holding a DIFFERENT ip is an error — we
// never hijack a live allocation.
func (c *Client) PreseedIPAMClaim(ctx context.Context, namespace, vmName, networkName, ip string, prefixLen int) error {
	name := vmName + ".default"
	want := fmt.Sprintf("%s/%d", ip, prefixLen)
	resource := c.DynamicClient.Resource(ipamClaimGVR).Namespace(namespace)

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.cni.cncf.io/v1alpha1",
		"kind":       "IPAMClaim",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]interface{}{
				"oct-shift-on-shift-virt/cluster": clusterFromVMName(vmName),
				"oct-shift-on-shift-virt/preseed": "sos-orchestrator",
			},
		},
		"spec": map[string]interface{}{
			"interface": "",
			"network":   networkName,
		},
	}}

	created, err := resource.Create(ctx, obj, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, gerr := resource.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fmt.Errorf("getting preexisting IPAMClaim %s/%s: %w", namespace, name, gerr)
		}
		ips, found, _ := unstructured.NestedStringSlice(existing.Object, "status", "ips")
		if found && len(ips) > 0 {
			if ips[0] == want {
				return nil
			}
			return fmt.Errorf("IPAMClaim %s/%s already holds %v, refusing to hijack (wanted %s)", namespace, name, ips, want)
		}
		created = existing
	} else if err != nil {
		return fmt.Errorf("creating IPAMClaim %s/%s: %w", namespace, name, err)
	}

	status := map[string]interface{}{
		"ips": []interface{}{want},
		"conditions": []interface{}{map[string]interface{}{
			"type":               "IPsAllocated",
			"status":             "True",
			"reason":             "SuccessfulAllocation",
			"message":            "preseeded by sos orchestrator",
			"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
			"observedGeneration": created.GetGeneration(),
		}},
	}
	withStatus := created.DeepCopy()
	if err := unstructured.SetNestedMap(withStatus.Object, status, "status"); err != nil {
		return err
	}
	if _, uerr := resource.UpdateStatus(ctx, withStatus, metav1.UpdateOptions{}); uerr != nil {
		// Older CRD builds may not expose a status subresource; plain
		// update is the fallback.
		if _, perr := resource.Update(ctx, withStatus, metav1.UpdateOptions{}); perr != nil {
			return fmt.Errorf("preseeding status.ips on IPAMClaim %s/%s: status: %v, plain: %v", namespace, name, uerr, perr)
		}
	}
	return nil
}

// clusterFromVMName best-effort trims known suffixes; only used for labels.
func clusterFromVMName(vmName string) string {
	for _, suf := range []string{"-dns-vm", "-jumpbox", "-mcs-relay", "-master-0", "-master-1", "-master-2"} {
		if len(vmName) > len(suf) && vmName[len(vmName)-len(suf):] == suf {
			return vmName[:len(vmName)-len(suf)]
		}
	}
	return vmName
}
