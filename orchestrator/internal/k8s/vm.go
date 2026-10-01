package k8s

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func GenerateMAC(clusterName string, nodeIndex int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", clusterName, nodeIndex)))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", h[0], h[1], h[2])
}

var vmGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachines",
}

type VMCreateParams struct {
	Name             string
	Namespace        string
	ClusterName      string
	Role             string
	CPUCores         int
	MemoryGi         int
	DiskGi           int
	StorageClassName string
	MACAddress       string
	UDNName          string
}

func (c *Client) CreateVM(ctx context.Context, params VMCreateParams) (string, error) {
	labels := map[string]interface{}{
		"oct-shift-on-shift-virt/cluster": params.ClusterName,
		"oct-shift-on-shift-virt/role":    params.Role,
		"redfish-enabled":                 "true",
	}

	vm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]interface{}{
				"name":      params.Name,
				"namespace": params.Namespace,
				"labels":    labels,
			},
			"spec": map[string]interface{}{
				"runStrategy": "Always",
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": labels,
					},
					"spec": map[string]interface{}{
						"domain": map[string]interface{}{
							"cpu": map[string]interface{}{
								"cores": int64(params.CPUCores),
							},
							"memory": map[string]interface{}{
								"guest": fmt.Sprintf("%dGi", params.MemoryGi),
							},
							"devices": map[string]interface{}{
								"disks": []interface{}{
									map[string]interface{}{
										"name": "rootdisk",
										"disk": map[string]interface{}{
											"bus": "virtio",
										},
										"bootOrder": int64(1),
									},
								},
								"interfaces": []interface{}{
									makePrimaryInterface(params.MACAddress),
								},
							},
						},
						"networks": []interface{}{
							map[string]interface{}{
								"name": "default",
								"pod":  map[string]interface{}{},
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "rootdisk",
								"dataVolume": map[string]interface{}{
									"name": params.Name + "-rootdisk",
								},
							},
						},
					},
				},
				"dataVolumeTemplates": []interface{}{
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name": params.Name + "-rootdisk",
						},
						"spec": map[string]interface{}{
							"storage": map[string]interface{}{
								"accessModes": []interface{}{"ReadWriteOnce"},
								"resources": map[string]interface{}{
									"requests": map[string]interface{}{
										"storage": fmt.Sprintf("%dGi", params.DiskGi),
									},
								},
								"storageClassName": params.StorageClassName,
							},
							"source": map[string]interface{}{
								"blank": map[string]interface{}{},
							},
						},
					},
				},
			},
		},
	}

	created, err := c.DynamicClient.Resource(vmGVR).Namespace(params.Namespace).Create(ctx, vm, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := c.DynamicClient.Resource(vmGVR).Namespace(params.Namespace).Get(ctx, params.Name, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("getting existing VM %s: %w", params.Name, getErr)
		}
		return extractMACAddress(existing), nil
	}
	if err != nil {
		return "", fmt.Errorf("creating VM %s: %w", params.Name, err)
	}
	return extractMACAddress(created), nil
}

func (c *Client) DeleteVM(ctx context.Context, namespace, name string) error {
	err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting VM %s: %w", name, err)
	}
	return nil
}

func (c *Client) ListVMs(ctx context.Context, namespace, clusterName string) ([]unstructured.Unstructured, error) {
	list, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "oct-shift-on-shift-virt/cluster=" + clusterName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing VMs for cluster %s: %w", clusterName, err)
	}
	return list.Items, nil
}

// makePrimaryInterface creates a bridge-mode interface on the default pod network.
// With Primary cUDN, the default pod network IS the UDN — bridge gives the VM
// direct L2 access to the cUDN subnet.
func makePrimaryInterface(mac string) map[string]interface{} {
	iface := map[string]interface{}{
		"name":   "default",
		"bridge": map[string]interface{}{},
	}
	if mac != "" {
		iface["macAddress"] = mac
	}
	return iface
}

var dvGVR = schema.GroupVersionResource{
	Group:    "cdi.kubevirt.io",
	Version:  "v1beta1",
	Resource: "datavolumes",
}

func (c *Client) CreateISODataVolume(ctx context.Context, namespace, dvName, sourcePVCName, sourcePVCNamespace, storageClassName string) error {
	dv := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]interface{}{
				"name":      dvName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"pvc": map[string]interface{}{
						"name":      sourcePVCName,
						"namespace": sourcePVCNamespace,
					},
				},
				"storage": map[string]interface{}{
					"accessModes": []interface{}{"ReadWriteOnce"},
					"volumeMode":  "Block",
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": "8Gi",
						},
					},
					"storageClassName": storageClassName,
				},
			},
		},
	}

	_, err := c.DynamicClient.Resource(dvGVR).Namespace(namespace).Create(ctx, dv, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating DataVolume %s: %w", dvName, err)
	}
	return nil
}

func (c *Client) WaitForDataVolume(ctx context.Context, namespace, dvName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dv, err := c.DynamicClient.Resource(dvGVR).Namespace(namespace).Get(ctx, dvName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting DataVolume %s: %w", dvName, err)
		}
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
		if phase == "Succeeded" {
			return nil
		}
		if phase == "Failed" {
			return fmt.Errorf("DataVolume %s failed", dvName)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("DataVolume %s did not complete within %v", dvName, timeout)
}

func (c *Client) AddISOToVM(ctx context.Context, namespace, vmName, isoPVCName string) error {
	vm, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting VM %s: %w", vmName, err)
	}

	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	for i, d := range disks {
		disk, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(disk, "name"); name == "rootdisk" {
			// Rootdisk boots first (order 1). On first boot the disk is empty so
			// BIOS falls through to the ISO (order 2). After the installer writes
			// RHCOS to disk, the VM reboots and boots from the installed rootdisk.
			disk["bootOrder"] = int64(1)
			disks[i] = disk
		}
	}
	disks = append(disks, map[string]interface{}{
		"name": "iso",
		"cdrom": map[string]interface{}{
			"bus": "sata",
		},
		"bootOrder": int64(2),
	})
	unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks")

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]interface{}{
		"name": "iso",
		"persistentVolumeClaim": map[string]interface{}{
			"claimName": isoPVCName,
		},
	})
	unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes")

	_, err = c.DynamicClient.Resource(vmGVR).Namespace(namespace).Update(ctx, vm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating VM %s with ISO: %w", vmName, err)
	}
	return nil
}

var vmiGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachineinstances",
}

// GetVMIIP reads the IP address from the VMI status.interfaces.
// With Primary cUDN and autoattachPodInterface=false, the UDN is the only interface.
func (c *Client) GetVMIIP(ctx context.Context, namespace, name string) (string, error) {
	vmi, err := c.DynamicClient.Resource(vmiGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting VMI %s: %w", name, err)
	}

	interfaces, found, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	if !found || len(interfaces) == 0 {
		return "", fmt.Errorf("VMI %s has no interfaces in status", name)
	}

	for _, iface := range interfaces {
		ifaceMap, ok := iface.(map[string]interface{})
		if !ok {
			continue
		}
		ip, _, _ := unstructured.NestedString(ifaceMap, "ipAddress")
		if ip != "" && !strings.HasPrefix(ip, "fe80") {
			return ip, nil
		}
	}
	return "", fmt.Errorf("VMI %s has no usable IP address", name)
}

// WaitForVMIReady polls until the VMI exists and has an IP address.
func (c *Client) WaitForVMIReady(ctx context.Context, namespace, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ip, err := c.GetVMIIP(ctx, namespace, name)
		if err == nil && ip != "" {
			return ip, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("VMI %s did not get an IP within %v", name, timeout)
}

// StopVM patches the VM runStrategy to Halted.
func (c *Client) StopVM(ctx context.Context, namespace, name string) error {
	vm, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting VM %s: %w", name, err)
	}
	unstructured.SetNestedField(vm.Object, "Halted", "spec", "runStrategy")
	_, err = c.DynamicClient.Resource(vmGVR).Namespace(namespace).Update(ctx, vm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("stopping VM %s: %w", name, err)
	}
	return nil
}

// StartVM patches the VM runStrategy to Always.
func (c *Client) StartVM(ctx context.Context, namespace, name string) error {
	vm, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting VM %s: %w", name, err)
	}
	unstructured.SetNestedField(vm.Object, "Always", "spec", "runStrategy")
	_, err = c.DynamicClient.Resource(vmGVR).Namespace(namespace).Update(ctx, vm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("starting VM %s: %w", name, err)
	}
	return nil
}

func extractMACAddress(vm *unstructured.Unstructured) string {
	interfaces, found, _ := unstructured.NestedSlice(vm.Object,
		"spec", "template", "spec", "domain", "devices", "interfaces")
	if !found || len(interfaces) == 0 {
		return ""
	}
	iface, ok := interfaces[0].(map[string]interface{})
	if !ok {
		return ""
	}
	mac, _, _ := unstructured.NestedString(iface, "macAddress")
	return mac
}
