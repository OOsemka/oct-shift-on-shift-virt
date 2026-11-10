package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/cni"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/installer"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/release"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
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
	for _, rt := range s.Routes() {
		s.mux.HandleFunc(rt.Pattern, rt.Handler)
	}
}

// Route is one API route. Routes is the single list the mux and the permission
// table (permissions.go) are built from, so they can't drift apart.
type Route struct {
	Pattern string
	Handler http.HandlerFunc
}

func (s *Server) Routes() []Route {
	return []Route{
		{"GET /api/v1/clusters", s.listClusters},
		{"GET /api/v1/clusters/{name}", s.getCluster},
		{"POST /api/v1/clusters", s.createCluster},
		{"DELETE /api/v1/clusters/{name}", s.deleteCluster},
		{"GET /api/v1/clusters/{name}/status", s.getClusterStatus},
		{"GET /api/v1/clusters/{name}/nodes", s.getClusterNodes},
		{"GET /api/v1/clusters/{name}/kubeconfig", s.getKubeconfig},
		{"GET /api/v1/clusters/{name}/kubeadmin", s.getKubeadmin},
		{"GET /api/v1/clusters/{name}/external-ca", s.getExternalCA},
		{"POST /api/v1/clusters/{name}/make-master", s.makeMaster},
		{"GET /api/v1/masters", s.listMasters},
		{"POST /api/v1/clusters/{name}/refresh-master", s.refreshMaster},
		{"GET /api/v1/clusters/{name}/logs", s.getClusterLogs},
		{"POST /api/v1/clusters/preview-config", s.previewConfig},
		{"GET /api/v1/storageclasses", s.listStorageClasses},
		{"GET /api/v1/nads", s.listNADs},
		{"GET /api/v1/ippools", s.listIPPools},
		{"GET /api/v1/defaults", s.getLabDefaults},
		{"GET /api/v1/quick-clusters/preview", s.previewQuickCluster},
		{"POST /api/v1/quick-clusters", s.createQuickCluster},
		{"GET /api/v1/ocp-versions", s.listOCPVersions},
		{"GET /api/v1/network-plugins", s.listNetworkPlugins},
		{"GET /api/v1/host-network", s.getHostNetwork},
		{"GET /healthz", s.healthz},
	}
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
		// Express: the master a clone came from, and why a cluster cannot
		// become a master ("" if it can).
		CloneOf       string `json:"cloneOf,omitempty"`
		MasterRefusal string `json:"masterRefusal,omitempty"`
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
		if nc, err := toNestedCluster(&item); err == nil {
			last := &items[len(items)-1]
			if nc.IsClone() {
				last.CloneOf = nc.Spec.Source.Master
			}
			last.MasterRefusal = nc.MasterRefusal()
		}
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
	// Refuse settings that can only produce a broken cluster, before anything
	// is created (the wizard checks the same things; this covers every client).
	if err := spec.ValidateNetwork(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, code, err := s.createNestedCluster(r.Context(), spec)
	if err != nil {
		s.writeError(w, code, err.Error())
		return
	}
	s.writeJSON(w, http.StatusCreated, created.Object)
}

// createNestedCluster is the single create path (wizard and quick cluster):
// network checks, address availability and pool membership, then the CR.
func (s *Server) createNestedCluster(ctx context.Context, spec crd.NestedClusterSpec) (*unstructured.Unstructured, int, error) {
	if err := crd.ValidateClusterNamespace(spec.Namespace); err != nil {
		return nil, http.StatusBadRequest, err
	}
	// A clone must name a real master and keep its internal layout; the
	// orchestrator would otherwise clone disks that do not fit the spec.
	if spec.Source != nil {
		m, err := s.getMaster(ctx, spec.Source.Master)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		want := spec
		if err := crd.ApplyMaster(&want, m); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if want.Network != spec.Network || want.Masters != spec.Masters || want.StorageClassName != spec.StorageClassName ||
			*want.Source != *spec.Source || !spec.ExternalAccess.HostRoutes {
			return nil, http.StatusBadRequest, fmt.Errorf("a clone must keep the master's network, sizing and storage class, and use host routes")
		}
	}
	// The cluster gets its own namespace: refuse one that already exists
	// unless this cluster created it, so a later delete cannot wipe it.
	if ns, err := s.client.Clientset.CoreV1().Namespaces().Get(ctx, spec.Namespace, metav1.GetOptions{}); err == nil {
		if !k8s.NamespaceOwnedBy(ns.Labels, spec.ClusterName) {
			return nil, http.StatusConflict, fmt.Errorf("namespace %q already exists; nested clusters need a new namespace of their own", spec.Namespace)
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, http.StatusInternalServerError, fmt.Errorf("checking namespace %q: %w", spec.Namespace, err)
	}
	if err := spec.ValidateNetwork(); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if code, err := s.checkHostNetwork(ctx, &spec); err != nil {
		return nil, code, err
	}
	// The version must be one the tool supports and must exist now
	// (GenerateISO resolves it again and pins the result); otherwise it
	// would only fail an hour into the install.
	if err := release.Supported(spec.OCPVersion); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if _, err := release.Default.Resolve(ctx, spec.OCPVersion); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if code, err := s.checkAddressAvailability(ctx, &spec); err != nil {
		return nil, code, err
	}

	ncNamespace := clusterNamespace

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
		Namespace(ncNamespace).Create(ctx, nc, metav1.CreateOptions{})
	if err != nil {
		s.logger.Error("failed to create cluster", "error", err.Error(), "cluster", spec.ClusterName)
		if strings.Contains(err.Error(), "already exists") {
			return nil, http.StatusConflict, fmt.Errorf("a cluster named %q already exists", spec.ClusterName)
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("failed to create cluster: %w", err)
	}

	s.logger.Info("cluster created", "cluster", spec.ClusterName, "namespace", ncNamespace)
	return created, 0, nil
}

// markDeleting sets the Deleting phase in an object's status (in memory).
func markDeleting(obj *unstructured.Unstructured) {
	statusMap, _, _ := unstructured.NestedMap(obj.Object, "status")
	if statusMap == nil {
		statusMap = map[string]interface{}{}
	}
	statusMap["phase"] = string(crd.PhaseDeleting)
	statusMap["message"] = "Deletion requested"
	obj.Object["status"] = statusMap
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	// A master's snapshots back its clones: delete the clones first.
	if items, merr := s.masters(r.Context()); merr == nil {
		for _, m := range items {
			if m.Name == name && m.Clones > 0 {
				s.writeError(w, http.StatusConflict, fmt.Sprintf("%s is a master with %d clone(s); delete them first", name, m.Clones))
				return
			}
		}
	}

	// Status first, so the UI reflects the request immediately. This is
	// cosmetic and must never stop the deletion: during an install the
	// orchestrator updates the status constantly, so the write can conflict.
	// Retry against the latest version, and delete regardless. (A conflict
	// here used to return "failed to initiate deletion" and delete nothing.)
	ns := obj.GetNamespace()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, gerr := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(ns).Get(r.Context(), name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		markDeleting(cur)
		_, uerr := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(ns).UpdateStatus(r.Context(), cur, metav1.UpdateOptions{})
		return uerr
	}); err != nil {
		s.logger.Info("could not mark cluster as Deleting; deleting anyway", "cluster", name, "error", err.Error())
	}

	// Then actually delete the object. Marking the phase alone tore down the
	// cluster's resources but left the CR behind forever, showing "Deleting"
	// with no deletionTimestamp (chris-sos-odf lingered for an hour that way,
	// and removing its finalizer by hand did nothing because there was no
	// deletion in flight to unblock). The real Delete sets a deletionTimestamp,
	// which drives the sos.octools.io/cleanup finalizer -> DeleteCluster ->
	// finalizer release -> the object disappears. That path is already proven.
	if err := s.client.DynamicClient.Resource(nestedClusterGVR).
		Namespace(ns).Delete(r.Context(), name, metav1.DeleteOptions{}); err != nil {
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
	ns := clusterSecretNamespace(obj)
	if secretRef == "" {
		s.writeError(w, http.StatusNotFound, "kubeconfig not yet available")
		return
	}

	data, err := s.client.GetSecretData(r.Context(), ns, secretRef, "kubeconfig")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to read kubeconfig from "+ns+"/"+secretRef+": "+err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"kubeconfig": string(data)})
}

// getExternalCA returns the public CA certificate that signs the host-route
// certificates (externalAccess.hostRoutes). Only ca.crt is read; the CA key
// stays in the Secret.
func (s *Server) getExternalCA(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	cluster, _, _ := unstructured.NestedString(obj.Object, "spec", "clusterName")
	if cluster == "" {
		cluster = obj.GetName()
	}
	data, err := s.client.GetSecretData(r.Context(), clusterSecretNamespace(obj), cluster+"-external-access", "ca.crt")
	if err != nil || len(data) == 0 {
		s.writeError(w, http.StatusNotFound, "no host-route CA for this cluster (host routes not enabled or not published yet)")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"caCert": string(data)})
}

func (s *Server) getKubeadmin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj, err := s.findCluster(r, name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "cluster not found")
		return
	}

	secretRef, _, _ := unstructured.NestedString(obj.Object, "status", "kubeadminSecretRef")
	ns := clusterSecretNamespace(obj)
	if secretRef == "" {
		s.writeError(w, http.StatusNotFound, "kubeadmin password not yet available")
		return
	}

	data, err := s.client.GetSecretData(r.Context(), ns, secretRef, "password")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to read kubeadmin password from "+ns+"/"+secretRef+": "+err.Error())
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

// listOCPVersions returns the supported minors (release.SupportedMinors,
// newest first) and, best effort, the release each one installs today:
// {"items":["5.0","4.22"],"latest":{"5.0":"5.0.1","4.22":"4.22.22"}}.
func (s *Server) listOCPVersions(w http.ResponseWriter, r *http.Request) {
	versions := release.SupportedMinors

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":  versions,
		"latest": release.Default.Latest(ctx, versions),
	})
}

// listNetworkPlugins returns the network plugin choices (OVN-Kubernetes,
// Calico, Cilium) with their versions, newest first, for the wizard.
func (s *Server) listNetworkPlugins(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"items": cni.Plugins()})
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

// clusterSecretNamespace returns the namespace the per-cluster Secrets
// actually live in. PostInstall writes kubeconfig/kubeadmin into the
// CLUSTER's namespace (spec.namespace), not the namespace holding the CR, so
// reading them with obj.GetNamespace() always missed and surfaced as a flat
// 500 behind the console's Download Kubeconfig / Copy Kubeadmin Password
// buttons.
func clusterSecretNamespace(obj *unstructured.Unstructured) string {
	if ns, _, _ := unstructured.NestedString(obj.Object, "spec", "namespace"); ns != "" {
		return ns
	}
	return obj.GetNamespace()
}
