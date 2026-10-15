package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/installer"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var nestedClusterGVR = schema.GroupVersionResource{
	Group:    crd.Group,
	Version:  crd.Version,
	Resource: crd.Resource,
}

var storageClassGVR = schema.GroupVersionResource{
	Group:    "storage.k8s.io",
	Version:  "v1",
	Resource: "storageclasses",
}

var nadGVR = schema.GroupVersionResource{
	Group:    "k8s.cni.cncf.io",
	Version:  "v1",
	Resource: "network-attachment-definitions",
}

type Server struct {
	client *k8s.Client
	logger *logging.Logger
	mux    *http.ServeMux
}

func NewServer(client *k8s.Client, logger *logging.Logger) *Server {
	s := &Server{
		client: client,
		logger: logger.WithComponent("api"),
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/clusters", s.listClusters)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}", s.getCluster)
	s.mux.HandleFunc("POST /api/v1/clusters", s.createCluster)
	s.mux.HandleFunc("DELETE /api/v1/clusters/{name}", s.deleteCluster)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}/status", s.getClusterStatus)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}/nodes", s.getClusterNodes)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}/kubeconfig", s.getKubeconfig)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}/kubeadmin", s.getKubeadmin)
	s.mux.HandleFunc("GET /api/v1/clusters/{name}/logs", s.getClusterLogs)
	s.mux.HandleFunc("POST /api/v1/clusters/preview-config", s.previewConfig)
	s.mux.HandleFunc("GET /api/v1/storageclasses", s.listStorageClasses)
	s.mux.HandleFunc("GET /api/v1/nads", s.listNADs)
	s.mux.HandleFunc("GET /api/v1/ocp-versions", s.listOCPVersions)
	s.mux.HandleFunc("GET /healthz", s.healthz)
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	list, err := s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace("").List(r.Context(), metav1.ListOptions{})
	if err != nil {
		s.logger.Error("failed to list clusters", "error", err.Error())
		s.writeError(w, http.StatusInternalServerError, "failed to list clusters")
		return
	}

	type clusterItem struct {
		Name        string `json:"name"`
		Namespace   string `json:"namespace"`
		Phase       string `json:"phase"`
		ConsoleURL  string `json:"consoleURL"`
		APIEndpoint string `json:"apiEndpoint"`
		OCPVersion  string `json:"ocpVersion"`
		Topology    string `json:"topology"`
		CreatedAt   string `json:"createdAt"`
	}

	var items []clusterItem
	for _, item := range list.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		consoleURL, _, _ := unstructured.NestedString(item.Object, "status", "consoleURL")
		apiEndpoint, _, _ := unstructured.NestedString(item.Object, "status", "apiEndpoint")
		ocpVersion, _, _ := unstructured.NestedString(item.Object, "spec", "ocpVersion")
		topology, _, _ := unstructured.NestedString(item.Object, "spec", "topology")

		items = append(items, clusterItem{
			Name:        item.GetName(),
			Namespace:   item.GetNamespace(),
			Phase:       phase,
			ConsoleURL:  consoleURL,
			APIEndpoint: apiEndpoint,
			OCPVersion:  ocpVersion,
			Topology:    topology,
			CreatedAt:   item.GetCreationTimestamp().Format("2006-01-02T15:04:05Z"),
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	s.writeJSON(w, http.StatusOK, obj.Object)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var spec crd.NestedClusterSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ncNamespace := "oct-shift-on-shift-virt"

	nc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": crd.Group + "/" + crd.Version,
			"kind":       crd.Kind,
			"metadata": map[string]interface{}{
				"name":      spec.ClusterName,
				"namespace": ncNamespace,
			},
		},
	}

	var specMap map[string]interface{}
	specBytes, _ := json.Marshal(spec)
	json.Unmarshal(specBytes, &specMap)
	nc.Object["spec"] = specMap

	created, err := s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(ncNamespace).Create(r.Context(), nc, metav1.CreateOptions{})
	if err != nil {
		s.logger.Error("failed to create cluster", "error", err.Error(), "cluster", spec.ClusterName)
		s.writeError(w, http.StatusInternalServerError, "failed to create cluster: "+err.Error())
		return
	}

	s.logger.Info("cluster created", "cluster", spec.ClusterName, "namespace", ncNamespace)
	s.writeJSON(w, http.StatusCreated, created.Object)
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	statusMap, _, _ := unstructured.NestedMap(obj.Object, "status")
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}
	statusMap["phase"] = string(crd.PhaseDeleting)
	statusMap["message"] = "Deletion requested"
	obj.Object["status"] = statusMap

	// Status first, so the UI reflects the request immediately.
	_, err = s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(obj.GetNamespace()).UpdateStatus(r.Context(), obj, metav1.UpdateOptions{})
	if err != nil {
		s.logger.Error("failed to mark cluster for deletion", "error", err.Error())
		s.writeError(w, http.StatusInternalServerError, "failed to initiate deletion")
		return
	}

	// Then actually delete the object. Marking the phase alone tore down the
	// cluster's resources but left the CR behind forever, showing "Deleting"
	// with no deletionTimestamp (chris-sos-odf lingered for an hour that way,
	// and removing its finalizer by hand did nothing because there was no
	// deletion in flight to unblock). The real Delete sets a deletionTimestamp,
	// which drives the sos.octools.io/cleanup finalizer -> DeleteCluster ->
	// finalizer release -> the object disappears. That path is already proven.
	if err := s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(obj.GetNamespace()).Delete(r.Context(), name, metav1.DeleteOptions{}); err != nil {
		s.logger.Error("failed to delete cluster object", "error", err.Error())
		s.writeError(w, http.StatusInternalServerError, "failed to delete cluster")
		return
	}

	s.logger.Info("cluster deletion requested", "cluster", name)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleting"})
}

func (s *Server) getClusterStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) getClusterNodes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	nodes, _, _ := unstructured.NestedSlice(obj.Object, "status", "nodes")
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"nodes": nodes})
}

func (s *Server) getKubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	secretRef, _, _ := unstructured.NestedString(obj.Object, "status", "kubeconfigSecretRef")
	ns := obj.GetNamespace()
	if secretRef == "" {
		s.writeError(w, http.StatusNotFound, "kubeconfig not yet available")
		return
	}

	data, err := s.client.GetSecretData(r.Context(), ns, secretRef, "kubeconfig")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to read kubeconfig")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"kubeconfig": string(data)})
}

func (s *Server) getKubeadmin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	secretRef, _, _ := unstructured.NestedString(obj.Object, "status", "kubeadminSecretRef")
	ns := obj.GetNamespace()
	if secretRef == "" {
		s.writeError(w, http.StatusNotFound, "kubeadmin password not yet available")
		return
	}

	data, err := s.client.GetSecretData(r.Context(), ns, secretRef, "password")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to read kubeadmin password")
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"password": string(data)})
}

func (s *Server) getClusterLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	history, _, _ := unstructured.NestedSlice(obj.Object, "status", "phaseHistory")
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"phases": history})
}

func (s *Server) previewConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var spec crd.NestedClusterSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var hosts []installer.HostConfig
	nodes := buildNodeList(spec)
	for _, n := range nodes {
		hosts = append(hosts, installer.HostConfig{
			Name: n.Name,
			Role: n.Role,
		})
	}

	installCfg := installer.GenerateInstallConfig(spec, hosts, "<PULL_SECRET>", "<SSH_KEY>")
	agentCfg := installer.GenerateAgentConfig(spec, hosts)

	s.writeJSON(w, http.StatusOK, map[string]string{
		"installConfig": installCfg,
		"agentConfig":   agentCfg,
	})
}

func (s *Server) listStorageClasses(w http.ResponseWriter, r *http.Request) {
	list, err := s.client.DynamicClient.Resource(storageClassGVR).
		List(r.Context(), metav1.ListOptions{})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to list storage classes")
		return
	}

	names := make([]string, 0, len(list.Items))
	for _, sc := range list.Items {
		names = append(names, sc.GetName())
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": names})
}

func (s *Server) listNADs(w http.ResponseWriter, r *http.Request) {
	type nadItem struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}

	var items []nadItem
	namespacesParam := r.URL.Query().Get("namespaces")

	if namespacesParam != "" {
		seen := map[string]bool{}
		for _, ns := range strings.Split(namespacesParam, ",") {
			ns = strings.TrimSpace(ns)
			if ns == "" || seen[ns] {
				continue
			}
			seen[ns] = true
			list, err := s.client.DynamicClient.Resource(nadGVR).
				Namespace(ns).List(r.Context(), metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, nad := range list.Items {
				items = append(items, nadItem{
					Name:      nad.GetName(),
					Namespace: nad.GetNamespace(),
				})
			}
		}
	} else {
		list, err := s.client.DynamicClient.Resource(nadGVR).
			Namespace("").List(r.Context(), metav1.ListOptions{})
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "failed to list NADs")
			return
		}
		for _, nad := range list.Items {
			items = append(items, nadItem{
				Name:      nad.GetName(),
				Namespace: nad.GetNamespace(),
			})
		}
	}

	if items == nil {
		items = []nadItem{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": items})
}

func (s *Server) listOCPVersions(w http.ResponseWriter, r *http.Request) {
	versions := []string{
		"4.22",
		"4.21",
		"4.20",
		"4.19",
		"4.18",
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": versions})
}

func (s *Server) findCluster(r *http.Request, name string) (*unstructured.Unstructured, error) {
	list, err := s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace("").List(r.Context(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].GetName() == name {
			return &list.Items[i], nil
		}
	}
	return nil, &notFoundError{name: name}
}

func buildNodeList(spec crd.NestedClusterSpec) []crd.NodeNameEntry {
	if len(spec.NodeNames) > 0 {
		return spec.NodeNames
	}
	var nodes []crd.NodeNameEntry
	for i := 0; i < spec.Masters.Count; i++ {
		nodes = append(nodes, crd.NodeNameEntry{
			Name: fmt.Sprintf("%s-master-%d", spec.ClusterName, i),
			Role: "master",
		})
	}
	for i := 0; i < spec.Workers.Count; i++ {
		nodes = append(nodes, crd.NodeNameEntry{
			Name: fmt.Sprintf("%s-worker-%d", spec.ClusterName, i),
			Role: "worker",
		})
	}
	return nodes
}

type notFoundError struct {
	name string
}

func (e *notFoundError) Error() string {
	return "cluster not found: " + e.name
}
