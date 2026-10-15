package orchestrator

import (
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/installer"
)

// The console wizard ships installConfigOverrides containing this exact line.
const wizardOverride = `apiVersion: v1
metadata:
  name: c
platform:
  none: {}
pullSecret: '<PULL_SECRET>'
sshKey: '<SSH_KEY>'`

func TestWizardOverrideGetsBlockScalarForTwoKeys(t *testing.T) {
	two := "ssh-ed25519 AAAAC3 user@host\nssh-rsa AAAAB3 sos-automation"
	out := sshKeyLineRe.ReplaceAllString(wizardOverride, installer.SSHKeyYAML(two))
	if strings.Contains(out, "<SSH_KEY>") {
		t.Fatalf("sshKey line not replaced:\n%s", out)
	}
	if !strings.Contains(out, "sshKey: |-") {
		t.Fatalf("expected block scalar:\n%s", out)
	}
	for _, k := range []string{"user@host", "sos-automation"} {
		if !strings.Contains(out, k) {
			t.Fatalf("lost key %q", k)
		}
	}
}

func TestWizardOverrideSingleKeyUnchangedShape(t *testing.T) {
	one := "ssh-ed25519 AAAAC3 user@host"
	out := sshKeyLineRe.ReplaceAllString(wizardOverride, installer.SSHKeyYAML(one))
	if !strings.Contains(out, "sshKey: 'ssh-ed25519 AAAAC3 user@host'") {
		t.Fatalf("single key should stay a quoted scalar:\n%s", out)
	}
}
