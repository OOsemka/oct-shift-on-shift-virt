package orchestrator

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func masterVM(t *testing.T, name string, bastion bool) *unstructured.Unstructured {
	t.Helper()
	vm := map[string]interface{}{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]interface{}{"name": name, "namespace": "m1-ns",
			"labels": map[string]interface{}{"oct-shift-on-shift-virt/cluster": "m1", "oct-shift-on-shift-virt/role": "master"}},
		"spec": map[string]interface{}{
			"runStrategy": "Halted",
			"dataVolumeTemplates": []interface{}{map[string]interface{}{
				"metadata": map[string]interface{}{"name": name + "-rootdisk"},
				"spec": map[string]interface{}{"sourceRef": map[string]interface{}{"kind": "DataSource", "name": "centos"},
					"storage": map[string]interface{}{"volumeMode": "Block", "accessModes": []interface{}{"ReadWriteMany"},
						"resources": map[string]interface{}{"requests": map[string]interface{}{"storage": "120Gi"}}}}}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"oct-shift-on-shift-virt/cluster": "m1"}},
				"spec": map[string]interface{}{
					"domain": map[string]interface{}{
						"firmware": map[string]interface{}{"uuid": "u-1", "serial": "s-1"},
						"devices": map[string]interface{}{"interfaces": []interface{}{
							map[string]interface{}{"name": "default", "macAddress": "52:54:00:00:00:01"},
							map[string]interface{}{"name": bastionExternalIface, "macAddress": "02:00:00:00:00:01"}}}},
					"affinity": map[string]interface{}{"podAntiAffinity": map[string]interface{}{"x": map[string]interface{}{"matchLabels": map[string]interface{}{"oct-shift-on-shift-virt/cluster": "m1"}}}},
					"networks": []interface{}{map[string]interface{}{"name": "default", "pod": map[string]interface{}{}},
						map[string]interface{}{"name": bastionExternalIface, "multus": map[string]interface{}{"networkName": "vlan-a"}}},
					"volumes": []interface{}{
						map[string]interface{}{"name": "rootdisk", "dataVolume": map[string]interface{}{"name": name + "-rootdisk"}},
						map[string]interface{}{"name": "cloudinit", "cloudInitNoCloud": map[string]interface{}{"secretRef": map[string]interface{}{"name": "m1-bastion-cloudinit"}}}},
				}}}}
	return &unstructured.Unstructured{Object: vm}
}

func TestCloneVMObject(t *testing.T) {
	vm := masterVM(t, "cluster-master-0", false)
	if rootDiskPVC(vm) != "cluster-master-0-rootdisk" {
		t.Fatal("root disk PVC not found")
	}
	size, modes, err := diskShape(vm)
	if err != nil || size != "120Gi" || len(modes) != 1 || modes[0] != "ReadWriteMany" {
		t.Fatalf("disk shape: %s %v %v", size, modes, err)
	}
	c := cloneVMObject(vm, "cluster-master-0", "c1-ns", "m1", "c1", "cluster-master-0-rootdisk")
	b, _ := json.Marshal(c.Object)
	js := string(b)
	if strings.Contains(js, `"m1"`) {
		t.Errorf("master name left in the clone VM: %s", js)
	}
	if !strings.Contains(js, `"persistentVolumeClaim":{"claimName":"cluster-master-0-rootdisk"}`) {
		t.Error("clone VM must use the restored PVC")
	}
	if strings.Contains(js, "dataVolumeTemplates") || !strings.Contains(js, `"runStrategy":"Always"`) {
		t.Error("clone VM must have no dataVolumeTemplates and run")
	}
	if !strings.Contains(js, `"uuid":"u-1"`) || !strings.Contains(js, "52:54:00:00:00:01") {
		t.Error("node VMs keep their firmware UUID and MAC")
	}
	if vm.Object["spec"].(map[string]interface{})["dataVolumeTemplates"] == nil {
		t.Error("the master VM object must not be modified")
	}
}

func TestPrepareCloneBastion(t *testing.T) {
	vm := masterVM(t, "m1-bastion", true)
	c := cloneVMObject(vm, "c1-bastion", "c1-ns", "m1", "c1", "c1-bastion-rootdisk")
	prepareCloneBastion(c, "vlan-a", "02:aa:bb:cc:dd:ee", "c1-bastion-cloudinit")
	b, _ := json.Marshal(c.Object)
	js := string(b)
	if strings.Contains(js, "u-1") || strings.Contains(js, "s-1") {
		t.Error("the bastion must get a new firmware UUID (cloud-init instance-id)")
	}
	if !strings.Contains(js, "02:aa:bb:cc:dd:ee") || strings.Contains(js, "02:00:00:00:00:01") {
		t.Error("external MAC not replaced")
	}
	if !strings.Contains(js, `"secretRef":{"name":"c1-bastion-cloudinit"}`) {
		t.Error("cloud-init secret not replaced")
	}
}

func TestNewExpressCreds(t *testing.T) {
	data, pw, err := newExpressCreds("c1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 23 || bcrypt.CompareHashAndPassword(data["kubeadmin-hash"], []byte(pw)) != nil {
		t.Fatalf("kubeadmin hash does not match the password (len %d)", len(pw))
	}
	parse := func(k string) *x509.Certificate {
		blk, _ := pem.Decode(data[k])
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, user := parse("admin-ca.crt"), parse("admin.crt")
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := user.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("admin cert does not chain to the new client CA: %v", err)
	}
	if user.Subject.CommonName != "system:admin" || len(user.Subject.Organization) != 1 || user.Subject.Organization[0] != "system:masters" {
		t.Errorf("admin cert subject: %v", user.Subject)
	}
	if string(data["issuer"]) != "https://c1.clusters.sos.invalid" {
		t.Errorf("issuer: %s", data["issuer"])
	}
	_, pw2, _ := newExpressCreds("c1", time.Now())
	if pw2 == pw {
		t.Error("passwords must be random")
	}
}

func TestBuildAdminKubeconfig(t *testing.T) {
	kc := buildAdminKubeconfig("m1.example.test", "Q0E=", []byte("CRT"), []byte("KEY"))
	if !validKubeconfig(kc) || !strings.Contains(kc, "https://api.m1.example.test:6443") {
		t.Fatalf("kubeconfig: %s", kc)
	}
}

func TestExpressScriptsEmbedded(t *testing.T) {
	for name, s := range map[string]string{"wait": expressWaitAPIScript, "finish": expressFinishScript} {
		if !strings.Contains(s, "source /m/lib.sh") {
			t.Errorf("%s script not embedded", name)
		}
	}
	if !strings.Contains(expressSanitizeScript, "SANITIZE_OK") {
		t.Error("sanitize script not embedded")
	}
	// Serving and kubelet CAs must never be rotated or purged on a running
	// clone: the nodes need them re-delivered first (nodes went NotReady).
	for _, bad := range []string{"kube-apiserver-to-kubelet", "loadbalancer-serving", "localhost-serving", "service-network-serving", "regenerate-top-level -A"} {
		if strings.Contains(expressFinishScript+expressSanitizeScript, bad) {
			t.Errorf("Express scripts must not touch %s", bad)
		}
	}
}
