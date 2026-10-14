package k8s

import (
	"context"
	"crypto/sha256"
	"fmt"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")

	hasDisk, hasVol := false, false
	for i, v := range volumes {
		vm2, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if nm, _, _ := unstructured.NestedString(vm2, "name"); nm == "iso" {
			hasVol = true
			claim, _, _ := unstructured.NestedString(vm2, "persistentVolumeClaim", "claimName")
			if claim != isoPVCName {
				unstructured.SetNestedField(vm2, isoPVCName, "persistentVolumeClaim", "claimName")
				volumes[i] = vm2
			}
		}
	}
	for _, d := range disks {
		disk, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		if nm, _, _ := unstructured.NestedString(disk, "name"); nm == "iso" {
			hasDisk = true
		}
	}
	if hasDisk && hasVol {
		return nil // already wired exactly once
	}
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
	if !hasDisk {
		disks = append(disks, map[string]interface{}{
			"name": "iso",
			"cdrom": map[string]interface{}{
				"bus": "sata",
			},
			"bootOrder": int64(2),
		})
	}
	if !hasVol {
		volumes = append(volumes, map[string]interface{}{
			"name": "iso",
			"persistentVolumeClaim": map[string]interface{}{
				"claimName": isoPVCName,
			},
		})
	}
	unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks")
	unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes")

	_, err = c.DynamicClient.Resource(vmGVR).Namespace(namespace).Update(ctx, vm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating VM %s with ISO: %w", vmName, err)
	}
	return nil
}

// SetVMPower sets spec.running on a VM (honoured under RunStrategy Always as
// an explicit pause/start) and waits for the transition to land. CD-ROM and
// PVC swaps require the guest to be down: KubeVirt's webhook rejects volume
// changes on running VMs.
func (c *Client) SetVMPower(ctx context.Context, namespace, vmName string, on bool, timeout time.Duration) error {
	resource := c.DynamicClient.Resource(vmGVR).Namespace(namespace)
	vm, err := resource.Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// spec.running is mutually exclusive with runStrategy (validator), so
	// power transitions must go through the start/stop subresources.
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]interface{}{"name": vmName, "namespace": namespace},
	}}
	action := "start"
	if !on {
		action = "stop"
	}
	curStatus, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
	want := "Running"
	if !on {
		want = "Stopped"
	}
	if curStatus != want {
		// client-go 0.31's dynamic ResourceInterface exposes no subresource
		// verbs; issue the start/stop PUT over a plain REST client (cheaper
		// than vendoring the full kubevirt clientset).
		restCfg := rest.CopyConfig(c.RestConfig)
		restCfg.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
		restCfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
		rc, rerr := rest.RESTClientFor(restCfg)
		if rerr != nil {
			return rerr
		}
		if serr := rc.Put().Namespace(namespace).Resource("virtualmachines").Name(vmName).SubResource(action).
			Body(obj).Do(ctx).Error(); serr != nil && !errors.IsNotFound(serr) {
			return fmt.Errorf("%sing VM %s: %w", action, vmName, serr)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cur, err := resource.Get(ctx, vmName, metav1.GetOptions{})
		if err == nil {
			status, _, _ := unstructured.NestedString(cur.Object, "status", "printableStatus")
			if (on && status == "Running") || (!on && status == "Stopped") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("VM %s did not reach power state %v within %s", vmName, on, timeout)
}

// WaitForPVCBound waits until a PVC is Bound (freshly cloned ISO PVCs take a
// moment under ODF/RBD).
func (c *Client) WaitForPVCBound(ctx context.Context, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pvc, err := c.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && pvc.Status.Phase == corev1.ClaimBound {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("PVC %s/%s not Bound within %s", namespace, name, timeout)
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
// StartVM powers on a VM that was explicitly stopped (under RunStrategy
// Always an explicit stop persists until a /start subresource call; a
// runStrategy update is a no-op). Already-running VMs return immediately.
// NOTE: requires update on subresources.kubevirt.io virtualmachines/start —
// see deploy/install.yaml; the legacy kubevirt.io/create rule alone is not
// sufficient (original octools RBAC gap: nothing in-repo ever stopped a VM
// before the ISO-mount flow existed).
func (c *Client) StartVM(ctx context.Context, namespace, name string) error {
	vm, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting VM %s: %w", name, err)
	}
	if status, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus"); status == "Running" {
		return nil
	}
	return c.SetVMPower(ctx, namespace, name, true, 3*time.Minute)
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

// RemoveISOFromVM detaches the agent discovery ISO (volume + cdrom) from a
// VM spec. Without this, every future crash-reboot of an installed node
// falls back to the ISO (bootOrder 2) and lands back in the agent shell
// ("installation cannot proceed" on console) instead of the installed OS.
func (c *Client) RemoveISOFromVM(ctx context.Context, namespace, vmName string) error {
	vm, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting VM %s: %w", vmName, err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	var kept []interface{}
	for _, d := range disks {
		disk, ok := d.(map[string]interface{})
		if !ok {
			kept = append(kept, d)
			continue
		}
		if name, _, _ := unstructured.NestedString(disk, "name"); name == "iso" {
			continue
		}
		kept = append(kept, d)
	}
	unstructured.SetNestedSlice(vm.Object, kept, "spec", "template", "spec", "domain", "devices", "disks")

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	var keptVols []interface{}
	for _, v := range volumes {
		vol, ok := v.(map[string]interface{})
		if !ok {
			keptVols = append(keptVols, v)
			continue
		}
		if name, _, _ := unstructured.NestedString(vol, "name"); name == "iso" {
			continue
		}
		keptVols = append(keptVols, v)
	}
	unstructured.SetNestedSlice(vm.Object, keptVols, "spec", "template", "spec", "volumes")

	if _, err := c.DynamicClient.Resource(vmGVR).Namespace(namespace).Update(ctx, vm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("removing ISO from VM %s: %w", vmName, err)
	}
	return nil
}

// CreateInfraVM creates a small helper VM (e.g. the MCS relay) from an
// existing DataSource (cluster image template such as
// openshift-virtualization-os-images/centos-stream9) with cloud-init
// NoCloud user-data. It rides the namespace's primary UDN exactly like the
// nested node VMs do (pod network IS the UDN in these namespaces), which is
// the whole point: pod-to-VM traffic on nft-blocked ports (22623) is
// rejected by host rules, but VM-to-VM traffic is not.
type InfraVMParams struct {
	Name             string
	Namespace        string
	ClusterName      string
	Cores            int
	MemoryGi         int
	DiskGi           int
	StorageClassName string
	DataSourceNS     string
	DataSourceName   string
	UserData         string
}

func (c *Client) CreateInfraVM(ctx context.Context, p InfraVMParams) error {
	// KubeVirt caps inline cloudInitNoCloud userData at 2048 bytes; the
	// relay unit is bigger, so ship it as a Secret and reference it.
	secretName := p.Name + "-cloudinit"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: p.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"userData": p.UserData},
	}
	existing, err := c.Clientset.CoreV1().Secrets(p.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if _, cerr := c.Clientset.CoreV1().Secrets(p.Namespace).Create(ctx, sec, metav1.CreateOptions{}); cerr != nil {
			return fmt.Errorf("cloud-init secret for %s: %w", p.Name, cerr)
		}
	} else {
		sec.ResourceVersion = existing.ResourceVersion
		if _, uerr := c.Clientset.CoreV1().Secrets(p.Namespace).Update(ctx, sec, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating cloud-init secret for %s: %w", p.Name, uerr)
		}
	}
	labels := map[string]interface{}{
		"oct-shift-on-shift-virt/cluster":   p.ClusterName,
		"oct-shift-on-shift-virt/component": "infra-" + p.Name,
	}
	vm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]interface{}{
				"name":      p.Name,
				"namespace": p.Namespace,
				"labels":    labels,
			},
			"spec": map[string]interface{}{
				"runStrategy": "Always",
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": labels},
					"spec": map[string]interface{}{
						"evictionStrategy": "None",
						"domain": map[string]interface{}{
							"cpu":    map[string]interface{}{"cores": int64(p.Cores)},
							"memory": map[string]interface{}{"guest": fmt.Sprintf("%dGi", p.MemoryGi)},
							"devices": map[string]interface{}{
								"disks": []interface{}{
									map[string]interface{}{
										"name":      "rootdisk",
										"disk":      map[string]interface{}{"bus": "virtio"},
										"bootOrder": int64(1),
									},
								},
								"interfaces": []interface{}{
									map[string]interface{}{"name": "default", "bridge": map[string]interface{}{}},
								},
							},
						},
						"networks": []interface{}{
							map[string]interface{}{"name": "default", "pod": map[string]interface{}{}},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name":       "rootdisk",
								"dataVolume": map[string]interface{}{"name": p.Name + "-rootdisk"},
							},
							map[string]interface{}{
								// This KubeVirt build's NoCloud volume only
								// exposes secretRef (secret keys "userData"/
								// "networkData"); inline userData is capped
								// at 2048B.
								"name": "cloudinit",
								"cloudInitNoCloud": map[string]interface{}{
									"secretRef": map[string]interface{}{
										"name": secretName,
									},
								},
							},
						},
					},
				},
				"dataVolumeTemplates": []interface{}{
					map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":      p.Name + "-rootdisk",
							"namespace": p.Namespace,
						},
						"spec": map[string]interface{}{
							"pvc": map[string]interface{}{
								"accessModes":      []interface{}{"ReadWriteOnce"},
								"storageClassName": p.StorageClassName,
								"resources": map[string]interface{}{
									"requests": map[string]interface{}{"storage": fmt.Sprintf("%dGi", p.DiskGi)},
								},
							},
							// sourceRef matches the live-proven sos-mcs-relay shape.
							"sourceRef": map[string]interface{}{
								"kind":      "DataSource",
								"name":      p.DataSourceName,
								"namespace": p.DataSourceNS,
							},
						},
					},
				},
			},
		},
	}
	_, err = c.DynamicClient.Resource(vmGVR).Namespace(p.Namespace).Create(ctx, vm, metav1.CreateOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("creating infra VM %s: %w", p.Name, err)
	}
	return nil
}
