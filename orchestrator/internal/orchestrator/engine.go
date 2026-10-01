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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	running  map[string]context.CancelFunc
}

func NewEngine(client *k8s.Client, logger *logging.Logger) *Engine {
	return &Engine{
		client:  client,
		logger:  logger,
		steps:   NewStepExecutor(client, logger),
		running: make(map[string]context.CancelFunc),
	}
}

func (e *Engine) Watch(ctx context.Context) error {
	e.logger.Info("starting NestedCluster watcher")

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
				e.cancelReconcile(nc.Name)
			}
		}
	}
}

func (e *Engine) handleClusterEvent(ctx context.Context, nc *crd.NestedCluster) {
	phase := nc.Status.Phase

	if phase == crd.PhaseReady || phase == crd.PhaseFailed {
		return
	}

	key := nc.Namespace + "/" + nc.Name

	if phase == crd.PhaseDeleting {
		e.mu.Lock()
		if cancel, exists := e.running[key]; exists {
			cancel()
			delete(e.running, key)
		}
		e.mu.Unlock()
		e.startReconcile(ctx, nc)
		return
	}

	e.startReconcile(ctx, nc)
}

func (e *Engine) startReconcile(ctx context.Context, nc *crd.NestedCluster) {
	key := nc.Namespace + "/" + nc.Name

	e.mu.Lock()
	if _, exists := e.running[key]; exists {
		e.mu.Unlock()
		return
	}
	reconcileCtx, cancel := context.WithCancel(ctx)
	e.running[key] = cancel
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.running, key)
			e.mu.Unlock()
		}()
		e.reconcile(reconcileCtx, nc)
	}()
}

func (e *Engine) cancelReconcile(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, cancel := range e.running {
		if key == name || key[len(key)-len(name):] == name {
			cancel()
			delete(e.running, key)
		}
	}
}

func (e *Engine) reconcile(ctx context.Context, nc *crd.NestedCluster) {
	log := e.logger.WithCluster(nc.Name).WithCorrelation()

	if nc.Status.Phase == crd.PhaseDeleting {
		log.Info("starting cluster deletion")
		if err := e.steps.DeleteCluster(ctx, nc); err != nil {
			log.Error("deletion failed", "error", err.Error())
			e.updateStatus(ctx, nc, crd.PhaseFailed, "Deletion failed: "+err.Error())
		}
		return
	}

	currentPhase := nc.Status.Phase
	if currentPhase == crd.PhaseEmpty {
		currentPhase = crd.PhaseOrder[0]
	}

	startIdx := 0
	for i, p := range crd.PhaseOrder {
		if p == currentPhase {
			startIdx = i
			break
		}
	}

	for i := startIdx; i < len(crd.PhaseOrder); i++ {
		phase := crd.PhaseOrder[i]
		phaseLog := log.WithPhase(string(phase))

		if err := ctx.Err(); err != nil {
			phaseLog.Info("reconciliation cancelled")
			return
		}

		e.updateStatus(ctx, nc, phase, "Executing "+string(phase))
		phaseLog.Info("executing phase")

		op := phaseLog.StartOperation(string(phase))
		err := e.steps.Execute(ctx, nc, phase)
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
