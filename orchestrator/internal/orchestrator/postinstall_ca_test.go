package orchestrator

import (
	"encoding/base64"
	"encoding/pem"
	"os"
	"strings"
	"testing"
)

func TestParseCABundle(t *testing.T) {
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-really-der")})
	good := base64.StdEncoding.EncodeToString(append(append([]byte{}, cert...), cert...))
	if parseCABundle("  "+good+"\n") != good {
		t.Fatal("two CERTIFICATE blocks must be accepted")
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")})
	for name, in := range map[string]string{
		"private key": base64.StdEncoding.EncodeToString(append(append([]byte{}, cert...), key...)),
		"not base64":  "!!!",
		"empty":       "",
		"trailing":    base64.StdEncoding.EncodeToString(append(append([]byte{}, cert...), []byte("junk")...)),
	} {
		if parseCABundle(in) != "" {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestWithCABundle(t *testing.T) {
	kc := "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: OLD\n    server: https://api.x:6443\n  name: x\n"
	out := withCABundle(kc, "NEW")
	if !strings.Contains(out, "    certificate-authority-data: NEW\n    server:") || strings.Contains(out, "OLD") {
		t.Fatalf("CA not replaced in place:\n%s", out)
	}
}

// The wait-install Job must never print credentials into its pod log.
func TestWaitInstallJobPrintsNoCredentials(t *testing.T) {
	src, err := os.ReadFile("steps.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"cat /tmp/work/config/auth/kubeconfig", "cat /tmp/work/config/auth/kubeadmin-password"} {
		if strings.Contains(string(src), bad) {
			t.Errorf("steps.go still contains %q", bad)
		}
	}
}

// openshift-install writes auth/kubeconfig without apiVersion/kind (keys in
// alphabetical order, "clusters:" first). 0.4.1 required "apiVersion:" and so
// rejected every real kubeconfig: PostInstall stored none and the network
// self-test, storage and virtualization all failed on a healthy cluster.
func TestInstallerCredentials(t *testing.T) {
	installerKC := "clusters:\n- cluster:\n    certificate-authority-data: Q0E=\n    server: https://api.x:6443\n  name: x\n" +
		"contexts:\n- context:\n    cluster: x\n    user: admin\n  name: admin\ncurrent-context: admin\npreferences: {}\n" +
		"users:\n- name: admin\n  user:\n    client-certificate-data: Q0VSVA==\n    client-key-data: S0VZ\n"
	kc, pw := installerCredentials([]byte(installerKC), []byte("abcde-fghij-klmno-pqrst\n"))
	if kc != installerKC || pw != "abcde-fghij-klmno-pqrst" {
		t.Fatalf("installer kubeconfig rejected: kc=%d bytes pw=%q", len(kc), pw)
	}
	withHeader := "apiVersion: v1\nkind: Config\n" + installerKC
	if kc, _ := installerCredentials([]byte(withHeader), []byte("p")); kc != withHeader {
		t.Error("kubeconfig with apiVersion rejected")
	}
	for name, c := range map[string][2]string{
		"empty kubeconfig":  {"", "p"},
		"not yaml":          {"{{{", "p"},
		"no clusters":       {"users:\n- name: a\n", "p"},
		"no users":          {"clusters:\n- name: a\n", "p"},
		"empty password":    {installerKC, ""},
		"password w/ space": {installerKC, "a b"},
		"multi-line pw":     {installerKC, "a\nb"},
	} {
		if kc, pw := installerCredentials([]byte(c[0]), []byte(c[1])); kc != "" || pw != "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
