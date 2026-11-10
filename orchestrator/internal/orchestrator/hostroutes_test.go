package orchestrator

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
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

	// Name constraints: the CA can vouch only for this cluster's names.
	if !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 3 {
		t.Fatalf("CA must be name-constrained to the 3 host names: %v", ca.PermittedDNSDomains)
	}
	rogue, _, err := issueLeaf(ca, mustCAKey(t, data), "rogue", []string{"intranet.example.com"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePEMCert(t, rogue).Verify(x509.VerifyOptions{DNSName: "intranet.example.com", Roots: pool, CurrentTime: now}); err == nil {
		t.Fatal("a certificate for a foreign name signed by the cluster CA must not verify")
	}

	// Same names: nothing to do.
	again, changed, err := externalAccessCertData(long, []string{oauth, console}, api, data, now)
	if err != nil || changed || string(again["routes.crt"]) != string(data["routes.crt"]) {
		t.Fatalf("re-run with same hosts should be a no-op: changed=%v err=%v", changed, err)
	}

	// New names: the CA is constrained to the old names, so it is replaced
	// together with the leaves.
	moved, changed, err := externalAccessCertData(long, []string{"c.example.com", "o.example.com"}, "a.example.com", data, now)
	if err != nil || !changed {
		t.Fatalf("host change should re-issue: changed=%v err=%v", changed, err)
	}
	if string(moved["ca.crt"]) == string(data["ca.crt"]) {
		t.Error("a name-constrained CA must be replaced when the names change")
	}
	pool2 := x509.NewCertPool()
	pool2.AddCert(parsePEMCert(t, moved["ca.crt"]))
	if _, err := parsePEMCert(t, moved["routes.crt"]).Verify(x509.VerifyOptions{DNSName: "c.example.com", Roots: pool2, CurrentTime: now}); err != nil {
		t.Errorf("re-issued cert does not verify against the new CA: %v", err)
	}
	// Same names but the leaves are missing: the CA is kept.
	partial := map[string][]byte{"ca.crt": data["ca.crt"], "ca.key": data["ca.key"]}
	kept, _, err := externalAccessCertData(long, []string{console, oauth}, api, partial, now)
	if err != nil || string(kept["ca.crt"]) != string(data["ca.crt"]) {
		t.Errorf("CA must be reused when the names are unchanged: err=%v", err)
	}
}

func mustCAKey(t *testing.T, data map[string][]byte) *ecdsa.PrivateKey {
	t.Helper()
	_, k, err := parseCertAndKey(data["ca.crt"], data["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	return k
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

func TestAppRouteNames(t *testing.T) {
	n := appRouteName("a-very-long-cluster-name", "some-namespace", "my-application-route")
	if len(n)+len("-oct-shift-on-shift-virt") > 63 {
		t.Fatalf("host route name %q too long for a DNS label once the router appends the namespace", n)
	}
	if n != appRouteName("a-very-long-cluster-name", "some-namespace", "my-application-route") {
		t.Error("route name must be deterministic")
	}
	if n == appRouteName("a-very-long-cluster-name", "other-namespace", "my-application-route") {
		t.Error("same route name in another namespace must not collide")
	}
	if s := appSiblingName(strings.Repeat("x", 80)); len(s) > 63 || !strings.HasSuffix(s, appRouteSiblingSuffix) {
		t.Errorf("sibling name %q", s)
	}
	a := []crd.AppRoute{{Source: "ns/a", URL: "https://x"}}
	if !sameAppRoutes(a, []crd.AppRoute{{Source: "ns/a", URL: "https://x"}}) || sameAppRoutes(a, nil) {
		t.Error("sameAppRoutes")
	}
}

func TestPublishWanted(t *testing.T) {
	cases := []struct {
		all  bool
		ns   string
		anno string
		want bool
	}{
		{false, "demo", "", false},
		{false, "demo", "true", true},
		{true, "demo", "", true},
		{true, "demo", "false", false},
		{true, "openshift-console", "", false},
		{true, "openshift-monitoring", "", false},
		{true, "kube-system", "", false},
		{true, "openshift-console", "true", true},
		{true, "default", "", true},
	}
	for _, c := range cases {
		if got := publishWanted(c.all, c.ns, c.anno); got != c.want {
			t.Errorf("publishWanted(%v, %q, %q) = %v, want %v", c.all, c.ns, c.anno, got, c.want)
		}
	}
}
