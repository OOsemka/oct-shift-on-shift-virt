package orchestrator

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

//go:embed manifests/network/*
var networkFS embed.FS

const (
	conditionNetworkReady = "NetworkReady"
	networkSelfTestScript = "selftest-network.sh"
	networkSelfTestTime   = 10 * time.Minute
)

func networkScriptsCM(cluster string) string { return cluster + "-network-selftest" }

// networkSelfTest checks the nested pod network end to end after the install
// (selftest-network.sh): nodes Ready, pod-to-pod across nodes, a new service
// resolving by name, the API service. Non-fatal like nested storage: the
// result lands on the NetworkReady condition. A cluster can report every
// operator Available with a broken network (seen with Cilium: CoreDNS lost
// its API watch and new services never resolved), and the installer's own
// wait-for checks do not notice.
func (s *StepExecutor) networkSelfTest(ctx context.Context, nc *crd.NestedCluster) {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("network-selftest")
	msg, err := s.doNetworkSelfTest(ctx, nc)
	if err != nil {
		log.Error("network self-test failed (cluster otherwise installed)", "error", err.Error())
		setCondition(nc, conditionNetworkReady, metav1.ConditionFalse, "SelfTestFailed", err.Error())
	} else {
		log.Info("network self-test passed", "detail", msg)
		setCondition(nc, conditionNetworkReady, metav1.ConditionTrue, "SelfTestPassed", msg)
	}
	s.updateCRStatus(ctx, nc)
}

func (s *StepExecutor) doNetworkSelfTest(ctx context.Context, nc *crd.NestedCluster) (string, error) {
	if nc.Status.KubeconfigRef == "" {
		return "", fmt.Errorf("no nested kubeconfig stored")
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return "", err
	}
	if err := s.ensureNetworkScripts(ctx, nc); err != nil {
		return "", err
	}
	logs, err := s.runScriptJob(ctx, nc, networkScriptsCM(nc.Spec.ClusterName),
		nc.Spec.ClusterName+"-network-selftest", networkSelfTestScript, nil, networkSelfTestTime)
	if err != nil {
		return "", err
	}
	return networkSummary(logs), nil
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
