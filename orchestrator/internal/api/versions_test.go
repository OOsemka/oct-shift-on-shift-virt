package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOCPVersionsOffersOnlySupported(t *testing.T) {
	srv, _, _ := hostNetworkServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ocp-versions", nil))
	var body struct {
		Items  []string          `json:"items"`
		Latest map[string]string `json:"latest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Items, []string{"5.0", "4.22"}) {
		t.Errorf("items = %v, want [5.0 4.22]", body.Items)
	}
	if body.Latest["4.22"] != "4.22.1" || body.Latest["5.0"] != "5.0.1" {
		t.Errorf("latest = %v", body.Latest)
	}
}

func TestCreateRefusesUnsupportedVersion(t *testing.T) {
	srv, _, dyn := hostNetworkServer(t)
	spec := manualSpec("old", "10.200.0.0/16")
	spec.OCPVersion = "4.21"
	rec := post(srv, "/api/v1/clusters", spec)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not supported") {
		t.Fatalf("4.21 must be refused with 400, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := dyn.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(context.Background(), "old", metav1.GetOptions{}); err == nil {
		t.Error("a NestedCluster was created for an unsupported version")
	}
	// An exact release of a supported minor is fine.
	spec = manualSpec("exact", "10.200.0.0/16")
	spec.OCPVersion = "4.22.1"
	if rec := post(srv, "/api/v1/clusters", spec); rec.Code != http.StatusCreated {
		t.Errorf("4.22.1 refused: %d %s", rec.Code, rec.Body.String())
	}
}
