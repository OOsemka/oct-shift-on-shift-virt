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
// nodes are restarted: one at a time by default (drain in the nested cluster,
// stop and start the VM, wait for a new boot ID and Ready, uncordon, wait for
// etcd and kube-apiserver to settle), or all together with
// RestartSimultaneous or a single node.
//
// Whether a node still needs its restart is decided from its running
// instance (VMI), not from the VM template: the flag is written to the
// template before the restart, so after a failure or an orchestrator restart
// the template alone would claim a node is done while it still runs without
// /dev/kvm. A retry therefore restarts exactly the nodes whose VMI lacks the
// flag, and starts (without draining) node VMs left stopped by a failed
// stop/start; nodes already restarted are never restarted twice.
func (s *StepExecutor) enableNestedVirtCPU(ctx context.Context, nc *crd.NestedCluster) error {
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	vms, err := s.client.ListVMs(ctx, ns, cluster)
	if err != nil {
		return err
	}
	var nodes []string
	for _, vm := range nodeVMs(vms) {
		nodes = append(nodes, vm.GetName())
	}
	if err := s.ensureStorageScripts(ctx, nc); err != nil {
		return err
	}
	r := &clusterRestarter{s: s, ctx: ctx, nc: nc}
	simultaneous := nc.Spec.NodeCount() == 1 || nc.RestartModeOf() == crd.RestartSimultaneous
	return restartForNestedVirt(nodes, simultaneous, r,
		func(msg string) { s.setProgress(ctx, nc, msg) },
		s.logger.WithCluster(cluster).WithComponent("virtualization"))
}

// nodeRestarter is what the restart needs from the host cluster (an interface
// so the retry behaviour can be tested without KubeVirt).
type nodeRestarter interface {
	// State reports the node VM's running instance.
	State(node string) (k8s.VMIRunsNestedVirt, error)
	// SetFlag adds svm/vmx to the VM template (idempotent).
	SetFlag(node string) error
	// Power stops or starts the VM and waits for it.
	Power(node string, on bool) error
	// Job runs roll-node.sh in the given mode and returns its output.
	Job(node, mode string, env ...corev1.EnvVar) (string, error)
}

type infoLogger interface {
	Info(msg string, keysAndValues ...interface{})
}

// noBootID stands for "any boot ID" when a node's VM is not running (its node
// is down, so whatever boots next is new).
const noBootID = "none"

func restartForNestedVirt(nodes []string, simultaneous bool, r nodeRestarter, progress func(string), log infoLogger) error {
	type pending struct {
		name    string
		running bool // false: VM stopped (a failed earlier stop/start)
	}
	var todo []pending
	for _, n := range nodes {
		st, err := r.State(n)
		if err != nil {
			return err
		}
		if st.Running && st.HasFlag {
			continue
		}
		todo = append(todo, pending{name: n, running: st.Running})
	}
	if len(todo) == 0 {
		log.Info("every node VM already runs with hardware virtualization; no restart needed")
		return nil
	}

	if simultaneous {
		progress(fmt.Sprintf("Restarting all %d node(s) at once to enable hardware virtualization", len(todo)))
		boots := map[string]string{}
		if out, err := r.Job("", "bootids"); err != nil {
			return fmt.Errorf("reading the nodes' boot IDs: %w", err)
		} else {
			boots = bootIDs(out)
		}
		var pairs []string
		for _, p := range todo {
			b := noBootID
			if p.running {
				if b = boots[p.name]; b == "" {
					return fmt.Errorf("node %s: could not read its boot ID", p.name)
				}
			}
			pairs = append(pairs, p.name+"="+b)
		}
		for _, p := range todo {
			if err := r.SetFlag(p.name); err != nil {
				return fmt.Errorf("adding the svm/vmx CPU flag to VM %s: %w", p.name, err)
			}
		}
		for _, p := range todo {
			if p.running {
				if err := r.Power(p.name, false); err != nil {
					return fmt.Errorf("stopping VM %s: %w (a retry restarts the nodes still without hardware virtualization)", p.name, err)
				}
			}
		}
		for _, p := range todo {
			if err := r.Power(p.name, true); err != nil {
				return fmt.Errorf("starting VM %s: %w (it is stopped; a retry starts it)", p.name, err)
			}
		}
		if _, err := r.Job("", "allready", corev1.EnvVar{Name: "BOOT_IDS", Value: strings.Join(pairs, " ")},
			corev1.EnvVar{Name: "API_WAIT", Value: "2400"}); err != nil {
			return fmt.Errorf("after restarting all nodes: %w", err)
		}
		log.Info("all nodes restarted with hardware virtualization", "nodes", len(todo))
		return nil
	}

	for i, p := range todo {
		node := p.name
		progress(fmt.Sprintf("Restarting node %d of %d (%s) to enable hardware virtualization", i+1, len(todo), node))
		left := len(todo) - i
		// After the drain the node is cordoned: every failure from there on
		// uncordons it, and says so if that fails too.
		failCordoned := func(err error) error {
			if _, uerr := r.Job(node, "uncordon"); uerr != nil {
				return fmt.Errorf("node %s: %w; it is still cordoned (uncordon failed: %v); %d node(s) not restarted", node, err, uerr, left)
			}
			return fmt.Errorf("node %s: %w (node uncordoned; %d node(s) not restarted)", node, err, left)
		}
		if err := r.SetFlag(node); err != nil {
			return fmt.Errorf("adding the svm/vmx CPU flag to VM %s: %w", node, err)
		}
		boot := noBootID
		if p.running {
			out, err := r.Job(node, "drain")
			if err != nil {
				return failCordoned(err)
			}
			if boot = keyValue(out, "BOOTID"); boot == "" {
				return failCordoned(fmt.Errorf("could not read its boot ID"))
			}
			if err := r.Power(node, false); err != nil {
				return failCordoned(fmt.Errorf("stopping its VM: %w", err))
			}
		}
		if err := r.Power(node, true); err != nil {
			return failCordoned(fmt.Errorf("starting its VM: %w (the VM is stopped; a retry starts it)", err))
		}
		if _, err := r.Job(node, "ready", corev1.EnvVar{Name: "BOOT_ID", Value: boot}, corev1.EnvVar{Name: "API_WAIT", Value: "2400"}); err != nil {
			return failCordoned(fmt.Errorf("after its restart: %w", err))
		}
		log.Info("node restarted with hardware virtualization", "node", node, "done", i+1, "of", len(todo))
	}
	return nil
}

// clusterRestarter implements nodeRestarter against the host cluster.
type clusterRestarter struct {
	s   *StepExecutor
	ctx context.Context
	nc  *crd.NestedCluster
}

func (c *clusterRestarter) State(node string) (k8s.VMIRunsNestedVirt, error) {
	return c.s.client.NestedVirtState(c.ctx, c.nc.Spec.Namespace, node)
}
func (c *clusterRestarter) SetFlag(node string) error {
	return c.s.client.EnableNestedVirtCPU(c.ctx, c.nc.Spec.Namespace, node)
}
func (c *clusterRestarter) Power(node string, on bool) error {
	return c.s.client.SetVMPower(c.ctx, c.nc.Spec.Namespace, node, on, 10*time.Minute)
}
func (c *clusterRestarter) Job(node, mode string, extra ...corev1.EnvVar) (string, error) {
	env := append([]corev1.EnvVar{{Name: "NODE", Value: node}, {Name: "MODE", Value: mode}}, extra...)
	return c.s.runStorageJob(c.ctx, c.nc, c.nc.Spec.ClusterName+"-roll-node", "roll-node.sh", env, 75*time.Minute)
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
