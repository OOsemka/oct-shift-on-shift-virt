package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/release"
)

// TestMain points the release resolver at a local fake update graph: the
// create path resolves spec.ocpVersion, and tests must not need the internet.
// Every stable-X.Y channel holds one release, X.Y.1.
func TestMain(m *testing.M) {
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var nodes []map[string]string
		if ch := r.URL.Query().Get("channel"); strings.HasPrefix(ch, "stable-") {
			v := strings.TrimPrefix(ch, "stable-") + ".1"
			nodes = append(nodes, map[string]string{
				"version": v,
				"payload": "quay.io/openshift-release-dev/ocp-release@sha256:" + strings.ReplaceAll(v, ".", ""),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"nodes": nodes})
	}))
	release.Default = &release.Resolver{GraphURL: graph.URL, Arch: "amd64", HTTP: graph.Client(), TTL: time.Minute}
	code := m.Run()
	graph.Close()
	os.Exit(code)
}
