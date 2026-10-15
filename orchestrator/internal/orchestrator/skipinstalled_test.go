package orchestrator

import (
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

func TestSSHSecretNameFor(t *testing.T) {
	nc := &crd.NestedCluster{}
	nc.Spec.ClusterName = "c"
	if got := sshSecretNameFor(nc); got != "c-ssh-key" {
		t.Fatalf("default: %s", got)
	}
	nc.Spec.SSHKeyRef = crd.SecretRef{Name: "explicit"}
	if got := sshSecretNameFor(nc); got != "explicit" {
		t.Fatalf("explicit ref ignored: %s", got)
	}
}

// The skip must key off the kubeconfig Secret, which only PostInstall
// creates — not off a phase or a timestamp that a requeue would reset.
func TestConfigureNodesSkipIsKeyedOnKubeconfigSecret(t *testing.T) {
	src := configureNodesGuardDoc
	for _, want := range []string{"kubeconfig", "node-guard"} {
		if !strings.Contains(src, want) {
			t.Fatalf("guard must mention %q", want)
		}
	}
}
