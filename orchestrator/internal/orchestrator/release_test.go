package orchestrator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

func TestWaitForImage(t *testing.T) {
	nc := &crd.NestedCluster{}
	nc.Spec.ClusterName = "c1"

	// ISO generated before releases were resolved: helper image as before.
	img, pull, secrets := waitForImage(nc)
	if img != "quay.io/cjanisze/sos-installer-helper:4.22" || pull != corev1.PullAlways || secrets != nil {
		t.Errorf("legacy: %s %s %v", img, pull, secrets)
	}

	nc.Status.InstallerImage = "quay.io/openshift-release-dev/ocp-v5.0-art-dev@sha256:abc"
	img, pull, secrets = waitForImage(nc)
	if img != nc.Status.InstallerImage || pull != corev1.PullIfNotPresent ||
		len(secrets) != 1 || secrets[0].Name != "c1-pull-secret" {
		t.Errorf("release image: %s %s %v", img, pull, secrets)
	}

	nc.Spec.PullSecretRef.Name = "custom"
	if _, _, secrets = waitForImage(nc); secrets[0].Name != "custom" {
		t.Errorf("pull secret ref ignored: %v", secrets)
	}
}

func TestParseInstallerImage(t *testing.T) {
	good := "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:258c5270"
	if got, err := parseInstallerImage([]byte(good + "\n")); err != nil || got != good {
		t.Errorf("good: %q %v", got, err)
	}
	for _, bad := range []string{"", "error: unauthorized", "quay.io/x:latest", good + "\n" + good} {
		if _, err := parseInstallerImage([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
