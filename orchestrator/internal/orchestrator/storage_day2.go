package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// EnableStorage runs a day-2 request (phase EnableStorage, set by the API on a
// Ready cluster): the storage step, then, when virtualization was requested,
// the svm/vmx CPU flag on every node VM with a rolling restart, then the
// virtualization step. Like at install, each part is non-fatal: the outcome is
// in StorageReady / VirtualizationReady and the cluster returns to Ready.
// Safe to repeat (an orchestrator restart resumes the phase from the start):
// the steps are idempotent and nodes that already have the flag are skipped.
func (s *StepExecutor) EnableStorage(ctx context.Context, nc *crd.NestedCluster) error {
	if !nc.ConditionTrue(crd.ConditionStorageReady) {
		s.setProgress(ctx, nc, "Configuring nested storage")
	}
	if err := s.configureStorage(ctx, nc); err != nil {
		return err
	}
	if !nc.Spec.Virtualization.Enabled {
		return nil
	}
	// configureVirtualization skips (with a reason) when storage is not ready;
	// don't restart the nodes for nothing.
	if c := meta.FindStatusCondition(nc.Status.Conditions, conditionStorageReady); c != nil && c.Status == metav1.ConditionTrue {
		if err := s.enableNestedVirtCPU(ctx, nc); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("virtualization").
				Error("rolling restart for hardware virtualization failed (cluster still healthy)", "error", err.Error())
			setCondition(nc, conditionVirtualizationReady, metav1.ConditionFalse, "NodeRestartFailed", err.Error())
			s.updateCRStatus(ctx, nc)
			return nil
		}
	}
	if c := meta.FindStatusCondition(nc.Status.Conditions, conditionStorageReady); c != nil && c.Status == metav1.ConditionTrue {
		s.setProgress(ctx, nc, "Installing OpenShift Virtualization in the nested cluster")
	}
	return s.configureVirtualization(ctx, nc)
}

// nodeVMs returns the cluster's node VMs (not the bastion), workers and infra
// first and masters last, each group by name, so the control plane restarts
// after the nodes that carry the workloads.
func nodeVMs(vms []unstructured.Unstructured) []unstructured.Unstructured {
	rank := map[string]int{"worker": 0, "infra": 1, "master": 2}
	var out []unstructured.Unstructured
	for _, vm := range vms {
		if _, ok := rank[vm.GetLabels()["oct-shift-on-shift-virt/role"]]; ok {
			out = append(out, vm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].GetLabels()["oct-shift-on-shift-virt/role"]], rank[out[j].GetLabels()["oct-shift-on-shift-virt/role"]]
		if ri != rj {
			return ri < rj
		}
		return out[i].GetName() < out[j].GetName()
	})
	return out
}

// enableNestedVirtCPU gives every node VM the svm/vmx CPU flag that new
// clusters get when virtualization is chosen at install (k8s.vmCPU), so the
// nested nodes get /dev/kvm. The flag only takes effect at a VM start, so the
// nodes are restarted one at a time: drain in the nested cluster, stop and
// start the VM, wait for the node to boot again and be Ready, uncordon, and
// wait for etcd and kube-apiserver to settle before the next node. With
// RestartSimultaneous, or a single node, all nodes restart together instead
// (restartAllAtOnce). Stops at the first error, leaving the remaining nodes
// untouched.
func (s *StepExecutor) enableNestedVirtCPU(ctx context.Context, nc *crd.NestedCluster) error {
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	log := s.logger.WithCluster(cluster).WithComponent("virtualization")
	vms, err := s.client.ListVMs(ctx, ns, cluster)
	if err != nil {
		return err
	}
	var todo []string
	for _, vm := range nodeVMs(vms) {
		vm := vm
		if !k8s.HasNestedVirtCPU(&vm) {
			todo = append(todo, vm.GetName())
		}
	}
	if len(todo) == 0 {
		log.Info("node VMs already pass hardware virtualization through; no restart needed")
		return nil
	}
	if err := s.ensureStorageScripts(ctx, nc); err != nil {
		return err
	}
	job := cluster + "-roll-node"
	run := func(node, mode string, extra ...corev1.EnvVar) (string, error) {
		env := append([]corev1.EnvVar{{Name: "NODE", Value: node}, {Name: "MODE", Value: mode}}, extra...)
		return s.runStorageJob(ctx, nc, job, "roll-node.sh", env, 75*time.Minute)
	}
	if nc.Spec.NodeCount() == 1 || nc.RestartModeOf() == crd.RestartSimultaneous {
		return s.restartAllAtOnce(ctx, nc, todo, run)
	}
	for i, node := range todo {
		s.setProgress(ctx, nc, fmt.Sprintf("Restarting node %d of %d (%s) to enable hardware virtualization", i+1, len(todo), node))
		if err := s.client.EnableNestedVirtCPU(ctx, ns, node); err != nil {
			return fmt.Errorf("adding the svm/vmx CPU flag to VM %s: %w", node, err)
		}
		logs, err := run(node, "drain")
		if err != nil {
			_, _ = run(node, "uncordon")
			return fmt.Errorf("node %s: %w (restart stopped; %d node(s) not restarted)", node, err, len(todo)-i)
		}
		boot := keyValue(logs, "BOOTID")
		if boot == "" {
			return fmt.Errorf("node %s: could not read its boot ID", node)
		}
		if err := s.client.SetVMPower(ctx, ns, node, false, 10*time.Minute); err != nil {
			return fmt.Errorf("stopping VM %s: %w", node, err)
		}
		if err := s.client.SetVMPower(ctx, ns, node, true, 10*time.Minute); err != nil {
			return fmt.Errorf("starting VM %s: %w", node, err)
		}
		if _, err := run(node, "ready", corev1.EnvVar{Name: "BOOT_ID", Value: boot}, corev1.EnvVar{Name: "API_WAIT", Value: "2400"}); err != nil {
			return fmt.Errorf("node %s after its restart: %w", node, err)
		}
		log.Info("node restarted with hardware virtualization", "node", node, "done", i+1, "of", len(todo))
	}
	return nil
}

// restartAllAtOnce applies the CPU flag to every node VM and restarts them all
// together, without draining: the user chose downtime over a rolling restart
// (or the cluster has a single node). It records each node's boot ID first and
// then waits until every node booted again and is Ready, and the control plane
// has settled.
func (s *StepExecutor) restartAllAtOnce(ctx context.Context, nc *crd.NestedCluster, nodes []string,
	run func(node, mode string, extra ...corev1.EnvVar) (string, error)) error {
	ns := nc.Spec.Namespace
	s.setProgress(ctx, nc, fmt.Sprintf("Restarting all %d node(s) at once to enable hardware virtualization", len(nodes)))
	logs, err := run("", "bootids")
	if err != nil {
		return fmt.Errorf("reading the nodes' boot IDs: %w", err)
	}
	boots := bootIDs(logs)
	var pairs []string
	for _, n := range nodes {
		b := boots[n]
		if b == "" {
			return fmt.Errorf("node %s: could not read its boot ID", n)
		}
		pairs = append(pairs, n+"="+b)
	}
	for _, n := range nodes {
		if err := s.client.EnableNestedVirtCPU(ctx, ns, n); err != nil {
			return fmt.Errorf("adding the svm/vmx CPU flag to VM %s: %w", n, err)
		}
	}
	for _, n := range nodes {
		if err := s.client.SetVMPower(ctx, ns, n, false, 10*time.Minute); err != nil {
			return fmt.Errorf("stopping VM %s: %w", n, err)
		}
	}
	for _, n := range nodes {
		if err := s.client.SetVMPower(ctx, ns, n, true, 10*time.Minute); err != nil {
			return fmt.Errorf("starting VM %s: %w", n, err)
		}
	}
	if _, err := run("", "allready", corev1.EnvVar{Name: "BOOT_IDS", Value: strings.Join(pairs, " ")},
		corev1.EnvVar{Name: "API_WAIT", Value: "2400"}); err != nil {
		return fmt.Errorf("after restarting all nodes: %w", err)
	}
	s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("virtualization").
		Info("all nodes restarted with hardware virtualization", "nodes", len(nodes))
	return nil
}

// bootIDs parses "BOOTID <node> <id>" lines.
func bootIDs(logs string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(logs, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "BOOTID" {
			out[f[1]] = f[2]
		}
	}
	return out
}

// setProgress updates the status message while the phase runs.
func (s *StepExecutor) setProgress(ctx context.Context, nc *crd.NestedCluster, msg string) {
	nc.Status.Message = msg
	s.updateCRStatus(ctx, nc)
}

// keyValue returns the value of the last "KEY=value" line in logs.
func keyValue(logs, key string) string {
	v := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.HasPrefix(l, key+"=") {
			v = strings.TrimSpace(strings.TrimPrefix(l, key+"="))
		}
	}
	return v
}

// enableStorageSummary is the Ready message after a day-2 request.
func enableStorageSummary(nc *crd.NestedCluster) string {
	part := func(t, name string) string {
		c := meta.FindStatusCondition(nc.Status.Conditions, t)
		switch {
		case c == nil:
			return name + " not configured"
		case c.Status == metav1.ConditionTrue:
			return name + " ready"
		default:
			return name + " failed: " + c.Message
		}
	}
	msg := "Ready; " + part(conditionStorageReady, "nested storage")
	if nc.Spec.Virtualization.Enabled {
		msg += "; " + part(conditionVirtualizationReady, "OpenShift Virtualization")
	}
	return msg
}
