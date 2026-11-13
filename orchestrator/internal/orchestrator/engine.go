package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/release"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

var nestedClusterGVR = schema.GroupVersionResource{
	Group:    crd.Group,
	Version:  crd.Version,
	Resource: crd.Resource,
}

type Engine struct {
	client   *k8s.Client
	logger   *logging.Logger
	steps    *StepExecutor
	mu       sync.Mutex
	running  map[string]*reconcileRun
	deleting map[string]bool
	// reconcileFn runs one reconcile; tests replace it. nil means e.reconcile.
	reconcileFn func(context.Context, *crd.NestedCluster)
}

// reconcileRun is one in-flight reconcile. Each run has its own entry so a
// cancelled run that exits late never removes the entry of the run that
// replaced it (a cancelled install used to delete the teardown's entry).
type reconcileRun struct {
	cancel   context.CancelFunc
	teardown bool
}

// isTeardown reports whether this event asks for the cluster to be deleted:
// a real deletion (finalizer) or the Deleting phase the API sets first.
func isTeardown(nc *crd.NestedCluster) bool {
	return nc.GetDeletionTimestamp() != nil || nc.Status.Phase == crd.PhaseDeleting
}

func NewEngine(client *k8s.Client, logger *logging.Logger) *Engine {
	return &Engine{
		client:   client,
		logger:   logger,
		steps:    NewStepExecutor(client, logger),
		running:  make(map[string]*reconcileRun),
		deleting: make(map[string]bool),
	}
}

func (e *Engine) Watch(ctx context.Context) error {
	e.logger.Info("starting NestedCluster watcher")

	// Safety net: a deletion only makes progress when an event arrives, so a
	// missed or stale event leaves a CR stuck in Deleting with its finalizer
	// held and nothing to retrigger it. Re-drive pending deletions on startup
	// and periodically thereafter.
	go e.resyncPendingDeletions(ctx)

	for {
		watcher, err := e.client.DynamicClient.Resource(nestedClusterGVR).
			Namespace("").Watch(ctx, metav1.ListOptions{})
		if err != nil {
			e.logger.Error("failed to start watch", "error", err.Error())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		}

		if err := e.handleWatchEvents(ctx, watcher); err != nil {
			e.logger.Error("watch stream ended", "error", err.Error())
		}
		watcher.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) handleWatchEvents(ctx context.Context, watcher watch.Interface) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			nc, err := unstructuredToNestedCluster(obj)
			if err != nil {
				e.logger.Error("failed to parse NestedCluster", "error", err.Error())
				continue
			}

			switch event.Type {
			case watch.Added, watch.Modified:
				e.handleClusterEvent(ctx, nc)
			case watch.Deleted:
				e.cancelReconcile(nc.Namespace + "/" + nc.Name)
			}
		}
	}
}

func (e *Engine) handleClusterEvent(ctx context.Context, nc *crd.NestedCluster) {
	phase := nc.Status.Phase
	key := nc.Namespace + "/" + nc.Name

	// Deletion (via finalizer) or an API-driven Deleting phase: stop any
	// in-flight install reconcile exactly once and drive teardown. Repeated
	// deletion events must NOT cancel the running teardown — status writes
	// from the teardown itself generate those events.
	if isTeardown(nc) {
		e.mu.Lock()
		if e.deleting == nil {
			e.deleting = map[string]bool{}
		}
		if e.deleting[key] {
			e.mu.Unlock()
			return
		}
		if run, exists := e.running[key]; exists {
			run.cancel()
			delete(e.running, key)
		}
		e.deleting[key] = true
		e.mu.Unlock()
		// wait for the cancelled reconcile to deregister so the teardown
		// reconcile isn't deduped away
		for i := 0; i < 60; i++ {
			e.mu.Lock()
			_, busy := e.running[key]
			e.mu.Unlock()
			if !busy {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !e.startReconcile(ctx, nc) {
			// An older reconcile is still deregistering. Clear the flag so
			// the next event (or the resync) retries — leaving it set made
			// chris-sos-odf unreachable to EVERY subsequent event, including
			// the resync that exists to rescue exactly this case.
			e.mu.Lock()
			delete(e.deleting, key)
			e.mu.Unlock()
		}
		return
	}

	// A live cluster is not being deleted. Drop a deleting mark left by an
	// earlier cluster with the same namespace/name, unless a teardown is
	// actually running: otherwise deleting a cluster that was deleted and
	// re-created with the same name never starts a teardown.
	e.mu.Lock()
	if run, busy := e.running[key]; !busy || !run.teardown {
		delete(e.deleting, key)
	}
	e.mu.Unlock()

	if phase == crd.PhaseReady || phase == crd.PhaseFailed || phase == crd.PhaseMaster {
		// Express clones finish isolating after Ready (a Job); record it.
		if phase == crd.PhaseReady && nc.IsClone() {
			go e.steps.CheckIsolation(ctx, nc)
		}
		if phase == crd.PhaseReady {
			e.followHostRoutes(ctx, nc)
		}
		return
	}

	e.startReconcile(ctx, nc)
}

// followHostRoutes keeps the host routes of a Ready cluster in step with its
// spec: hostRoutes turned on after the install publishes the API and console
// (once per spec generation), and once published the per-application routes
// follow the annotated nested routes.
func (e *Engine) followHostRoutes(ctx context.Context, nc *crd.NestedCluster) {
	if hostRoutesPending(nc) {
		go e.steps.PublishRoutesOnReady(ctx, nc)
		return
	}
	if nc.Spec.ExternalAccess.HostRoutes {
		go e.steps.SyncAppRoutes(ctx, nc)
	}
}

// startReconcile reports whether a reconcile was actually started. Callers
// that set per-key state first (the deletion branch) MUST check it: a silent
// dedupe here with that state left behind latches the cluster out of every
// future event.
func (e *Engine) startReconcile(ctx context.Context, nc *crd.NestedCluster) bool {
	key := nc.Namespace + "/" + nc.Name

	e.mu.Lock()
	if _, exists := e.running[key]; exists {
		e.mu.Unlock()
		return false
	}
	reconcileCtx, cancel := context.WithCancel(ctx)
	run := &reconcileRun{cancel: cancel, teardown: isTeardown(nc)}
	e.running[key] = run
	e.mu.Unlock()

	fn := e.reconcileFn
	if fn == nil {
		fn = e.reconcile
	}
	go func() {
		defer func() {
			e.mu.Lock()
			if e.running[key] == run {
				delete(e.running, key)
			}
			// Every teardown clears the deleting mark when it ends, whichever
			// path started it. Only the finalizer path used to, so a teardown
			// started by the Deleting phase left the mark behind forever.
			if run.teardown {
				delete(e.deleting, key)
			}
			e.mu.Unlock()
			cancel()
		}()
		fn(reconcileCtx, nc)
	}()
	return true
}

// cancelReconcile stops the reconcile of exactly this namespace/name. It used
// to match by name suffix, so deleting "demo" also cancelled "xdemo" (and a
// name longer than the key panicked).
func (e *Engine) cancelReconcile(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if run, ok := e.running[key]; ok {
		run.cancel()
		delete(e.running, key)
	}
}

func (e *Engine) reconcile(ctx context.Context, nc *crd.NestedCluster) {
	log := e.logger.WithCluster(nc.Name).WithCorrelation()

	// A plain `oc delete nestedcluster` (no API handler involved) must still
	// tear the cluster down: our finalizer keeps this object reconcilable
	// until cleanup completes, then we release it.
	if nc.GetDeletionTimestamp() != nil {
		if !hasFinalizer(nc, cleanupFinalizer) {
			return
		}
		e.updateStatus(ctx, nc, crd.PhaseDeleting, "NestedCluster deletion requested")
		log.Info("starting cluster deletion")
		if err := e.steps.DeleteCluster(ctx, nc); err != nil {
			log.Error("deletion failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, "Deletion failed: "+err.Error())
			return
		}
		if err := e.removeFinalizer(ctx, nc); err != nil {
			log.Error("removing cleanup finalizer failed", "error", err.Error())
		}
		return
	}

	// Adopt the cleanup finalizer on first sight of a live cluster.
	if !hasFinalizer(nc, cleanupFinalizer) {
		if err := e.addFinalizer(ctx, nc); err != nil {
			log.Error("adding cleanup finalizer failed", "error", err.Error())
		}
	}

	if nc.Status.Phase == crd.PhaseDeleting {
		log.Info("starting cluster deletion")
		if err := e.steps.DeleteCluster(ctx, nc); err != nil {
			log.Error("deletion failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, "Deletion failed: "+err.Error())
			return
		}
		// The API handler sets phase Deleting and THEN deletes the CR, so
		// this branch usually runs on the event without deletionTimestamp,
		// and the real deletion events are deduped while it runs. Release
		// the finalizer here too if the live object is being deleted —
		// otherwise only the 2-minute resync frees it (chris-sos-odf,
		// 2026-10-21: namespace gone, CR lingering).
		if cur, err := e.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
			Get(ctx, nc.Name, metav1.GetOptions{}); err == nil && cur.GetDeletionTimestamp() != nil {
			if err := e.removeFinalizer(ctx, nc); err != nil {
				log.Error("removing cleanup finalizer failed", "error", err.Error())
			}
		}
		return
	}

	currentPhase := nc.Status.Phase
	// "Make master" (requested through the API on a Ready cluster): shut it
	// down and snapshot its disks; it then stays in phase Master.
	if currentPhase == crd.PhaseMakeMaster {
		if err := e.steps.MakeMaster(ctx, nc); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("make master failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, "Make master failed: "+err.Error())
			return
		}
		e.updateStatus(ctx, nc, crd.PhaseMaster, "Master: clones are created from its snapshots")
		return
	}
	if currentPhase == crd.PhaseRefreshMaster {
		if err := e.steps.RefreshMaster(ctx, nc); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("refresh master failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, "Refresh master failed: "+err.Error())
			return
		}
		e.updateStatus(ctx, nc, crd.PhaseMaster, "Master: snapshots refreshed")
		return
	}
	// Day 2 (requested through the API on a Ready cluster): add nested storage
	// and optionally OpenShift Virtualization, then back to Ready. Never runs
	// the install order, so a clone does not repeat FinishIsolation.
	if crd.IsEnableStoragePhase(currentPhase) {
		if err := e.steps.EnableStorage(ctx, nc); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("enable storage failed", "error", err.Error())
			what := "nested storage"
			if currentPhase == crd.PhaseEnableVirtualization {
				what = "OpenShift Virtualization"
			}
			e.updateStatus(ctx, nc, crd.PhaseReady, "Ready; adding "+what+" failed: "+err.Error())
			return
		}
		e.updateStatus(ctx, nc, crd.PhaseReady, enableStorageSummary(nc))
		return
	}
	order := crd.PhasesFor(nc)
	if currentPhase == crd.PhaseEmpty {
		// A new cluster (also one applied with oc, bypassing the API): fail
		// before anything is created if the network settings cannot work.
		// Existing clusters are not re-checked so an upgrade never fails them.
		if err := nc.Spec.ValidateNetwork(); err != nil {
			log.Error("refusing to install", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, err.Error())
			return
		}
		if err := release.Supported(nc.Spec.OCPVersion); err != nil {
			log.Error("refusing to install", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, err.Error())
			return
		}
		// The internal network must not overlap the host's node network
		// (OVN-Kubernetes UDN rule; otherwise no egress, and the agent
		// installer never starts). Skipped if nodes cannot be read.
		if host, err := e.client.HostNodeAddresses(ctx); err != nil {
			log.Info("cannot list host nodes; skipping the internal network overlap check", "error", err.Error())
		} else if err := nc.Spec.CheckHostNetwork(host); err != nil {
			log.Error("refusing to install", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, err.Error())
			return
		}
		// New clusters get the bastion load balancer; clusters created
		// earlier keep their HAProxy pods (the marker is never added later).
		nc.Status.LoadBalancer = crd.LoadBalancerBastion
		currentPhase = order[0]
	}

	startIdx := 0
	for i, p := range order {
		if p == currentPhase {
			startIdx = i
			break
		}
	}

	for i := startIdx; i < len(order); i++ {
		phase := order[i]
		phaseLog := log.WithPhase(string(phase))

		if err := ctx.Err(); err != nil {
			phaseLog.Info("reconciliation cancelled")
			return
		}

		e.updateStatus(ctx, nc, phase, "Executing "+string(phase))
		phaseLog.Info("executing phase")

		op := phaseLog.StartOperation(string(phase))
		err := e.steps.Execute(ctx, nc, phase)
		if err != nil && ctx.Err() != nil {
			// Cancelled (usually by a delete): not a failure, and the
			// teardown owns the status now. Logged as info, not as errors.
			phaseLog.Info("reconciliation cancelled during phase", "reason", ctx.Err().Error())
			return
		}
		op.Done(err)
		if err != nil {
			phaseLog.Error("phase failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, fmt.Sprintf("Phase %s failed: %s", phase, err.Error()))
			return
		}

		e.recordPhaseTransition(ctx, nc, phase, true, "")
		phaseLog.Info("phase completed")
	}

	log.Info("cluster installation complete")
	e.updateStatus(ctx, nc, crd.PhaseReady, "Cluster is ready")
	nc.Status.APIEndpoint = nc.APIEndpoint()
	nc.Status.ConsoleURL = nc.ConsoleEndpoint()
	nc.Status.IngressDomain = nc.IngressWildcard()
	e.updateCRStatus(ctx, nc)
}

func (e *Engine) updateStatus(ctx context.Context, nc *crd.NestedCluster, phase crd.ClusterPhase, message string) {
	nc.Status.Phase = phase
	nc.Status.Message = message
	now := metav1.Now()
	if nc.Status.StartedAt == nil {
		nc.Status.StartedAt = &now
	}
	if phase == crd.PhaseReady || phase == crd.PhaseFailed {
		nc.Status.CompletedAt = &now
	}
	e.updateCRStatus(ctx, nc)
}

func (e *Engine) recordPhaseTransition(ctx context.Context, nc *crd.NestedCluster, phase crd.ClusterPhase, success bool, message string) {
	now := metav1.Now()
	transition := crd.PhaseTransition{
		Phase:     phase,
		StartedAt: now,
		EndedAt:   &now,
		Message:   message,
		Success:   success,
	}
	nc.Status.PhaseHistory = append(nc.Status.PhaseHistory, transition)
}

func (e *Engine) updateCRStatus(ctx context.Context, nc *crd.NestedCluster) {
	statusBytes, err := json.Marshal(nc.Status)
	if err != nil {
		e.logger.Error("failed to marshal status", "error", err.Error())
		return
	}

	var statusMap map[string]interface{}
	if err := json.Unmarshal(statusBytes, &statusMap); err != nil {
		e.logger.Error("failed to unmarshal status", "error", err.Error())
		return
	}

	existing, err := e.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(nc.Namespace).Get(ctx, nc.Name, metav1.GetOptions{})
	if err != nil {
		e.logger.Error("failed to get CR for status update", "error", err.Error(), "cluster", nc.Name)
		return
	}

	existing.Object["status"] = statusMap
	_, err = e.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(nc.Namespace).UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		e.logger.Error("failed to update CR status", "error", err.Error(), "cluster", nc.Name)
	}
}

func unstructuredToNestedCluster(obj *unstructured.Unstructured) (*crd.NestedCluster, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	nc := &crd.NestedCluster{}
	if err := json.Unmarshal(data, nc); err != nil {
		return nil, err
	}
	return nc, nil
}

// cleanupFinalizer gives the orchestrator a chance to tear down cluster
// resources when the NestedCluster itself is deleted.
const cleanupFinalizer = "sos.octools.io/cleanup"

func hasFinalizer(nc *crd.NestedCluster, f string) bool {
	for _, existing := range nc.GetFinalizers() {
		if existing == f {
			return true
		}
	}
	return false
}

func (e *Engine) addFinalizer(ctx context.Context, nc *crd.NestedCluster) error {
	return e.patchFinalizers(ctx, nc, true)
}

func (e *Engine) removeFinalizer(ctx context.Context, nc *crd.NestedCluster) error {
	return e.patchFinalizers(ctx, nc, false)
}

func (e *Engine) patchFinalizers(ctx context.Context, nc *crd.NestedCluster, add bool) error {
	cur, err := e.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Get(ctx, nc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// NEVER adopt a finalizer onto an object that is already being deleted.
	// Reconciles carry the object from the watch event, which can be STALE
	// (no deletionTimestamp yet). Without this guard such a reconcile falls
	// into the adoption branch and re-adds the finalizer microseconds after
	// the teardown released it — the API server can then never complete the
	// delete, no further events arrive, and the CR is stranded in Deleting
	// forever with every resource already gone. Observed on both
	// chris-sos-odf and chris-sos-odf2, and it is also why manually patching
	// the finalizer away appeared to succeed yet changed nothing.
	if add && cur.GetDeletionTimestamp() != nil {
		return nil
	}
	finalizers, _, _ := unstructured.NestedStringSlice(cur.Object, "metadata", "finalizers")
	out := make([]string, 0, len(finalizers)+1)
	for _, f := range finalizers {
		if f == cleanupFinalizer {
			continue
		}
		out = append(out, f)
	}
	if add {
		out = append(out, cleanupFinalizer)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = e.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Patch(ctx, nc.Name, types.MergePatchType,
			[]byte(`{"metadata":{"finalizers":`+string(raw)+`}}`), metav1.PatchOptions{})
	return err
}

// resyncPendingDeletions re-drives any NestedCluster that is being deleted but
// still holds our finalizer. The watch is the only trigger for reconciles, so
// a single missed/stale event strands such an object permanently — every
// resource already torn down, the CR sitting in Deleting forever. This sweeps
// on startup and every two minutes so recovery never needs an operator.
func (e *Engine) resyncPendingDeletions(ctx context.Context) {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for {
		list, err := e.client.DynamicClient.Resource(nestedClusterGVR).
			Namespace("").List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				obj := list.Items[i]
				if obj.GetDeletionTimestamp() == nil {
					// Ready clusters: follow annotated nested routes and the
					// Express isolation job (neither changes the CR itself).
					if nc, cerr := unstructuredToNestedCluster(&obj); cerr == nil && nc.Status.Phase == crd.PhaseReady {
						if nc.IsClone() {
							go e.steps.CheckIsolation(ctx, nc)
						}
						e.followHostRoutes(ctx, nc)
					}
					continue
				}
				hasOurs := false
				for _, f := range obj.GetFinalizers() {
					if f == cleanupFinalizer {
						hasOurs = true
						break
					}
				}
				if !hasOurs {
					continue
				}
				nc, cerr := unstructuredToNestedCluster(&obj)
				if cerr != nil {
					continue
				}
				// Break a stale latch: if the key is marked deleting but no
				// reconcile is running, nothing will ever clear it and
				// handleClusterEvent would just return.
				key := nc.GetNamespace() + "/" + nc.GetName()
				e.mu.Lock()
				if e.deleting[key] {
					if _, busy := e.running[key]; !busy {
						delete(e.deleting, key)
						e.logger.WithCluster(nc.Spec.ClusterName).
							Info("cleared a stale deleting latch (no reconcile was running)")
					}
				}
				e.mu.Unlock()
				e.logger.WithCluster(nc.Spec.ClusterName).
					Info("re-driving a stranded deletion (deletionTimestamp set, finalizer still held)")
				e.handleClusterEvent(ctx, nc)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
