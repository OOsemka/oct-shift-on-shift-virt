package api

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/OOsemka/oct-storefront/backend-security-kit/auth"
	"github.com/OOsemka/oct-storefront/backend-security-kit/kubeclient"
)

// Every route has a permission rule and the kit accepts the table, so a new route
// can't ship unchecked and the table can't drift from the mux.
func TestEveryRouteHasAPermissionRule(t *testing.T) {
	s := &Server{}
	table, err := s.AuthRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != len(s.Routes()) {
		t.Fatalf("table has %d routes, mux has %d", len(table), len(s.Routes()))
	}
	for _, rt := range table {
		if rt.Public && rt.Pattern != "GET /healthz" {
			t.Errorf("unexpected public route %s", rt.Pattern)
		}
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	tok := filepath.Join(t.TempDir(), "t")
	_ = os.WriteFile(tok, []byte("sa"), 0o600)
	kc, err := kubeclient.New(srv.URL, ca, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.New(auth.Options{Extension: "oct-shift-on-shift-virt", Client: kc}, table); err != nil {
		t.Fatalf("auth.New: %v", err)
	}
}

// The nested cluster's admin credentials need get secrets where they are stored.
func TestCredentialRoutesNeedSecretAccess(t *testing.T) {
	s := &Server{}
	for _, p := range []string{"GET /api/v1/clusters/{name}/kubeconfig", "GET /api/v1/clusters/{name}/kubeadmin"} {
		checks, _, err := s.checksFor(p)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range checks {
			if c.Resource == "secrets" && c.Verb == "get" && c.Namespace != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no get-secrets check", p)
		}
	}
}
