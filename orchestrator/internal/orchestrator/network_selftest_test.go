package orchestrator

import (
	"strings"
	"testing"
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

// The script is shipped verbatim: it must source the shared lib.sh, test a
// service created during the run (the CoreDNS-watch failure mode) and keep
// pod specs valid under the restricted pod security profile.
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
	} {
		if !strings.Contains(s, want) {
			t.Errorf("selftest-network.sh missing %q", want)
		}
	}
	if !strings.Contains(mustReadCSI("lib.sh"), "poll()") {
		t.Error("lib.sh no longer provides poll(), which the network self-test uses")
	}
}
