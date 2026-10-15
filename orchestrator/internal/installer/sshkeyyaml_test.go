package installer

import (
	"strings"
	"testing"
)

func TestSSHKeyYAMLSingle(t *testing.T) {
	got := SSHKeyYAML("ssh-ed25519 AAAAC3 user@host")
	want := "sshKey: 'ssh-ed25519 AAAAC3 user@host'"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSSHKeyYAMLMultiIsBlockScalar(t *testing.T) {
	got := SSHKeyYAML("ssh-ed25519 AAAAC3 user@host\nssh-rsa AAAAB3 sos-automation")
	if !strings.HasPrefix(got, "sshKey: |-\n") {
		t.Fatalf("multi-key must use a block scalar, got:\n%s", got)
	}
	if strings.Contains(got, "'") {
		t.Fatalf("block scalar must not be quoted:\n%s", got)
	}
	for _, k := range []string{"user@host", "sos-automation"} {
		if !strings.Contains(got, k) {
			t.Fatalf("lost key %q:\n%s", k, got)
		}
	}
}
