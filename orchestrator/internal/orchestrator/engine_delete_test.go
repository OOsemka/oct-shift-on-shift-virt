package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

// fakeRuns records reconciles: installs block until cancelled, teardowns
// return at once.
type fakeRuns struct {
	mu        sync.Mutex
	teardowns int
	cancelled int
}

func (f *fakeRuns) reconcile(ctx context.Context, nc *crd.NestedCluster) {
	if isTeardown(nc) {
		f.mu.Lock()
		f.teardowns++
		f.mu.Unlock()
		return
	}
	<-ctx.Done()
	f.mu.Lock()
	f.cancelled++
	f.mu.Unlock()
}

func (f *fakeRuns) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardowns, f.cancelled
}

func newTestEngine(f *fakeRuns) *Engine {
	return &Engine{
		running:     map[string]*reconcileRun{},
		deleting:    map[string]bool{},
		reconcileFn: f.reconcile,
	}
}

func cluster(phase crd.ClusterPhase, deleted bool) *crd.NestedCluster {
	nc := &crd.NestedCluster{}
	nc.Namespace = "oct-shift-on-shift-virt"
	nc.Name = "demo"
	nc.Status.Phase = phase
	if deleted {
		now := metav1.Now()
		nc.DeletionTimestamp = &now
	}
	return nc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *Engine) idle(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, busy := e.running[key]
	return !busy && !e.deleting[key]
}

// Delete, re-create with the same name, delete again: the second delete must
// cancel the running install and start a teardown.
func TestDeleteRecreateDeleteSameName(t *testing.T) {
	f := &fakeRuns{}
	e := newTestEngine(f)
	ctx := context.Background()
	key := "oct-shift-on-shift-virt/demo"

	// First cluster installs, then the API marks it Deleting (no
	// deletionTimestamp yet): the teardown must clear the deleting mark.
	e.handleClusterEvent(ctx, cluster(crd.PhaseCreateNamespace, false))
	e.handleClusterEvent(ctx, cluster(crd.PhaseDeleting, false))
	waitFor(t, "first teardown", func() bool { td, c := f.counts(); return td == 1 && c == 1 })
	waitFor(t, "engine idle after first delete", func() bool { return e.idle(key) })

	// Re-created with the same name: a new install runs.
	e.handleClusterEvent(ctx, cluster(crd.PhaseCreateNamespace, false))
	waitFor(t, "second install running", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		run, ok := e.running[key]
		return ok && !run.teardown
	})

	// Deleted again: the install is cancelled and a teardown runs.
	e.handleClusterEvent(ctx, cluster(crd.PhaseDeleting, true))
	waitFor(t, "second teardown", func() bool { td, c := f.counts(); return td == 2 && c == 2 })
	waitFor(t, "engine idle after second delete", func() bool { return e.idle(key) })
}

// A stale deleting mark (left by an older engine version or a lost event)
// must not swallow the delete of a live, re-created cluster.
func TestStaleDeletingMarkIsClearedByLiveCluster(t *testing.T) {
	f := &fakeRuns{}
	e := newTestEngine(f)
	ctx := context.Background()
	key := "oct-shift-on-shift-virt/demo"
	e.deleting[key] = true

	e.handleClusterEvent(ctx, cluster(crd.PhaseCreateNamespace, false))
	e.handleClusterEvent(ctx, cluster(crd.PhaseDeleting, true))
	waitFor(t, "teardown despite stale mark", func() bool { td, c := f.counts(); return td == 1 && c == 1 })
}

// cancelReconcile must only stop the exact cluster, not one whose name ends
// the same way.
func TestCancelReconcileExactKey(t *testing.T) {
	e := newTestEngine(&fakeRuns{})
	stopped := false
	e.running["ns/xdemo"] = &reconcileRun{cancel: func() { stopped = true }}
	e.cancelReconcile("ns/demo")
	e.cancelReconcile("a-much-longer-namespace/and-a-long-name")
	if stopped {
		t.Fatal("cancelReconcile(ns/demo) cancelled ns/xdemo")
	}
	if _, ok := e.running["ns/xdemo"]; !ok {
		t.Fatal("ns/xdemo was removed")
	}
}
