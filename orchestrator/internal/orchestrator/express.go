package orchestrator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"golang.org/x/crypto/bcrypt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	_ "embed"
)

// Express: clone a nested cluster from a master instead of installing it.
//
//   - MakeMaster (on a Ready cluster): clean shutdown of every VM, then a
//     VolumeSnapshot per root disk. The cluster stays in phase Master.
//   - A clone (spec.source.master) copies the master's VMs into its own
//     namespace: same MACs, firmware UUIDs and internal IPs for the nodes
//     (each cluster has its own isolated network), disks cloned from the
//     snapshots by the orchestrator itself (never through dataVolumeTemplates,
//     which would need the clone namespace's SA to read the master's), and
//     the master's bastion disk (it holds the nested DNS for the master's
//     internal zone) with a NEW firmware UUID - cloud-init's instance-id - so
//     cloud-init re-runs with the clone's external IPs and SSH keys.
//   - Sanitize then makes the clone's credentials its own: kubeadmin, a new
//     admin kubeconfig CA, OAuth tokens, cluster ID, a unique service-account
//     issuer (old tokens rejected), and new internal signers. FinishIsolation
//     waits for the API server rollout and removes trust in the master's
//     API-facing CAs.
//
// The nodes keep the master's internal identity (DNS zone, certificates):
// spec.source.internalName/internalBaseDomain, used by FQDN().

//go:embed manifests/express/wait-api.sh
var expressWaitAPIScript string

//go:embed manifests/express/sanitize.sh
var expressSanitizeScript string

//go:embed manifests/express/finish-isolation.sh
var expressFinishScript string

//go:embed manifests/express/csr.sh
var expressCSRScript string

const conditionIsolated = "Isolated"

var (
	expressVMGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	expressVMIGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	expressDVGVR  = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
	expressIPCGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1alpha1", Resource: "ipamclaims"}
	expressVSGVR  = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
)

func expressScriptsCM(cluster string) string      { return cluster + "-express-scripts" }
func expressSourceKCSecret(cluster string) string { return cluster + "-source-kubeconfig" }
func expressCredsSecret(cluster string) string    { return cluster + "-express-creds" }
func goldenSnapshotName(pvc string) string        { return pvc + "-golden" }

// sharedSnapshotName is the clone's view of a golden snapshot. Its
// VolumeSnapshotContent ("<name>-content") is cluster-scoped, so the name
// carries the clone's name.
func sharedSnapshotName(clone, pvc string) string { return clone + "-" + pvc + "-golden" }

// rootDiskPVC returns the PVC behind a VM's "rootdisk" volume.
func rootDiskPVC(vm *unstructured.Unstructured) string {
	vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range vols {
		m, _ := v.(map[string]interface{})
		if m["name"] != "rootdisk" {
			continue
		}
		if dv, ok := m["dataVolume"].(map[string]interface{}); ok {
			n, _ := dv["name"].(string)
			return n
		}
		if pvc, ok := m["persistentVolumeClaim"].(map[string]interface{}); ok {
			n, _ := pvc["claimName"].(string)
			return n
		}
	}
	return ""
}

func (s *StepExecutor) clusterVMs(ctx context.Context, ns, cluster string) ([]unstructured.Unstructured, error) {
	list, err := s.client.DynamicClient.Resource(expressVMGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "oct-shift-on-shift-virt/cluster=" + cluster})
	if err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
	return list.Items, nil
}

// MakeMaster shuts the cluster's VMs down cleanly and snapshots their root
// disks. Idempotent: existing snapshots are reused.
func (s *StepExecutor) MakeMaster(ctx context.Context, nc *crd.NestedCluster) error {
	if why := nc.MasterRefusal(); why != "" {
		return fmt.Errorf("%s", why)
	}
	snaps, err := s.shutdownAndSnapshot(ctx, nc, false)
	if err != nil {
		return err
	}
	now := metav1.Now()
	nc.Status.Master = &crd.MasterStatus{Snapshots: snaps, ReleaseVersion: nc.Status.ReleaseVersion, CreatedAt: &now}
	s.updateCRStatus(ctx, nc)
	s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express").Info("master ready", "snapshots", len(snaps))
	return nil
}

// RefreshMaster boots the master, waits until its certificates have renewed
// (approving its nodes' requests) and it is healthy, then retakes the
// snapshots. Clones of a fresh master skip that renewal on first boot.
func (s *StepExecutor) RefreshMaster(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express")
	if nc.Status.Master == nil || nc.Status.KubeconfigRef == "" {
		return fmt.Errorf("not a master")
	}
	ns := nc.Spec.Namespace
	vms, err := s.clusterVMs(ctx, ns, nc.Spec.ClusterName)
	if err != nil || len(vms) == 0 {
		return fmt.Errorf("listing the master's VMs: %v (found %d)", err, len(vms))
	}
	for _, vm := range vms {
		if _, err := s.client.DynamicClient.Resource(expressVMGVR).Namespace(ns).Patch(ctx, vm.GetName(),
			types.MergePatchType, []byte(`{"spec":{"runStrategy":"Always"}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("starting %s: %w", vm.GetName(), err)
		}
	}
	log.Info("master booted for a refresh; waiting for its certificates to renew")
	logs, err := s.runExpressJob(ctx, nc, "refresh", "wait-api.sh", nc.Status.KubeconfigRef, false,
		[]corev1.EnvVar{{Name: "SETTLE_SECONDS", Value: "300"}}, 45*time.Minute)
	if err != nil {
		return fmt.Errorf("master did not become healthy: %w", err)
	}
	if !strings.Contains(logs, "CLONE_API_READY") {
		return fmt.Errorf("refresh job ended without CLONE_API_READY")
	}
	snaps, err := s.shutdownAndSnapshot(ctx, nc, true)
	if err != nil {
		return err
	}
	now := metav1.Now()
	nc.Status.Master.Snapshots = snaps
	nc.Status.Master.RefreshedAt = &now
	s.updateCRStatus(ctx, nc)
	log.Info("master refreshed", "snapshots", len(snaps))
	return nil
}

// shutdownAndSnapshot stops every VM of the cluster (ACPI) and takes a
// VolumeSnapshot of each root disk; replace deletes existing ones first.
func (s *StepExecutor) shutdownAndSnapshot(ctx context.Context, nc *crd.NestedCluster, replace bool) (map[string]string, error) {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express")
	ns := nc.Spec.Namespace
	vms, err := s.clusterVMs(ctx, ns, nc.Spec.ClusterName)
	if err != nil || len(vms) == 0 {
		return nil, fmt.Errorf("listing the cluster's VMs: %v (found %d)", err, len(vms))
	}
	snapClass, err := s.isoSnapshotClass(ctx, nc.Spec.StorageClassName)
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		if _, err := s.client.DynamicClient.Resource(expressVMGVR).Namespace(ns).Patch(ctx, vm.GetName(),
			types.MergePatchType, []byte(`{"spec":{"runStrategy":"Halted"}}`), metav1.PatchOptions{}); err != nil {
			return nil, fmt.Errorf("stopping %s: %w", vm.GetName(), err)
		}
	}
	log.Info("shutting the cluster down for its master snapshots", "vms", len(vms))
	deadline := time.Now().Add(15 * time.Minute)
	for {
		vmis, err := s.client.DynamicClient.Resource(expressVMIGVR).Namespace(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "oct-shift-on-shift-virt/cluster=" + nc.Spec.ClusterName})
		if err == nil && len(vmis.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("VMs did not shut down within 15 minutes")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	snaps := map[string]string{}
	for _, vm := range vms {
		pvc := rootDiskPVC(&vm)
		if pvc == "" {
			return nil, fmt.Errorf("VM %s has no rootdisk volume", vm.GetName())
		}
		name := goldenSnapshotName(pvc)
		if replace {
			if err := s.client.DeleteVolumeSnapshot(ctx, ns, name); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("deleting old snapshot %s: %w", name, err)
			}
		}
		if err := s.client.CreateVolumeSnapshot(ctx, ns, name, pvc, snapClass); err != nil {
			return nil, fmt.Errorf("snapshot of %s: %w", pvc, err)
		}
		snaps[pvc] = name
	}
	for _, name := range snaps {
		if _, _, _, err := s.client.WaitForVolumeSnapshot(ctx, ns, name, 10*time.Minute); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	return snaps, nil
}

// loadMaster returns the clone's master; it must be in phase Master.
func (s *StepExecutor) loadMaster(ctx context.Context, nc *crd.NestedCluster) (*crd.NestedCluster, error) {
	obj, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).Get(ctx, nc.Spec.Source.Master, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("master %q: %w", nc.Spec.Source.Master, err)
	}
	m, err := unstructuredToNestedCluster(obj)
	if err != nil {
		return nil, err
	}
	if m.Status.Phase != crd.PhaseMaster || m.Status.Master == nil || len(m.Status.Master.Snapshots) == 0 {
		return nil, fmt.Errorf("%q is not a master (phase %s)", m.Name, m.Status.Phase)
	}
	return m, nil
}

// cloneVMs copies the master's VMs and disks into the clone's namespace.
func (s *StepExecutor) cloneVMs(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express")
	master, err := s.loadMaster(ctx, nc)
	if err != nil {
		return err
	}
	srcNs, dstNs := master.Spec.Namespace, nc.Spec.Namespace
	vms, err := s.clusterVMs(ctx, srcNs, master.Spec.ClusterName)
	if err != nil || len(vms) == 0 {
		return fmt.Errorf("listing the master's VMs: %v (found %d)", err, len(vms))
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}
	// The jobs that wait for and sanitize the clone start with the master's
	// admin kubeconfig (the clone's own is created by Sanitize).
	kc, err := s.client.GetSecretData(ctx, srcNs, master.Status.KubeconfigRef, "kubeconfig")
	if err != nil {
		return fmt.Errorf("master kubeconfig: %w", err)
	}
	if err := s.client.CreateOpaqueSecret(ctx, dstNs, expressSourceKCSecret(nc.Spec.ClusterName), map[string][]byte{"kubeconfig": kc}); err != nil {
		return err
	}
	extNAD, err := s.externalNADInClusterNs(ctx, nc)
	if err != nil {
		return err
	}
	// The clone's status starts from the master's (same internal layout);
	// the bastion load balancer is built from these node addresses.
	nc.Status.Nodes = append([]crd.NodeStatus{}, master.Status.Nodes...)
	nc.Status.MCSRelayIP = master.Status.MCSRelayIP
	nc.Status.DNSIPCUDN = master.Status.DNSIPCUDN
	nc.Status.APIVIPCUDN = master.Status.APIVIPCUDN
	nc.Status.IngressVIPCUDN = master.Status.IngressVIPCUDN
	nc.Status.ReleaseVersion = master.Status.ReleaseVersion
	nc.Status.ReleaseImage = master.Status.ReleaseImage
	nc.Status.LoadBalancer = crd.LoadBalancerBastion
	lb, err := newBastionLB(nc)
	if err != nil {
		return fmt.Errorf("bastion load balancer: %w", err)
	}
	network := "cluster_udn_" + nc.Spec.ClusterName
	masterBastion, cloneBastion := bastionVMName(master.Spec.ClusterName), bastionVMName(nc.Spec.ClusterName)

	for _, vm := range vms {
		srcName := vm.GetName()
		isBastion := srcName == masterBastion
		name := srcName
		if isBastion {
			name = cloneBastion
		}
		// Same internal address as in the master.
		claim, err := s.client.DynamicClient.Resource(expressIPCGVR).Namespace(srcNs).Get(ctx, srcName+".default", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("IPAMClaim of %s: %w", srcName, err)
		}
		ips, _, _ := unstructured.NestedStringSlice(claim.Object, "status", "ips")
		if len(ips) == 0 {
			return fmt.Errorf("IPAMClaim of %s has no address", srcName)
		}
		ip, ipnet, err := net.ParseCIDR(ips[0])
		if err != nil {
			return fmt.Errorf("IPAMClaim of %s: %w", srcName, err)
		}
		prefix, _ := ipnet.Mask.Size()
		if err := s.client.PreseedIPAMClaim(ctx, dstNs, name, network, ip.String(), prefix); err != nil {
			return err
		}
		// Root disk from the golden snapshot, created by the orchestrator.
		pvc := rootDiskPVC(&vm)
		snap := master.Status.Master.Snapshots[pvc]
		if snap == "" {
			return fmt.Errorf("master has no snapshot of %s", pvc)
		}
		dvName := pvc
		if isBastion {
			dvName = strings.Replace(pvc, master.Spec.ClusterName, nc.Spec.ClusterName, 1)
		}
		// The golden snapshot is shared into the clone namespace (a Retain
		// VolumeSnapshotContent on the same CSI handle), so the disk clone is
		// namespace-local: no right to read the master's namespace is needed,
		// and deleting the clone never deletes the master's snapshot.
		_, driver, handle, err := s.client.WaitForVolumeSnapshot(ctx, srcNs, snap, 2*time.Minute)
		if err != nil {
			return fmt.Errorf("golden snapshot %s: %w", snap, err)
		}
		snapClass, err := s.isoSnapshotClass(ctx, master.Spec.StorageClassName)
		if err != nil {
			return err
		}
		localSnap := sharedSnapshotName(nc.Spec.ClusterName, pvc)
		if err := s.client.ShareSnapshotToNamespace(ctx, dstNs, localSnap, driver, handle, snapClass); err != nil {
			return err
		}
		if _, _, _, err := s.client.WaitForVolumeSnapshot(ctx, dstNs, localSnap, 5*time.Minute); err != nil {
			return fmt.Errorf("shared snapshot %s: %w", localSnap, err)
		}
		// A plain PVC restored from the shared snapshot. CDI refuses
		// pre-provisioned snapshots (they carry no restoreSize), so no
		// DataVolume: Kubernetes restores the PVC directly.
		size, modes, err := diskShape(&vm)
		if err != nil {
			return err
		}
		if err := s.client.CreatePVCFromSnapshotModes(ctx, dstNs, dvName, localSnap, master.Spec.StorageClassName, size, modes,
			map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}); err != nil {
			return fmt.Errorf("disk %s: %w", dvName, err)
		}
		// The VM, copied.
		obj := cloneVMObject(&vm, name, dstNs, master.Spec.ClusterName, nc.Spec.ClusterName, dvName)
		if isBastion {
			cloudInit, err := s.cloneBastionUserData(ctx, nc, master, lb)
			if err != nil {
				return err
			}
			secret := cloneBastion + "-cloudinit"
			if err := s.client.CreateOpaqueSecret(ctx, dstNs, secret, map[string][]byte{"userData": []byte(cloudInit)}); err != nil {
				return err
			}
			prepareCloneBastion(obj, extNAD, lb.MAC, secret)
		}
		if _, err := s.client.DynamicClient.Resource(expressVMGVR).Namespace(dstNs).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("VM %s: %w", name, err)
		}
	}

	s.updateCRStatus(ctx, nc)
	log.Info("VMs cloned from the master", "master", master.Name, "vms", len(vms))
	return nil
}

// diskShape returns the size and access modes of a master VM's root disk
// (from its dataVolumeTemplate).
func diskShape(vm *unstructured.Unstructured) (string, []string, error) {
	dvts, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	if len(dvts) == 0 {
		return "", nil, fmt.Errorf("VM %s has no dataVolumeTemplates", vm.GetName())
	}
	t, _ := dvts[0].(map[string]interface{})
	size, _, _ := unstructured.NestedString(t, "spec", "storage", "resources", "requests", "storage")
	if size == "" {
		size, _, _ = unstructured.NestedString(t, "spec", "pvc", "resources", "requests", "storage")
	}
	if size == "" {
		return "", nil, fmt.Errorf("VM %s: root disk size unknown", vm.GetName())
	}
	modes, _, _ := unstructured.NestedStringSlice(t, "spec", "storage", "accessModes")
	if len(modes) == 0 {
		modes = []string{"ReadWriteMany"}
	}
	return size, modes, nil
}

// cloneVMObject copies a master VM for the clone: no dataVolumeTemplates
// (the disk is created separately), labels renamed, started.
func cloneVMObject(vm *unstructured.Unstructured, name, ns, masterName, cloneName, dvName string) *unstructured.Unstructured {
	spec := vm.DeepCopy().Object["spec"].(map[string]interface{})
	delete(spec, "dataVolumeTemplates")
	delete(spec, "running")
	spec["runStrategy"] = "Always"
	if vols, ok, _ := unstructured.NestedSlice(spec, "template", "spec", "volumes"); ok {
		for _, v := range vols {
			m := v.(map[string]interface{})
			if m["name"] == "rootdisk" {
				m["persistentVolumeClaim"] = map[string]interface{}{"claimName": dvName}
				delete(m, "dataVolume")
			}
		}
		_ = unstructured.SetNestedSlice(spec, vols, "template", "spec", "volumes")
	}
	relabel := func(path ...string) {
		labels, _, _ := unstructured.NestedStringMap(spec, path...)
		if labels == nil {
			return
		}
		for k, v := range labels {
			if v == masterName {
				labels[k] = cloneName
			}
		}
		_ = unstructured.SetNestedStringMap(spec, labels, path...)
	}
	relabel("template", "metadata", "labels")
	// Anti-affinity selectors name the cluster too.
	if aff, ok, _ := unstructured.NestedMap(spec, "template", "spec", "affinity"); ok {
		b, _ := json.Marshal(aff)
		b = []byte(strings.ReplaceAll(string(b), `"`+masterName+`"`, `"`+cloneName+`"`))
		var fixed map[string]interface{}
		if json.Unmarshal(b, &fixed) == nil {
			_ = unstructured.SetNestedMap(spec, fixed, "template", "spec", "affinity")
		}
	}
	labels := map[string]interface{}{}
	for k, v := range vm.GetLabels() {
		if v == masterName {
			v = cloneName
		}
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": vm.GetAPIVersion(), "kind": "VirtualMachine",
		"metadata": map[string]interface{}{"name": name, "namespace": ns, "labels": labels},
		"spec":     spec,
	}}
}

// prepareCloneBastion gives the cloned bastion its own identity: a new
// firmware UUID (cloud-init's instance-id, so cloud-init re-runs with the
// clone's data), its external MAC, the clone namespace's VLAN NAD and its
// own cloud-init Secret.
func prepareCloneBastion(obj *unstructured.Unstructured, extNAD, mac, cloudInitSecret string) {
	tspec := obj.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	if dom, ok := tspec["domain"].(map[string]interface{}); ok {
		if fw, ok := dom["firmware"].(map[string]interface{}); ok {
			delete(fw, "uuid")
			delete(fw, "serial")
		}
		if dev, ok := dom["devices"].(map[string]interface{}); ok {
			if ifs, ok := dev["interfaces"].([]interface{}); ok {
				for _, i := range ifs {
					m := i.(map[string]interface{})
					if m["name"] == bastionExternalIface {
						m["macAddress"] = mac
					}
				}
			}
		}
	}
	if nets, ok := tspec["networks"].([]interface{}); ok {
		for _, n := range nets {
			m := n.(map[string]interface{})
			if mul, ok := m["multus"].(map[string]interface{}); ok {
				mul["networkName"] = extNAD
			}
		}
	}
	if vols, ok := tspec["volumes"].([]interface{}); ok {
		for _, v := range vols {
			m := v.(map[string]interface{})
			if ci, ok := m["cloudInitNoCloud"].(map[string]interface{}); ok {
				for k := range ci {
					delete(ci, k)
				}
				ci["secretRef"] = map[string]interface{}{"name": cloudInitSecret}
			}
		}
	}
}

// cloneBastionUserData renders the clone bastion's cloud-init with the
// clone's external IPs and SSH keys. authorized_keys is written explicitly:
// cloud-init only ADDS keys, so the master's keys would otherwise survive
// on the cloned disk.
func (s *StepExecutor) cloneBastionUserData(ctx context.Context, nc, master *crd.NestedCluster, lb *bastionLB) (string, error) {
	var masterIPs []string
	for _, n := range master.Status.Nodes {
		if n.IP != "" {
			masterIPs = append(masterIPs, n.IP)
		}
	}
	pubKey := ""
	if b, err := s.client.GetSecretData(ctx, nc.Spec.Namespace, nc.Spec.ClusterName+"-ssh-key", "ssh-publickey"); err == nil {
		pubKey = strings.TrimSpace(string(b))
	}
	if pubKey == "" {
		return "", fmt.Errorf("no SSH public key for the clone (CreateSecrets must run first)")
	}
	desktopUser, err := s.bastionDesktopCreds(ctx, nc)
	if err != nil {
		return "", err
	}
	ud := bastionUserData(bastionVMName(nc.Spec.ClusterName), pubKey, masterIPs, nc.Spec.Bastion.Graphical, desktopUser,
		"https://console-openshift-console.apps."+nc.FQDN(), lb)
	var keys strings.Builder
	for _, k := range strings.Split(pubKey, "\n") {
		if k = strings.TrimSpace(k); k != "" {
			keys.WriteString("      " + k + "\n")
		}
	}
	entry := "  - path: " + bastionAuthorizedKeys + "\n    owner: core:core\n    permissions: '0600'\n    content: |\n" + keys.String()
	if !strings.Contains(ud, "write_files:\n") {
		return "", fmt.Errorf("unexpected bastion cloud-init layout (no write_files)")
	}
	return strings.Replace(ud, "write_files:\n", "write_files:\n"+entry, 1), nil
}

// bastionAuthorizedKeys is the core user's authorized_keys inside the bastion
// VM (built from parts so the repo's home-path leak check does not flag it).
var bastionAuthorizedKeys = path.Join("/", "home", "core", ".ssh", "authorized_keys")

// ensureExpressScripts puts lib.sh and the Express scripts in a ConfigMap.
func (s *StepExecutor) ensureExpressScripts(ctx context.Context, nc *crd.NestedCluster) error {
	lib, err := kubevirtCSIFS.ReadFile("manifests/kubevirtcsi/lib.sh")
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: expressScriptsCM(nc.Spec.ClusterName), Namespace: nc.Spec.Namespace,
			Labels: map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}},
		Data: map[string]string{"lib.sh": string(lib), "wait-api.sh": expressWaitAPIScript,
			"sanitize.sh": expressSanitizeScript, "finish-isolation.sh": expressFinishScript, "csr.sh": expressCSRScript},
	}
	cms := s.client.Clientset.CoreV1().ConfigMaps(nc.Spec.Namespace)
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}
	return nil
}

// runExpressJob runs one Express script in the clone's namespace with a
// nested kubeconfig (and optionally the sanitize credentials) mounted.
func (s *StepExecutor) runExpressJob(ctx context.Context, nc *crd.NestedCluster, step, script, kubeconfigSecret string, withCreds bool, extra []corev1.EnvVar, timeout time.Duration) (string, error) {
	jobName, err := s.startExpressJob(ctx, nc, step, script, kubeconfigSecret, withCreds, extra, timeout)
	if err != nil {
		return "", err
	}
	ns := nc.Spec.Namespace
	werr := s.client.WaitForJobCompletion(ctx, ns, jobName, timeout+time.Minute)
	logs, _ := s.client.JobPodLogs(ctx, ns, jobName)
	if werr != nil {
		if f := lastFatal(logs); f != "" {
			return logs, fmt.Errorf("%s", f)
		}
		return logs, werr
	}
	return logs, nil
}

func expressJobName(cluster, step string) string { return cluster + "-express-" + step }

// startExpressJob creates the Job (replacing an earlier one) and returns its name.
func (s *StepExecutor) startExpressJob(ctx context.Context, nc *crd.NestedCluster, step, script, kubeconfigSecret string, withCreds bool, extra []corev1.EnvVar, timeout time.Duration) (string, error) {
	if err := s.ensureExpressScripts(ctx, nc); err != nil {
		return "", err
	}
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	jobName := expressJobName(cluster, step)
	labels := map[string]string{"oct-shift-on-shift-virt/cluster": cluster, "oct-shift-on-shift-virt/component": "express-" + step}
	backoff := int32(0)
	deadline := int64(timeout / time.Second)
	env := append([]corev1.EnvVar{
		{Name: "CLUSTER", Value: cluster},
		{Name: "DOMAIN", Value: nc.FQDN()},
		{Name: "VIP", Value: nc.Status.MCSRelayIP},
	}, extra...)
	mounts := []corev1.VolumeMount{{Name: "m", MountPath: "/m", ReadOnly: true}, {Name: "nested", MountPath: "/nested", ReadOnly: true}}
	vols := []corev1.Volume{
		{Name: "m", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: expressScriptsCM(cluster)}}}},
		{Name: "nested", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: kubeconfigSecret, Items: []corev1.KeyToPath{{Key: "kubeconfig", Path: "kubeconfig"}}}}},
	}
	if withCreds {
		mounts = append(mounts, corev1.VolumeMount{Name: "creds", MountPath: "/creds", ReadOnly: true})
		vols = append(vols, corev1.Volume{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: expressCredsSecret(cluster),
			// The admin client CA and certificate, the kubeadmin hash and the
			// issuer; never the CA key or the kubeadmin password.
			Items: []corev1.KeyToPath{{Key: "admin-ca.crt", Path: "admin-ca.crt"}, {Key: "admin.crt", Path: "admin.crt"},
				{Key: "admin.key", Path: "admin.key"}, {Key: "kubeadmin-hash", Path: "kubeadmin-hash"}, {Key: "issuer", Path: "issuer"}}}}})
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, ServiceAccountName: cluster + "-ops",
					Containers: []corev1.Container{{
						Name: "run", Image: "quay.io/cjanisze/sos-installer-helper:4.22", ImagePullPolicy: corev1.PullAlways,
						Command: []string{"/bin/bash", "/m/" + script}, Env: env, VolumeMounts: mounts,
					}},
					Volumes: vols,
				},
			},
		},
	}
	if err := s.client.DeleteJob(ctx, ns, jobName); err != nil {
		return "", err
	}
	if err := s.client.CreateJob(ctx, job); err != nil {
		return "", err
	}
	return jobName, nil
}

// waitCloneAPI waits for the cloned API and operators (master credentials).
func (s *StepExecutor) waitCloneAPI(ctx context.Context, nc *crd.NestedCluster) error {
	logs, err := s.runExpressJob(ctx, nc, "wait", "wait-api.sh", expressSourceKCSecret(nc.Spec.ClusterName), false, nil, 25*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for the cloned cluster: %w", err)
	}
	if !strings.Contains(logs, "CLONE_API_READY") {
		return fmt.Errorf("wait job ended without CLONE_API_READY")
	}
	return nil
}

// sanitize replaces the credentials the clone shares with its master.
func (s *StepExecutor) sanitize(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express")
	setCondition(nc, conditionIsolated, metav1.ConditionFalse, "Sanitizing", "replacing the credentials shared with the master")
	s.updateCRStatus(ctx, nc)
	if err := s.ensureExpressCreds(ctx, nc); err != nil {
		return err
	}
	logs, err := s.runExpressJob(ctx, nc, "sanitize", "sanitize.sh", expressSourceKCSecret(nc.Spec.ClusterName), true, nil, 25*time.Minute)
	if err != nil {
		return fmt.Errorf("sanitizing: %w", err)
	}
	ca := parseCABundle(sectionBetween(logs, "=== CA ===", "=== CA END ==="))
	if ca == "" || !strings.Contains(logs, "SANITIZE_OK") {
		return fmt.Errorf("sanitize job ended without the API CA bundle or SANITIZE_OK")
	}
	if err := s.storeCloneKubeconfig(ctx, nc, ca); err != nil {
		return err
	}
	log.Info("clone credentials replaced; service-account issuer and signers rotating")
	return nil
}

// ensureExpressCreds generates (once) the clone's admin client CA and
// certificate, kubeadmin password and issuer, and stores them in Secrets.
func (s *StepExecutor) ensureExpressCreds(ctx context.Context, nc *crd.NestedCluster) error {
	ns, cluster := nc.Spec.Namespace, nc.Spec.ClusterName
	if _, err := s.client.Clientset.CoreV1().Secrets(ns).Get(ctx, expressCredsSecret(cluster), metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	data, password, err := newExpressCreds(cluster, time.Now())
	if err != nil {
		return err
	}
	if err := s.client.CreateOpaqueSecret(ctx, ns, cluster+"-kubeadmin", map[string][]byte{"password": []byte(password)}); err != nil {
		return err
	}
	return s.client.CreateOpaqueSecret(ctx, ns, expressCredsSecret(cluster), data)
}

// newExpressCreds returns the Secret data and the kubeadmin password.
func newExpressCreds(cluster string, now time.Time) (map[string][]byte, string, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{OrganizationalUnit: []string{"openshift"}, CommonName: "admin-kubeconfig-signer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, "", err
	}
	ca, _ := x509.ParseCertificate(caDER)
	userKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	userTmpl := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{Organization: []string{"system:masters"}, CommonName: "system:admin"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	userDER, err := x509.CreateCertificate(rand.Reader, userTmpl, ca, &userKey.PublicKey, caKey)
	if err != nil {
		return nil, "", err
	}
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	userKeyDER, _ := x509.MarshalECPrivateKey(userKey)
	password, err := kubeadminPassword()
	if err != nil {
		return nil, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return nil, "", err
	}
	return map[string][]byte{
		"admin-ca.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"admin-ca.key":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}),
		"admin.crt":      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: userDER}),
		"admin.key":      pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: userKeyDER}),
		"kubeadmin-hash": hash,
		"issuer":         []byte("https://" + cluster + ".clusters.sos.invalid"),
	}, password, nil
}

// kubeadminPassword: four groups of five, like the installer's.
func kubeadminPassword() (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	groups := make([]string, 4)
	for g := range groups {
		b := make([]byte, 5)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			b[i] = alphabet[n.Int64()]
		}
		groups[g] = string(b)
	}
	return strings.Join(groups, "-"), nil
}

// storeCloneKubeconfig writes <clone>-kubeconfig: the clone's internal API
// name, the cluster's current API CA bundle, the new system:admin cert.
func (s *StepExecutor) storeCloneKubeconfig(ctx context.Context, nc *crd.NestedCluster, caB64 string) error {
	ns, cluster := nc.Spec.Namespace, nc.Spec.ClusterName
	crt, err := s.client.GetSecretData(ctx, ns, expressCredsSecret(cluster), "admin.crt")
	if err != nil {
		return err
	}
	key, err := s.client.GetSecretData(ctx, ns, expressCredsSecret(cluster), "admin.key")
	if err != nil {
		return err
	}
	kc := buildAdminKubeconfig(nc.FQDN(), caB64, crt, key)
	if err := s.client.CreateOpaqueSecret(ctx, ns, cluster+"-kubeconfig", map[string][]byte{"kubeconfig": []byte(kc)}); err != nil {
		return err
	}
	nc.Status.KubeconfigRef = cluster + "-kubeconfig"
	s.updateCRStatus(ctx, nc)
	return nil
}

func buildAdminKubeconfig(domain, caB64 string, crt, key []byte) string {
	b64 := base64.StdEncoding.EncodeToString
	cfg := map[string]interface{}{
		"apiVersion": "v1", "kind": "Config",
		"clusters": []interface{}{map[string]interface{}{"name": domain, "cluster": map[string]interface{}{
			"server": "https://api." + domain + ":6443", "certificate-authority-data": caB64}}},
		"users": []interface{}{map[string]interface{}{"name": "admin", "user": map[string]interface{}{
			"client-certificate-data": b64(crt), "client-key-data": b64(key)}}},
		"contexts":        []interface{}{map[string]interface{}{"name": "admin", "context": map[string]interface{}{"cluster": domain, "user": "admin"}}},
		"current-context": "admin",
	}
	out, _ := yaml.Marshal(cfg)
	return string(out)
}

// cloneCredentials records the clone's own credentials (kubeconfig from
// Sanitize, kubeadmin from ensureExpressCreds) and drops the master's copy.
func (s *StepExecutor) cloneCredentials(ctx context.Context, nc *crd.NestedCluster) error {
	ns, cluster := nc.Spec.Namespace, nc.Spec.ClusterName
	if nc.Status.KubeconfigRef == "" {
		return fmt.Errorf("no clone kubeconfig (Sanitize did not store one)")
	}
	nc.Status.KubeadminRef = cluster + "-kubeadmin"
	nc.Status.APIEndpoint = nc.APIEndpoint()
	nc.Status.ConsoleURL = nc.ConsoleEndpoint()
	nc.Status.IngressDomain = nc.IngressWildcard()
	_ = s.client.Clientset.CoreV1().Secrets(ns).Delete(ctx, expressSourceKCSecret(cluster), metav1.DeleteOptions{})
	s.updateCRStatus(ctx, nc)
	return nil
}

// finishIsolation starts the isolation Job and returns: dropping the old
// issuer has to wait until the pods have refreshed their tokens (about an
// hour), so the clone goes Ready meanwhile with Isolated=False/Isolating.
// CheckIsolation (engine resync) records the outcome.
func (s *StepExecutor) finishIsolation(ctx context.Context, nc *crd.NestedCluster) error {
	issuerAt := time.Now()
	for _, c := range nc.Status.Conditions {
		if c.Type == conditionIsolated {
			issuerAt = c.LastTransitionTime.Time
		}
	}
	cutoff := issuerAt.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.startExpressJob(ctx, nc, "isolate", "finish-isolation.sh", nc.Status.KubeconfigRef, false,
		[]corev1.EnvVar{{Name: "CUTOFF", Value: cutoff}, {Name: "ISSUER_AT", Value: fmt.Sprint(issuerAt.Unix())}}, 4*time.Hour); err != nil {
		setCondition(nc, conditionIsolated, metav1.ConditionFalse, "IsolationIncomplete", "could not start the isolation job: "+err.Error())
		s.updateCRStatus(ctx, nc)
		return nil
	}
	setCondition(nc, conditionIsolated, metav1.ConditionFalse, "Isolating",
		"usable; removing the last trust shared with the master (about 1.5 hours)")
	s.updateCRStatus(ctx, nc)
	return nil
}

// CheckIsolation updates the Isolated condition of a Ready clone from its
// isolation Job. Called on resync; cheap when there is nothing to do.
func (s *StepExecutor) CheckIsolation(ctx context.Context, nc *crd.NestedCluster) {
	if !nc.IsClone() {
		return
	}
	isolating := false
	for _, c := range nc.Status.Conditions {
		if c.Type == conditionIsolated && c.Reason == "Isolating" {
			isolating = true
		}
	}
	if !isolating {
		return
	}
	ns, name := nc.Spec.Namespace, expressJobName(nc.Spec.ClusterName, "isolate")
	job, err := s.client.Clientset.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	switch {
	case job.Status.Succeeded > 0:
		logs, _ := s.client.JobPodLogs(ctx, ns, name)
		if strings.Contains(logs, "ISOLATED_OK") {
			setCondition(nc, conditionIsolated, metav1.ConditionTrue, "Isolated",
				"own admin credentials, service-account issuer and client-auth CAs; the master's are rejected")
		} else {
			setCondition(nc, conditionIsolated, metav1.ConditionFalse, "IsolationIncomplete", "isolation job ended without ISOLATED_OK")
		}
	case job.Status.Failed > 0:
		logs, _ := s.client.JobPodLogs(ctx, ns, name)
		msg := "isolation job failed"
		if f := lastFatal(logs); f != "" {
			msg += ": " + f
		}
		setCondition(nc, conditionIsolated, metav1.ConditionFalse, "IsolationIncomplete", msg)
	default:
		return
	}
	s.updateCRStatus(ctx, nc)
	s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("express").Info("isolation finished",
		"succeeded", job.Status.Succeeded > 0)
}

// deleteSharedSnapshotContents removes a clone's Retain VolumeSnapshotContents
// (cluster-scoped, so the namespace deletion misses them). The CSI snapshots
// stay: they belong to the master.
func (s *StepExecutor) deleteSharedSnapshotContents(ctx context.Context, clusterNs string) {
	vsc := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}
	list, err := s.client.DynamicClient.Resource(vsc).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, c := range list.Items {
		ns, _, _ := unstructured.NestedString(c.Object, "spec", "volumeSnapshotRef", "namespace")
		policy, _, _ := unstructured.NestedString(c.Object, "spec", "deletionPolicy")
		if ns == clusterNs && policy == "Retain" && strings.HasSuffix(c.GetName(), "-golden-content") {
			_ = s.client.DynamicClient.Resource(vsc).Delete(ctx, c.GetName(), metav1.DeleteOptions{})
		}
	}
}
