package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// hostNodeAddresses reads the host nodes' addresses. ok is false when the
// orchestrator may not list nodes (an install whose RBAC has not been updated
// yet): callers then skip the overlap check instead of blocking every create.
func (s *Server) hostNodeAddresses(ctx context.Context) (host []crd.HostNodeAddress, ok bool, err error) {
	host, err = s.client.HostNodeAddresses(ctx)
	if err == nil {
		return host, true, nil
	}
	if apierrors.IsForbidden(err) {
		s.logger.Info("cannot list host nodes; skipping the internal network overlap check (update the orchestrator RBAC)", "error", err.Error())
		return nil, false, nil
	}
	return nil, false, err
}

// checkHostNetwork refuses an internal network that overlaps the host
// cluster's node network (crd.CheckHostNetwork).
func (s *Server) checkHostNetwork(ctx context.Context, spec *crd.NestedClusterSpec) (int, error) {
	host, ok, err := s.hostNodeAddresses(ctx)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("listing host cluster nodes: %w", err)
	}
	if !ok {
		return 0, nil
	}
	if err := spec.CheckHostNetwork(host); err != nil {
		return http.StatusBadRequest, err
	}
	return 0, nil
}

type hostNetwork struct {
	NodeAddresses []crd.HostNodeAddress `json:"nodeAddresses"`
	// SuggestedCUDNSubnet is the first default internal network clear of the
	// host nodes and the default nested pod/service networks ("" if none).
	SuggestedCUDNSubnet string `json:"suggestedCudnSubnet"`
	SuggestedAPIVIP     string `json:"suggestedApiVIP,omitempty"`
	SuggestedIngressVIP string `json:"suggestedIngressVIP,omitempty"`
}

// getHostNetwork returns the host nodes' addresses and a free default
// internal network for the wizard. Read-only.
func (s *Server) getHostNetwork(w http.ResponseWriter, r *http.Request) {
	host, err := s.client.HostNodeAddresses(r.Context())
	if err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsForbidden(err) {
			code = http.StatusServiceUnavailable
		}
		s.writeError(w, code, "cannot list host cluster nodes: "+err.Error())
		return
	}
	resp := hostNetwork{
		NodeAddresses:       host,
		SuggestedCUDNSubnet: crd.FreeCUDNSubnet(host, crd.DefaultClusterNetworkCIDR, crd.DefaultServiceNetworkCIDR),
	}
	resp.SuggestedAPIVIP, resp.SuggestedIngressVIP = crd.InternalVIPs(resp.SuggestedCUDNSubnet)
	s.writeJSON(w, http.StatusOK, resp)
}
