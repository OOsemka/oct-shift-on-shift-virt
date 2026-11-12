package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

// enableStorage: POST /api/v1/clusters/{name}/enable-storage. Adds nested
// storage, and with "virtualization": true OpenShift Virtualization, to a
// Ready cluster. Virtualization needs storage, so it turns storage on as well
// (as in the wizard). The orchestrator then runs phase EnableStorage: the
// storage step, a rolling restart of the node VMs when virtualization needs
// the host CPU's svm/vmx flag, and the virtualization step; the cluster
// returns to Ready, with StorageReady / VirtualizationReady saying how it went.
func (s *Server) enableStorage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req crd.EnableStorageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
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
	if why := nc.EnableStorageRefusal(req); why != "" {
		code := http.StatusBadRequest
		if nc.Status.Phase != crd.PhaseReady {
			code = http.StatusConflict
		}
		s.writeError(w, code, why)
		return
	}
	// A named host StorageClass must exist; empty means the host default
	// (resolved by the storage step, as for a new cluster).
	if sc := req.InfraStorageClassName; sc != "" && !nc.Spec.Storage.Enabled {
		if _, err := s.client.Clientset.StorageV1().StorageClasses().Get(r.Context(), sc, metav1.GetOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				s.writeError(w, http.StatusBadRequest, fmt.Sprintf("host StorageClass %q does not exist", sc))
				return
			}
			s.writeError(w, http.StatusInternalServerError, "checking the host StorageClass: "+err.Error())
			return
		}
	}

	ns := obj.GetNamespace()
	res := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(ns)
	// Spec first, then the phase: if the phase update failed, the cluster stays
	// Ready and the request can be repeated (the refusal looks at the result
	// conditions, not at the spec flags).
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, gerr := res.Get(r.Context(), name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		c, cerr := toNestedCluster(cur)
		if cerr != nil {
			return cerr
		}
		c.Spec.ApplyEnableStorage(req)
		specMap, merr := toMap(c.Spec)
		if merr != nil {
			return merr
		}
		cur.Object["spec"] = specMap
		_, uerr := res.Update(r.Context(), cur, metav1.UpdateOptions{})
		return uerr
	}); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not update the cluster spec: "+err.Error())
		return
	}
	msg := enableStorageMessage(nc, req)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, gerr := res.Get(r.Context(), name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		_ = unstructured.SetNestedField(cur.Object, string(crd.PhaseEnableStorage), "status", "phase")
		_ = unstructured.SetNestedField(cur.Object, msg, "status", "message")
		_, uerr := res.UpdateStatus(r.Context(), cur, metav1.UpdateOptions{})
		return uerr
	}); err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not start: "+err.Error())
		return
	}
	s.logger.Info("enable storage requested", "cluster", name, "virtualization", req.Virtualization)
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": msg})
}

// enableStorageMessage is the status message while the request runs: it says
// only what is new (storage that already works is not "added" again; its step
// still runs, to raise the storage quota for the virtualization boot images).
func enableStorageMessage(nc *crd.NestedCluster, req crd.EnableStorageRequest) string {
	switch {
	case !req.Virtualization:
		return "Adding nested storage"
	case nc.ConditionTrue(crd.ConditionStorageReady):
		return "Adding OpenShift Virtualization (nested storage is already in place)"
	default:
		return "Adding nested storage and OpenShift Virtualization"
	}
}

// toMap converts a typed value to the map form unstructured objects use.
func toMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}
