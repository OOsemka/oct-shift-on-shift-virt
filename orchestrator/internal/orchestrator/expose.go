package orchestrator

import (
	"context"
	"fmt"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// exposeCluster publishes the nested API and ingress through host-cluster
// Services + passthrough Routes so the nested cluster is reachable without
// workstation /etc/hosts hacks or VLAN routing.
//
// The bridge is the per-cluster HAProxy pods: they live in the nested
// namespace (so they hold cUDN interfaces and can reach the nested masters),
// while their primary podIP is a default-network address the host router can
// reach like any pod. A selector-less Service + hand-written Endpoints aimed
// at those podIPs lets a Route front them; passthrough termination tunnels
// the client TLS untouched (only the nested cluster holds certs for its own
// names).
//
// Caveats (documented for users, not bugs):
//   - API route works fully with oc --insecure-skip-tls-verify: the
//     apiserver cert doesn't carry the route host as SAN (passthrough means
//     the client authenticates the NESTED cert end-to-end).
//   - The console route serves whatever the nested router matches the SNI on
//     (the route host, not console-openshift-console.apps.<nested>) — for
//     full console/OAuth flows use the nested DNS names.
//
// If haproxy pods are rescheduled their podIPs move; PostInstall re-runs of
// a requeued cluster refresh the Endpoints.
func (s *StepExecutor) exposeCluster(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("expose")
	clusterNs := nc.Spec.Namespace

	appsDomain, err := s.client.AppsDomain(ctx)
	if err != nil {
		return err
	}

	labels := map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}

	apiIPs, err := s.podIPsFor(ctx, clusterNs, nc.Spec.ClusterName, "haproxy-api")
	if err != nil {
		return fmt.Errorf("haproxy-api pod IPs: %w", err)
	}
	edgeIPs, err := s.podIPsFor(ctx, clusterNs, nc.Spec.ClusterName, "haproxy-ingress")
	if err != nil {
		return fmt.Errorf("haproxy-ingress pod IPs: %w", err)
	}

	apiSvc := nc.Spec.ClusterName + "-api"
	edgeSvc := nc.Spec.ClusterName + "-edge"
	apiPorts := map[string]int32{"api": 6443}
	edgePorts := map[string]int32{"https": 443, "http": 80}

	if err := s.client.EnsureHeadlessService(ctx, clusterNs, apiSvc, apiPorts, labels); err != nil {
		return fmt.Errorf("api service: %w", err)
	}
	if err := s.client.EnsureEndpoints(ctx, clusterNs, apiSvc, apiIPs, apiPorts); err != nil {
		return fmt.Errorf("api endpoints: %w", err)
	}
	if err := s.client.EnsureHeadlessService(ctx, clusterNs, edgeSvc, edgePorts, labels); err != nil {
		return fmt.Errorf("edge service: %w", err)
	}
	if err := s.client.EnsureEndpoints(ctx, clusterNs, edgeSvc, edgeIPs, edgePorts); err != nil {
		return fmt.Errorf("edge endpoints: %w", err)
	}

	apiHost := nc.Spec.ClusterName + "-api." + appsDomain
	edgeHost := nc.Spec.ClusterName + "-edge." + appsDomain
	if err := s.client.EnsurePassthroughRoute(ctx, clusterNs, nc.Spec.ClusterName+"-api-route", apiHost, apiSvc, "api", labels); err != nil {
		return fmt.Errorf("api route: %w", err)
	}
	if err := s.client.EnsurePassthroughRoute(ctx, clusterNs, nc.Spec.ClusterName+"-edge-route", edgeHost, edgeSvc, "https", labels); err != nil {
		return fmt.Errorf("edge route: %w", err)
	}

	nc.Status.HostAPIRoute = "https://" + apiHost
	nc.Status.HostConsoleRoute = "https://" + edgeHost

	log.Info("nested cluster exposed via host routes",
		"api_route", nc.Status.HostAPIRoute,
		"edge_route", nc.Status.HostConsoleRoute,
		"api_endpoint_ips", apiIPs, "edge_endpoint_ips", edgeIPs)
	log.Info("access: oc --insecure-skip-tls-verify --server=" + nc.Status.HostAPIRoute + " login kubeadmin:<password> (cert SAN caveat, see docs)")
	return nil
}

func (s *StepExecutor) podIPsFor(ctx context.Context, namespace, clusterName, component string) ([]string, error) {
	pods, err := s.client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "oct-shift-on-shift-virt/cluster=" + clusterName + ",oct-shift-on-shift-virt/component=" + component,
	})
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, p := range pods.Items {
		if p.Status.Phase == "Running" && p.Status.PodIP != "" {
			ips = append(ips, p.Status.PodIP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no Running pods for component %s", component)
	}
	return ips, nil
}
