package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNetworkSummary(t *testing.T) {
	logs := "OK nodes Ready\nOK pod-to-pod\nSUMMARY Cilium: pod-to-pod across nodes, new service DNS, API service OK\n"
	if got := networkSummary(logs); !strings.HasPrefix(got, "Cilium:") {
		t.Errorf("summary = %q", got)
	}
	if got := networkSummary("no summary"); got != "network self-test passed" {
		t.Errorf("fallback summary = %q", got)
	}
}

// The real Cilium failure: one dns-default pod 1/2 Ready, CoreDNS logging
// "Failed to watch", Cilium dropping its connections to the API servers.
const ciliumFailureLog = `OK nested API reachable (TLS verified as api.c1.example.com)
OK nodes Ready, network operator Available (networkType Cilium)
FATAL: dns-default not ready within 240s: dns-default-abcde 1/2 (ready/containers); last dns log lines of dns-default-abcde: plugin/kubernetes: Failed to watch
DIAG ---- diagnostics ----
DIAG network operator:
DIAG Available=True AsExpected
DIAG cilium-dbg status --brief (cilium-xyz12):
DIAG OK
DIAG cilium policy drops (10s sample on cilium-xyz12):
DIAG xx drop (Policy denied) flow 0x0 identity 12345->host: 10.128.0.5:41234 -> 10.100.0.101:6443 tcp SYN
DIAG dns-default-abcde dns log (error|Failed|timeout, last 20):
DIAG [ERROR] plugin/kubernetes: Failed to watch *v1.Service: i/o timeout
DIAG dns-default pods (name node phase ready/containers):
DIAG dns-default-abcde master-1 Running 1/2
DIAG dns-default-fghij master-2 Running 2/2
`

func TestNetworkFailureKeepsDiagnostics(t *testing.T) {
	err := errors.New(lastFatal(ciliumFailureLog))
	if !strings.HasPrefix(err.Error(), "dns-default not ready") {
		t.Fatalf("DIAG lines must not hide the FATAL line: %q", err)
	}
	detail, cond := networkFailure(err, ciliumFailureLog)
	for _, want := range []string{"Policy denied", "Failed to watch", "dns-default-abcde master-1 Running 1/2"} {
		if !strings.Contains(detail, want) {
			t.Errorf("log detail missing %q:\n%s", want, detail)
		}
		if !strings.Contains(cond, want) {
			t.Errorf("condition missing %q: %s", want, cond)
		}
	}
	if strings.Contains(detail, "DIAG ") || strings.Contains(detail, "OK nodes Ready") {
		t.Errorf("detail should hold only the DIAG block, prefix stripped:\n%s", detail)
	}
	if !strings.HasPrefix(cond, err.Error()+" | diagnostics: ") || strings.Contains(cond, "----") {
		t.Errorf("condition = %q", cond)
	}
}

func TestNetworkFailureBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("FATAL: DNS not following the API\n")
	for i := 0; i < 400; i++ {
		b.WriteString("DIAG [ERROR] plugin/kubernetes: Failed to watch *v1.EndpointSlice: Get \"https://172.30.0.1:443/apis\": dial tcp i/o timeout ż\n")
	}
	b.WriteString("DIAG dns-default-abcde master-1 Running 1/2\n")
	logs := b.String()
	detail, cond := networkFailure(errors.New(lastFatal(logs)), logs)
	if len(detail) > networkDiagLogChars || len(cond) > networkCondChars {
		t.Errorf("detail %d bytes (max %d), condition %d bytes (max %d)", len(detail), networkDiagLogChars, len(cond), networkCondChars)
	}
	if !strings.HasSuffix(detail, "1/2") || !strings.HasSuffix(cond, "1/2") {
		t.Error("the tail (dns-default readiness) must survive trimming")
	}
	if !utf8.ValidString(detail) || !utf8.ValidString(cond) {
		t.Error("trimming split a rune")
	}

	// A FATAL line that fills the condition gets no diagnostics appended.
	long := strings.Repeat("x", 1950)
	if _, c := networkFailure(errors.New(long), logs); c != long {
		t.Errorf("long FATAL: condition has %d bytes, want the FATAL line only", len(c))
	}

	// No DIAG block (script died early): the log tail stands in.
	d, c := networkFailure(errors.New("nested API not reachable at 10.0.0.1:6443"), "OK something\nFATAL: nested API not reachable at 10.0.0.1:6443\n")
	if !strings.Contains(d, "FATAL: nested API not reachable") || !strings.HasPrefix(c, "nested API not reachable") {
		t.Errorf("fallback detail=%q condition=%q", d, c)
	}
}

func TestTailLines(t *testing.T) {
	lines := []string{"aaaa", "bbbb", "cccc"}
	if got := tailLines(lines, "; ", 10); got != "bbbb; cccc" {
		t.Errorf("tailLines = %q", got)
	}
	if got := tailLines(lines, "; ", 100); got != "aaaa; bbbb; cccc" {
		t.Errorf("tailLines = %q", got)
	}
	if got := tailLines([]string{"żżżż"}, "\n", 3); got != "ż" {
		t.Errorf("long single line = %q, want its end on a rune boundary", got)
	}
	if got := tailLines(nil, "\n", 10); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// The script is shipped verbatim: it must source the shared lib.sh, split
// the checks so each failure names its cause, test a service created during
// the run (the CoreDNS-watch failure mode), print bounded diagnostics after a
// FATAL line and keep pod specs valid under the restricted pod security profile.
func TestNetworkSelfTestScript(t *testing.T) {
	b, err := networkFS.ReadFile("manifests/network/" + networkSelfTestScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		". /m/lib.sh", "--image-for=network-tools", "expose pod server --name=selftest",
		"selftest.${NS}.svc.cluster.local", "kubernetes.default.svc/readyz",
		"runAsNonRoot: true", "drop: [ALL]", "SUMMARY ", "trap cleanup EXIT",
		// split checks with their own messages
		"get ds dns-default", "dns-default not ready", "https://${API_CIP}/readyz",
		"service routing broken", "getent ahostsv4", "DNS not following the API", "SVC_WAIT=180",
		// diagnostics
		`fatal() { echo "FATAL: $*"; diagnostics; exit 1; }`, "sed 's/^/DIAG /'",
		"cilium-dbg status --brief", "timeout 12 $N -n cilium exec", "monitor --type drop", "Policy denied",
		"grep -E 'error|Failed|timeout'", "get clusteroperator network",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("selftest-network.sh missing %q", want)
		}
	}
	// Order: DNS readiness, then the API service, then the new service by
	// ClusterIP, by DNS, by name. The API check used to run last and was
	// skipped whenever the new-service check failed.
	order := []string{"poll \"$DNS_WAIT\" dns_ready", "http_ok \"https://${API_CIP}/readyz\"",
		"http_ok \"http://${SVC_CIP}/\"", "resolves_to \"$SVC_NAME\"", "http_ok \"http://${SVC_NAME}/\""}
	last := -1
	for _, o := range order {
		i := strings.Index(s, o)
		if i < 0 || i < last {
			t.Errorf("check %q missing or out of order", o)
		}
		last = i
	}
	if !strings.Contains(mustReadCSI("lib.sh"), "poll()") {
		t.Error("lib.sh no longer provides poll(), which the network self-test uses")
	}
}
