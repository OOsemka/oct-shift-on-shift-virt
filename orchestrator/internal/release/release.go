// Package release turns the OpenShift version picked in the wizard ("4.22",
// "5.0" or an exact "4.22.22") into a release payload pinned by digest.
//
// Minor versions resolve to the newest release in the stable-X.Y channel of
// the OpenShift update graph (the same data `oc adm upgrade` uses), falling
// back to fast-X.Y while a new minor has no stable release yet. Before this,
// the version came from whatever openshift-install binary was baked into the
// helper image, so "4.22" installed 4.22.14 long after 4.22.22 shipped, and
// the wizard's choice was ignored altogether.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGraphURL is the public OpenShift update graph (Cincinnati).
const DefaultGraphURL = "https://api.openshift.com/api/upgrades_info/v1/graph"

// fallbackRepo is where an exact version is looked up by tag when the graph
// cannot be reached.
const fallbackRepo = "quay.io/openshift-release-dev/ocp-release"

// Release is a resolved OpenShift release.
type Release struct {
	Version string `json:"version"`
	// Image is the release payload, pinned by digest when it came from the
	// graph (tag only for the offline fallback).
	Image string `json:"image"`
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+)(?:-([0-9A-Za-z.-]+))?)?$`)

// Version is a parsed OpenShift version.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // "rc.3", "ec.1"; empty for GA
	HasPatch            bool   // false for "4.22"
}

// Parse accepts "X.Y", "X.Y.Z" and "X.Y.Z-pre".
func Parse(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("invalid OpenShift version %q: use X.Y (latest release of that minor) or X.Y.Z", s)
	}
	v := Version{Pre: m[4]}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
		v.HasPatch = true
	}
	return v, nil
}

// MinorString is "X.Y".
func (v Version) MinorString() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Less orders versions numerically; a pre-release sorts before its GA.
func Less(a, b Version) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch < b.Patch
	}
	if a.Pre == b.Pre {
		return false
	}
	if a.Pre == "" || b.Pre == "" {
		return a.Pre != "" // GA is newer than any pre-release
	}
	return preLess(a.Pre, b.Pre)
}

func preLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

// Resolver looks releases up in the update graph, with a short cache so the
// wizard's version list does not query the graph on every page load.
type Resolver struct {
	GraphURL string
	Arch     string
	HTTP     *http.Client
	TTL      time.Duration

	mu    sync.Mutex
	cache map[string]cachedChannel
}

type cachedChannel struct {
	nodes   []graphNode
	fetched time.Time
}

type graphNode struct {
	Version string `json:"version"`
	Payload string `json:"payload"`
}

// NewFromEnv builds a resolver; RELEASE_GRAPH_URL and RELEASE_ARCH override
// the public graph and amd64 (e.g. a mirrored update service).
func NewFromEnv() *Resolver {
	r := &Resolver{
		GraphURL: DefaultGraphURL,
		Arch:     "amd64",
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		TTL:      10 * time.Minute,
	}
	if u := strings.TrimSpace(os.Getenv("RELEASE_GRAPH_URL")); u != "" {
		r.GraphURL = u
	}
	if a := strings.TrimSpace(os.Getenv("RELEASE_ARCH")); a != "" {
		r.Arch = a
	}
	return r
}

// Default is the process-wide resolver (API and install phases share its cache).
var Default = NewFromEnv()

func (r *Resolver) channel(ctx context.Context, name string) ([]graphNode, error) {
	r.mu.Lock()
	if c, ok := r.cache[name]; ok && time.Since(c.fetched) < r.TTL {
		r.mu.Unlock()
		return c.nodes, nil
	}
	r.mu.Unlock()

	q := url.Values{"channel": {name}, "arch": {r.Arch}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.GraphURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying the OpenShift update graph (%s): %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenShift update graph (%s): HTTP %d", name, resp.StatusCode)
	}
	var body struct {
		Nodes []graphNode `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding the OpenShift update graph (%s): %w", name, err)
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedChannel{}
	}
	r.cache[name] = cachedChannel{nodes: body.Nodes, fetched: time.Now()}
	r.mu.Unlock()
	return body.Nodes, nil
}

// Resolve returns the release to install for a requested version.
//
//	"4.22"    newest GA release in stable-4.22 (fast-4.22 if stable is empty)
//	"4.22.22" that release (stable, fast, then candidate channel)
//
// When the graph is unreachable an exact GA version still resolves by tag in
// quay.io/openshift-release-dev/ocp-release; a minor version cannot.
func (r *Resolver) Resolve(ctx context.Context, requested string) (Release, error) {
	want, err := Parse(requested)
	if err != nil {
		return Release{}, err
	}
	minor := want.MinorString()

	if !want.HasPatch {
		var lastErr error
		for _, ch := range []string{"stable-" + minor, "fast-" + minor} {
			nodes, err := r.channel(ctx, ch)
			if err != nil {
				lastErr = err
				continue
			}
			if rel, ok := newestGA(nodes, want); ok {
				return rel, nil
			}
		}
		if lastErr != nil {
			return Release{}, fmt.Errorf("cannot resolve the latest OpenShift %s release: %w", minor, lastErr)
		}
		return Release{}, fmt.Errorf("no OpenShift %s release is available yet (channels stable-%s and fast-%s are empty)", minor, minor, minor)
	}

	exact := strings.TrimSpace(requested)
	reachable := false
	var lastErr error
	for _, ch := range []string{"stable-" + minor, "fast-" + minor, "candidate-" + minor} {
		nodes, err := r.channel(ctx, ch)
		if err != nil {
			lastErr = err
			continue
		}
		reachable = true
		for _, n := range nodes {
			if n.Version == exact && n.Payload != "" {
				return Release{Version: n.Version, Image: n.Payload}, nil
			}
		}
	}
	if !reachable && want.Pre == "" {
		return Release{Version: exact, Image: fmt.Sprintf("%s:%s-%s", fallbackRepo, exact, archSuffix(r.Arch))}, nil
	}
	if !reachable {
		return Release{}, fmt.Errorf("cannot resolve OpenShift %s: %w", exact, lastErr)
	}
	return Release{}, fmt.Errorf("OpenShift %s is not a published release (not in the stable, fast or candidate %s channels)", exact, minor)
}

// newestGA picks the newest non-pre-release node of want's minor.
func newestGA(nodes []graphNode, want Version) (Release, bool) {
	var best *Version
	var rel Release
	for _, n := range nodes {
		v, err := Parse(n.Version)
		if err != nil || v.Pre != "" || !v.HasPatch || n.Payload == "" {
			continue
		}
		// fast-5.0 also lists the 4.22 releases that upgrade into it.
		if v.Major != want.Major || v.Minor != want.Minor {
			continue
		}
		if best == nil || Less(*best, v) {
			vv := v
			best = &vv
			rel = Release{Version: n.Version, Image: n.Payload}
		}
	}
	return rel, best != nil
}

// Latest resolves several minor versions, best effort: minors that cannot be
// resolved are left out. Used to show "4.22 (4.22.22)" in the wizard.
func (r *Resolver) Latest(ctx context.Context, minors []string) map[string]string {
	out := map[string]string{}
	for _, m := range minors {
		if rel, err := r.Resolve(ctx, m); err == nil {
			out[m] = rel.Version
		}
	}
	return out
}

func archSuffix(arch string) string {
	switch arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return arch
	}
}
