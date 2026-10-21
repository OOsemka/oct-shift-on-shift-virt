package orchestrator

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The desktop install must be a retrying unit that waits for DNS, not a
// one-shot background command racing dns.py's arrival.
func TestBastionUserDataDesktopUnit(t *testing.T) {
	ud := bastionUserData("b", "ssh-ed25519 AAAA k", []string{"10.0.0.100"}, true, "core", "https://console.example")
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(ud, "#cloud-config\n")), &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v", err)
	}
	files := map[string]string{}
	for _, f := range doc["write_files"].([]interface{}) {
		m := f.(map[string]interface{})
		files[m["path"].(string)] = m["content"].(string)
	}
	unit := files["/etc/systemd/system/sos-desktop-install.service"]
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Fatalf("desktop unit missing or not retrying:\n%s", unit)
	}
	if !strings.Contains(files["/usr/local/sbin/sos-desktop-install"], "until getent hosts") {
		t.Fatal("desktop install does not wait for DNS")
	}
	if strings.Contains(ud, "nohup") {
		t.Fatal("one-shot nohup install is back")
	}
	if plain := bastionUserData("b", "", nil, false, "", ""); strings.Contains(plain, "sos-desktop") {
		t.Fatal("non-graphical bastion must not get the desktop unit")
	}
}
