package crd

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Lab defaults: one profile of settings that admins store once, so the full
// wizard can leave the credentials empty and the quick compact cluster needs
// nothing but a name. They live in the Secret LabDefaultsSecret in the
// operator namespace; admins write it through the Kubernetes API with their
// own permissions, the orchestrator only reads it. The pull secret is never
// sent back to the browser.
const (
	LabDefaultsSecret      = "sos-lab-defaults"
	LabDefaultsPullSecret  = "pullSecret"
	LabDefaultsSSHKey      = "sshPublicKey"
	LabDefaultsSettingsKey = "settings.json"
)

// LabSettings are the non-secret lab defaults.
type LabSettings struct {
	BaseDomain       string `json:"baseDomain,omitempty"`
	OCPVersion       string `json:"ocpVersion,omitempty"`
	StorageClassName string `json:"storageClassName,omitempty"`
	// IPPool supplies the network and external addresses of quick clusters.
	IPPool         string `json:"ipPool,omitempty"`
	MasterCores    int    `json:"masterCores,omitempty"`
	MasterMemoryGi int    `json:"masterMemoryGi,omitempty"`
	MasterDiskGi   int    `json:"masterDiskGi,omitempty"`
}

// WithDefaults fills unset compact-node sizes with the wizard's defaults.
func (s LabSettings) WithDefaults() LabSettings {
	if s.MasterCores <= 0 {
		s.MasterCores = 8
	}
	if s.MasterMemoryGi <= 0 {
		s.MasterMemoryGi = 32
	}
	if s.MasterDiskGi <= 0 {
		s.MasterDiskGi = 120
	}
	return s
}

// ParseLabSettings reads settings.json; empty or missing means no settings.
func ParseLabSettings(raw []byte) (LabSettings, error) {
	var s LabSettings
	if len(strings.TrimSpace(string(raw))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("lab defaults settings.json is not valid JSON: %w", err)
	}
	return s, nil
}

// ValidatePullSecret checks the shape of a pull secret without logging it.
func ValidatePullSecret(raw string) error {
	var ps struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ps); err != nil {
		return fmt.Errorf("the pull secret is not valid JSON")
	}
	if len(ps.Auths) == 0 {
		return fmt.Errorf("the pull secret has no registries under \"auths\"")
	}
	return nil
}

// parseSSHKey returns the type and decoded blob of an OpenSSH public key line.
func parseSSHKey(line string) (string, []byte, error) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 {
		return "", nil, fmt.Errorf("expected \"<type> <key> [comment]\"")
	}
	if !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") && !strings.HasPrefix(f[0], "sk-") {
		return "", nil, fmt.Errorf("%q is not an SSH public key type", f[0])
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 16 {
		return "", nil, fmt.Errorf("the key data is not valid base64")
	}
	return f[0], blob, nil
}

// ValidateSSHPublicKey accepts one OpenSSH public key (never a private key).
func ValidateSSHPublicKey(key string) error {
	if strings.Contains(key, "PRIVATE KEY") {
		return fmt.Errorf("this is a private key; paste the public key (the .pub file)")
	}
	_, _, err := parseSSHKey(key)
	return err
}

// SSHKeyFingerprint is the SHA256 fingerprint ssh-keygen -l prints.
func SSHKeyFingerprint(key string) string {
	typ, blob, err := parseSSHKey(key)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return typ + " SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}
