package cni

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestVersionsNewestFirstTwoEach(t *testing.T) {
	for _, nt := range []string{Calico, Cilium} {
		v := Versions(nt)
		if len(v) != 2 {
			t.Fatalf("%s: want latest + previous (2 versions), got %v", nt, v)
		}
		if !semverLess(v[1], v[0]) {
			t.Errorf("%s: versions not newest first: %v", nt, v)
		}
	}
	if Versions(OVNKubernetes) != nil {
		t.Error("OVNKubernetes has no embedded versions")
	}
}

func TestSemverLessNumeric(t *testing.T) {
	if !semverLess("v1.9.10", "v1.20.4") || semverLess("v1.20.4", "v1.19.10") || !semverLess("v3.32.3", "v3.33.0") {
		t.Error("semverLess must compare numerically")
	}
}

func TestValidate(t *testing.T) {
	calicoLatest := Versions(Calico)[0]
	cases := []struct {
		name, nt, ver, ocp string
		wantProblem        string
	}{
		{"default ovn", "", "", "4.18", ""},
		{"explicit ovn", OVNKubernetes, "", "4.22", ""},
		{"ovn with version", OVNKubernetes, "v1.0.0", "4.22", "must be empty"},
		{"calico latest implicit", Calico, "", "4.22", ""},
		{"calico explicit", Calico, calicoLatest, "4.22.3", ""},
		{"cilium previous", Cilium, Versions(Cilium)[1], "4.23", ""},
		{"unknown type", "Flannel", "", "4.22", "not supported"},
		{"unknown version", Calico, "v0.0.1", "4.22", "not offered"},
		{"old openshift", Cilium, "", "4.21", "needs OpenShift 4.22"},
	}
	for _, c := range cases {
		got := strings.Join(Validate(c.nt, c.ver, c.ocp), "; ")
		if c.wantProblem == "" && got != "" {
			t.Errorf("%s: unexpected problems: %s", c.name, got)
		}
		if c.wantProblem != "" && !strings.Contains(got, c.wantProblem) {
			t.Errorf("%s: want problem containing %q, got %q", c.name, c.wantProblem, got)
		}
	}
}

func TestManifestsOVNIsEmpty(t *testing.T) {
	m, err := Manifests(Params{NetworkType: ""})
	if err != nil || m != nil {
		t.Fatalf("OVN must render nothing, got %v, %v", m, err)
	}
}

func TestManifestsRenderEveryVersion(t *testing.T) {
	for _, nt := range []string{Calico, Cilium} {
		for _, v := range Versions(nt) {
			p := Params{NetworkType: nt, Version: v, ClusterName: "potaytoes", BaseDomain: "lab.example", ClusterNetworkCIDR: "10.132.0.0/14"}
			m, err := Manifests(p)
			if err != nil {
				t.Fatalf("%s %s: %v", nt, v, err)
			}
			if len(m) == 0 {
				t.Fatalf("%s %s: no manifests", nt, v)
			}
			all := ""
			for name, b := range m {
				if !strings.HasPrefix(name, "cni-"+strings.ToLower(nt)+"-") || !strings.HasSuffix(name, ".yaml") {
					t.Errorf("%s %s: bad file name %q", nt, v, name)
				}
				if strings.Contains(string(b), "__SOS_") || strings.Contains(string(b), "<cluster_name>") {
					t.Errorf("%s %s: %s still has a placeholder", nt, v, name)
				}
				assertParses(t, name, b)
				all += string(b)
			}
			if !strings.Contains(all, "api-int.potaytoes.lab.example") {
				t.Errorf("%s %s: API host not substituted", nt, v)
			}
			if !strings.Contains(all, "10.132.0.0/14") {
				t.Errorf("%s %s: pod CIDR not substituted", nt, v)
			}
			if !strings.Contains(all, "deployKubeProxy: false") {
				t.Errorf("%s %s: kube-proxy must be off (eBPF / kube-proxy replacement)", nt, v)
			}
		}
	}
}

func TestCalicoInstallationEncapsulates(t *testing.T) {
	m, err := Manifests(Params{NetworkType: Calico, ClusterName: "c", BaseDomain: "d", ClusterNetworkCIDR: "10.128.0.0/14"})
	if err != nil {
		t.Fatal(err)
	}
	inst := string(m["cni-calico-03-cr-installation.yaml"])
	for _, want := range []string{"kind: Installation", "encapsulation: VXLAN", "bgp: Disabled", "cidr: 10.128.0.0/14"} {
		if !strings.Contains(inst, want) {
			t.Errorf("Installation missing %q:\n%s", want, inst)
		}
	}
}

func TestCiliumOpenShiftSettings(t *testing.T) {
	m, err := Manifests(Params{NetworkType: Cilium, ClusterName: "c", BaseDomain: "d", ClusterNetworkCIDR: "10.128.0.0/14"})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, b := range m {
		all += string(b)
	}
	for _, want := range []string{
		"/var/run/multus/cni/net.d", "/var/lib/cni/bin", "tunnel-protocol: \"vxlan\"",
		"system:openshift:scc:privileged", "name: cilium\n",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("Cilium manifests missing %q", want)
		}
	}
}

// assertParses decodes every YAML document and checks it is a Kubernetes object.
func assertParses(t *testing.T, name string, b []byte) {
	t.Helper()
	r := utilyaml.NewYAMLReader(bufioReader(b))
	for {
		doc, err := r.Read()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(bytes.TrimSpace(stripComments(doc))) == 0 {
			continue
		}
		var obj map[string]interface{}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatalf("%s: invalid YAML: %v", name, err)
		}
		if obj["kind"] == nil || obj["apiVersion"] == nil {
			t.Errorf("%s: document without kind/apiVersion: %.80s", name, doc)
		}
	}
}

func stripComments(doc []byte) []byte {
	var out [][]byte
	for _, l := range bytes.Split(doc, []byte("\n")) {
		if t := bytes.TrimSpace(l); len(t) > 0 && t[0] != '#' && string(t) != "---" {
			out = append(out, l)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
