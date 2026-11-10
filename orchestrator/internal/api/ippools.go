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
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// clusterNamespace is where NestedClusters and IPPools live.
const clusterNamespace = "oct-shift-on-shift-virt"

var ipPoolGVR = schema.GroupVersionResource{
	Group:    crd.Group,
	Version:  crd.Version,
	Resource: crd.IPPoolResource,
}

func toIPPool(obj unstructured.Unstructured) crd.IPPool {
	p := crd.IPPool{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if raw, ok := obj.Object["spec"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &p.Spec)
	}
	return p
}

// clusterSpecs lists every NestedCluster's spec (all namespaces), so address
// usage includes clusters that are still being deleted.
func (s *Server) clusterSpecs(ctx context.Context) ([]crd.NestedClusterSpec, error) {
	list, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]crd.NestedClusterSpec, 0, len(list.Items))
	for _, item := range list.Items {
		var spec crd.NestedClusterSpec
		b, _ := json.Marshal(item.Object["spec"])
		if json.Unmarshal(b, &spec) == nil {
			if spec.ClusterName == "" {
				spec.ClusterName = item.GetName()
			}
			out = append(out, spec)
		}
	}
	return out, nil
}

func (s *Server) getIPPool(ctx context.Context, name string) (*crd.IPPool, error) {
	obj, err := s.client.DynamicClient.Resource(ipPoolGVR).Namespace(clusterNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	p := toIPPool(*obj)
	return &p, nil
}

// checkAddressAvailability refuses addresses another cluster already uses on
// the same network and, when the request names an IP pool, addresses or
// network settings that do not match the pool.
func (s *Server) checkAddressAvailability(ctx context.Context, spec *crd.NestedClusterSpec) (int, error) {
	others, err := s.clusterSpecs(ctx)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("listing clusters to check addresses: %w", err)
	}
	if err := spec.CheckAddressesFree(others); err != nil {
		return http.StatusConflict, err
	}
	if spec.ExternalAccess.PublishAppRoutes && !spec.ExternalAccess.HostRoutes {
		return http.StatusBadRequest, fmt.Errorf("publishing application routes needs host routes")
	}
	if spec.ExternalAccess.HostRoutes && spec.ExternalAccess.IPPool == "" {
		return http.StatusBadRequest, fmt.Errorf("host routes need the external addresses to come from an IP pool")
	}
	if spec.ExternalAccess.IPPool != "" {
		pool, err := s.getIPPool(ctx, spec.ExternalAccess.IPPool)
		if err != nil {
			pool = nil
		}
		if err := spec.CheckPool(pool); err != nil {
			return http.StatusBadRequest, err
		}
	}
	return 0, nil
}

type poolAddress struct {
	IP     string          `json:"ip"`
	UsedBy *crd.AddressUse `json:"usedBy,omitempty"`
}

type poolItem struct {
	Name         string        `json:"name"`
	NADName      string        `json:"nadName"`
	PrefixLength int           `json:"prefixLength"`
	Gateway      string        `json:"gateway,omitempty"`
	Description  string        `json:"description,omitempty"`
	Addresses    []poolAddress `json:"addresses"`
	Total        int           `json:"total"`
	Free         int           `json:"free"`
	// Problem is set when the pool is invalid; such pools offer no addresses.
	Problem string `json:"problem,omitempty"`
}

// listIPPools returns every pool with each address marked free or used, so the
// wizard can offer only free addresses and the IP Pools tab can show who holds
// what. Reading pools through the orchestrator lets people who create clusters
// see them without RBAC on IPPools; creating and deleting pools goes through
// the Kubernetes API with the user's own permissions.
func (s *Server) listIPPools(w http.ResponseWriter, r *http.Request) {
	list, err := s.client.DynamicClient.Resource(ipPoolGVR).Namespace(clusterNamespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		s.logger.Error("failed to list IP pools", "error", err.Error())
		s.writeError(w, http.StatusInternalServerError, "failed to list IP pools: "+err.Error())
		return
	}
	clusters, err := s.clusterSpecs(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to list clusters: "+err.Error())
		return
	}
	use := crd.ExternalAddressUse(clusters)

	items := []poolItem{}
	for _, obj := range list.Items {
		p := toIPPool(obj)
		item := poolItem{
			Name: p.Name, NADName: p.Spec.NADName, PrefixLength: p.Spec.PrefixLength,
			Gateway: p.Spec.Gateway, Description: p.Spec.Description, Addresses: []poolAddress{},
		}
		if err := p.Spec.Validate(); err != nil {
			item.Problem = err.Error()
			items = append(items, item)
			continue
		}
		addrs, _ := p.Spec.Expand()
		for _, a := range addrs {
			pa := poolAddress{IP: a}
			if u, ok := use[p.Spec.NADName][a]; ok {
				u := u
				pa.UsedBy = &u
			} else {
				item.Free++
			}
			item.Addresses = append(item.Addresses, pa)
		}
		item.Total = len(addrs)
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"pools": items})
}
