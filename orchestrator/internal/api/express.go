package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

// Express API: turn a Ready cluster into a master, list masters, and (in
// the quick-cluster endpoints) clone from one.

func toNestedCluster(obj *unstructured.Unstructured) (*crd.NestedCluster, error) {
	b, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	var nc crd.NestedCluster
	if err := json.Unmarshal(b, &nc); err != nil {
		return nil, err
	}
	if nc.Spec.ClusterName == "" {
		nc.Spec.ClusterName = obj.GetName()
	}
	return &nc, nil
}

// makeMaster: POST /api/v1/clusters/{name}/make-master. The orchestrator
// shuts the cluster down and snapshots it; it then stays in phase Master.
func (s *Server) makeMaster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	nc, err := toNestedCluster(obj)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if nc.Status.Phase != crd.PhaseReady {
		s.writeError(w, http.StatusConflict, fmt.Sprintf("only a Ready cluster can become a master (phase %s)", nc.Status.Phase))
		return
	}
	if why := nc.MasterRefusal(); why != "" {
		s.writeError(w, http.StatusBadRequest, why)
		return
	}
	ns := obj.GetNamespace()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, gerr := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(ns).Get(r.Context(), name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		_ = unstructured.SetNestedField(cur.Object, string(crd.PhaseMakeMaster), "status", "phase")
		_ = unstructured.SetNestedField(cur.Object, "Shutting down and snapshotting for Express clones", "status", "message")
		_, uerr := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(ns).UpdateStatus(r.Context(), cur, metav1.UpdateOptions{})
		return uerr
	}); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not start make-master: "+err.Error())
		return
	}
	s.logger.Info("make master requested", "cluster", name)
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "making master"})
}

type masterItem struct {
	Name           string         `json:"name"`
	OCPVersion     string         `json:"ocpVersion"`
	ReleaseVersion string         `json:"releaseVersion,omitempty"`
	Masters        crd.NodeSizing `json:"masters"`
	InternalDomain string         `json:"internalDomain"`
	CreatedAt      *metav1.Time   `json:"createdAt,omitempty"`
	Clones         int            `json:"clones"`
}

// listMasters: GET /api/v1/masters.
func (s *Server) listMasters(w http.ResponseWriter, r *http.Request) {
	items, err := s.masters(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (s *Server) masters(ctx context.Context) ([]masterItem, error) {
	list, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(clusterNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	clones := map[string]int{}
	var all []*crd.NestedCluster
	for i := range list.Items {
		nc, err := toNestedCluster(&list.Items[i])
		if err != nil {
			continue
		}
		all = append(all, nc)
		if nc.IsClone() {
			clones[nc.Spec.Source.Master]++
		}
	}
	items := []masterItem{}
	for _, nc := range all {
		if nc.Status.Phase != crd.PhaseMaster || nc.Status.Master == nil {
			continue
		}
		items = append(items, masterItem{
			Name: nc.Name, OCPVersion: nc.Spec.OCPVersion, ReleaseVersion: nc.Status.Master.ReleaseVersion,
			Masters: nc.Spec.Masters, InternalDomain: nc.FQDN(), CreatedAt: nc.Status.Master.CreatedAt,
			Clones: clones[nc.Name],
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// getMaster returns a master by name (phase Master, operator namespace).
func (s *Server) getMaster(ctx context.Context, name string) (*crd.NestedCluster, error) {
	obj, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(clusterNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("master %q not found", name)
	}
	nc, err := toNestedCluster(obj)
	if err != nil {
		return nil, err
	}
	if nc.Status.Phase != crd.PhaseMaster || nc.Status.Master == nil {
		return nil, fmt.Errorf("%q is not a master", name)
	}
	return nc, nil
}

// applyMasterToPlan turns a quick-cluster plan into a clone of master.
func (s *Server) applyMasterToPlan(ctx context.Context, plan *quickPlan, master string) {
	m, err := s.getMaster(ctx, master)
	if err == nil {
		err = crd.ApplyMaster(&plan.Spec, m)
	}
	if err != nil {
		plan.Problems = append(plan.Problems, err.Error())
	}
}
