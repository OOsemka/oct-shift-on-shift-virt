package orchestrator

import (
	"strings"
	"testing"
)

// Reproduces the real run-15/16 wait-install log shape: the job ran under
// `set -x`, so bash echoed each marker in a trace line BEFORE the marker
// itself. Anchoring on the first match captured that trace plus a dangling
// quote and produced an unusable kubeconfig secret.
const tracedLog = `+ openshift-install agent wait-for install-complete --dir=/tmp/work/config
level=info msg=Install complete!
+ echo '=== KUBECONFIG ==='
=== KUBECONFIG ===
+ cat /tmp/work/config/auth/kubeconfig
apiVersion: v1
clusters:
- cluster:
    server: https://api.sos-nightly.chris.lab:6443
  name: sos-nightly
kind: Config
users:
- name: admin
  user:
    client-key-data: QUJD
=== KUBEADMIN ===
+ echo '=== KUBEADMIN ==='
+ cat /tmp/work/config/auth/kubeadmin-password
Tupco-EKoyB-aeuiR-z9pkE
=== DONE ===
+ echo '=== DONE ==='
`

func TestSectionBetweenIgnoresXtrace(t *testing.T) {
	kc := sectionBetween(tracedLog, "=== KUBECONFIG ===", "=== KUBEADMIN ===")
	if i := strings.Index(kc, "apiVersion:"); i > 0 {
		kc = kc[i:]
	}
	if !strings.HasPrefix(kc, "apiVersion:") {
		t.Fatalf("kubeconfig must start at apiVersion, got %.40q", kc)
	}
	for _, bad := range []string{"+ cat ", "+ echo ", "=== KUBECONFIG ==="} {
		if strings.Contains(kc, bad) {
			t.Fatalf("kubeconfig carries shell trace %q", bad)
		}
	}
	if !strings.Contains(kc, "client-key-data:") {
		t.Fatal("kubeconfig truncated")
	}

	pw := sectionBetween(tracedLog, "=== KUBEADMIN ===", "=== DONE ===")
	if f := strings.Fields(pw); len(f) > 0 {
		pw = f[len(f)-1]
	}
	if pw != "Tupco-EKoyB-aeuiR-z9pkE" {
		t.Fatalf("kubeadmin password not clean: %q", pw)
	}
}

func TestSectionBetweenMissingMarkers(t *testing.T) {
	if got := sectionBetween("no markers here", "=== A ===", "=== B ==="); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
