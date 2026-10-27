package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func vsc(name, driver string, isDefault bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshotClass")
	u.SetName(name)
	u.Object["driver"] = driver
	if isDefault {
		u.SetAnnotations(map[string]string{"snapshot.storage.kubernetes.io/is-default-class": "true"})
	}
	return u
}

// The ISO snapshot class used to be hardcoded to one lab's ODF name
// (odf-storagecluster-rbdplugin-snapclass); other ODF installs call it
// ocs-storagecluster-rbdplugin-snapclass and MountISO failed there. It must
// follow the VM StorageClass's driver, even when another driver's class is
// the cluster default.
func TestISOSnapshotClassFollowsDriver(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "ocs-storage-ceph-rbd-virt-replica2"}, Provisioner: "openshift-storage.rbd.csi.ceph.com"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "localsc"}, Provisioner: "kubernetes.io/no-provisioner"},
	)
	scheme := runtime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotclasses"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{gvr: "VolumeSnapshotClassList"},
		vsc("px-csi-snapclass", "pxd.portworx.com", true),
		vsc("ocs-storagecluster-cephfsplugin-snapclass", "openshift-storage.cephfs.csi.ceph.com", false),
		vsc("ocs-storagecluster-rbdplugin-snapclass", "openshift-storage.rbd.csi.ceph.com", false),
	)
	s := &StepExecutor{client: &k8s.Client{Clientset: cs, DynamicClient: dyn}}

	got, err := s.isoSnapshotClass(context.Background(), "ocs-storage-ceph-rbd-virt-replica2")
	if err != nil || got != "ocs-storagecluster-rbdplugin-snapclass" {
		t.Fatalf("RBD class: got %q, %v; want ocs-storagecluster-rbdplugin-snapclass", got, err)
	}
	if _, err := s.isoSnapshotClass(context.Background(), "localsc"); err == nil || !strings.Contains(err.Error(), "no VolumeSnapshotClass") {
		t.Fatalf("no-provisioner class must fail with an actionable error, got %v", err)
	}
}
