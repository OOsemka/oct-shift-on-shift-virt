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
