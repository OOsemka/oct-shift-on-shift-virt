package k8s

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestVMCPU(t *testing.T) {
	plain := vmCPU(VMCreateParams{CPUCores: 8})
	if !reflect.DeepEqual(plain, map[string]interface{}{"cores": int64(8)}) {
		t.Errorf("without nested virtualization: %v", plain)
	}

	nested := vmCPU(VMCreateParams{CPUCores: 8, NestedVirt: true})
	feats, _ := nested["features"].([]interface{})
	got := map[string]string{}
	for _, f := range feats {
		m := f.(map[string]interface{})
		got[m["name"].(string)] = m["policy"].(string)
	}
	// optional, never require: "require vmx" would make the VM unschedulable
	// on AMD hosts (and svm on Intel).
	if !reflect.DeepEqual(got, map[string]string{"svm": "optional", "vmx": "optional"}) || nested["cores"] != int64(8) {
		t.Errorf("with nested virtualization: %v", nested)
	}
}

// The master VM spec for nested OpenShift: IO thread for etcd's disk, rng,
// no balloon, LiveMigrateIfPossible, spread across hosts, and none of the
// one-queue-per-vCPU options.
func TestCreateVMNestedOpenShiftTuning(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"})
	c := &Client{DynamicClient: dyn}
	if _, err := c.CreateVM(context.Background(), VMCreateParams{
		Name: "cluster-master-0", Namespace: "ns", ClusterName: "c1", Role: "master",
		CPUCores: 8, MemoryGi: 32, DiskGi: 120, StorageClassName: "sc", MACAddress: "52:54:00:00:00:01",
	}); err != nil {
		t.Fatal(err)
	}
	vm, err := dyn.Resource(vmGVR).Namespace("ns").Get(context.Background(), "cluster-master-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := vm.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	domain := spec["domain"].(map[string]interface{})
	devices := domain["devices"].(map[string]interface{})
	root := devices["disks"].([]interface{})[0].(map[string]interface{})

	if domain["ioThreadsPolicy"] != "shared" || root["dedicatedIOThread"] != true {
		t.Errorf("IO thread: policy=%v dedicated=%v", domain["ioThreadsPolicy"], root["dedicatedIOThread"])
	}
	if _, ok := devices["rng"]; !ok {
		t.Error("no virtio-rng")
	}
	if devices["autoattachMemBalloon"] != false {
		t.Error("memory balloon still attached")
	}
	for _, k := range []string{"networkInterfaceMultiqueue", "blockMultiQueue"} {
		if _, ok := devices[k]; ok {
			t.Errorf("%s set: it creates one queue per vCPU", k)
		}
	}
	if spec["evictionStrategy"] != "LiveMigrateIfPossible" {
		t.Errorf("evictionStrategy = %v", spec["evictionStrategy"])
	}
	term := spec["affinity"].(map[string]interface{})["podAntiAffinity"].(map[string]interface{})["preferredDuringSchedulingIgnoredDuringExecution"].([]interface{})[0].(map[string]interface{})["podAffinityTerm"].(map[string]interface{})
	sel := term["labelSelector"].(map[string]interface{})["matchLabels"].(map[string]interface{})
	tmplLabels := vm.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["metadata"].(map[string]interface{})["labels"].(map[string]interface{})
	if term["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("topologyKey = %v", term["topologyKey"])
	}
	// The selector must match the VM's own pod labels, or it spreads nothing.
	for k, v := range sel {
		if tmplLabels[k] != v {
			t.Errorf("anti-affinity selects %s=%v, VM pods carry %v", k, v, tmplLabels[k])
		}
	}
	if sel["oct-shift-on-shift-virt/role"] != "master" || sel["oct-shift-on-shift-virt/cluster"] != "c1" {
		t.Errorf("selector = %v", sel)
	}
	if _, ok := domain["cpu"].(map[string]interface{})["features"]; ok {
		t.Error("svm/vmx requested without nested virtualization")
	}
}
