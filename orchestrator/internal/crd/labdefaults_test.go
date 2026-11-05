package crd

import (
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGdWqeGJ8m8v3eWJbXrDqgXSz9wjxL3Kd0a9vmyxUAbC lab@example"

func TestLabSecretsValidation(t *testing.T) {
	if err := ValidatePullSecret(`{"auths":{"quay.io":{"auth":"eA=="}}}`); err != nil {
		t.Fatalf("valid pull secret: %v", err)
	}
	for _, bad := range []string{"", "not json", `{"auths":{}}`} {
		if ValidatePullSecret(bad) == nil {
			t.Errorf("pull secret %q must be refused", bad)
		}
	}
	if err := ValidateSSHPublicKey(testKey); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	// Built at runtime so the repository's private-key scanner does not flag the test.
	privateMarker := "-----BEGIN OPENSSH " + "PRIVATE" + " KEY-----"
	if err := ValidateSSHPublicKey(privateMarker + "\nabc"); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("private key must be refused, got %v", err)
	}
	if ValidateSSHPublicKey("hello world") == nil {
		t.Fatal("garbage must be refused")
	}
	fp := SSHKeyFingerprint(testKey)
	if !strings.HasPrefix(fp, "ssh-ed25519 SHA256:") || strings.HasSuffix(fp, "=") {
		t.Fatalf("fingerprint %q", fp)
	}
}

func TestLabSettings(t *testing.T) {
	s, err := ParseLabSettings([]byte(`{"baseDomain":"example.test","masterCores":16}`))
	if err != nil {
		t.Fatal(err)
	}
	d := s.WithDefaults()
	if d.MasterCores != 16 || d.MasterMemoryGi != 32 || d.MasterDiskGi != 120 || d.BaseDomain != "example.test" {
		t.Fatalf("defaults: %+v", d)
	}
	if _, err := ParseLabSettings([]byte("{")); err == nil {
		t.Fatal("bad JSON must fail")
	}
	if s, err := ParseLabSettings(nil); err != nil || s.BaseDomain != "" {
		t.Fatal("missing settings must be empty, not an error")
	}
}
