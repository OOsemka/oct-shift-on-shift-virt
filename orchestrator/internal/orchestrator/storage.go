package orchestrator

import (
	"context"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Nested storage: the upstream kubevirt/csi-driver in split mode. The
// controller runs here on the host in the cluster namespace (it creates
// DataVolumes and hot-plugs them into the master VMs); the node plugin runs
// inside the nested cluster. Every image comes from the nested cluster's own
// release payload, pinned by digest. Design and findings: SOS-STORAGE-PLAN.
//
//go:embed manifests/kubevirtcsi/*
var kubevirtCSIFS embed.FS

const (
	conditionStorageReady        = "StorageReady"
	conditionVirtualizationReady = "VirtualizationReady"

	// Shared by every cluster: the controller's resizer reads the host PV
	// behind a DataVolume (finding F17). get only, no list/watch.
	pvReaderClusterRole = "oct-sos-kubevirt-csi-pv-reader"

	defaultStorageAllowanceGi = 100
	// OpenShift Virtualization imports its boot-source images (golden
	// images) into the nested cluster, all of which land on host storage.
	virtualizationBootSourcesGi = 180

	csiClusterLabel = "oct-shift-on-shift-virt/csi-cluster"
)

func storageScriptsCM(cluster string) string { return cluster + "-storage-scripts" }
func pvReaderBinding(cluster string) string  { return pvReaderClusterRole + "-" + cluster }

// configureStorage provides the nested cluster with a default StorageClass
// backed by a host StorageClass. Non-fatal: the cluster itself is healthy
// without it, so failures land on the StorageReady condition instead of
// failing an hour-long install.
func (s *StepExecutor) configureStorage(ctx context.Context, nc *crd.NestedCluster) error {
	if !nc.Spec.StorageWanted() {
		return nil
	}
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("storage")
	msg, err := s.doConfigureStorage(ctx, nc)
	if err != nil {
		log.Error("nested storage not configured (cluster still healthy)", "error", err.Error())
		setCondition(nc, conditionStorageReady, metav1.ConditionFalse, "ConfigureFailed", err.Error())
	} else {
		log.Info("nested storage ready", "detail", msg)
		setCondition(nc, conditionStorageReady, metav1.ConditionTrue, "SelfTestPassed", msg)
	}
	s.updateCRStatus(ctx, nc)
	return nil
}

func (s *StepExecutor) doConfigureStorage(ctx context.Context, nc *crd.NestedCluster) (string, error) {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("storage")
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	if nc.Status.KubeconfigRef == "" {
		return "", fmt.Errorf("no nested kubeconfig yet (PostInstall did not store one)")
	}

	sc, provisioner, err := s.resolveInfraStorageClass(ctx, nc.Spec.Storage.InfraStorageClassName)
	if err != nil {
		return "", err
	}
	vsc := s.resolveInfraSnapshotClass(ctx, provisioner)
	log.Info("host storage selected", "storageClass", sc, "provisioner", provisioner, "snapshotClass", vsc)

	if err := s.ensureStorageHostRBAC(ctx, nc); err != nil {
		return "", err
	}
	if err := s.ensureDriverConfig(ctx, nc, sc, vsc); err != nil {
		return "", err
	}
	quotaGi, err := s.ensureStorageQuota(ctx, nc)
	if err != nil {
		return "", err
	}
	if err := s.ensureStorageScripts(ctx, nc); err != nil {
		return "", err
	}

	logs, err := s.runStorageJob(ctx, nc, cluster+"-configure-storage", "configure-storage.sh", []corev1.EnvVar{
		{Name: "INFRA_SC", Value: sc},
		{Name: "INFRA_VSC", Value: vsc},
		{Name: "VM_MAP", Value: vmMap(nc)},
	}, 20*time.Minute)
	if err != nil {
		return "", fmt.Errorf("nested driver setup: %w", err)
	}
	images := parseImageLines(logs)
	ctrl, err := renderTemplate(mustReadCSI("controller.yaml.tmpl"), mergeVars(images, map[string]string{
		"HOST_NS": ns, "CLUSTER": cluster,
	}))
	if err != nil {
		return "", fmt.Errorf("controller manifest: %w (images reported by the job: %d)", err, len(images))
	}
	if err := s.applyController(ctx, ns, ctrl); err != nil {
		return "", err
	}
	if err := s.waitDeploymentAvailable(ctx, ns, "kubevirt-csi-controller", 10*time.Minute); err != nil {
		return "", err
	}
	log.Info("host CSI controller available")

	if _, err := s.runStorageJob(ctx, nc, cluster+"-selftest-storage", "selftest-storage.sh", []corev1.EnvVar{
		{Name: "IMG_CLI", Value: images["IMG_CLI"]},
	}, 25*time.Minute); err != nil {
		return "", fmt.Errorf("self-test: %w", err)
	}

	snap := "snapshots via " + vsc
	if vsc == "" {
		snap = "no snapshots (host has no VolumeSnapshotClass for " + provisioner + ")"
	}
	return fmt.Sprintf("StorageClass kubevirt-csi backed by host %s; quota %dGi; %s", sc, quotaGi, snap), nil
}

// configureVirtualization installs OpenShift Virtualization in the nested
// cluster and points CDI at kubevirt-csi. Needs nested storage; non-fatal.
func (s *StepExecutor) configureVirtualization(ctx context.Context, nc *crd.NestedCluster) error {
	if !nc.Spec.Virtualization.Enabled {
		return nil
	}
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("virtualization")
	if c := meta.FindStatusCondition(nc.Status.Conditions, conditionStorageReady); c == nil || c.Status != metav1.ConditionTrue {
		msg := "skipped: nested storage is not ready, and virtual machines need it"
		log.Error("OpenShift Virtualization not installed", "reason", msg)
		setCondition(nc, conditionVirtualizationReady, metav1.ConditionFalse, "StorageNotReady", msg)
		s.updateCRStatus(ctx, nc)
		return nil
	}
	sc, provisioner, err := s.resolveInfraStorageClass(ctx, nc.Spec.Storage.InfraStorageClassName)
	vsc := ""
	if err == nil {
		vsc = s.resolveInfraSnapshotClass(ctx, provisioner)
	}
	if err == nil {
		err = s.ensureStorageScripts(ctx, nc)
	}
	if err == nil {
		_, err = s.runStorageJob(ctx, nc, nc.Spec.ClusterName+"-configure-virtualization", "configure-virtualization.sh", []corev1.EnvVar{
			{Name: "INFRA_VSC", Value: vsc},
		}, 75*time.Minute)
	}
	if err != nil {
		log.Error("OpenShift Virtualization not configured (cluster still healthy)", "error", err.Error())
		setCondition(nc, conditionVirtualizationReady, metav1.ConditionFalse, "ConfigureFailed", err.Error())
	} else {
		msg := "OpenShift Virtualization installed; VM disks use kubevirt-csi (host " + sc + ")"
		log.Info("nested virtualization ready")
		setCondition(nc, conditionVirtualizationReady, metav1.ConditionTrue, "Installed", msg)
	}
	s.updateCRStatus(ctx, nc)
	return nil
}

func setCondition(nc *crd.NestedCluster, typ string, status metav1.ConditionStatus, reason, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	meta.SetStatusCondition(&nc.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: nc.Generation,
	})
}

// resolveInfraStorageClass picks the host class: the wizard's choice, else
// the default virtualization class, else the cluster default.
func (s *StepExecutor) resolveInfraStorageClass(ctx context.Context, want string) (name, provisioner string, err error) {
	if want != "" {
		sc, err := s.client.Clientset.StorageV1().StorageClasses().Get(ctx, want, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("host StorageClass %q: %w", want, err)
		}
		return sc.Name, sc.Provisioner, nil
	}
	list, err := s.client.Clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("listing host StorageClasses: %w", err)
	}
	for _, ann := range []string{"storageclass.kubevirt.io/is-default-virt-class", "storageclass.kubernetes.io/is-default-class"} {
		for _, sc := range list.Items {
			if sc.Annotations[ann] == "true" {
				return sc.Name, sc.Provisioner, nil
			}
		}
	}
	return "", "", fmt.Errorf("no host StorageClass selected and the host has no default (virtualization) StorageClass")
}

// resolveInfraSnapshotClass returns the host VolumeSnapshotClass for the
// class's provisioner (preferring a default one), or "" when there is none.
// The host-wide default class may belong to a different driver, so match
// on driver first.
func (s *StepExecutor) resolveInfraSnapshotClass(ctx context.Context, provisioner string) string {
	list, err := s.client.DynamicClient.Resource(schema.GroupVersionResource{
		Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses",
	}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	var match []string
	for _, v := range list.Items {
		if d, _ := v.Object["driver"].(string); d != provisioner {
			continue
		}
		if v.GetAnnotations()["snapshot.storage.kubernetes.io/is-default-class"] == "true" {
			return v.GetName()
		}
		match = append(match, v.GetName())
	}
	sort.Strings(match)
	if len(match) > 0 {
		return match[0]
	}
	return ""
}

// ensureStorageHostRBAC: the controller's host identity (namespace-scoped,
// upstream rule set), the shared PV reader, and a narrow grant letting the
// ops job write the controller's tenant kubeconfig.
func (s *StepExecutor) ensureStorageHostRBAC(ctx context.Context, nc *crd.NestedCluster) error {
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	labels := map[string]string{"oct-shift-on-shift-virt/cluster": cluster}
	if err := s.client.CreateServiceAccount(ctx, ns, "kubevirt-csi", labels); err != nil {
		return fmt.Errorf("controller service account: %w", err)
	}
	if err := s.ensureRole(ctx, ns, "kubevirt-csi", labels, []rbacv1.PolicyRule{
		{APIGroups: []string{"cdi.kubevirt.io"}, Resources: []string{"datavolumes"}, Verbs: []string{"get", "create", "delete"}},
		{APIGroups: []string{"kubevirt.io"}, Resources: []string{"virtualmachineinstances", "virtualmachines"}, Verbs: []string{"list", "get"}},
		{APIGroups: []string{"subresources.kubevirt.io"}, Resources: []string{"virtualmachines/addvolume", "virtualmachines/removevolume", "virtualmachineinstances/removevolume"}, Verbs: []string{"update"}},
		{APIGroups: []string{"snapshot.storage.k8s.io"}, Resources: []string{"volumesnapshots"}, Verbs: []string{"get", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get", "patch"}},
	}, "kubevirt-csi"); err != nil {
		return err
	}
	if err := s.client.CreateClusterRole(ctx, pvReaderClusterRole, map[string]string{"app.kubernetes.io/part-of": "oct-shift-on-shift-virt"}, []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get"}},
	}); err != nil {
		return fmt.Errorf("pv reader cluster role: %w", err)
	}
	if err := s.client.CreateClusterRoleBinding(ctx, pvReaderBinding(cluster), pvReaderClusterRole, "kubevirt-csi", ns, labels); err != nil {
		return fmt.Errorf("pv reader binding: %w", err)
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}
	// create cannot be limited by resourceNames; get/update/patch can.
	return s.ensureRole(ctx, ns, cluster+"-ops-storage", labels, []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"kvcluster-kubeconfig"}, Verbs: []string{"get", "update", "patch"}},
	}, cluster+"-ops")
}

func (s *StepExecutor) ensureRole(ctx context.Context, ns, name string, labels map[string]string, rules []rbacv1.PolicyRule, sa string) error {
	roles := s.client.Clientset.RbacV1().Roles(ns)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Rules: rules}
	if _, err := roles.Create(ctx, role, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		cur, gerr := roles.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		cur.Rules = rules
		if _, err := roles.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating role %s: %w", name, err)
		}
	} else if err != nil {
		return fmt.Errorf("creating role %s: %w", name, err)
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: ns}},
	}
	if _, err := s.client.Clientset.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating rolebinding %s: %w", name, err)
	}
	return nil
}

// enforcementYAML pins the controller to exactly one host class. Upstream
// defaults to allowAll, which would let any nested user pick any host class.
func enforcementYAML(sc, vsc string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "allowAll: false\nallowDefault: false\nallowList:\n- %s\n", sc)
	if vsc != "" {
		fmt.Fprintf(&b, "storageSnapshotMapping:\n- storageClasses:\n  - %s\n  volumeSnapshotClasses:\n  - %s\n", sc, vsc)
	}
	return b.String()
}

func (s *StepExecutor) ensureDriverConfig(ctx context.Context, nc *crd.NestedCluster, sc, vsc string) error {
	ns := nc.Spec.Namespace
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "driver-config", Namespace: ns,
			Labels: map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}},
		Data: map[string]string{
			"infraClusterNamespace":        ns,
			"infraClusterLabels":           csiClusterLabel + "=" + nc.Spec.ClusterName,
			"infraStorageClassEnforcement": enforcementYAML(sc, vsc),
		},
	}
	return s.upsertConfigMap(ctx, cm)
}

func (s *StepExecutor) upsertConfigMap(ctx context.Context, cm *corev1.ConfigMap) error {
	cms := s.client.Clientset.CoreV1().ConfigMaps(cm.Namespace)
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating configmap %s: %w", cm.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("creating configmap %s: %w", cm.Name, err)
	}
	return nil
}

func (s *StepExecutor) ensureStorageScripts(ctx context.Context, nc *crd.NestedCluster) error {
	entries, err := kubevirtCSIFS.ReadDir("manifests/kubevirtcsi")
	if err != nil {
		return err
	}
	data := map[string]string{}
	for _, e := range entries {
		data[e.Name()] = mustReadCSI(e.Name())
	}
	return s.upsertConfigMap(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: storageScriptsCM(nc.Spec.ClusterName), Namespace: nc.Spec.Namespace,
			Labels: map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}},
		Data: data,
	})
}

func mustReadCSI(name string) string {
	b, err := kubevirtCSIFS.ReadFile("manifests/kubevirtcsi/" + name)
	if err != nil {
		panic(err) // embedded at build time; a miss is a build bug
	}
	return string(b)
}

// storageQuotaGi caps the namespace's total storage requests: what the
// cluster already uses (VM disks, ISO), plus the allowance for nested
// volumes, plus room for the virtualization boot images. Without it a nested
// user could fill the host storage backend.
func storageQuotaGi(existingBytes int64, allowanceGi int, virtualization bool) int64 {
	if allowanceGi <= 0 {
		allowanceGi = defaultStorageAllowanceGi
	}
	const gi = int64(1) << 30
	q := (existingBytes+gi-1)/gi + int64(allowanceGi)
	if virtualization {
		q += virtualizationBootSourcesGi
	}
	return q
}

func (s *StepExecutor) ensureStorageQuota(ctx context.Context, nc *crd.NestedCluster) (int64, error) {
	ns := nc.Spec.Namespace
	pvcs, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing PVCs for the storage quota: %w", err)
	}
	var existing int64
	for _, p := range pvcs.Items {
		if p.Labels[csiClusterLabel] != "" {
			continue // nested volumes: these are what the allowance is for
		}
		if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			existing += q.Value()
		}
	}
	hardGi := storageQuotaGi(existing, nc.Spec.Storage.AllowanceGi, nc.Spec.Virtualization.Enabled)
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: nc.Spec.ClusterName + "-storage", Namespace: ns,
			Labels: map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsStorage: resource.MustParse(fmt.Sprintf("%dGi", hardGi)),
		}},
	}
	quotas := s.client.Clientset.CoreV1().ResourceQuotas(ns)
	if _, err := quotas.Create(ctx, quota, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		cur, gerr := quotas.Get(ctx, quota.Name, metav1.GetOptions{})
		if gerr != nil {
			return 0, gerr
		}
		cur.Spec.Hard = quota.Spec.Hard
		if _, err := quotas.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return 0, fmt.Errorf("updating storage quota: %w", err)
		}
	} else if err != nil {
		return 0, fmt.Errorf("creating storage quota: %w", err)
	}
	return hardGi, nil
}

// vmMap pairs node IPs with VM names ("ip=vm ..."): nested nodes carry no
// providerID, so the job annotates them by InternalIP.
func vmMap(nc *crd.NestedCluster) string {
	var parts []string
	for _, n := range nc.Status.Nodes {
		if n.Name != "" && n.IP != "" {
			parts = append(parts, n.IP+"="+n.Name)
		}
	}
	return strings.Join(parts, " ")
}

var imageLineRe = regexp.MustCompile(`(?m)^IMAGE (IMG_[A-Z_]+)=(\S+@sha256:[0-9a-f]{64})\s*$`)

func parseImageLines(logs string) map[string]string {
	out := map[string]string{}
	for _, m := range imageLineRe.FindAllStringSubmatch(logs, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func mergeVars(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

var placeholderRe = regexp.MustCompile(`\$\{[A-Z_]+\}`)

// renderTemplate replaces ${VAR} only. $(VAR) is Kubernetes env expansion
// and must survive untouched, so os.Expand is not an option.
func renderTemplate(tmpl string, vars map[string]string) (string, error) {
	out := placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := vars[m[2:len(m)-1]]; ok {
			return v
		}
		return m
	})
	if left := placeholderRe.FindAllString(out, -1); len(left) > 0 {
		return "", fmt.Errorf("unrendered placeholders %v", left)
	}
	return out, nil
}

func (s *StepExecutor) applyController(ctx context.Context, ns, manifest string) error {
	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(manifest), &dep); err != nil {
		return fmt.Errorf("parsing controller manifest: %w", err)
	}
	deps := s.client.Clientset.AppsV1().Deployments(ns)
	if _, err := deps.Create(ctx, &dep, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		cur, gerr := deps.Get(ctx, dep.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		cur.Labels = dep.Labels
		cur.Spec.Template = dep.Spec.Template
		// a changed tenant kubeconfig is only read at start
		if cur.Spec.Template.Annotations == nil {
			cur.Spec.Template.Annotations = map[string]string{}
		}
		cur.Spec.Template.Annotations["oct-shift-on-shift-virt/configured-at"] = time.Now().UTC().Format(time.RFC3339)
		if _, err := deps.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating controller deployment: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("creating controller deployment: %w", err)
	}
	return nil
}

func (s *StepExecutor) waitDeploymentAvailable(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d, err := s.client.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas == *d.Spec.Replicas && d.Status.AvailableReplicas == *d.Spec.Replicas &&
			d.Status.Replicas == *d.Spec.Replicas {
			return nil
		}
		if time.Now().After(deadline) {
			why := "not found"
			if err == nil {
				why = fmt.Sprintf("%d/%d available", d.Status.AvailableReplicas, *d.Spec.Replicas)
			}
			return fmt.Errorf("deployment %s/%s not available after %v (%s)", ns, name, timeout, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// runStorageJob runs one of the embedded scripts in the cluster namespace —
// the only place that can reach the nested API — and returns its log.
func (s *StepExecutor) runStorageJob(ctx context.Context, nc *crd.NestedCluster, jobName, script string, extra []corev1.EnvVar, timeout time.Duration) (string, error) {
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	labels := map[string]string{
		"oct-shift-on-shift-virt/cluster":   cluster,
		"oct-shift-on-shift-virt/component": strings.TrimPrefix(jobName, cluster+"-"),
	}
	backoff := int32(1)
	deadline := int64(timeout / time.Second)
	env := append([]corev1.EnvVar{
		{Name: "CLUSTER", Value: cluster},
		{Name: "HOST_NS", Value: ns},
		{Name: "DOMAIN", Value: nc.FQDN()},
		{Name: "VIP", Value: s.resolveControlPlaneVIP(ctx, nc)},
	}, extra...)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cluster + "-ops",
					Containers: []corev1.Container{{
						Name:            "run",
						Image:           "quay.io/cjanisze/sos-installer-helper:4.22",
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/bin/bash", "/m/" + script},
						Env:             env,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "m", MountPath: "/m", ReadOnly: true},
							{Name: "nested", MountPath: "/nested", ReadOnly: true},
							{Name: "ps", MountPath: "/ps", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "m", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: storageScriptsCM(cluster)}}}},
						{Name: "nested", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: nc.Status.KubeconfigRef,
							Items:      []corev1.KeyToPath{{Key: "kubeconfig", Path: "kubeconfig"}}}}},
						{Name: "ps", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: cluster + "-pull-secret",
							Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: ".dockerconfigjson"}}}}},
					},
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

func lastFatal(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "FATAL: ") {
			return strings.TrimPrefix(lines[i], "FATAL: ")
		}
	}
	return ""
}
