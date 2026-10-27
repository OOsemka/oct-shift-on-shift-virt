package k8s

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var routeGVR = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
var infraGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "infrastructures"}

// AppsDomain returns the host cluster's default apps (router) domain, e.g.
// "apps.sos-ocp.example.com". Routes whose hosts live under it are accepted by
// the default ingress controller without touching host-cluster config.
func (c *Client) AppsDomain(ctx context.Context) (string, error) {
	infra, err := c.DynamicClient.Resource(infraGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err == nil {
		if d, _, _ := unstructured.NestedString(infra.Object, "status", "appliedClusterDomain"); d != "" {
			return "apps." + d, nil
		}
		if d, _, _ := unstructured.NestedString(infra.Object, "status", "domain"); d != "" {
			return "apps." + d, nil
		}
	}
	// Fallback: default ingresscontroller's spec/status domain.
	icGVR := schema.GroupVersionResource{Group: "operator.openshift.io", Version: "v1", Resource: "ingresscontrollers"}
	ic, err2 := c.DynamicClient.Resource(icGVR).Namespace("openshift-ingress-operator").Get(ctx, "default", metav1.GetOptions{})
	if err2 == nil {
		if d, _, _ := unstructured.NestedString(ic.Object, "status", "domain"); d != "" {
			return d, nil
		}
		if d, _, _ := unstructured.NestedString(ic.Object, "spec", "domain"); d != "" {
			return d, nil
		}
	}
	return "", fmt.Errorf("could not determine host apps domain (appliedClusterDomain and ingresscontroller both unreadable): %v / %v", err, err2)
}

// EnsureHeadlessService creates (or updates ports of) a selector-less
// Service. Selector-less + manual Endpoints lets a Route point at pods that
// kube-proxy cannot resolve for other reasons (here: UDN-attached pods whose
// podIP is still a default-network address we can hand-code).
func (c *Client) EnsureHeadlessService(ctx context.Context, namespace, name string, ports map[string]int32, labels map[string]string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports:     []corev1.ServicePort{},
		},
	}
	for pname, port := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name: pname, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP,
		})
	}
	existing, err := c.Clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		_, err = c.Clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
		return err
	}
	svc.ResourceVersion = existing.ResourceVersion
	svc.Spec.ClusterIP = existing.Spec.ClusterIP // None is immutable-stable, keep server view
	_, err = c.Clientset.CoreV1().Services(namespace).Update(ctx, svc, metav1.UpdateOptions{})
	return err
}

// EnsureEndpoints writes the address list for a selector-less service.
func (c *Client) EnsureEndpoints(ctx context.Context, namespace, name string, ips []string, ports map[string]int32) error {
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Subsets:    []corev1.EndpointSubset{},
	}
	if len(ips) > 0 {
		var subset corev1.EndpointSubset
		for _, ip := range ips {
			subset.Addresses = append(subset.Addresses, corev1.EndpointAddress{IP: ip})
		}
		for pname, port := range ports {
			subset.Ports = append(subset.Ports, corev1.EndpointPort{Name: pname, Port: port, Protocol: corev1.ProtocolTCP})
		}
		ep.Subsets = append(ep.Subsets, subset)
	}
	existing, err := c.Clientset.CoreV1().Endpoints(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		_, err = c.Clientset.CoreV1().Endpoints(namespace).Create(ctx, ep, metav1.CreateOptions{})
		return err
	}
	ep.ResourceVersion = existing.ResourceVersion
	_, err = c.Clientset.CoreV1().Endpoints(namespace).Update(ctx, ep, metav1.UpdateOptions{})
	return err
}

// EnsurePassthroughRoute creates/updates a TLS-passthrough Route in
// namespace serving host and fanning out to serviceName:portName.
// Passthrough is mandatory here: the nested apiserver/router hold their own
// certs; the host router must not terminate (it could not present a valid
// cert for the nested names, and reencrypt health checks would fail).
// EnsurePassthroughRoute creates the route and returns the hostname it ends
// up with. An EMPTY host asks the router to generate one
// (<name>-<namespace>.<appsDomain>), which avoids needing create on
// routes/custom-host — a privilege that lets a service account claim
// arbitrary hostnames, and one the orchestrator does not have. The generated
// host is read back so callers can use it for SNI routing and status.
func (c *Client) EnsurePassthroughRoute(ctx context.Context, namespace, name, host, serviceName, portName string, labels map[string]string) (string, error) {
	route := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "route.openshift.io/v1",
			"kind":       "Route",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels":    toIfaceMap(labels),
			},
			"spec": map[string]interface{}{
				"to": map[string]interface{}{
					"kind":   "Service",
					"name":   serviceName,
					"weight": int64(100),
				},
				"port": map[string]interface{}{"targetPort": portName},
				"tls":  map[string]interface{}{"termination": "passthrough"},
			},
		},
	}
	if host != "" {
		unstructured.SetNestedField(route.Object, host, "spec", "host")
	}
	resource := c.DynamicClient.Resource(routeGVR).Namespace(namespace)
	if existing, gerr := resource.Get(ctx, name, metav1.GetOptions{}); gerr == nil {
		// Never rewrite spec.host on an existing route: the router assigned
		// it, and changing it would also need routes/custom-host.
		cur, _, _ := unstructured.NestedString(existing.Object, "spec", "host")
		unstructured.SetNestedField(route.Object, cur, "spec", "host")
		route.SetResourceVersion(existing.GetResourceVersion())
		if _, uerr := resource.Update(ctx, route, metav1.UpdateOptions{}); uerr != nil {
			return "", uerr
		}
		return cur, nil
	}
	created, cerr := resource.Create(ctx, route, metav1.CreateOptions{})
	if cerr != nil {
		if !strings.Contains(cerr.Error(), "already exists") {
			return "", cerr
		}
		if created, cerr = resource.Get(ctx, name, metav1.GetOptions{}); cerr != nil {
			return "", cerr
		}
	}
	assigned, _, _ := unstructured.NestedString(created.Object, "spec", "host")
	return assigned, nil
}

func toIfaceMap(in map[string]string) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// RouteHostsByCluster maps cluster name -> the hostname the router assigned
// to its api route. The hostname is authoritative only after creation, so the
// SNI rules are built from this rather than from a computed name.
func (c *Client) RouteHostsByCluster(ctx context.Context, namespace string) (map[string]string, error) {
	list, err := c.DynamicClient.Resource(routeGVR).Namespace(namespace).
		List(ctx, metav1.ListOptions{LabelSelector: "oct-shift-on-shift-virt/component=api-route"})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range list.Items {
		r := list.Items[i]
		cluster := r.GetLabels()["oct-shift-on-shift-virt/cluster"]
		host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
		if cluster != "" && host != "" {
			out[cluster] = host
		}
	}
	return out, nil
}
