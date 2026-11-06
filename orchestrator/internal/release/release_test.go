package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func node(v string) graphNode {
	return graphNode{Version: v, Payload: "quay.io/openshift-release-dev/ocp-release@sha256:" + strings.ReplaceAll(v, ".", "")}
}

// fakeGraph serves channel -> nodes and counts requests.
func fakeGraph(t *testing.T, channels map[string][]graphNode) (*Resolver, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Query().Get("arch") != "amd64" {
			t.Errorf("arch = %q", r.URL.Query().Get("arch"))
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"nodes": channels[r.URL.Query().Get("channel")]})
	}))
	t.Cleanup(srv.Close)
	return &Resolver{GraphURL: srv.URL, Arch: "amd64", HTTP: srv.Client(), TTL: time.Minute}, &hits
}

func TestResolveMinorPicksNewestStable(t *testing.T) {
	r, _ := fakeGraph(t, map[string][]graphNode{
		// Unsorted, with a 4.21 upgrade source and a pre-release.
		"stable-4.22": {node("4.22.9"), node("4.22.22"), node("4.21.40"), node("4.22.14"), node("4.22.23-rc.1")},
	})
	rel, err := r.Resolve(context.Background(), "4.22")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "4.22.22" || !strings.Contains(rel.Image, "@sha256:42222") {
		t.Errorf("got %+v", rel)
	}
}

func TestResolveMinorFallsBackToFast(t *testing.T) {
	r, _ := fakeGraph(t, map[string][]graphNode{
		"stable-5.0": {},
		"fast-5.0":   {node("4.22.22"), node("5.0.0"), node("5.0.1")},
	})
	rel, err := r.Resolve(context.Background(), "5.0")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "5.0.1" {
		t.Errorf("got %+v, want 5.0.1 (not the 4.22 upgrade source)", rel)
	}
}

func TestResolveMinorWithoutReleases(t *testing.T) {
	r, _ := fakeGraph(t, map[string][]graphNode{"candidate-5.1": {node("5.1.0-ec.1")}})
	if _, err := r.Resolve(context.Background(), "5.1"); err == nil || !strings.Contains(err.Error(), "no OpenShift 5.1 release") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveExact(t *testing.T) {
	r, _ := fakeGraph(t, map[string][]graphNode{
		"stable-4.22":    {node("4.22.14"), node("4.22.22")},
		"candidate-5.0":  {node("5.0.0-rc.8")},
		"stable-5.0":     {node("5.0.0")},
		"fast-5.0":       {node("5.0.0")},
		"candidate-4.22": {node("4.22.22")},
	})
	for req, want := range map[string]string{"4.22.14": "4.22.14", "5.0.0": "5.0.0", "5.0.0-rc.8": "5.0.0-rc.8"} {
		rel, err := r.Resolve(context.Background(), req)
		if err != nil || rel.Version != want || !strings.Contains(rel.Image, "@sha256:") {
			t.Errorf("Resolve(%s) = %+v, %v", req, rel, err)
		}
	}
	if _, err := r.Resolve(context.Background(), "4.22.99"); err == nil || !strings.Contains(err.Error(), "not a published release") {
		t.Errorf("4.22.99: err = %v", err)
	}
}

func TestResolveOfflineFallback(t *testing.T) {
	r := &Resolver{GraphURL: "http://127.0.0.1:1", Arch: "amd64", HTTP: &http.Client{Timeout: time.Second}, TTL: time.Minute}
	rel, err := r.Resolve(context.Background(), "4.22.22")
	if err != nil || rel.Image != "quay.io/openshift-release-dev/ocp-release:4.22.22-x86_64" {
		t.Errorf("exact offline: %+v, %v", rel, err)
	}
	if _, err := r.Resolve(context.Background(), "4.22"); err == nil {
		t.Error("a minor version must not resolve without the graph")
	}
	if _, err := r.Resolve(context.Background(), "5.0.0-rc.1"); err == nil {
		t.Error("a pre-release must not resolve by tag without the graph")
	}
}

func TestResolveCaches(t *testing.T) {
	r, hits := fakeGraph(t, map[string][]graphNode{"stable-4.22": {node("4.22.22")}})
	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(context.Background(), "4.22"); err != nil {
			t.Fatal(err)
		}
	}
	if *hits != 1 {
		t.Errorf("graph queried %d times, want 1", *hits)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "4", "latest", "4.22.x", "v4.22", "4.22; rm -rf /"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestLess(t *testing.T) {
	ordered := []string{"4.21.40", "4.22.2", "4.22.10", "5.0.0-ec.1", "5.0.0-rc.2", "5.0.0-rc.10", "5.0.0", "5.0.1"}
	for i := 0; i+1 < len(ordered); i++ {
		a, _ := Parse(ordered[i])
		b, _ := Parse(ordered[i+1])
		if !Less(a, b) || Less(b, a) {
			t.Errorf("want %s < %s", ordered[i], ordered[i+1])
		}
	}
}

func TestLatest(t *testing.T) {
	r, _ := fakeGraph(t, map[string][]graphNode{"stable-4.22": {node("4.22.22")}, "stable-5.0": {node("5.0.1")}})
	got := r.Latest(context.Background(), []string{"5.0", "4.22", "4.9"})
	if got["5.0"] != "5.0.1" || got["4.22"] != "4.22.22" || len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
