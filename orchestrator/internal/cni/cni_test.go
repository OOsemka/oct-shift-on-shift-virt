package cni

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"regexp"
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
			p := Params{NetworkType: nt, Version: v, ClusterName: "potaytoes", BaseDomain: "lab.example", ClusterNetworkCIDR: "10.132.0.0/14", DNSServerIP: "10.100.0.12"}
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
	m, err := Manifests(Params{NetworkType: Calico, ClusterName: "c", BaseDomain: "d", ClusterNetworkCIDR: "10.128.0.0/14", DNSServerIP: "10.100.0.12"})
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

func TestCalicoPinsOperatorDNS(t *testing.T) {
	p := Params{NetworkType: Calico, ClusterName: "c", BaseDomain: "d", ClusterNetworkCIDR: "10.128.0.0/14"}
	if _, err := Manifests(p); err == nil {
		t.Fatal("Calico without a DNS server address must be refused")
	}
	p.DNSServerIP = "10.100.0.12"
	m, err := Manifests(p)
	if err != nil {
		t.Fatal(err)
	}
	op := string(m["cni-calico-02-tigera-operator.yaml"])
	for _, want := range []string{"dnsPolicy: None", "nameservers:\n          - 10.100.0.12"} {
		if !strings.Contains(op, want) {
			t.Errorf("tigera-operator missing %q", want)
		}
	}
	if strings.Contains(op, "ClusterFirstWithHostNet") {
		t.Error("tigera-operator still resolves through cluster DNS")
	}
	cm := string(m["cni-calico-01-configmap-kubernetes-services-endpoint.yaml"])
	if !strings.Contains(cm, `KUBERNETES_SERVICE_HOST: "api-int.c.d"`) {
		t.Errorf("API endpoint must be the api-int name (the certificate covers it):\n%s", cm)
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
		`policy-cidr-match-mode: "nodes"`, "kind: CiliumClusterwideNetworkPolicy", "- remote-node",
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

// Third-party images run privileged in the nested network stack: a re-pushed
// tag must not change what new clusters run. hack/update-cni-manifests.sh pins
// every image by digest; this keeps it that way.
func TestEveryImagePinnedByDigest(t *testing.T) {
	re := regexp.MustCompile(`(?m)^\s*(?:- )?image:\s*"?([^"\s]+)"?\s*$`)
	seen := 0
	err := fs.WalkDir(manifestsFS, "manifests", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(manifestsFS, path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "kind: ImageSet") {
			return nil // bare names + digest: field; TestCalicoImageSet checks it
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen++
			if !strings.Contains(m[1], "@sha256:") {
				t.Errorf("%s: image %q is not pinned by digest (run hack/update-cni-manifests.sh)", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no image: lines found; the pattern no longer matches the manifests")
	}
}

// The ImageSet pins the images the Tigera operator deploys itself. The
// operator only uses one named calico-<CalicoRelease>, rejects images it does
// not know (they all live under calico/) and needs a sha256 digest for each.
func TestCalicoImageSet(t *testing.T) {
	digest := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for _, v := range Versions(Calico) {
		m, err := Manifests(Params{NetworkType: Calico, Version: v, ClusterName: "c", BaseDomain: "d",
			ClusterNetworkCIDR: "10.128.0.0/14", DNSServerIP: "10.100.0.12"})
		if err != nil {
			t.Fatal(err)
		}
		b, ok := m["cni-calico-03-imageset.yaml"]
		if !ok {
			t.Fatalf("%s: no ImageSet rendered", v)
		}
		var is struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Images []struct {
					Image  string `json:"image"`
					Digest string `json:"digest"`
				} `json:"images"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &is); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if is.APIVersion != "operator.tigera.io/v1" || is.Kind != "ImageSet" || is.Metadata.Name != "calico-"+v {
			t.Errorf("%s: got %s %s %q, want operator.tigera.io/v1 ImageSet calico-%s", v, is.APIVersion, is.Kind, is.Metadata.Name, v)
		}
		seen := map[string]bool{}
		for _, img := range is.Spec.Images {
			if !strings.HasPrefix(img.Image, "calico/") || strings.ContainsAny(img.Image, ":@") {
				t.Errorf("%s: image %q must be a bare calico/<name>", v, img.Image)
			}
			if !digest.MatchString(img.Digest) {
				t.Errorf("%s: %s has digest %q", v, img.Image, img.Digest)
			}
			if seen[img.Image] {
				t.Errorf("%s: %s listed twice", v, img.Image)
			}
			seen[img.Image] = true
		}
		if !seen["calico/node"] {
			t.Errorf("%s: calico/node missing from the ImageSet", v)
		}
	}
}
