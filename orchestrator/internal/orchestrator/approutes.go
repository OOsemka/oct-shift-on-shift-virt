package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Per-application host routes: a nested Route annotated
// sos.octools.io/publish-host-route: "true" is published under the host's
// *.apps (router-assigned name <cluster>-<app>-<hash>-<operator ns>):
//
//   - host: an EDGE route (the host router's own wildcard certificate, which
//     people already trust) to a Service+EndpointSlice on the bastion's
//     external ingress address, port 80. Edge, not reencrypt: setting a
//     destination CA needs routes/custom-host, which the orchestrator does
//     not hold. The hop from the host router to the bastion is plain HTTP on
//     the lab network; console, OAuth and API stay passthrough.
//   - nested: a sibling route with the host-side name (edge, insecure
//     Allow, so the plain-HTTP hop is served), same service/port/path;
//   - bastion DNS: the host-side name resolves to the nested ingress inside;
//   - the annotated route gets sos.octools.io/host-url.
//
// Passthrough routes cannot be served over the HTTP hop and are skipped.
// The resync reconciles every couple of minutes; removing the annotation or
// the route removes what was created.

const (
	appRoutesComponent    = "app-routes"
	annoPublishHostRoute  = "sos.octools.io/publish-host-route"
	annoHostURL           = "sos.octools.io/host-url"
	labelAppRouteSibling  = "sos.octools.io/host-route-sibling"
	annoAppRouteSource    = "oct-shift-on-shift-virt/source"
	appRouteSiblingSuffix = "-sos-host"
)

// publishWanted decides whether a nested route is published: explicitly
// (annotation "true"), or automatically with publishAppRoutes for routes
// outside the system namespaces unless the annotation says "false".
func publishWanted(publishAll bool, namespace, annotation string) bool {
	switch annotation {
	case "true":
		return true
	case "false":
		return false
	}
	if !publishAll {
		return false
	}
	return !strings.HasPrefix(namespace, "openshift") && !strings.HasPrefix(namespace, "kube-")
}

func appHTTPService(cluster string) string { return cluster + "-ext-http" }

func trunc(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Trim(s, "-.")
}

// appRouteName is the host route's name. The router appends
// "-<operator namespace>", and the result must fit a 63-character DNS label.
func appRouteName(cluster, ns, name string) string {
	sum := sha256.Sum256([]byte(ns + "/" + name))
	return trunc(cluster, 14) + "-" + trunc(name, 12) + "-" + hex.EncodeToString(sum[:])[:5]
}

func appSiblingName(name string) string {
	return trunc(name, 63-len(appRouteSiblingSuffix)) + appRouteSiblingSuffix
}

// nestedDynamic is a client for the nested API through its host API route,
// TLS verified against the cluster's host-route CA.
func (s *StepExecutor) nestedDynamic(ctx context.Context, nc *crd.NestedCluster) (dynamic.Interface, error) {
	kc, err := s.client.GetSecretData(ctx, nc.Spec.Namespace, nc.Status.KubeconfigRef, "kubeconfig")
	if err != nil {
		return nil, err
	}
	ca, err := s.client.GetSecretData(ctx, nc.Spec.Namespace, externalAccessSecret(nc.Spec.ClusterName), "ca.crt")
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, err
	}
	cfg.Host = nc.Status.HostAPIURL
	cfg.TLSClientConfig.CAData = ca
	cfg.TLSClientConfig.CAFile = ""
	cfg.TLSClientConfig.ServerName = ""
	cfg.Timeout = 20 * time.Second
	return dynamic.NewForConfig(cfg)
}

var appRouteSyncs sync.Map // cluster -> *sync.Mutex (one sync at a time per cluster)

// SyncAppRoutes reconciles the per-application host routes of a Ready
// cluster with host routes. Best effort: errors are logged and retried on
// the next resync.
func (s *StepExecutor) SyncAppRoutes(ctx context.Context, nc *crd.NestedCluster) {
	if nc.Status.Phase != crd.PhaseReady || !nc.Spec.ExternalAccess.HostRoutes || nc.Status.HostAPIURL == "" || nc.Status.KubeconfigRef == "" {
		return
	}
	mu, _ := appRouteSyncs.LoadOrStore(nc.Name, &sync.Mutex{})
	if !mu.(*sync.Mutex).TryLock() {
		return
	}
	defer mu.(*sync.Mutex).Unlock()
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent(appRoutesComponent)
	if err := s.syncAppRoutes(ctx, nc); err != nil {
		log.Info("application route sync incomplete (retried on the next resync)", "error", err.Error())
	}
}

type appRouteWant struct {
	ns, name string
	route    *unstructured.Unstructured
}

func (s *StepExecutor) syncAppRoutes(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent(appRoutesComponent)
	cluster, opNs := nc.Spec.ClusterName, operatorNamespace()
	nested, err := s.nestedDynamic(ctx, nc)
	if err != nil {
		return fmt.Errorf("nested API client: %w", err)
	}
	list, err := nested.Resource(hostRouteGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nested routes: %w", err)
	}
	want := map[string]appRouteWant{} // host route name -> source
	var siblings []unstructured.Unstructured
	for i := range list.Items {
		r := &list.Items[i]
		if r.GetLabels()[labelAppRouteSibling] == "true" {
			siblings = append(siblings, *r)
			continue
		}
		explicit := r.GetAnnotations()[annoPublishHostRoute]
		if !publishWanted(nc.Spec.ExternalAccess.PublishAppRoutes, r.GetNamespace(), explicit) {
			// No longer published: drop the URL we wrote earlier.
			if _, had := r.GetAnnotations()[annoHostURL]; had {
				patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, annoHostURL)
				_, _ = nested.Resource(hostRouteGVR).Namespace(r.GetNamespace()).Patch(ctx, r.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			}
			continue
		}
		if term, _, _ := unstructured.NestedString(r.Object, "spec", "tls", "termination"); term == "passthrough" {
			if explicit == "true" { // say so only when asked explicitly
				s.annotateNestedRoute(ctx, nested, r, "unsupported: passthrough routes cannot be published (use edge or reencrypt)")
			}
			continue
		}
		want[appRouteName(cluster, r.GetNamespace(), r.GetName())] = appRouteWant{ns: r.GetNamespace(), name: r.GetName(), route: r}
	}

	labels := map[string]string{"oct-shift-on-shift-virt/cluster": cluster, "oct-shift-on-shift-virt/component": appRoutesComponent}
	if len(want) > 0 {
		if err := s.ensureExternalService(ctx, opNs, appHTTPService(cluster), "http", 80, nc.Spec.ExternalAccess.IngressIP, labels); err != nil {
			return fmt.Errorf("host HTTP service: %w", err)
		}
	}
	var published []crd.AppRoute
	for hostName, w := range want {
		host, err := s.ensureEdgeRoute(ctx, opNs, hostName, appHTTPService(cluster), labels, w.ns+"/"+w.name)
		if err != nil || host == "" {
			log.Info("host route not ready", "route", w.ns+"/"+w.name, "error", fmt.Sprint(err))
			continue
		}
		if err := s.ensureNestedSibling(ctx, nested, w.route, host); err != nil {
			log.Info("nested sibling route failed", "route", w.ns+"/"+w.name, "error", err.Error())
			continue
		}
		s.annotateNestedRoute(ctx, nested, w.route, "https://"+host)
		published = append(published, crd.AppRoute{Source: w.ns + "/" + w.name, URL: "https://" + host})
	}

	// Remove host routes (and their nested siblings) that are no longer wanted.
	routes := s.client.DynamicClient.Resource(hostRouteGVR).Namespace(opNs)
	if existing, err := routes.List(ctx, metav1.ListOptions{LabelSelector: "oct-shift-on-shift-virt/cluster=" + cluster +
		",oct-shift-on-shift-virt/component=" + appRoutesComponent}); err == nil {
		for _, hr := range existing.Items {
			if _, ok := want[hr.GetName()]; ok {
				continue
			}
			_ = routes.Delete(ctx, hr.GetName(), metav1.DeleteOptions{})
			log.Info("host route removed", "route", hr.GetName())
		}
	}
	wantSources := map[string]bool{}
	for _, w := range want {
		wantSources[w.ns+"/"+w.name] = true
	}
	for _, sib := range siblings {
		if !wantSources[sib.GetAnnotations()[annoAppRouteSource]] {
			_ = nested.Resource(hostRouteGVR).Namespace(sib.GetNamespace()).Delete(ctx, sib.GetName(), metav1.DeleteOptions{})
		}
	}

	// Status and split DNS, only when the set changed.
	sort.Slice(published, func(i, j int) bool { return published[i].Source < published[j].Source })
	if !sameAppRoutes(nc.Status.AppRoutes, published) {
		nc.Status.AppRoutes = published
		s.updateCRStatus(ctx, nc)
		if err := s.configureBastionDNS(ctx, nc); err != nil {
			return fmt.Errorf("updating the nested DNS: %w", err)
		}
		log.Info("application host routes updated", "count", len(published))
	}
	return nil
}

func sameAppRoutes(a, b []crd.AppRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensureEdgeRoute creates/updates an edge route with a router-assigned host
// and returns that host. Edge with the router's default certificate needs no
// certificate fields (and so no routes/custom-host).
func (s *StepExecutor) ensureEdgeRoute(ctx context.Context, ns, name, svc string, labels map[string]string, source string) (string, error) {
	lbl := map[string]interface{}{}
	for k, v := range labels {
		lbl[k] = v
	}
	spec := map[string]interface{}{
		"to":   map[string]interface{}{"kind": "Service", "name": svc, "weight": int64(100)},
		"port": map[string]interface{}{"targetPort": "http"},
		"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
	}
	routes := s.client.DynamicClient.Resource(hostRouteGVR).Namespace(ns)
	if cur, err := routes.Get(ctx, name, metav1.GetOptions{}); err == nil {
		host, _, _ := unstructured.NestedString(cur.Object, "spec", "host")
		return host, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	r := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "route.openshift.io/v1", "kind": "Route",
		"metadata": map[string]interface{}{"name": name, "namespace": ns, "labels": lbl,
			"annotations": map[string]interface{}{annoAppRouteSource: source}},
		"spec": spec,
	}}
	created, err := routes.Create(ctx, r, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	host, _, _ := unstructured.NestedString(created.Object, "spec", "host")
	return host, nil
}

// ensureNestedSibling makes the nested router answer to the host-side name:
// same service, port and path as the published route, edge with insecure
// Allow (the host router forwards plain HTTP).
func (s *StepExecutor) ensureNestedSibling(ctx context.Context, nested dynamic.Interface, src *unstructured.Unstructured, host string) error {
	to, _, _ := unstructured.NestedMap(src.Object, "spec", "to")
	spec := map[string]interface{}{
		"host": host, "to": to,
		"tls": map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Allow"},
	}
	if port, ok, _ := unstructured.NestedMap(src.Object, "spec", "port"); ok {
		spec["port"] = port
	}
	if path, ok, _ := unstructured.NestedString(src.Object, "spec", "path"); ok && path != "" {
		spec["path"] = path
	}
	name := appSiblingName(src.GetName())
	res := nested.Resource(hostRouteGVR).Namespace(src.GetNamespace())
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "route.openshift.io/v1", "kind": "Route",
		"metadata": map[string]interface{}{"name": name, "namespace": src.GetNamespace(),
			"labels":      map[string]interface{}{labelAppRouteSibling: "true"},
			"annotations": map[string]interface{}{annoAppRouteSource: src.GetNamespace() + "/" + src.GetName()}},
		"spec": spec,
	}}
	cur, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = res.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if h, _, _ := unstructured.NestedString(cur.Object, "spec", "host"); h == host {
		return nil
	}
	obj.SetResourceVersion(cur.GetResourceVersion())
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func (s *StepExecutor) annotateNestedRoute(ctx context.Context, nested dynamic.Interface, r *unstructured.Unstructured, value string) {
	if r.GetAnnotations()[annoHostURL] == value {
		return
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annoHostURL, value)
	_, _ = nested.Resource(hostRouteGVR).Namespace(r.GetNamespace()).Patch(ctx, r.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
}
