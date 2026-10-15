package k8s

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Proves the generated pair is genuinely OpenSSH-compatible by making
// ssh-keygen derive the public key from our private key and comparing.
func TestGenerateSSHKeyPairMatchesSSHKeygen(t *testing.T) {
	priv, pub, err := GenerateSSHKeyPair("sos-automation")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp("", "k*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	os.WriteFile(f.Name(), priv, 0o600)

	out, err := exec.Command("ssh-keygen", "-y", "-f", f.Name()).Output()
	if err != nil {
		t.Fatalf("ssh-keygen rejected our private key: %v", err)
	}
	derived := strings.Fields(string(out))
	ours := strings.Fields(string(pub))
	if len(derived) < 2 || len(ours) < 2 {
		t.Fatalf("malformed keys: %q / %q", out, pub)
	}
	if derived[0] != ours[0] || derived[1] != ours[1] {
		t.Fatalf("public key mismatch:\n ssh-keygen: %s %.40s\n ours:       %s %.40s",
			derived[0], derived[1], ours[0], ours[1])
	}
	t.Logf("OK: %s %.30s... (%d byte private PEM)", ours[0], ours[1], len(priv))
}
