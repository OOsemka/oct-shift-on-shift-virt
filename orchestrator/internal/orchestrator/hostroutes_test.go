package orchestrator

import (
	"crypto/x509"
	"encoding/pem"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func parsePEMCert(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExternalAccessCertData(t *testing.T) {
	long := strings.Repeat("a", 40)
	console := long + "-console-oct-shift-on-shift-virt.apps.host.example.com"
	oauth := long + "-oauth-oct-shift-on-shift-virt.apps.host.example.com"
	api := long + "-api-oct-shift-on-shift-virt.apps.host.example.com"
	now := time.Now()

	data, changed, err := externalAccessCertData(long, []string{console, oauth}, api, nil, now)
	if err != nil || !changed {
		t.Fatalf("first issue: changed=%v err=%v", changed, err)
	}
	ca := parsePEMCert(t, data["ca.crt"])
	routes := parsePEMCert(t, data["routes.crt"])
	apiCert := parsePEMCert(t, data["api.crt"])
	if !ca.IsCA {
		t.Fatal("CA is not a CA")
	}
	for _, c := range []*x509.Certificate{ca, routes, apiCert} {
		if len(c.Subject.CommonName) > 64 {
			t.Errorf("CN longer than 64: %q", c.Subject.CommonName)
		}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for host, c := range map[string]*x509.Certificate{console: routes, oauth: routes, api: apiCert} {
		if _, err := c.Verify(x509.VerifyOptions{DNSName: host, Roots: pool, CurrentTime: now}); err != nil {
			t.Errorf("%s does not verify: %v", host, err)
		}
	}
	if _, err := apiCert.Verify(x509.VerifyOptions{DNSName: console, Roots: pool, CurrentTime: now}); err == nil {
		t.Error("API cert must not cover the console host")
	}

	// Same names: nothing to do.
	again, changed, err := externalAccessCertData(long, []string{oauth, console}, api, data, now)
	if err != nil || changed || string(again["routes.crt"]) != string(data["routes.crt"]) {
		t.Fatalf("re-run with same hosts should be a no-op: changed=%v err=%v", changed, err)
	}

	// New names: leaves re-issued, CA kept.
	moved, changed, err := externalAccessCertData(long, []string{"c.example.com", "o.example.com"}, "a.example.com", data, now)
	if err != nil || !changed {
		t.Fatalf("host change should re-issue: changed=%v err=%v", changed, err)
	}
	if string(moved["ca.crt"]) != string(data["ca.crt"]) || string(moved["ca.key"]) != string(data["ca.key"]) {
		t.Error("CA must be reused when only the hosts change")
	}
	if _, err := parsePEMCert(t, moved["routes.crt"]).Verify(x509.VerifyOptions{DNSName: "c.example.com", Roots: pool, CurrentTime: now}); err != nil {
		t.Errorf("re-issued cert does not verify against the kept CA: %v", err)
	}
}

func TestHostRouteNamesAndLabels(t *testing.T) {
	a, c, o := hostRouteNames("demo")
	if a != "demo-api" || c != "demo-console" || o != "demo-oauth" {
		t.Fatalf("route names: %s %s %s", a, c, o)
	}
	l := hostRoutesLabels("demo")
	if l["oct-shift-on-shift-virt/cluster"] != "demo" || l["oct-shift-on-shift-virt/component"] != "host-routes" {
		t.Fatalf("labels: %v", l)
	}
	if !strings.Contains(publishRoutesScript, "HOSTROUTES_OK") {
		t.Fatal("publish script not embedded")
	}
}

func TestDNSScriptExtraRecords(t *testing.T) {
	s := &StepExecutor{}
	py := s.generateDNSScript("10.0.0.5", "10.0.0.6", "demo.example.test",
		map[string]string{"Demo-Console-ns.apps.host.example.com": "10.0.0.6"})
	if !strings.Contains(py, `"demo-console-ns.apps.host.example.com.": "10.0.0.6",`) {
		t.Fatalf("extra record missing:\n%s", py[:600])
	}
	if py3, err := exec.LookPath("python3"); err == nil {
		cmd := exec.Command(py3, "-c", "import sys; compile(sys.stdin.read(), 'dns.py', 'exec')")
		cmd.Stdin = strings.NewReader(py)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dns.py does not compile: %v\n%s", err, out)
		}
	}
	if strings.Contains(py, "#SOS_EXTRA_RECORDS#") {
		t.Fatal("placeholder left in the script")
	}
	if plain := s.generateDNSScript("10.0.0.5", "10.0.0.6", "demo.example.test", nil); strings.Contains(plain, "#SOS_EXTRA") || !strings.Contains(plain, `"api-int.demo.example.test.": "10.0.0.5"`) {
		t.Fatal("script without extras is wrong")
	}
}
