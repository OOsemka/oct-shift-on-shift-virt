package orchestrator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Host routes (externalAccess.hostRoutes): the nested API and console are
// published through passthrough Routes on the HOST cluster, under names the
// host's *.apps wildcard already resolves, so people need no DNS records for
// the nested domain and no bastion desktop.
//
//   - Host side (operator namespace): Services with manual EndpointSlices to
//     the bastion's external API/Ingress addresses (the host routers run on
//     the host network and reach them), and Routes <cluster>-api,
//     <cluster>-console, <cluster>-oauth. The router assigns the hostnames
//     (the orchestrator deliberately has no routes/custom-host).
//   - Nested side (a Job in the cluster namespace): the console and OAuth get
//     the host-side names through ingress.config componentRoutes, and the
//     API server serves a certificate for the host-side API name
//     (apiserver namedCertificates). Both need a certificate: without one
//     the console refuses the custom route and the authentication operator's
//     route health check fails, so certificates are mandatory.
//   - Certificates: a per-cluster CA and two leaf certificates, generated
//     here and kept in the Secret <cluster>-external-access (cluster
//     namespace), so re-runs reuse the CA. The CA certificate is public and
//     offered for download (GET /api/v1/clusters/{name}/external-ca).
//
// Only the console, OAuth and API become reachable this way; other nested
// *.apps applications still need DNS.

//go:embed manifests/hostroutes/publish-routes.sh
var publishRoutesScript string

const (
	conditionHostRoutesReady = "HostRoutesReady"
	hostRoutesComponent      = "host-routes"
	hostRoutesJobTimeout     = 35 * time.Minute
)

var hostRouteGVR = schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}

func externalAccessSecret(cluster string) string { return cluster + "-external-access" }
func hostRoutesScriptsCM(cluster string) string  { return cluster + "-host-routes-scripts" }

// hostRouteNames are the host Routes for a cluster: API, console, OAuth.
func hostRouteNames(cluster string) (api, console, oauth string) {
	return cluster + "-api", cluster + "-console", cluster + "-oauth"
}

func hostRouteServices(cluster string) (api, ingress string) {
	return cluster + "-ext-api", cluster + "-ext-ingress"
}

func hostRoutesLabels(cluster string) map[string]string {
	return map[string]string{
		"oct-shift-on-shift-virt/cluster":   cluster,
		"oct-shift-on-shift-virt/component": hostRoutesComponent,
	}
}

// publishRoutes is the PublishRoutes phase. Non-fatal: the result goes on the
// HostRoutesReady condition and the install continues either way.
func (s *StepExecutor) publishRoutes(ctx context.Context, nc *crd.NestedCluster) error {
	if !nc.Spec.ExternalAccess.HostRoutes {
		return nil
	}
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent(hostRoutesComponent)
	if !useBastionLB(nc) {
		setCondition(nc, conditionHostRoutesReady, metav1.ConditionFalse, "NeedsBastionLoadBalancer",
			"host routes need the bastion load balancer (clusters created with 0.3.0 or later)")
		s.updateCRStatus(ctx, nc)
		return nil
	}
	consoleURL, apiURL, err := s.doPublishRoutes(ctx, nc)
	if err != nil {
		log.Error("publishing host routes failed (non-fatal)", "error", err.Error())
		setCondition(nc, conditionHostRoutesReady, metav1.ConditionFalse, "PublishFailed", err.Error())
		s.updateCRStatus(ctx, nc)
		return nil
	}
	nc.Status.HostConsoleURL, nc.Status.HostAPIURL = consoleURL, apiURL
	msg := fmt.Sprintf("console %s, API %s", consoleURL, apiURL)
	if nc.IsClone() {
		msg += " (the console, login and API certificate for these names finish rolling out within a few minutes)"
	}
	setCondition(nc, conditionHostRoutesReady, metav1.ConditionTrue, "Published", msg)
	s.updateCRStatus(ctx, nc)
	log.Info("API and console published through host routes", "console", consoleURL, "api", apiURL)
	return nil
}

func (s *StepExecutor) doPublishRoutes(ctx context.Context, nc *crd.NestedCluster) (string, string, error) {
	cluster := nc.Spec.ClusterName
	ea := nc.Spec.ExternalAccess
	if net.ParseIP(ea.APIIP) == nil || net.ParseIP(ea.IngressIP) == nil {
		return "", "", fmt.Errorf("external API/Ingress addresses are not set")
	}
	// A host Route makes its backend reachable under the host's *.apps
	// domain, so the backend must be an address an admin set aside for
	// nested clusters (an IP pool), never an arbitrary one from the spec.
	if err := s.checkHostRoutePool(ctx, nc); err != nil {
		return "", "", err
	}
	if nc.Status.KubeconfigRef == "" {
		return "", "", fmt.Errorf("no nested kubeconfig yet (PostInstall did not store one)")
	}
	opNs := operatorNamespace()
	labels := hostRoutesLabels(cluster)
	apiSvc, ingressSvc := hostRouteServices(cluster)
	if err := s.ensureExternalService(ctx, opNs, apiSvc, "api", 6443, ea.APIIP, labels); err != nil {
		return "", "", fmt.Errorf("API service: %w", err)
	}
	if err := s.ensureExternalService(ctx, opNs, ingressSvc, "https", 443, ea.IngressIP, labels); err != nil {
		return "", "", fmt.Errorf("ingress service: %w", err)
	}
	apiRoute, consoleRoute, oauthRoute := hostRouteNames(cluster)
	apiHost, err := s.client.EnsurePassthroughRoute(ctx, opNs, apiRoute, "", apiSvc, "api", labels)
	if err != nil {
		return "", "", fmt.Errorf("API route: %w", err)
	}
	consoleHost, err := s.client.EnsurePassthroughRoute(ctx, opNs, consoleRoute, "", ingressSvc, "https", labels)
	if err != nil {
		return "", "", fmt.Errorf("console route: %w", err)
	}
	oauthHost, err := s.client.EnsurePassthroughRoute(ctx, opNs, oauthRoute, "", ingressSvc, "https", labels)
	if err != nil {
		return "", "", fmt.Errorf("OAuth route: %w", err)
	}
	if apiHost == "" || consoleHost == "" || oauthHost == "" {
		return "", "", fmt.Errorf("the host router assigned no hostname to a route (is the name too long for a DNS label?)")
	}

	if err := s.ensureExternalAccessCerts(ctx, nc, []string{consoleHost, oauthHost}, apiHost); err != nil {
		return "", "", err
	}
	// Inside the nested cluster the host-side names would resolve to the
	// host's router, which nested pods (on their own isolated network) cannot
	// reach: the console and authentication operators then time out on their
	// route health checks. The bastion's nested DNS answers those names with
	// the nested ingress instead (split horizon), so re-push it now that the
	// router has assigned the names.
	if err := s.configureBastionDNS(ctx, nc); err != nil {
		return "", "", fmt.Errorf("adding the host-route names to the nested DNS: %w", err)
	}
	if err := s.ensureHostRoutesScripts(ctx, nc); err != nil {
		return "", "", err
	}
	logs, err := s.runHostRoutesJob(ctx, nc, []corev1.EnvVar{
		{Name: "CONSOLE_HOST", Value: consoleHost},
		{Name: "OAUTH_HOST", Value: oauthHost},
		{Name: "API_HOST", Value: apiHost},
		// Clones go Ready without waiting for the API certificate rollout.
		{Name: "WAIT_API_CERT", Value: fmt.Sprint(!nc.IsClone())},
	})
	if err != nil {
		return "", "", fmt.Errorf("configuring the nested cluster: %w", err)
	}
	if !strings.Contains(logs, "HOSTROUTES_OK") {
		return "", "", fmt.Errorf("nested configuration job ended without HOSTROUTES_OK")
	}
	return "https://" + consoleHost, "https://" + apiHost, nil
}

func (s *StepExecutor) checkHostRoutePool(ctx context.Context, nc *crd.NestedCluster) error {
	name := nc.Spec.ExternalAccess.IPPool
	if name == "" {
		return fmt.Errorf("host routes need the external addresses to come from an IP pool")
	}
	gvr := schema.GroupVersionResource{Group: crd.Group, Version: crd.Version, Resource: crd.IPPoolResource}
	var pool *crd.IPPool
	if obj, err := s.client.DynamicClient.Resource(gvr).Namespace(operatorNamespace()).Get(ctx, name, metav1.GetOptions{}); err == nil {
		p := crd.IPPool{Name: obj.GetName(), Namespace: obj.GetNamespace()}
		if raw, ok := obj.Object["spec"]; ok {
			b, _ := json.Marshal(raw)
			_ = json.Unmarshal(b, &p.Spec)
		}
		pool = &p
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading IP pool %q: %w", name, err)
	}
	return nc.Spec.CheckPool(pool)
}

// hostRouteDNSRecords returns the console and OAuth host-route names mapped to
// the nested ingress address, for the bastion's nested DNS. Empty unless the
// cluster uses host routes and the routes exist (the router has named them).
func (s *StepExecutor) hostRouteDNSRecords(ctx context.Context, nc *crd.NestedCluster, ingressIP string) map[string]string {
	if !nc.Spec.ExternalAccess.HostRoutes || ingressIP == "" {
		return nil
	}
	_, console, oauth := hostRouteNames(nc.Spec.ClusterName)
	out := map[string]string{}
	routes := s.client.DynamicClient.Resource(hostRouteGVR).Namespace(operatorNamespace())
	for _, name := range []string{console, oauth} {
		r, err := routes.Get(ctx, name, metav1.GetOptions{})
		if err != nil || r.GetLabels()["oct-shift-on-shift-virt/component"] != hostRoutesComponent {
			continue
		}
		if host, _, _ := unstructured.NestedString(r.Object, "spec", "host"); host != "" {
			out[host] = ingressIP
		}
	}
	return out
}

// ensureExternalService creates a selector-less Service and its EndpointSlice
// pointing at an address outside the cluster (the bastion's external IP).
func (s *StepExecutor) ensureExternalService(ctx context.Context, ns, name, portName string, port int32, ip string, labels map[string]string) error {
	svcs := s.client.Clientset.CoreV1().Services(ns)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: portName, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP}}},
	}
	if existing, err := svcs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		existing.Spec.Ports = svc.Spec.Ports
		existing.Labels = labels
		if _, err := svcs.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return err
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := svcs.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else {
		return err
	}

	sliceLabels := map[string]string{"kubernetes.io/service-name": name}
	for k, v := range labels {
		sliceLabels[k] = v
	}
	pn, proto := portName, corev1.ProtocolTCP
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: ns, Labels: sliceLabels},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &pn, Port: &port, Protocol: &proto}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{ip}}},
	}
	slices := s.client.Clientset.DiscoveryV1().EndpointSlices(ns)
	if existing, err := slices.Get(ctx, name, metav1.GetOptions{}); err == nil {
		existing.Ports, existing.Endpoints, existing.Labels = slice.Ports, slice.Endpoints, sliceLabels
		_, err = slices.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	_, err := slices.Create(ctx, slice, metav1.CreateOptions{})
	return err
}

// ensureExternalAccessCerts keeps the CA and leaf certificates in the
// <cluster>-external-access Secret. The CA is reused; leaf certificates are
// re-issued when the host names change.
func (s *StepExecutor) ensureExternalAccessCerts(ctx context.Context, nc *crd.NestedCluster, routeHosts []string, apiHost string) error {
	ns, name := nc.Spec.Namespace, externalAccessSecret(nc.Spec.ClusterName)
	secrets := s.client.Clientset.CoreV1().Secrets(ns)
	var existing map[string][]byte
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		existing = sec.Data
		if existing == nil {
			existing = map[string][]byte{}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	data, changed, err := externalAccessCertData(nc.Spec.ClusterName, routeHosts, apiHost, existing, time.Now())
	if err != nil {
		return fmt.Errorf("generating certificates: %w", err)
	}
	if !changed {
		return nil
	}
	if existing != nil {
		sec.Data = data
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
		return err
	}
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: hostRoutesLabels(nc.Spec.ClusterName)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}, metav1.CreateOptions{})
	return err
}

// externalAccessCertData returns the Secret data with a CA (reused from
// existing when present) and leaf certificates for the given names. changed
// is false when existing already matches.
func externalAccessCertData(cluster string, routeHosts []string, apiHost string, existing map[string][]byte, now time.Time) (map[string][]byte, bool, error) {
	hosts := append([]string{}, routeHosts...)
	sort.Strings(hosts)
	hostsKey := strings.Join(hosts, ",") + ";" + apiHost
	if existing != nil && string(existing["hosts"]) == hostsKey &&
		len(existing["ca.crt"]) > 0 && len(existing["routes.crt"]) > 0 && len(existing["api.crt"]) > 0 {
		return existing, false, nil
	}

	var caCert *x509.Certificate
	var caKey *ecdsa.PrivateKey
	var caPEM, caKeyPEM []byte
	if existing != nil && len(existing["ca.crt"]) > 0 && len(existing["ca.key"]) > 0 {
		c, k, err := parseCertAndKey(existing["ca.crt"], existing["ca.key"])
		if err == nil && c.IsCA && now.Before(c.NotAfter) && caPermitsExactly(c, allNames(routeHosts, apiHost)) {
			caCert, caKey, caPEM, caKeyPEM = c, k, existing["ca.crt"], existing["ca.key"]
		}
	}
	if caCert == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, false, err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          randomSerial(),
			Subject:               pkix.Name{CommonName: shortCN(cluster + " external access CA")},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.AddDate(10, 0, 0),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
			IsCA:                  true,
			MaxPathLenZero:        true,
			// People trust this CA in their browsers, so it must not be able
			// to vouch for anything but this cluster's own host-route names.
			PermittedDNSDomainsCritical: true,
			PermittedDNSDomains:         allNames(routeHosts, apiHost),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
		if err != nil {
			return nil, false, err
		}
		caCert, _ = x509.ParseCertificate(der)
		caKey = k
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, false, err
		}
		caKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}

	routesCrt, routesKey, err := issueLeaf(caCert, caKey, cluster+" external routes", routeHosts, now)
	if err != nil {
		return nil, false, err
	}
	apiCrt, apiKey, err := issueLeaf(caCert, caKey, cluster+" external api", []string{apiHost}, now)
	if err != nil {
		return nil, false, err
	}
	return map[string][]byte{
		"ca.crt": caPEM, "ca.key": caKeyPEM,
		"routes.crt": routesCrt, "routes.key": routesKey,
		"api.crt": apiCrt, "api.key": apiKey,
		"hosts": []byte(hostsKey),
	}, true, nil
}

// allNames is the sorted, de-duplicated set of host names a CA may sign.
func allNames(routeHosts []string, apiHost string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range append(append([]string{}, routeHosts...), apiHost) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// caPermitsExactly reports whether a CA is name-constrained to exactly names
// (CAs from 0.4.x had no constraints and are replaced on the next publish).
func caPermitsExactly(ca *x509.Certificate, names []string) bool {
	got := append([]string{}, ca.PermittedDNSDomains...)
	sort.Strings(got)
	return ca.PermittedDNSDomainsCritical && strings.Join(got, ",") == strings.Join(names, ",")
}

// issueLeaf signs a server certificate. The names go in subjectAltName only:
// the CN is limited to 64 characters, and route hostnames are often longer.
func issueLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, names []string, now time.Time) ([]byte, []byte, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: shortCN(cn)},
		DNSNames:     names,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 825),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func parseCertAndKey(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, fmt.Errorf("bad PEM")
	}
	c, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return c, k, nil
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// shortCN keeps a CommonName within the 64-character X.509 limit.
func shortCN(cn string) string {
	if len(cn) > 64 {
		return cn[:64]
	}
	return cn
}

// ensureHostRoutesScripts puts lib.sh (shared with the storage jobs) and the
// publish script into a ConfigMap in the cluster namespace.
func (s *StepExecutor) ensureHostRoutesScripts(ctx context.Context, nc *crd.NestedCluster) error {
	lib, err := kubevirtCSIFS.ReadFile("manifests/kubevirtcsi/lib.sh")
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hostRoutesScriptsCM(nc.Spec.ClusterName), Namespace: nc.Spec.Namespace,
			Labels: hostRoutesLabels(nc.Spec.ClusterName)},
		Data: map[string]string{"lib.sh": string(lib), "publish-routes.sh": publishRoutesScript},
	}
	cms := s.client.Clientset.CoreV1().ConfigMaps(nc.Spec.Namespace)
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// runHostRoutesJob runs publish-routes.sh in the cluster namespace with the
// nested kubeconfig and the certificates mounted (same runner shape as the
// storage jobs; the orchestrator pod cannot reach nested addresses).
func (s *StepExecutor) runHostRoutesJob(ctx context.Context, nc *crd.NestedCluster, extra []corev1.EnvVar) (string, error) {
	cluster, ns := nc.Spec.ClusterName, nc.Spec.Namespace
	jobName := cluster + "-publish-routes"
	labels := hostRoutesLabels(cluster)
	backoff := int32(0) // deterministic; a retry only doubles the wait on failure
	deadline := int64(hostRoutesJobTimeout / time.Second)
	env := append([]corev1.EnvVar{
		{Name: "CLUSTER", Value: cluster},
		{Name: "DOMAIN", Value: nc.FQDN()},
		{Name: "VIP", Value: s.resolveControlPlaneVIP(ctx, nc)},
	}, extra...)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cluster + "-ops",
					Containers: []corev1.Container{{
						Name:            "run",
						Image:           "quay.io/cjanisze/sos-installer-helper:4.22",
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/bin/bash", "/m/publish-routes.sh"},
						Env:             env,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "m", MountPath: "/m", ReadOnly: true},
							{Name: "nested", MountPath: "/nested", ReadOnly: true},
							{Name: "certs", MountPath: "/certs", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "m", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: hostRoutesScriptsCM(cluster)}}}},
						{Name: "nested", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: nc.Status.KubeconfigRef,
							Items:      []corev1.KeyToPath{{Key: "kubeconfig", Path: "kubeconfig"}}}}},
						// Leaf certificates and keys only; the CA key never leaves the Secret.
						{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: externalAccessSecret(cluster),
							Items: []corev1.KeyToPath{
								{Key: "routes.crt", Path: "routes.crt"}, {Key: "routes.key", Path: "routes.key"},
								{Key: "api.crt", Path: "api.crt"}, {Key: "api.key", Path: "api.key"},
							}}}},
					},
				},
			},
		},
	}
	if err := s.client.DeleteJob(ctx, ns, jobName); err != nil {
		return "", err
	}
	if err := s.client.CreateJob(ctx, job); err != nil {
		return "", err
	}
	werr := s.client.WaitForJobCompletion(ctx, ns, jobName, hostRoutesJobTimeout+time.Minute)
	logs, _ := s.client.JobPodLogs(ctx, ns, jobName)
	if werr != nil {
		if f := lastFatal(logs); f != "" {
			return logs, fmt.Errorf("%s", f)
		}
		return logs, werr
	}
	return logs, nil
}

// deleteHostRoutes removes the host-side objects (operator namespace). The
// cluster-namespace Secret and ConfigMap go with the namespace.
func (s *StepExecutor) deleteHostRoutes(ctx context.Context, cluster string) {
	opNs := operatorNamespace()
	sel := metav1.ListOptions{LabelSelector: "oct-shift-on-shift-virt/cluster=" + cluster +
		",oct-shift-on-shift-virt/component=" + hostRoutesComponent}
	routes := s.client.DynamicClient.Resource(hostRouteGVR).Namespace(opNs)
	if list, err := routes.List(ctx, sel); err == nil {
		for _, r := range list.Items {
			_ = routes.Delete(ctx, r.GetName(), metav1.DeleteOptions{})
		}
	}
	if list, err := s.client.Clientset.DiscoveryV1().EndpointSlices(opNs).List(ctx, sel); err == nil {
		for _, es := range list.Items {
			_ = s.client.Clientset.DiscoveryV1().EndpointSlices(opNs).Delete(ctx, es.Name, metav1.DeleteOptions{})
		}
	}
	if list, err := s.client.Clientset.CoreV1().Services(opNs).List(ctx, sel); err == nil {
		for _, svc := range list.Items {
			_ = s.client.Clientset.CoreV1().Services(opNs).Delete(ctx, svc.Name, metav1.DeleteOptions{})
		}
	}
}
