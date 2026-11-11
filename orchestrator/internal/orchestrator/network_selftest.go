package orchestrator

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

//go:embed manifests/network/*
var networkFS embed.FS

const (
	conditionNetworkReady = "NetworkReady"
	networkSelfTestScript = "selftest-network.sh"
	// Worst case of the script: nested API wait 5m (lib.sh), dns-default 4m,
	// test pods 5m, then the checks and diagnostics.
	networkSelfTestTime = 20 * time.Minute

	// Bounds on the evidence kept after the Job is deleted: the orchestrator
	// log gets the tail of the script's DIAG block up to networkDiagLogChars,
	// the condition (trimmed to 2000 by setCondition) the FATAL line plus a
	// short form of the block up to networkCondChars in total.
	networkDiagLogChars = 3000
	networkCondChars    = 1990
)

func networkScriptsCM(cluster string) string { return cluster + "-network-selftest" }

// networkSelfTest checks the nested pod network end to end after the install
// (selftest-network.sh): nodes Ready, dns-default Ready, pod-to-pod across
// nodes, the API service, a new service by ClusterIP, by DNS and by name.
// Non-fatal like nested storage: the result lands on the NetworkReady
// condition. A cluster can report every operator Available with a broken
// network (seen with Cilium: CoreDNS lost its API watch and new services
// never resolved), and the installer's own wait-for checks do not notice.
func (s *StepExecutor) networkSelfTest(ctx context.Context, nc *crd.NestedCluster) {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("network-selftest")
	msg, logs, err := s.doNetworkSelfTest(ctx, nc)
	if err != nil {
		diag, cond := networkFailure(err, logs)
		log.Error("network self-test failed (cluster otherwise installed)", "error", err.Error(), "diagnostics", diag)
		setCondition(nc, conditionNetworkReady, metav1.ConditionFalse, "SelfTestFailed", cond)
	} else {
		log.Info("network self-test passed", "detail", msg)
		setCondition(nc, conditionNetworkReady, metav1.ConditionTrue, "SelfTestPassed", msg)
	}
	s.updateCRStatus(ctx, nc)
}

// doNetworkSelfTest returns the summary and the Job log; runScriptJob returns
// the log with its error too, so the diagnostics outlive the Job.
func (s *StepExecutor) doNetworkSelfTest(ctx context.Context, nc *crd.NestedCluster) (string, string, error) {
	if nc.Status.KubeconfigRef == "" {
		return "", "", fmt.Errorf("no nested kubeconfig stored")
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return "", "", err
	}
	if err := s.ensureNetworkScripts(ctx, nc); err != nil {
		return "", "", err
	}
	logs, err := s.runScriptJob(ctx, nc, networkScriptsCM(nc.Spec.ClusterName),
		nc.Spec.ClusterName+"-network-selftest", networkSelfTestScript, nil, networkSelfTestTime)
	if err != nil {
		return "", logs, err
	}
	return networkSummary(logs), logs, nil
}

// ensureNetworkScripts publishes the self-test with the shared lib.sh (nested
// kubeconfig through the cUDN VIP, fatal, poll).
func (s *StepExecutor) ensureNetworkScripts(ctx context.Context, nc *crd.NestedCluster) error {
	script, err := networkFS.ReadFile("manifests/network/" + networkSelfTestScript)
	if err != nil {
		return err
	}
	return s.upsertConfigMap(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: networkScriptsCM(nc.Spec.ClusterName), Namespace: nc.Spec.Namespace,
			Labels: map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}},
		Data: map[string]string{
			"lib.sh":              mustReadCSI("lib.sh"),
			networkSelfTestScript: string(script),
		},
	})
}

// networkSummary returns the script's SUMMARY line (or a generic message).
func networkSummary(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "SUMMARY ") {
			return strings.TrimPrefix(lines[i], "SUMMARY ")
		}
	}
	return "network self-test passed"
}

// networkFailure builds the orchestrator log detail and the condition message
// of a failed self-test. The script's DIAG block is the evidence (network
// operator, Cilium drops, CoreDNS errors, dns-default readiness, most useful
// last); without one (the script died before its checks) the log tail stands
// in for it.
func networkFailure(err error, logs string) (detail, condition string) {
	diag := networkDiagnostics(logs)
	if len(diag) == 0 {
		diag = compactLines(strings.Split(logs, "\n"))
	}
	detail = tailLines(diag, "\n", networkDiagLogChars)
	condition = err.Error()
	const sep = " | diagnostics: "
	if room := networkCondChars - len(condition) - len(sep); room > 80 {
		if short := tailLines(compactLines(diag), "; ", room); short != "" {
			condition += sep + short
		}
	}
	return detail, condition
}

// networkDiagnostics returns the script's DIAG lines without the prefix.
func networkDiagnostics(logs string) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "DIAG ") {
			out = append(out, strings.TrimPrefix(l, "DIAG "))
		}
	}
	return out
}

// compactLines drops blank lines and "----" separators.
func compactLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "----") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// tailLines joins the last lines that fit in max bytes with sep. If even the
// last line is longer, its end is kept (cut on a rune boundary).
func tailLines(lines []string, sep string, max int) string {
	if max <= 0 || len(lines) == 0 {
		return ""
	}
	n, i := 0, len(lines)
	for i > 0 {
		add := len(lines[i-1])
		if i < len(lines) {
			add += len(sep)
		}
		if n+add > max {
			break
		}
		n += add
		i--
	}
	if i < len(lines) {
		return strings.Join(lines[i:], sep)
	}
	l := lines[len(lines)-1]
	start := len(l) - max
	for start < len(l) && !utf8.RuneStart(l[start]) {
		start++
	}
	return l[start:]
}
