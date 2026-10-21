package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Exposing the nested API through a host-cluster Route needs a bridge, and
// the bridge has to be a pod in a NORMAL namespace that is dual-homed onto
// the external VLAN. Both halves of that are forced by measurement, not
// preference:
//
//   - A nested cluster's pods live in a primary-UDN namespace, which blocks
//     inbound traffic from outside the namespace. A default-network pod gets
//     neither TCP nor ICMP to a nested HAProxy pod, so a Service whose
//     endpoints are those pods can never carry traffic. (An earlier design
//     did exactly that and also tripped the endpoints-restriction admission
//     plugin; the admission error was only the first wall.)
//   - The ingress nodes have no interface on the nested VLAN — they sit on a
//     different subnet and the gateway does not forward. `curl` from an
//     ingress node to the nested VLAN VIP times out, so the hostNetwork
//     router cannot reach a nested cluster either.
//
// ONE proxy is shared by every nested cluster and selects its backend by TLS
// SNI. The VLAN NAD is localnet with no IPAM, so each attachment costs a
// STATIC address: a proxy per cluster would burn a third VLAN IP per cluster
// and need another wizard field, while SNI routing costs one address total.
//
// Termination is passthrough so client certificates survive end to end and
// the admin kubeconfig keeps working with only its server URL changed. The
// nested apiserver therefore presents its own certificate, which carries no
// SAN for the route host — clients need --insecure-skip-tls-verify until the
// route host is added to the nested APIServer servingCerts.
//
// Ingress/apps is deliberately NOT exposed this way: the nested router
// dispatches on Host/SNI, so a passthrough route carrying the host-side name
// gets 503 (measured: 503 bare, 200 with the nested Host header). Console
// access is served by the graphical bastion instead.
const edgeProxyName = "sos-edge-proxy"

// edgeProxyVLANIP is the single static address the shared proxy holds on the
// external VLAN. Empty disables API-route exposure entirely (the nested
// cluster is unaffected — it stays reachable via its VLAN VIPs and DNS).
func edgeProxyVLANIP() string { return strings.TrimSpace(os.Getenv("EDGE_PROXY_VLAN_IP")) }

// edgeProxyNAD is the <namespace>/<name> of the NetworkAttachmentDefinition
// that reaches the nested clusters' external VLAN. There is deliberately no
// default: it is environment-specific, and guessing would attach the proxy to
// a network that does not exist (or worse, to the wrong one).
func edgeProxyNAD() string { return strings.TrimSpace(os.Getenv("EDGE_PROXY_NAD")) }

func edgeProxyImage() string {
	if v := strings.TrimSpace(os.Getenv("EDGE_PROXY_IMAGE")); v != "" {
		return v
	}
	return "docker.io/library/haproxy:2.9-alpine"
}

// exposeCluster publishes THIS cluster's API as <cluster>-api.<appsDomain>
// and (re)builds the shared proxy's config from every NestedCluster that has
// an external API VIP, so removing a cluster drops its backend too.
func (s *StepExecutor) exposeCluster(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("expose")

	vlanIP := edgeProxyVLANIP()
	if vlanIP == "" || edgeProxyNAD() == "" {
		log.Info("API route exposure disabled (set EDGE_PROXY_VLAN_IP and EDGE_PROXY_NAD to enable); the nested cluster is still reachable via its VLAN VIPs and DNS")
		return nil
	}
	if strings.TrimSpace(nc.Spec.ExternalAccess.APIIP) == "" {
		return fmt.Errorf("externalAccess.apiIP is empty — nothing for the edge proxy to forward to")
	}
	opNs := operatorNamespace()
	// The Route comes FIRST and the router assigns its hostname. We do not
	// set spec.host: that needs create on routes/custom-host, a privilege
	// that lets a service account claim arbitrary hostnames and one this
	// operator deliberately does not hold. The generated name
	// (<route>-<namespace>.<appsDomain>) is read back and used for the SNI
	// rules, so the proxy matches exactly what clients will send.
	labels := map[string]string{
		"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
		"oct-shift-on-shift-virt/component": "api-route",
	}
	routeHost, err := s.client.EnsurePassthroughRoute(ctx, opNs, nc.Spec.ClusterName+"-api", "", edgeProxyName, "api", labels)
	if err != nil {
		return fmt.Errorf("api route: %w", err)
	}
	if routeHost == "" {
		return fmt.Errorf("router assigned no hostname to the api route")
	}

	backends, err := s.collectEdgeBackends(ctx, nc, routeHost)
	if err != nil {
		return err
	}
	cfg := renderEdgeProxyConfig(backends)
	sum := sha256.Sum256([]byte(cfg))
	cfgSHA := hex.EncodeToString(sum[:])[:16]

	if err := s.ensureEdgeProxy(ctx, opNs, vlanIP, cfg, cfgSHA); err != nil {
		return err
	}

	nc.Status.HostAPIRoute = "https://" + routeHost
	log.Info("nested API published through a host route",
		"route", nc.Status.HostAPIRoute, "backend", nc.Spec.ExternalAccess.APIIP+":6443",
		"note", "clients need --insecure-skip-tls-verify (nested cert has no SAN for this host)")
	return nil
}

type edgeBackend struct {
	Cluster string
	SNIHost string
	APIIP   string
}

// collectEdgeBackends lists every NestedCluster so the shared config always
// reflects reality — a deleted cluster's backend disappears on the next
// reconcile instead of lingering.
func (s *StepExecutor) collectEdgeBackends(ctx context.Context, current *crd.NestedCluster, currentHost string) ([]edgeBackend, error) {
	hosts, herr := s.client.RouteHostsByCluster(ctx, operatorNamespace())
	if herr != nil {
		return nil, fmt.Errorf("reading api route hosts: %w", herr)
	}
	hosts[current.Spec.ClusterName] = currentHost
	list, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nested clusters for the edge proxy: %w", err)
	}
	seen := map[string]bool{}
	var out []edgeBackend
	add := func(name, apiIP string) {
		host := hosts[name]
		if name == "" || strings.TrimSpace(apiIP) == "" || host == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, edgeBackend{Cluster: name, SNIHost: host, APIIP: apiIP})
	}
	for i := range list.Items {
		item := list.Items[i]
		if item.GetDeletionTimestamp() != nil {
			continue
		}
		nc, cerr := unstructuredToNestedCluster(&item)
		if cerr != nil {
			continue
		}
		add(nc.Spec.ClusterName, nc.Spec.ExternalAccess.APIIP)
	}
	// The cluster being reconciled may not be in the cache yet.
	add(current.Spec.ClusterName, current.Spec.ExternalAccess.APIIP)
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out, nil
}

// renderEdgeProxyConfig builds a TCP proxy that peeks at the TLS SNI to pick
// a nested cluster, without terminating the connection.
func renderEdgeProxyConfig(backends []edgeBackend) string {
	var b strings.Builder
	b.WriteString(`global
  maxconn 4096

defaults
  mode tcp
  timeout connect 5s
  timeout client 1h
  timeout server 1h

frontend nested-api
  bind *:6443
  tcp-request inspect-delay 5s
  tcp-request content accept if { req_ssl_hello_type 1 }
`)
	for _, be := range backends {
		fmt.Fprintf(&b, "  use_backend be_%s if { req_ssl_sni -i %s }\n", sanitizeHAName(be.Cluster), be.SNIHost)
	}
	b.WriteString("  default_backend be_unmatched\n\n")
	for _, be := range backends {
		fmt.Fprintf(&b, "backend be_%s\n  server nested %s:6443\n\n", sanitizeHAName(be.Cluster), be.APIIP)
	}
	// A nameless backend is legal and simply closes unmatched connections,
	// which is what we want for an SNI we do not recognise.
	b.WriteString("backend be_unmatched\n")
	return b.String()
}

func sanitizeHAName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

func (s *StepExecutor) ensureEdgeProxy(ctx context.Context, ns, vlanIP, cfg, cfgSHA string) error {
	labels := map[string]string{"app": edgeProxyName, "oct-shift-on-shift-virt/component": "edge-proxy"}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: edgeProxyName + "-cfg", Namespace: ns, Labels: labels},
		Data:       map[string]string{"haproxy.cfg": cfg},
	}
	cms := s.client.Clientset.CoreV1().ConfigMaps(ns)
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("edge proxy configmap: %w", err)
		}
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating edge proxy configmap: %w", err)
		}
	}

	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: edgeProxyName, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": edgeProxyName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						// Static address: the VLAN NAD is localnet with no IPAM.
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name":%q,"namespace":%q,"ips":["%s/24"]}]`,
							nadName(edgeProxyNAD()), nadNamespace(edgeProxyNAD()), vlanIP),
						// Roll the pod when the backend set changes; haproxy
						// reads its config only at start.
						"sos.octools.io/config-sha": cfgSHA,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "haproxy",
						Image: edgeProxyImage(),
						Ports: []corev1.ContainerPort{{ContainerPort: 6443}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "cfg", MountPath: "/usr/local/etc/haproxy"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "cfg",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: edgeProxyName + "-cfg"}}},
					}},
				},
			},
		},
	}
	deps := s.client.Clientset.AppsV1().Deployments(ns)
	if _, err := deps.Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("edge proxy deployment: %w", err)
		}
		existing, gerr := deps.Get(ctx, edgeProxyName, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		dep.ResourceVersion = existing.ResourceVersion
		if _, err := deps.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating edge proxy deployment: %w", err)
		}
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: edgeProxyName, Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": edgeProxyName},
			Ports:    []corev1.ServicePort{{Name: "api", Port: 6443, TargetPort: intstr.FromInt32(6443)}},
		},
	}
	svcs := s.client.Clientset.CoreV1().Services(ns)
	if _, err := svcs.Create(ctx, svc, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("edge proxy service: %w", err)
	}
	return nil
}

func nadNamespace(ref string) string {
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i]
	}
	return "default"
}

func nadName(ref string) string {
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// operatorNamespace is where the shared edge proxy and the Routes live. It is
// a NORMAL namespace (not a primary UDN), which is precisely why a pod there
// can be reached by the host router.
func operatorNamespace() string {
	if v := strings.TrimSpace(os.Getenv("OPERATOR_NAMESPACE")); v != "" {
		return v
	}
	return "oct-shift-on-shift-virt"
}
