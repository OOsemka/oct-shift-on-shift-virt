package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func vmWithRole(name, role string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": name}}}
	u.SetLabels(map[string]string{"oct-shift-on-shift-virt/role": role})
	return u
}

// Workers and infra restart before the masters; the bastion is never touched.
func TestNodeVMsOrder(t *testing.T) {
	got := nodeVMs([]unstructured.Unstructured{
		vmWithRole("cluster-master-1", "master"), vmWithRole("bastion", "bastion"), vmWithRole("cluster-worker-0", "worker"),
		vmWithRole("cluster-master-0", "master"), vmWithRole("cluster-infra-0", "infra"),
	})
	var names []string
	for _, vm := range got {
		names = append(names, vm.GetName())
	}
	if want := "cluster-worker-0 cluster-infra-0 cluster-master-0 cluster-master-1"; strings.Join(names, " ") != want {
		t.Fatalf("order = %v, want %s", names, want)
	}
}

func TestKeyValue(t *testing.T) {
	logs := "OK nested API reachable\nOK cluster-master-0 drained\nBOOTID=abc-123\n"
	if v := keyValue(logs, "BOOTID"); v != "abc-123" {
		t.Fatalf("got %q", v)
	}
	if v := keyValue("no id here", "BOOTID"); v != "" {
		t.Fatalf("got %q", v)
	}
}

func TestEnableStorageSummary(t *testing.T) {
	nc := &crd.NestedCluster{}
	nc.Spec.Virtualization.Enabled = true
	nc.Status.Conditions = []metav1.Condition{
		{Type: conditionStorageReady, Status: metav1.ConditionTrue},
		{Type: conditionVirtualizationReady, Status: metav1.ConditionFalse, Message: "no /dev/kvm"},
	}
	got := enableStorageSummary(nc)
	if !strings.Contains(got, "nested storage ready") || !strings.Contains(got, "OpenShift Virtualization failed: no /dev/kvm") {
		t.Fatalf("summary = %q", got)
	}
}

func TestBootIDs(t *testing.T) {
	got := bootIDs("noise\nBOOTID cluster-master-0 aaa\nBOOTID cluster-master-1 bbb\nBOOTID broken\n")
	if len(got) != 2 || got["cluster-master-0"] != "aaa" || got["cluster-master-1"] != "bbb" {
		t.Errorf("bootIDs = %v", got)
	}
}

// fakeCluster simulates node VMs: SetFlag changes only the template; the
// running instance gets the template's flag at a start.
type fakeCluster struct {
	template map[string]bool // svm/vmx on the VM template
	running  map[string]bool // VMI exists
	vmiFlag  map[string]bool // svm/vmx on the running VMI
	cordoned map[string]bool
	starts   map[string]int
	failOn   string // "op:node" fails once, e.g. "ready:m1", "start:m1", "stop:m1", "drain:m1"
	calls    []string
}

func newFakeCluster(nodes ...string) *fakeCluster {
	f := &fakeCluster{template: map[string]bool{}, running: map[string]bool{}, vmiFlag: map[string]bool{},
		cordoned: map[string]bool{}, starts: map[string]int{}}
	for _, n := range nodes {
		f.running[n] = true
	}
	return f
}

func (f *fakeCluster) fail(op, node string) error {
	if f.failOn == op+":"+node {
		f.failOn = ""
		return fmt.Errorf("injected %s failure", op)
	}
	return nil
}

func (f *fakeCluster) State(n string) (k8s.VMIRunsNestedVirt, error) {
	return k8s.VMIRunsNestedVirt{Running: f.running[n], HasFlag: f.running[n] && f.vmiFlag[n]}, nil
}
func (f *fakeCluster) SetFlag(n string) error { f.template[n] = true; return nil }
func (f *fakeCluster) Power(n string, on bool) error {
	op := map[bool]string{true: "start", false: "stop"}[on]
	f.calls = append(f.calls, op+":"+n)
	if err := f.fail(op, n); err != nil {
		return err
	}
	if on {
		if !f.running[n] {
			f.running[n], f.vmiFlag[n] = true, f.template[n]
			f.starts[n]++
		}
	} else {
		f.running[n], f.vmiFlag[n] = false, false
	}
	return nil
}
func (f *fakeCluster) Job(n, mode string, _ ...corev1.EnvVar) (string, error) {
	f.calls = append(f.calls, mode+":"+n)
	if err := f.fail(mode, n); err != nil {
		return "", err
	}
	switch mode {
	case "drain":
		f.cordoned[n] = true
		return "BOOTID=b-" + n, nil
	case "ready", "uncordon":
		f.cordoned[n] = false
	case "bootids":
		out := ""
		for node, up := range f.running {
			if up {
				out += "BOOTID " + node + " b-" + node + "\n"
			}
		}
		return out, nil
	}
	return "", nil
}

type nopLog struct{}

func (nopLog) Info(string, ...interface{}) {}

func runRestart(f *fakeCluster, simultaneous bool, nodes ...string) error {
	return restartForNestedVirt(nodes, simultaneous, f, func(string) {}, nopLog{})
}

// Chris's review: a failure after the flag is written must not make a retry
// skip the node. Every failure point, rolling mode: the retry restarts
// exactly the nodes still without the flag, never one twice, and no node is
// left cordoned.
func TestRestartRetryAfterFailureRolling(t *testing.T) {
	for _, failOn := range []string{"drain:m1", "stop:m1", "start:m1", "ready:m1"} {
		t.Run(failOn, func(t *testing.T) {
			f := newFakeCluster("w0", "m1", "m2")
			f.failOn = failOn
			if err := runRestart(f, false, "w0", "m1", "m2"); err == nil {
				t.Fatal("expected the injected failure")
			}
			if !f.template["m1"] {
				t.Fatal("test premise: the flag is written to m1's template before the failure")
			}
			if f.cordoned["m1"] {
				t.Errorf("m1 left cordoned after %s", failOn)
			}
			if err := runRestart(f, false, "w0", "m1", "m2"); err != nil {
				t.Fatalf("retry: %v", err)
			}
			for _, n := range []string{"w0", "m1", "m2"} {
				if !f.running[n] || !f.vmiFlag[n] {
					t.Errorf("%s does not run with hardware virtualization after the retry", n)
				}
				if f.cordoned[n] {
					t.Errorf("%s still cordoned", n)
				}
			}
			if f.starts["w0"] != 1 {
				t.Errorf("w0 (done before the failure) started %d times, want 1", f.starts["w0"])
			}
			if f.starts["m1"] != 1 || f.starts["m2"] != 1 {
				t.Errorf("starts m1=%d m2=%d, want 1 each", f.starts["m1"], f.starts["m2"])
			}
			// A third run has nothing left to do.
			f.calls = nil
			if err := runRestart(f, false, "w0", "m1", "m2"); err != nil || len(f.calls) != 0 {
				t.Errorf("third run: err=%v calls=%v, want nothing", err, f.calls)
			}
		})
	}
}

// Simultaneous mode wrote every flag before any stop: a retry after a failure
// must still restart every node that is not running with the flag.
func TestRestartRetryAfterFailureSimultaneous(t *testing.T) {
	for _, failOn := range []string{"stop:m1", "start:m1", "allready:"} {
		t.Run(failOn, func(t *testing.T) {
			f := newFakeCluster("m0", "m1", "m2")
			f.failOn = failOn
			if err := runRestart(f, true, "m0", "m1", "m2"); err == nil {
				t.Fatal("expected the injected failure")
			}
			if err := runRestart(f, true, "m0", "m1", "m2"); err != nil {
				t.Fatalf("retry: %v", err)
			}
			for _, n := range []string{"m0", "m1", "m2"} {
				if !f.running[n] || !f.vmiFlag[n] {
					t.Errorf("%s does not run with hardware virtualization after the retry", n)
				}
				if f.starts[n] != 1 {
					t.Errorf("%s started %d times, want 1", n, f.starts[n])
				}
			}
		})
	}
}

// A node VM left stopped is started without a drain (its node is down).
func TestRestartStartsStoppedVMWithoutDrain(t *testing.T) {
	f := newFakeCluster("m0")
	f.running["m0"] = false
	if err := runRestart(f, false, "m0"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "drain:") || strings.HasPrefix(c, "stop:") {
			t.Errorf("unexpected %s for a stopped VM (calls %v)", c, f.calls)
		}
	}
	if !f.running["m0"] || !f.vmiFlag["m0"] {
		t.Error("m0 not running with the flag")
	}
}

// When uncordon fails too, the error says the node is still cordoned.
func TestRestartReportsStillCordoned(t *testing.T) {
	f := &failingUncordon{fakeCluster: newFakeCluster("m0")}
	f.failOn = "ready:m0"
	err := restartForNestedVirt([]string{"m0"}, false, f, func(string) {}, nopLog{})
	if err == nil || !strings.Contains(err.Error(), "still cordoned") {
		t.Fatalf("error %v does not say the node is still cordoned", err)
	}
}

type failingUncordon struct{ *fakeCluster }

func (f *failingUncordon) Job(n, mode string, env ...corev1.EnvVar) (string, error) {
	if mode == "uncordon" {
		return "", fmt.Errorf("API unavailable")
	}
	return f.fakeCluster.Job(n, mode, env...)
}
