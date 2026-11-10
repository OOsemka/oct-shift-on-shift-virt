package api

import (
	"fmt"
	"net/http"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-storefront/backend-security-kit/auth"
)

// The permission table (backend-security-kit): what the caller must be allowed to do
// for each route, in Kubernetes RBAC terms, checked with the caller's own token. It
// mirrors RBAC on the objects the orchestrator acts on: NestedClusters live in this
// tool's namespace, a nested cluster's credentials in its own namespace.

func nestedClusters(verb string, name auth.Extractor) auth.Check {
	return auth.Check{Verb: verb, Group: crd.Group, Resource: crd.Resource, Namespace: auth.Fixed(clusterNamespace), Name: name}
}

// clusterSecretsNamespace resolves {name} to the namespace holding that nested
// cluster's kubeconfig and kubeadmin password. An unknown cluster checks this tool's
// namespace (the handler then answers 404).
func (s *Server) clusterSecretsNamespace(r *http.Request) string {
	obj, err := s.findCluster(r, r.PathValue("name"))
	if err != nil || obj == nil {
		return clusterNamespace
	}
	return clusterSecretNamespace(obj)
}

func (s *Server) checksFor(pattern string) (checks []auth.Check, public bool, err error) {
	byName := auth.PathValue("name")
	list := nestedClusters("list", nil)
	get := nestedClusters("get", byName)
	switch pattern {
	case "GET /healthz":
		return nil, true, nil
	case "GET /api/v1/clusters", "GET /api/v1/masters", "GET /api/v1/defaults", "GET /api/v1/ocp-versions",
		"GET /api/v1/network-plugins", "GET /api/v1/quick-clusters/preview", "POST /api/v1/clusters/preview-config":
		return []auth.Check{list}, false, nil
	case "GET /api/v1/clusters/{name}", "GET /api/v1/clusters/{name}/status", "GET /api/v1/clusters/{name}/nodes",
		"GET /api/v1/clusters/{name}/logs", "GET /api/v1/clusters/{name}/external-ca":
		return []auth.Check{get}, false, nil
	case "GET /api/v1/clusters/{name}/kubeconfig", "GET /api/v1/clusters/{name}/kubeadmin":
		// Admin credentials of the nested cluster: only for someone who could read the
		// Secrets that hold them.
		return []auth.Check{get, {Verb: "get", Resource: "secrets", Namespace: s.clusterSecretsNamespace}}, false, nil
	case "POST /api/v1/clusters", "POST /api/v1/quick-clusters":
		// Creating a cluster makes the orchestrator create a new namespace, VMs and
		// networks on the caller's behalf.
		return []auth.Check{nestedClusters("create", nil), {Verb: "create", Resource: "namespaces"}}, false, nil
	case "DELETE /api/v1/clusters/{name}":
		return []auth.Check{nestedClusters("delete", byName)}, false, nil
	case "POST /api/v1/clusters/{name}/make-master", "POST /api/v1/clusters/{name}/refresh-master":
		return []auth.Check{nestedClusters("update", byName)}, false, nil
	case "GET /api/v1/storageclasses":
		return []auth.Check{{Verb: "list", Group: "storage.k8s.io", Resource: "storageclasses"}}, false, nil
	case "GET /api/v1/nads":
		return []auth.Check{{Verb: "list", Group: "k8s.cni.cncf.io", Resource: "network-attachment-definitions"}}, false, nil
	case "GET /api/v1/ippools":
		return []auth.Check{{Verb: "list", Group: crd.Group, Resource: "ippools", Namespace: auth.Fixed(clusterNamespace)}}, false, nil
	case "GET /api/v1/host-network":
		return []auth.Check{{Verb: "list", Resource: "nodes"}}, false, nil
	}
	return nil, false, fmt.Errorf("no permission rule for route %q", pattern)
}

// AuthRoutes is Routes with their permission checks, for auth.New. A route without a
// rule is an error, so a new route can't ship unchecked.
func (s *Server) AuthRoutes() ([]auth.Route, error) {
	var out []auth.Route
	for _, rt := range s.Routes() {
		checks, public, err := s.checksFor(rt.Pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, auth.Route{Pattern: rt.Pattern, Checks: checks, Public: public, Handler: rt.Handler})
	}
	return out, nil
}
