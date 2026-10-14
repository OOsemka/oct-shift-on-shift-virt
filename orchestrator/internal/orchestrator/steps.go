package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/installer"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type StepExecutor struct {
	client *k8s.Client
	logger *logging.Logger
}

func NewStepExecutor(client *k8s.Client, logger *logging.Logger) *StepExecutor {
	return &StepExecutor{client: client, logger: logger}
}

func (s *StepExecutor) updateCRStatus(ctx context.Context, nc *crd.NestedCluster) {
	gvr := schema.GroupVersionResource{Group: crd.Group, Version: crd.Version, Resource: crd.Resource}
	statusBytes, err := json.Marshal(nc.Status)
	if err != nil {
		s.logger.Error("failed to marshal status", "error", err.Error())
		return
	}
	var statusMap map[string]interface{}
	if err := json.Unmarshal(statusBytes, &statusMap); err != nil {
		s.logger.Error("failed to unmarshal status", "error", err.Error())
		return
	}
	existing, err := s.client.DynamicClient.Resource(gvr).Namespace(nc.Namespace).Get(ctx, nc.Name, metav1.GetOptions{})
	if err != nil {
		s.logger.Error("failed to get CR for status update", "error", err.Error(), "cluster", nc.Name)
		return
	}
	existing.Object["status"] = statusMap
	if _, err := s.client.DynamicClient.Resource(gvr).Namespace(nc.Namespace).UpdateStatus(ctx, existing, metav1.UpdateOptions{}); err != nil {
		s.logger.Error("failed to update CR status", "error", err.Error(), "cluster", nc.Name)
	}
}

func (s *StepExecutor) Execute(ctx context.Context, nc *crd.NestedCluster, phase crd.ClusterPhase) error {
	switch phase {
	case crd.PhaseCreateNamespace:
		return s.createNamespace(ctx, nc)
	case crd.PhaseCreateNetwork:
		return s.createNetwork(ctx, nc)
	case crd.PhaseCreateSecrets:
		return s.createSecrets(ctx, nc)
	case crd.PhaseCreateVMs:
		return s.createVMs(ctx, nc)
	case crd.PhaseCollectIPs:
		return s.collectIPs(ctx, nc)
	case crd.PhaseDeployHAProxy:
		return s.deployHAProxy(ctx, nc)
	case crd.PhaseDeployDNS:
		if err := s.deployDNS(ctx, nc); err != nil {
			return err
		}
		// Jumpbox alongside DNS: operators get in-cluster SSH from minute
		// one instead of hand-building a box mid-incident. Best-effort —
		// a missing jumpbox must never block the install.
		if jumpboxEnabled() {
			if err := s.deployJumpbox(ctx, nc); err != nil {
				s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("jumpbox").
					Error("jumpbox deploy failed (continuing)", "error", err.Error())
			}
		}
		return nil
	case crd.PhaseGenerateISO:
		return s.generateISO(ctx, nc)
	case crd.PhaseMountISO:
		return s.mountISO(ctx, nc)
	case crd.PhasePowerOn:
		return s.powerOn(ctx, nc)
	case crd.PhaseConfigureNodes:
		return s.configureNodes(ctx, nc)
	case crd.PhaseMCSRelay:
		return s.deployMCSRelay(ctx, nc)
	case crd.PhaseWaitBootstrap:
		return s.waitBootstrap(ctx, nc)
	case crd.PhaseWaitInstall:
		return s.waitInstall(ctx, nc)
	case crd.PhasePostInstall:
		return s.postInstall(ctx, nc)
	case crd.PhaseReady:
		// terminal state — never executed as a step. It is listed in
		// PhaseOrder (so phase lookups resolve), but running it through
		// Execute used to hit the default branch and mark a successfully
		// installed cluster Failed ("unknown phase: Ready").
		return nil
	default:
		return fmt.Errorf("unknown phase: %s", phase)
	}
}

func (s *StepExecutor) createNamespace(ctx context.Context, nc *crd.NestedCluster) error {
	labels := k8s.CUDNNamespaceLabels(nc.Spec.ClusterName)
	return s.client.CreateNamespace(ctx, nc.Spec.Namespace, labels)
}

func (s *StepExecutor) createNetwork(ctx context.Context, nc *crd.NestedCluster) error {
	return s.client.CreateCUDN(ctx, nc.Spec.ClusterName, nc.Spec.Namespace,
		nc.Spec.Network.CUDNSubnet, nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP)
}

func (s *StepExecutor) createSecrets(ctx context.Context, nc *crd.NestedCluster) error {
	ns := nc.Spec.Namespace
	crNs := "oct-shift-on-shift-virt"

	if nc.Spec.PullSecret != "" {
		secretName := nc.Spec.ClusterName + "-pull-secret"
		if err := s.client.CreatePullSecret(ctx, ns, secretName, nc.Spec.PullSecret); err != nil {
			return fmt.Errorf("creating pull secret: %w", err)
		}
		nc.Spec.PullSecretRef = crd.SecretRef{Name: secretName}
	} else {
		srcName := nc.Spec.PullSecretRef.Name
		if srcName == "" {
			srcName = "pull-secret"
		}
		dstName := nc.Spec.ClusterName + "-pull-secret"
		if err := s.client.CopySecret(ctx, crNs, srcName, ns, dstName); err != nil {
			return fmt.Errorf("copying pull secret to cluster namespace: %w", err)
		}
		nc.Spec.PullSecretRef = crd.SecretRef{Name: dstName}
	}

	// Registry credentials for our private helper images (sos-ssh-helper,
	// sos-installer-helper variants). Best-effort: installs work against
	// public registries without it.
	if err := s.client.CopySecret(ctx, crNs, "quay-pull-secret", ns, "quay-pull-secret"); err != nil &&
		!strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("copying quay-pull-secret into cluster namespace: %w", err)
	}

	sshSecretName := nc.Spec.ClusterName + "-ssh-key"
	if nc.Spec.SSHKey != "" {
		if err := s.client.CreateSSHKeySecret(ctx, ns, sshSecretName, nc.Spec.SSHKey); err != nil {
			return fmt.Errorf("creating SSH key secret: %w", err)
		}
		nc.Spec.SSHKeyRef = crd.SecretRef{Name: sshSecretName}
	}

	// Private half (ConfigureNodes + node-guard SSH into the nested VMs from
	// a Job inside the cluster namespace; the key must live here, not only
	// on the operator's laptop).
	switch {
	case nc.Spec.SSHPrivateKey != "":
		if err := s.client.UpdateSecretData(ctx, ns, sshSecretName, "ssh-privatekey", []byte(nc.Spec.SSHPrivateKey)); err != nil {
			return fmt.Errorf("storing SSH private key: %w", err)
		}
	case nc.Spec.SSHPrivateKeyRef.Name != "":
		val, err := s.client.GetSecretData(ctx, crNs, nc.Spec.SSHPrivateKeyRef.Name, "ssh-privatekey")
		if err != nil {
			return fmt.Errorf("reading sshPrivateKeyRef %s: %w", nc.Spec.SSHPrivateKeyRef.Name, err)
		}
		if err := s.client.UpdateSecretData(ctx, ns, sshSecretName, "ssh-privatekey", val); err != nil {
			return fmt.Errorf("storing SSH private key from ref: %w", err)
		}
	}

	return nil
}

func (s *StepExecutor) createVMs(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName)
	nodes := s.buildNodeList(nc)

	for i, node := range nodes {
		nodeLog := log.WithNode(node.Name)
		sizing := s.sizingForRole(nc, node.Role)
		mac := k8s.GenerateMAC(nc.Spec.ClusterName, i)

		_, err := s.client.CreateVM(ctx, k8s.VMCreateParams{
			Name:             node.Name,
			Namespace:        nc.Spec.Namespace,
			ClusterName:      nc.Spec.ClusterName,
			Role:             node.Role,
			CPUCores:         sizing.CPUCores,
			MemoryGi:         sizing.MemoryGi,
			DiskGi:           sizing.DiskGi,
			StorageClassName: nc.Spec.StorageClassName,
			MACAddress:       mac,
			UDNName:          nc.Spec.ClusterName,
		})
		if err != nil {
			return fmt.Errorf("creating VM %s: %w", node.Name, err)
		}

		nodeLog.Info("VM created", "mac", mac)

		nc.Status.Nodes = append(nc.Status.Nodes, crd.NodeStatus{
			Name:        node.Name,
			Role:        node.Role,
			VMStatus:    "Created",
			PowerState:  "Off",
			BootPhase:   "Unknown",
			AgentStatus: "Pending",
			MAC:         mac,
		})
	}

	return nil
}

func (s *StepExecutor) collectIPs(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("ip-collect")
	clusterNs := nc.Spec.Namespace

	log.Info("waiting for VMs to get IPs from cUDN DHCP")

	for i := range nc.Status.Nodes {
		node := &nc.Status.Nodes[i]
		// Blank-disk VMs take a while for SeaBIOS to fall through to a PXE
		// DISCOVER and get a lease snooped onto the VMI status (observed up
		// to ~8min under cluster load; 5min raced and false-failed once).
		ip, err := s.client.WaitForVMIReady(ctx, clusterNs, node.Name, 15*time.Minute)
		if err != nil {
			return fmt.Errorf("waiting for IP on %s: %w", node.Name, err)
		}
		node.IP = ip
		node.VMStatus = "Running"
		node.PowerState = "On"
		log.WithNode(node.Name).Info("VM got DHCP IP from cUDN", "ip", ip)
	}

	log.Info("all VMs have IPs, stopping VMs for ISO attachment")

	for i := range nc.Status.Nodes {
		node := &nc.Status.Nodes[i]
		if err := s.client.StopVM(ctx, clusterNs, node.Name); err != nil {
			return fmt.Errorf("stopping VM %s: %w", node.Name, err)
		}
		node.VMStatus = "Stopped"
		node.PowerState = "Off"
		log.WithNode(node.Name).Info("VM stopped")
	}

	// Wait for VMIs to terminate before attaching ISO disks
	time.Sleep(15 * time.Second)

	return nil
}

func (s *StepExecutor) deployHAProxy(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("haproxy")
	clusterNs := nc.Spec.Namespace

	// OCP installer requires apiVIPs != ingressVIPs, so we deploy two
	// HAProxy pods — each gets its own OVN-assigned cUDN IP.
	apiConfig := s.generateHAProxyConfigAPI(nc)
	ingressConfig := s.generateHAProxyConfigIngress(nc)
	apiConfigName := nc.Spec.ClusterName + "-haproxy-api-config"
	ingressConfigName := nc.Spec.ClusterName + "-haproxy-ingress-config"

	if err := s.ensureHAProxyConfig(ctx, clusterNs, apiConfigName, apiConfig); err != nil {
		return fmt.Errorf("ensuring haproxy api config: %w", err)
	}
	if err := s.ensureHAProxyConfig(ctx, clusterNs, ingressConfigName, ingressConfig); err != nil {
		return fmt.Errorf("ensuring haproxy ingress config: %w", err)
	}

	saName := nc.Spec.ClusterName + "-haproxy"
	crbName := "sos-haproxy-anyuid-" + nc.Spec.ClusterName
	ownerLabels := map[string]string{
		"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName,
	}

	if err := s.client.CreateServiceAccount(ctx, clusterNs, saName, ownerLabels); err != nil {
		return fmt.Errorf("creating haproxy service account: %w", err)
	}
	if err := s.client.CreateClusterRoleBinding(ctx, crbName, "system:openshift:scc:anyuid", saName, clusterNs, ownerLabels); err != nil {
		return fmt.Errorf("granting anyuid SCC to haproxy: %w", err)
	}

	nadName := nc.Spec.ExternalAccess.NADName
	nadSrcNs := clusterNs
	if parts := strings.SplitN(nadName, "/", 2); len(parts) == 2 {
		nadSrcNs = parts[0]
		nadName = parts[1]
	}
	if nadSrcNs != clusterNs {
		if err := s.client.CopyNAD(ctx, nadSrcNs, nadName, clusterNs); err != nil {
			return fmt.Errorf("copying NAD to cluster namespace: %w", err)
		}
	}

	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// --- API HAProxy (6443 + MCS 22623/22624 via relay, see generateHAProxyConfigAPI) ---
	apiDeployName := nc.Spec.ClusterName + "-haproxy-api"
	apiNADAnnotation := fmt.Sprintf(`[{"name":"%s","ips":["%s/24"]}]`, nadName, nc.Spec.ExternalAccess.APIIP)
	apiLabels := map[string]interface{}{
		"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
		"oct-shift-on-shift-virt/component": "haproxy-api",
	}
	apiDeploy := s.buildHAProxyDeployment(apiDeployName, clusterNs, saName, apiConfigName, apiNADAnnotation, apiLabels,
		[]interface{}{
			map[string]interface{}{"containerPort": int64(6443), "name": "api"},
			map[string]interface{}{"containerPort": int64(22623), "name": "mcs"},
			map[string]interface{}{"containerPort": int64(22624), "name": "mcs-auth"},
		}, sha16(apiConfig))
	err := s.applyHAProxyDeployment(ctx, clusterNs, deployGVR, apiDeploy)
	if err != nil {
		return fmt.Errorf("applying haproxy-api deployment: %w", err)
	}

	// --- Ingress HAProxy (ports 443 + 80) ---
	ingressDeployName := nc.Spec.ClusterName + "-haproxy-ingress"
	ingressNADAnnotation := fmt.Sprintf(`[{"name":"%s","ips":["%s/24"]}]`, nadName, nc.Spec.ExternalAccess.IngressIP)
	ingressLabels := map[string]interface{}{
		"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
		"oct-shift-on-shift-virt/component": "haproxy-ingress",
	}
	ingressDeploy := s.buildHAProxyDeployment(ingressDeployName, clusterNs, saName, ingressConfigName, ingressNADAnnotation, ingressLabels,
		[]interface{}{
			map[string]interface{}{"containerPort": int64(443), "name": "https"},
			map[string]interface{}{"containerPort": int64(80), "name": "http"},
		}, sha16(ingressConfig))
	if err := s.applyHAProxyDeployment(ctx, clusterNs, deployGVR, ingressDeploy); err != nil {
		return fmt.Errorf("applying haproxy-ingress deployment: %w", err)
	}

	// Wait for both pods to get cUDN IPs.
	log.Info("waiting for haproxy-api pod to get cUDN IP")
	apiIP, err := s.client.WaitForDeploymentCUDNIP(ctx, clusterNs, apiDeployName, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for haproxy-api cUDN IP: %w", err)
	}
	log.Info("waiting for haproxy-ingress pod to get cUDN IP")
	ingressIP, err := s.client.WaitForDeploymentCUDNIP(ctx, clusterNs, ingressDeployName, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for haproxy-ingress cUDN IP: %w", err)
	}

	// Persist the REAL serving IPs in STATUS only — spec keeps the
	// user-requested VIPs untouched (writing observed cUDN leases back into
	// spec poisoned every later consumer, incl. DNS record fallbacks and the
	// node-guard env, with stale/mis-parsed addresses).
	// carry new DHCP leases, and every consumer downstream (DNS records,
	// ConfigureNodes, guard, status) must see the current truth.
	if nc.Status.APIVIPCUDN != apiIP || nc.Status.IngressVIPCUDN != ingressIP {
		nc.Status.APIVIPCUDN = apiIP
		nc.Status.IngressVIPCUDN = ingressIP
		s.updateCRStatus(ctx, nc)
	}
	log.Info("haproxy LBs deployed",
		"api_vip", apiIP, "ingress_vip", ingressIP,
		"external_api_ip", nc.Spec.ExternalAccess.APIIP,
		"external_ingress_ip", nc.Spec.ExternalAccess.IngressIP)

	return nil
}

// ensureHAProxyConfig creates or updates a HAProxy config secret. On content
// drift it rolls the consuming haproxy pods: before Ready (the only time
// phases execute) no nested node depends on the VIP yet, DNS records are
// generated later from freshly-waited cUDN IPs, and the VLAN secondary IP is
// static — so a roll is safe here and a live process reload is not needed.
func (s *StepExecutor) ensureHAProxyConfig(ctx context.Context, namespace, name, cfg string) error {
	existing, err := s.client.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return s.client.CreateOpaqueSecret(ctx, namespace, name, map[string][]byte{"haproxy.cfg": []byte(cfg)})
	}
	if string(existing.Data["haproxy.cfg"]) == cfg {
		return nil
	}
	if err := s.client.UpdateSecretData(ctx, namespace, name, "haproxy.cfg", []byte(cfg)); err != nil {
		return err
	}
	// Roll itself happens via the config-sha pod-template annotation in
	// applyHAProxyDeployment (deployment-driven, no exec, no manual delete).
	_ = name
	return nil
}

// sha16 is a short stable config checksum for rollout annotations.
func sha16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// applyHAProxyDeployment creates the deployment, or — if it exists with a
// different config checksum — patches the pod-template annotation, which
// makes the Deployment roll its pods. This replaces live process reloads:
// before Ready no nested node depends on the VIPs yet, DNS records are
// generated from freshly-waited cUDN IPs afterwards, and the VLAN secondary
// address is statically annotated, so a roll is the safe, exec-free way to
// apply config changes (haproxy picks the new secret up on start).
func (s *StepExecutor) applyHAProxyDeployment(ctx context.Context, namespace string, gvr schema.GroupVersionResource, dep *unstructured.Unstructured) error {
	name, _, _ := unstructured.NestedString(dep.Object, "metadata", "name")
	resource := s.client.DynamicClient.Resource(gvr).Namespace(namespace)
	existing, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		_, cerr := resource.Create(ctx, dep, metav1.CreateOptions{})
		if cerr != nil && !strings.Contains(cerr.Error(), "already exists") {
			return cerr
		}
		return nil
	}
	want, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", "sos.octools.io/config-sha")
	have, _, _ := unstructured.NestedString(existing.Object, "spec", "template", "metadata", "annotations", "sos.octools.io/config-sha")
	if want != "" && want != have {
		unstructured.SetNestedField(existing.Object, want, "spec", "template", "metadata", "annotations", "sos.octools.io/config-sha")
		if _, uerr := resource.Update(ctx, existing, metav1.UpdateOptions{}); uerr != nil {
			return uerr
		}
		s.logger.WithComponent("haproxy").Info("haproxy deployment config-sha changed; rolling pods", "deployment", name, "from", have, "to", want)
	}
	return nil
}

func (s *StepExecutor) buildHAProxyDeployment(name, namespace, saName, configSecret, nadAnnotation string, labels map[string]interface{}, ports []interface{}, configSha string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels":    labels,
			},
			"spec": map[string]interface{}{
				"replicas": int64(1),
				"selector": map[string]interface{}{
					"matchLabels": labels,
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": labels,
						"annotations": map[string]interface{}{
							"k8s.v1.cni.cncf.io/networks": nadAnnotation,
							"sos.octools.io/config-sha":   configSha,
						},
					},
					"spec": map[string]interface{}{
						"serviceAccountName": saName,
						"securityContext": map[string]interface{}{
							"runAsUser":    int64(0),
							"runAsNonRoot": false,
						},
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "haproxy",
								"image": "docker.io/library/haproxy:2.9-alpine",
								"ports": ports,
								"volumeMounts": []interface{}{
									map[string]interface{}{
										"name":      "config",
										"mountPath": "/usr/local/etc/haproxy",
										"readOnly":  true,
									},
								},
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "config",
								"secret": map[string]interface{}{
									"secretName": configSecret,
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *StepExecutor) generateHAProxyConfigAPI(nc *crd.NestedCluster) string {
	var apiServers string
	for i, node := range nc.Status.Nodes {
		if node.IP == "" {
			continue
		}
		apiServers += fmt.Sprintf("  server master%d %s:6443 check\n", i, node.IP)
	}

	// MCS (22623/22624) is fronted here, but its servers can NEVER be the
	// master VMs directly: host-cluster nftables REJECTS tcp/22623 unless
	// both endpoints are pod-network (haproxy pod -> master VM fails, while
	// 6443/443 are open). The MCSRelay phase points these backends at a
	// relay VM (VM->VM traffic is unrestricted) and hot-reloads this config.
	// Until then the backends exist but are empty (haproxy boots fine).
	var mcsServers, mcsAuthServers string
	if relay := nc.Status.MCSRelayIP; relay != "" {
		mcsServers = fmt.Sprintf("  server relay %s:12262 check\n", relay)
		mcsAuthServers = fmt.Sprintf("  server relay %s:12264 check\n", relay)
	}
	return fmt.Sprintf(`global
  maxconn 4096

defaults
  mode tcp
  timeout connect 10s
  timeout client  1m
  timeout server  1m

frontend api-server
  bind *:6443
  default_backend api-server-backend

frontend api-int
  bind *:22623
  default_backend mcs-backend

frontend mcs-auth
  bind *:22624
  default_backend mcs-auth-backend

backend api-server-backend
  balance roundrobin
%s
backend mcs-backend
  balance roundrobin
%s
backend mcs-auth-backend
  balance roundrobin
%s`, apiServers, mcsServers, mcsAuthServers)
}

func (s *StepExecutor) generateHAProxyConfigIngress(nc *crd.NestedCluster) string {
	var httpsServers, httpServers string
	for i, node := range nc.Status.Nodes {
		if node.IP == "" {
			continue
		}
		httpsServers += fmt.Sprintf("  server master%d %s:443 check\n", i, node.IP)
		httpServers += fmt.Sprintf("  server master%d %s:80 check\n", i, node.IP)
	}

	return fmt.Sprintf(`global
  maxconn 4096

defaults
  mode tcp
  timeout connect 10s
  timeout client  1m
  timeout server  1m

frontend ingress-https
  bind *:443
  default_backend ingress-https-backend

backend ingress-https-backend
  balance roundrobin
%s
frontend ingress-http
  bind *:80
  default_backend ingress-http-backend

backend ingress-http-backend
  balance roundrobin
%s`, httpsServers, httpServers)
}

func (s *StepExecutor) deployDNS(ctx context.Context, nc *crd.NestedCluster) error {
	if dnsVMMode() {
		return s.deployDNSVM(ctx, nc)
	}
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("dns")
	targetNs := nc.Spec.Namespace

	apiVIP := nc.Status.APIVIPCUDN
	if apiVIP == "" {
		apiVIP = nc.Spec.Network.APIVIP
	}
	ingressVIP := nc.Status.IngressVIPCUDN
	if ingressVIP == "" {
		ingressVIP = nc.Spec.Network.IngressVIP
	}
	clusterName := nc.Spec.ClusterName
	baseDomain := nc.Spec.BaseDomain
	localZone := clusterName + "." + baseDomain

	// On resume (orchestrator restart between phases) status may be empty
	// while the HAProxy pods are already up — collect live cUDN IPs directly.
	// These DHCP-assigned pod IPs are the de-facto VIPs; the wizard-supplied
	// spec.network.apiVIP/ingressVIP are never actually bound to anything and
	// MUST NOT be served in DNS (a VM querying api.<zone> would hit a dead IP,
	// possibly one DHCP later hands to a node — silent split-brain).
	// NOTE (corrected 2026-10-14): the host nftables 22623/22624 rejects are
	// SYN-match rules in OUTPUT (host/pod-originated traffic only). VM->VIP
	// MCS traffic rides the OVN pipeline and never hits netfilter — proven by
	// master-0 -> haproxy cUDN :22623 == HTTP 200 with zero exceptions. So
	// the HAProxy pod lease IS a legal MCS front; keep it authoritative. The
	// relay VM (if present, with 22623 frontends) is only a fallback VIP.
	if nc.Status.APIVIPCUDN == "" || nc.Status.IngressVIPCUDN == "" {
		if nc.Status.APIVIPCUDN == "" {
			ip, err := s.client.WaitForDeploymentCUDNIP(ctx, targetNs, clusterName+"-haproxy-api", 2*time.Minute)
			if err == nil {
				nc.Status.APIVIPCUDN = ip
			}
		}
		if nc.Status.IngressVIPCUDN == "" {
			ip, err := s.client.WaitForDeploymentCUDNIP(ctx, targetNs, clusterName+"-haproxy-ingress", 2*time.Minute)
			if err == nil {
				nc.Status.IngressVIPCUDN = ip
			}
		}
	}
	if nc.Status.APIVIPCUDN != "" {
		apiVIP = nc.Status.APIVIPCUDN
	} else if nc.Status.MCSRelayIP != "" {
		apiVIP = nc.Status.MCSRelayIP
	}
	if nc.Status.IngressVIPCUDN != "" {
		ingressVIP = nc.Status.IngressVIPCUDN
	}

	if apiVIP == "" || ingressVIP == "" {
		return fmt.Errorf("VIPs not available: apiVIP=%q ingressVIP=%q (DeployHAProxy must run first)", apiVIP, ingressVIP)
	}
	nc.Status.APIVIPCUDN = apiVIP
	nc.Status.IngressVIPCUDN = ingressVIP

	dnsScript := s.generateDNSScript(apiVIP, ingressVIP, localZone)

	configMapName := clusterName + "-dns-script"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: targetNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   clusterName,
				"oct-shift-on-shift-virt/component": "dns",
			},
		},
		Data: map[string]string{
			"dns.py": dnsScript,
		},
	}
	_, err := s.client.Clientset.CoreV1().ConfigMaps(targetNs).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		// Update path: phase retries must pick up script fixes (e.g. EDNS-safe
		// replies) instead of silently keeping a stale broken script.
		if _, uerr := s.client.Clientset.CoreV1().ConfigMaps(targetNs).Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating dns configmap: %w", uerr)
		}
	} else if err != nil {
		return fmt.Errorf("creating dns configmap: %w", err)
	}

	// anyuid SCC for the DNS pod: the UBI python-311 image runs as UID 1001
	// and cannot bind :53 (installer hosts' stub resolver speaks port 53
	// only). capabilities.add NET_BIND_SERVICE is not an option — it fails
	// anyuid validation and admission silently falls back to restricted-v2.
	// Needs SA + ClusterRoleBinding (an `oc adm policy ... -n ns` style
	// namespaced RoleBinding does NOT work for SCCs) + explicit runAsUser: 0.
	dnsSA := clusterName + "-dns"
	ownerLabels := map[string]string{"oct-shift-on-shift-virt/cluster": clusterName}
	if err := s.client.CreateServiceAccount(ctx, targetNs, dnsSA, ownerLabels); err != nil {
		return fmt.Errorf("creating dns service account: %w", err)
	}
	if err := s.client.CreateClusterRoleBinding(ctx, "sos-dns-anyuid-"+clusterName, "system:openshift:scc:anyuid", dnsSA, targetNs, ownerLabels); err != nil {
		return fmt.Errorf("granting anyuid SCC to dns: %w", err)
	}

	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	deployName := clusterName + "-dns"
	selectorLabels := map[string]interface{}{
		"app": deployName,
	}
	dnsLabels := map[string]interface{}{
		"app":                               deployName,
		"oct-shift-on-shift-virt/cluster":   clusterName,
		"oct-shift-on-shift-virt/component": "dns",
	}

	replicas := int64(1)
	dnsDeploy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      deployName,
				"namespace": targetNs,
				"labels":    dnsLabels,
			},
			"spec": map[string]interface{}{
				"replicas": replicas,
				"selector": map[string]interface{}{
					"matchLabels": selectorLabels,
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": dnsLabels,
						"annotations": map[string]interface{}{
							// roll DNS pods whenever the served zone
							// changes (records embed live haproxy cUDN
							// leases); python reads its script only
							// at startup. Correct position: pod
							// TEMPLATE metadata, not the pod spec.
							"sos.octools.io/script-sha": sha16(dnsScript),
						},
					},
					"spec": map[string]interface{}{
						"serviceAccountName": dnsSA,
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "dns",
								"image": "registry.access.redhat.com/ubi9/python-311:latest",
								"command": []interface{}{
									"python3", "/scripts/dns.py",
								},
								// runAsUser: 0 is REQUIRED (bind :53). Do NOT add
								// capabilities — anyuid rejects capability
								// additions and the pod silently lands on
								// restricted-v2, still unable to bind :53.
								"securityContext": map[string]interface{}{
									"runAsUser": int64(0),
								},
								"ports": []interface{}{
									map[string]interface{}{
										"containerPort": int64(53),
										"name":          "dns-udp",
										"protocol":      "UDP",
									},
									map[string]interface{}{
										"containerPort": int64(53),
										"name":          "dns-tcp",
										"protocol":      "TCP",
									},
								},
								"volumeMounts": []interface{}{
									map[string]interface{}{
										"name":      "scripts",
										"mountPath": "/scripts",
										"readOnly":  true,
									},
								},
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "scripts",
								"configMap": map[string]interface{}{
									"name": configMapName,
								},
							},
						},
					},
				},
			},
		},
	}

	// create-or-update: a retried phase must apply the current template even
	// if an outdated one exists (e.g. pre-fix pod listening on 5353 only).
	existing, err := s.client.DynamicClient.Resource(deployGVR).Namespace(targetNs).Get(ctx, deployName, metav1.GetOptions{})
	if err == nil {
		dnsDeploy.SetResourceVersion(existing.GetResourceVersion())
		_, err = s.client.DynamicClient.Resource(deployGVR).Namespace(targetNs).Update(ctx, dnsDeploy, metav1.UpdateOptions{})
	} else if apierrors.IsNotFound(err) {
		_, err = s.client.DynamicClient.Resource(deployGVR).Namespace(targetNs).Create(ctx, dnsDeploy, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying dns deployment: %w", err)
	}

	svcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	svcName := clusterName + "-dns"
	dnsSvc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      svcName,
				"namespace": targetNs,
				"labels": map[string]interface{}{
					"oct-shift-on-shift-virt/cluster":   clusterName,
					"oct-shift-on-shift-virt/component": "dns",
				},
			},
			"spec": map[string]interface{}{
				"selector": selectorLabels,
				"ports": []interface{}{
					map[string]interface{}{
						"name":       "dns-udp",
						"protocol":   "UDP",
						"port":       int64(53),
						"targetPort": int64(53),
					},
					map[string]interface{}{
						"name":       "dns-tcp",
						"protocol":   "TCP",
						"port":       int64(53),
						"targetPort": int64(53),
					},
				},
			},
		},
	}

	_, err = s.client.DynamicClient.Resource(svcGVR).Namespace(targetNs).
		Create(ctx, dnsSvc, metav1.CreateOptions{})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("creating dns service: %w", err)
	}

	log.Info("waiting for DNS pod to become ready", "deployment", deployName)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		deploy, err := s.client.DynamicClient.Resource(deployGVR).Namespace(targetNs).
			Get(ctx, deployName, metav1.GetOptions{})
		if err == nil {
			readyReplicas, _, _ := unstructured.NestedInt64(deploy.Object, "status", "readyReplicas")
			if readyReplicas >= 1 {
				// Guard: SCC admission caches ClusterRoleBinding grants for
				// ~1min. A pod admitted before the grant propagates lands on
				// restricted-v2, the script logs "could not bind :53" but the
				// container still runs on :5353 — ready AND broken. Detect via
				// the scc annotation and force recreation.
				if scc, bad := s.podWithWrongSCC(ctx, targetNs, "app="+deployName, "anyuid"); bad != "" {
					_ = s.client.Clientset.CoreV1().Pods(targetNs).Delete(ctx, bad, metav1.DeleteOptions{})
					log.Info("DNS pod admitted on wrong SCC, recreating (SCC grant cache lag)", "pod", bad, "scc", scc)
					time.Sleep(5 * time.Second)
					continue
				}
				log.Info("DNS server ready",
					"service", svcName+"."+targetNs+".svc.cluster.local",
					"api_record", "api."+localZone+" -> "+apiVIP,
					"ingress_wildcard", "*.apps."+localZone+" -> "+ingressVIP)

				// Collect the DNS pod's cUDN IP: VMs must use it as their
				// resolver (ConfigureNodes phase pins it via nmcli).
				dnsIP, dnsErr := s.client.WaitForDeploymentCUDNIP(ctx, targetNs, deployName, 30*time.Second)
				if dnsErr != nil {
					log.Info("DNS pod cUDN IP not available; VMs will not have DNS in NMState config", "error", dnsErr.Error())
				} else {
					nc.Spec.Network.DNSIP = dnsIP
					nc.Status.DNSIPCUDN = dnsIP
					log.Info("DNS pod cUDN IP collected", "ip", dnsIP)
				}
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}

	return fmt.Errorf("timeout waiting for DNS deployment %s to become ready", deployName)
}

// podWithWrongSCC returns the name of the first Running+Ready pod matching
// selector whose admitted SCC differs from want ("" if all match).
func (s *StepExecutor) podWithWrongSCC(ctx context.Context, namespace, selector, want string) (string, string) {
	pods, err := s.client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", ""
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		scc := pod.Annotations["openshift.io/scc"]
		if scc != "" && scc != want {
			return scc, pod.Name
		}
	}
	return "", ""
}

func (s *StepExecutor) generateDNSScript(apiVIP, ingressVIP, localZone string) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
"""Authoritative DNS for a nested OCP cluster (SOS).
Listens on :53 AND :5353 (UDP+TCP). OCP installer hosts' stub resolver can
only talk to port 53 — a non-standard port in resolv.conf is silently ignored.
Requires root (anyuid SCC + runAsUser: 0 on the pod) to bind :53.
Replies must be byte-exact: glibc tolerates trailing bytes in the question
section, Go's resolver (assisted-agent) discards such replies as malformed.
"""

import socketserver, struct, sys

RECORDS = {
    "api.%[3]s.":     "%[1]s",
    "api-int.%[3]s.": "%[1]s",
}

WILDCARD_SUFFIX = ".apps.%[3]s."
WILDCARD_IP = "%[2]s"
LOCAL_ZONE = "%[3]s."
UPSTREAM = "172.30.0.10"
UPSTREAM_PORT = 53

def build_response(query, qname_lower, qtype):
    """Return a minimal DNS response bytes or None to forward."""
    # Only A queries get answers — but a non-A query (AAAA, SRV, ...) for a
    # name INSIDE our zone must still be answered NOERROR/empty. Silently
    # dropping it starves glibc's happy-eyeballs AAAA probe until its
    # timeout, which made every guest getent call take 20+ seconds.
    if qtype != 1:
        if qname_lower.endswith(LOCAL_ZONE):
            return _empty_answer(query)
        return None  # forward other types upstream

    ip = RECORDS.get(qname_lower)
    if ip is None and qname_lower.endswith(WILDCARD_SUFFIX):
        ip = WILDCARD_IP

    if ip is None:
        if qname_lower.endswith(LOCAL_ZONE):
            return _nxdomain(query)
        return None  # forward

    parts = ip.split(".")
    rdata = struct.pack("BBBB", *[int(p) for p in parts])
    # Build answer: pointer to qname in question (0xc00c), type A, class IN, TTL 60
    answer = b"\xc0\x0c" + struct.pack(">HHIH", 1, 1, 60, 4) + rdata

    header = query[:2]  # keep ID
    flags = 0x8400  # QR=1, AA=1, RCODE=0
    header += struct.pack(">HHHHH", flags, 1, 1, 0, 0)
    # question section: ONLY name+qtype+qclass — copying query[12:] would
    # carry EDNS OPT records while ARCOUNT says 0 -> Go resolver rejects it.
    _, qend = parse_qname(query, 12)
    q_section = query[12:qend + 4]
    return header + q_section + answer

def _nxdomain(query):
    header = query[:2] + struct.pack(">HHHHH", 0x8403, 1, 0, 0, 0)
    _, qend = parse_qname(query, 12)
    return header + query[12:qend + 4]

def forward(data):
    import socket
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(3)
    try:
        sock.sendto(data, (UPSTREAM, UPSTREAM_PORT))
        resp, _ = sock.recvfrom(4096)
        return resp
    except Exception:
        return _nxdomain(data)
    finally:
        sock.close()

def parse_qname(data, offset):
    labels = []
    while True:
        length = data[offset]
        if length == 0:
            offset += 1
            break
        offset += 1
        labels.append(data[offset:offset+length].decode())
        offset += length
    return ".".join(labels) + ".", offset

class DNSHandler(socketserver.BaseRequestHandler):
    def handle(self):
        if isinstance(self.request, tuple):
            data, sock = self.request
        else:
            data = self.request.recv(1024)
            sock = self.request

        if len(data) < 12:
            return

        try:
            qname, offset = parse_qname(data, 12)
        except (IndexError, ValueError):
            return
        qtype = struct.unpack(">H", data[offset:offset+2])[0]

        resp = build_response(data, qname.lower(), qtype)
        if resp is None:
            resp = forward(data)

        if isinstance(self.request, tuple):
            sock.sendto(resp, self.client_address)
        else:
            # TCP DNS: 2-byte big-endian length prefix before the message
            sock.sendall(struct.pack(">H", len(resp)) + resp)

class ThreadedUDPServer(socketserver.ThreadingMixIn, socketserver.UDPServer):
    allow_reuse_address = True

class ThreadedTCPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True

if __name__ == "__main__":
    import threading
    bound = []
    for port in (53, 5353):
        try:
            udp = ThreadedUDPServer(("0.0.0.0", port), DNSHandler)
            tcp = ThreadedTCPServer(("0.0.0.0", port), DNSHandler)
        except OSError as e:
            print(f"could not bind :{port}: {e}", flush=True)
            continue
        threading.Thread(target=udp.serve_forever, daemon=True).start()
        threading.Thread(target=tcp.serve_forever, daemon=True).start()
        bound.append(port)
    if not bound:
        sys.exit(1)  # e.g. UID 1001 without anyuid -> cannot bind :53
    print(f"DNS listening on {bound} (zone={LOCAL_ZONE})", flush=True)
    threading.Event().wait()
`, apiVIP, ingressVIP, localZone)
}

func (s *StepExecutor) generateISO(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	crNs := nc.Namespace           // CR namespace (has internet access)
	clusterNs := nc.Spec.Namespace // cluster namespace (cUDN, no internet)

	var hosts []installer.HostConfig
	for _, node := range nc.Status.Nodes {
		hosts = append(hosts, installer.HostConfig{
			Name: node.Name,
			Role: node.Role,
			MAC:  node.MAC,
			IP:   node.IP, // DHCP-assigned IP from CollectIPs phase
		})
	}

	pullSecretName := nc.Spec.PullSecretRef.Name
	if pullSecretName == "" {
		pullSecretName = nc.Spec.ClusterName + "-pull-secret"
	}
	pullSecret, err := s.client.GetSecretData(ctx, clusterNs, pullSecretName, ".dockerconfigjson")
	if err != nil {
		return fmt.Errorf("getting pull secret: %w", err)
	}

	sshKeyName := nc.Spec.SSHKeyRef.Name
	if sshKeyName == "" && nc.Spec.SSHKey != "" {
		sshKeyName = nc.Spec.ClusterName + "-ssh-key"
	}
	var sshKey []byte
	if sshKeyName != "" {
		sshKey, err = s.client.GetSecretData(ctx, clusterNs, sshKeyName, "ssh-publickey")
		if err != nil {
			return fmt.Errorf("getting SSH key: %w", err)
		}
	}

	installConfig := installer.GenerateInstallConfig(nc.Spec, hosts, string(pullSecret), string(sshKey))
	if nc.Spec.InstallConfigOverrides != "" {
		installConfig = nc.Spec.InstallConfigOverrides
		// The preview API uses literal "<PULL_SECRET>" and "<SSH_KEY>" placeholders
		// so secrets don't leak to the browser. Replace them with actual values.
		installConfig = strings.ReplaceAll(installConfig, "<PULL_SECRET>", installer.EscapeSingleQuotes(string(pullSecret)))
		installConfig = strings.ReplaceAll(installConfig, "<SSH_KEY>", string(sshKey))
	}

	// Always use the generated agent-config which includes DHCP-assigned
	// IPs, MACs, and interfaces discovered during CollectIPs. The preview
	// API cannot include these (IPs/MACs are unknown at preview time), so
	// agentConfigOverrides from the UI will be missing critical fields like
	// interfaces and rendezvousIP. We ignore the override for agent-config.
	agentConfig := installer.GenerateAgentConfig(nc.Spec, hosts)

	sanitizedInstallConfig := installConfig
	sanitizedInstallConfig = strings.ReplaceAll(sanitizedInstallConfig, string(pullSecret), "<PULL_SECRET>")
	if len(sshKey) > 0 {
		sanitizedInstallConfig = strings.ReplaceAll(sanitizedInstallConfig, string(sshKey), "<SSH_KEY>")
	}
	nc.Status.InstallConfig = sanitizedInstallConfig
	nc.Status.AgentConfig = agentConfig
	s.updateCRStatus(ctx, nc)
	log.Info("stored sanitized install-config and agent-config in CR status")

	configData := map[string][]byte{
		"install-config.yaml": []byte(installConfig),
		"agent-config.yaml":   []byte(agentConfig),
	}
	if err := s.client.CreateOpaqueSecret(ctx, crNs, nc.Spec.ClusterName+"-install-config", configData); err != nil {
		return fmt.Errorf("storing install configs: %w", err)
	}

	pvcName := nc.Spec.ClusterName + "-installer"
	if err := s.client.CreatePVC(ctx, crNs, pvcName, nc.Spec.StorageClassName, "5Gi"); err != nil {
		return fmt.Errorf("creating installer PVC: %w", err)
	}

	jobName := nc.Spec.ClusterName + "-generate-iso"
	backoffLimit := int32(0)
	deadline := int64(1800)

	isoJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: crNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "generate-iso",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "installer",
							Image:           "quay.io/cjanisze/sos-installer-helper:4.22",
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{Name: "HOME", Value: "/tmp"},
								// persistent nested DNS (VM-first); baked as a
								// nameserver= karg into the live ISO.
								{Name: "DNS_IP", Value: nc.Status.DNSIPCUDN},
							},
							Command: []string{"/bin/bash", "-c"},
							Args: []string{`set -ex
mkdir -p /output/config
cp /secrets/install-config.yaml /output/config/
cp /secrets/agent-config.yaml /output/config/

echo "Generating agent ISO..."
openshift-install agent create image --dir=/output/config --log-level=info

ISO=$(ls /output/config/*.iso | head -1)

# Bake the nested DNS server into the live ISO's grub entries. Pre-pivot
# guests get their resolver from cUDN DHCP otherwise — a stale lease (dead
# DNS pod IP) strands ignition fetch for hours with no in-guest recourse
# (nmcli is a no-op pre-pivot). nameserver= is a dracut karg honored by
# the RHCOS live initramfs, so it fixes the resolver at the exact stage
# where ignition fetch runs. agent-config v1alpha1 has no kernelArguments
# field, so post-process with coreos-installer instead. No root needed:
# extract the C9S AppStream rpm with rpm2archive+tar and run the binary.
if [ -n "${DNS_IP:-}" ]; then
  echo "Injecting nameserver=$DNS_IP karg into $ISO"
  mkdir -p /tmp/ci && cd /tmp/ci
  REL=https://mirror.stream.centos.org/9-stream/AppStream/x86_64/os/Packages
  if command -v coreos-installer >/dev/null 2>&1; then
    CI=coreos-installer
  else
    curl -sL -o ci.rpm "$REL/coreos-installer-0.25.0-1.el9.x86_64.rpm"
    rpm2archive ci.rpm && tar xzf ci.rpm.tgz
    CI=$(find . -type f -name coreos-installer | head -1)
  fi
  test -n "$CI" || { echo "coreos-installer unavailable"; exit 41; }
  "$CI" --version
  "$CI" iso kargs modify -a "nameserver=$DNS_IP" "$ISO"
  "$CI" iso kargs show "$ISO" | grep -c "nameserver=$DNS_IP"
  echo "karg injected"
fi

ls -la /output/config/
echo "ISO generation complete"
`},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "secrets", MountPath: "/secrets", ReadOnly: true},
								{Name: "output", MountPath: "/output"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "secrets",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: nc.Spec.ClusterName + "-install-config",
								},
							},
						},
						{
							Name: "output",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := s.client.CreateJob(ctx, isoJob); err != nil {
		return fmt.Errorf("creating ISO generation job: %w", err)
	}

	log.Info("waiting for ISO generation job to complete", "job", jobName, "namespace", crNs)
	if err := s.client.WaitForJobCompletion(ctx, crNs, jobName, 30*time.Minute); err != nil {
		return fmt.Errorf("ISO generation failed: %w", err)
	}

	log.Info("ISO generation complete, copying installer state to cluster namespace")

	stateData, err := s.readFileFromPVC(ctx, crNs, pvcName, "/data/config/.openshift_install_state.json")
	if err != nil {
		return fmt.Errorf("reading installer state from PVC: %w", err)
	}
	kubeconfigData, err := s.readFileFromPVC(ctx, crNs, pvcName, "/data/config/auth/kubeconfig")
	if err != nil {
		return fmt.Errorf("reading kubeconfig from PVC: %w", err)
	}
	kubeadminData, err := s.readFileFromPVC(ctx, crNs, pvcName, "/data/config/auth/kubeadmin-password")
	if err != nil {
		return fmt.Errorf("reading kubeadmin password from PVC: %w", err)
	}
	if err := s.client.CreateOpaqueSecret(ctx, clusterNs, nc.Spec.ClusterName+"-installer-state", map[string][]byte{
		".openshift_install_state.json": stateData,
		"kubeconfig":                    kubeconfigData,
		"kubeadmin-password":            kubeadminData,
	}); err != nil {
		return fmt.Errorf("creating installer state secret in cluster namespace: %w", err)
	}

	// ISO is distributed to VMs via VolumeSnapshot (mountISO phase),
	// not via an HTTP server. No ISO server deployment needed.
	log.Info("ISO generation and state extraction complete")

	return nil
}

func (s *StepExecutor) mountISO(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	crNs := nc.Namespace
	clusterNs := nc.Spec.Namespace
	snapshotClass := "odf-storagecluster-rbdplugin-snapclass"
	srcPVC := nc.Spec.ClusterName + "-installer"
	blockPVC := nc.Spec.ClusterName + "-iso-block"
	snapName := nc.Spec.ClusterName + "-iso-block-snap"
	dstSnapName := nc.Spec.ClusterName + "-iso"

	// Step 1: dd the ISO from the filesystem PVC to a block PVC (both in orchestrator ns)
	log.Info("converting ISO to block PVC in orchestrator namespace")
	if err := s.convertISOToBlock(ctx, crNs, srcPVC, blockPVC,
		nc.Spec.StorageClassName, nc.Spec.ClusterName); err != nil {
		return fmt.Errorf("converting ISO to block: %w", err)
	}

	// Step 2: Snapshot the block PVC
	log.Info("creating VolumeSnapshot of block PVC")
	if err := s.client.CreateVolumeSnapshot(ctx, crNs, snapName, blockPVC, snapshotClass); err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}

	log.Info("waiting for snapshot to be ready")
	_, driver, handle, err := s.client.WaitForVolumeSnapshot(ctx, crNs, snapName, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for snapshot: %w", err)
	}
	log.Info("snapshot ready", "driver", driver, "handle", handle)

	// Step 3: Clone snapshot to cluster namespace
	log.Info("cloning snapshot to cluster namespace")
	if err := s.client.CloneSnapshotToNamespace(ctx, clusterNs, dstSnapName, driver, handle, snapshotClass); err != nil {
		return fmt.Errorf("cloning snapshot cross-namespace: %w", err)
	}

	_, _, _, err = s.client.WaitForVolumeSnapshot(ctx, clusterNs, dstSnapName, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for destination snapshot: %w", err)
	}

	// Steps 4-5: give every VM an ISO PVC holding THIS ISO generation and
	// wire the cdrom. PVC sources are immutable and KubeVirt rejects volume
	// changes on running VMs, so: stop -> (recreate PVC from snapshot when
	// stale) -> ensure cdrom -> leave stopped; the PowerOn phase starts it.
	dstSnap, err := s.client.DynamicClient.Resource(volumeSnapshotGVR()).Namespace(clusterNs).
		Get(ctx, dstSnapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting destination snapshot for ISO generation tag: %w", err)
	}
	gen := string(dstSnap.GetUID())
	for _, node := range nc.Status.Nodes {
		isoPVC := node.Name + "-iso"
		nlog := log.WithNode(node.Name)
		existing, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(clusterNs).Get(ctx, isoPVC, metav1.GetOptions{})
		if err == nil && existing.Annotations["sos.octools.io/iso-generation"] == gen {
			nlog.Info("ISO PVC already current; skipping remount")
			if err := s.client.AddISOToVM(ctx, clusterNs, node.Name, isoPVC); err != nil {
				return fmt.Errorf("ensuring ISO cdrom on %s: %w", node.Name, err)
			}
			continue
		}
		nlog.Info("mounting ISO generation: stopping VM")
		if err := s.client.SetVMPower(ctx, clusterNs, node.Name, false, 3*time.Minute); err != nil {
			return fmt.Errorf("stopping %s for ISO mount: %w", node.Name, err)
		}
		if existing != nil {
			// BOOTINDEX TRAP (see SOS-E2E-STATUS): KubeVirt boots
			// rootdisk (bootindex 1) BEFORE the ISO cdrom (2). A node
			// being re-ISO'd carries a partial/failed install whose
			// stale bootloader silently wins the boot, so the freshly
			// patched ISO is never read and the retry re-boots the old
			// OS forever. Wipe the boot sectors of any rootdisk that
			// predates this ISO generation.
			if err := s.wipeRootdiskBoot(ctx, clusterNs, node.Name, nc.Spec.ClusterName); err != nil {
				return fmt.Errorf("wiping stale rootdisk on %s: %w", node.Name, err)
			}
		}
		if err == nil {
			if derr := s.client.Clientset.CoreV1().PersistentVolumeClaims(clusterNs).Delete(ctx, isoPVC, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				return fmt.Errorf("deleting stale ISO PVC %s: %w", isoPVC, derr)
			}
			for i := 0; i < 60; i++ {
				if _, gerr := s.client.Clientset.CoreV1().PersistentVolumeClaims(clusterNs).Get(ctx, isoPVC, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
					break
				}
				time.Sleep(2 * time.Second)
			}
		}
		if err := s.client.CreatePVCFromSnapshot(ctx, clusterNs, isoPVC, dstSnapName,
			nc.Spec.StorageClassName, "5Gi"); err != nil {
			return fmt.Errorf("creating ISO PVC for %s: %w", node.Name, err)
		}
		pvc, gerr := s.client.Clientset.CoreV1().PersistentVolumeClaims(clusterNs).Get(ctx, isoPVC, metav1.GetOptions{})
		if gerr == nil {
			if pvc.Annotations == nil {
				pvc.Annotations = map[string]string{}
			}
			pvc.Annotations["sos.octools.io/iso-generation"] = gen
			if _, uerr := s.client.Clientset.CoreV1().PersistentVolumeClaims(clusterNs).Update(ctx, pvc, metav1.UpdateOptions{}); uerr != nil {
				nlog.Info("could not tag ISO PVC generation (cosmetic)", "error", uerr.Error())
			}
		}
		if err := s.client.WaitForPVCBound(ctx, clusterNs, isoPVC, 3*time.Minute); err != nil {
			return fmt.Errorf("waiting for ISO PVC %s: %w", isoPVC, err)
		}
		if err := s.client.AddISOToVM(ctx, clusterNs, node.Name, isoPVC); err != nil {
			return fmt.Errorf("attaching ISO to %s: %w", node.Name, err)
		}
		nlog.Info("ISO generation mounted and cdrom wired; PowerOn will start the VM")
	}

	return nil
}

// volumeSnapshotGVR is the snapshot API coordinates used by mountISO.
func volumeSnapshotGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
}

// convertISOToBlock creates a block-mode PVC and copies the ISO file into it
// via dd. Runs in the orchestrator namespace to avoid permission issues.
func (s *StepExecutor) convertISOToBlock(ctx context.Context, namespace, srcPVC, dstPVC, storageClassName, clusterName string) error {
	if err := s.client.CreateBlockPVC(ctx, namespace, dstPVC, storageClassName, "5Gi"); err != nil {
		return fmt.Errorf("creating block PVC %s: %w", dstPVC, err)
	}

	jobName := dstPVC + "-dd"
	backoffLimit := int32(0)
	deadline := int64(600)

	ddJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   clusterName,
				"oct-shift-on-shift-virt/component": "iso-dd",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "dd",
							Image:   "docker.io/library/alpine:3.20",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{"dd if=/source/config/agent.x86_64.iso of=/dev/block bs=4M && sync"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "source", MountPath: "/source", ReadOnly: true},
							},
							VolumeDevices: []corev1.VolumeDevice{
								{Name: "target", DevicePath: "/dev/block"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "source",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: srcPVC,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: "target",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: dstPVC,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := s.client.CreateJob(ctx, ddJob); err != nil {
		return fmt.Errorf("creating dd job: %w", err)
	}

	if err := s.client.WaitForJobCompletion(ctx, namespace, jobName, 10*time.Minute); err != nil {
		return fmt.Errorf("dd job failed: %w", err)
	}

	s.client.DeleteJob(ctx, namespace, jobName)
	return nil
}

// wipeRootdiskBoot zeroes the first 100Mi of a node's rootdisk PVC so the
// BIOS falls through to the ISO cdrom (bootindex 2) instead of a stale
// bootloader left by a previous partial install. Called only on ISO
// re-generation (retry) paths — the very act of re-mounting an ISO asserts
// the intent to reinstall that node. No-op when the rootdisk PVC does not
// exist yet (first mount before DataVolumes).
func (s *StepExecutor) wipeRootdiskBoot(ctx context.Context, namespace, nodeName, clusterName string) error {
	log := s.logger.WithCluster(clusterName).WithComponent("installer")
	rootPVC := nodeName + "-rootdisk"
	if _, err := s.client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, rootPVC, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	log.Info("wiping stale rootdisk boot sectors before ISO re-mount", "node", nodeName, "pvc", rootPVC)

	s.client.DeleteJob(ctx, namespace, nodeName+"-rootdisk-wipe")
	jobName := nodeName + "-rootdisk-wipe"
	backoffLimit := int32(0)
	deadline := int64(300)
	wipeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   clusterName,
				"oct-shift-on-shift-virt/component": "rootdisk-wipe",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "wipe",
						Image:   "docker.io/library/alpine:3.20",
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{"dd if=/dev/zero of=/dev/rootdisk bs=1M count=100 conv=fsync && sync"},
						VolumeDevices: []corev1.VolumeDevice{
							{Name: "rootdisk", DevicePath: "/dev/rootdisk"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "rootdisk",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: rootPVC},
						},
					}},
				},
			},
		},
	}
	if err := s.client.CreateJob(ctx, wipeJob); err != nil {
		return fmt.Errorf("creating rootdisk wipe job: %w", err)
	}
	if err := s.client.WaitForJobCompletion(ctx, namespace, jobName, 5*time.Minute); err != nil {
		return fmt.Errorf("rootdisk wipe job failed: %w", err)
	}
	s.client.DeleteJob(ctx, namespace, jobName)
	return nil
}

func (s *StepExecutor) powerOn(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("power")
	clusterNs := nc.Spec.Namespace

	for i := range nc.Status.Nodes {
		node := &nc.Status.Nodes[i]
		if err := s.client.StartVM(ctx, clusterNs, node.Name); err != nil {
			return fmt.Errorf("starting VM %s: %w", node.Name, err)
		}
		node.PowerState = "On"
		node.VMStatus = "Running"
		log.WithNode(node.Name).Info("VM started via KubeVirt API")
	}

	log.Info("all VMs started, waiting for VMIs to be ready")
	for _, node := range nc.Status.Nodes {
		if _, err := s.client.WaitForVMIReady(ctx, clusterNs, node.Name, 10*time.Minute); err != nil {
			return fmt.Errorf("waiting for VMI %s: %w", node.Name, err)
		}
	}

	return nil
}

// detectRendezvousNode polls all node IPs for port 8090 (assisted-service)
// and returns the IP of the node that responds. The agent-based installer runs
// the assisted-service only on the rendezvous node. On cUDN with DHCP, the
// rendezvousIP from agent-config may not match the actual rendezvous because
// OVN-K DHCP can reassign IPs and the agent's set-node-zero.sh has only a
// 30-second timeout for IP matching.
func (s *StepExecutor) detectRendezvousNode(ctx context.Context, nc *crd.NestedCluster) (string, string) {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("rendezvous-detect")
	configuredRendezvous := ""
	if len(nc.Status.Nodes) > 0 {
		configuredRendezvous = nc.Status.Nodes[0].IP
	}

	log.Info("polling node IPs for port 8090 (assisted-service)",
		"configured_rendezvous", configuredRendezvous,
		"node_count", len(nc.Status.Nodes))

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			log.Info("context cancelled during rendezvous detection")
			return configuredRendezvous, ""
		}

		for _, node := range nc.Status.Nodes {
			if node.IP == "" {
				continue
			}
			conn, err := net.DialTimeout("tcp", node.IP+":8090", 3*time.Second)
			if err == nil {
				conn.Close()
				if node.IP != configuredRendezvous {
					log.Info("actual rendezvous differs from configured",
						"actual_rendezvous_ip", node.IP,
						"actual_rendezvous_node", node.Name,
						"configured_rendezvous_ip", configuredRendezvous)
				} else {
					log.Info("rendezvous confirmed",
						"ip", node.IP, "node", node.Name)
				}
				return node.IP, node.Name
			}
		}

		time.Sleep(10 * time.Second)
	}

	log.Info("rendezvous detection timed out, falling back to configured",
		"configured_rendezvous", configuredRendezvous)
	return configuredRendezvous, ""
}

func (s *StepExecutor) waitBootstrap(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	clusterNs := nc.Spec.Namespace

	// Rendezvous discovery: ConfigureNodes probes :8090 from INSIDE the
	// cluster namespace; the orchestrator pod cannot cross into the UDN, so
	// detectRendezvousNode from here never works — only fall back to it when
	// ConfigureNodes recorded nothing.
	if nc.Status.RendezvousIP == "" {
		actualIP, actualNode := s.detectRendezvousNode(ctx, nc)
		if actualNode != "" {
			log.Info("detected rendezvous node", "ip", actualIP, "node", actualNode)
		}
	} else {
		log.Info("rendezvous already recorded by ConfigureNodes", "ip", nc.Status.RendezvousIP, "node", nc.Status.RendezvousNode)
	}

	jobName := nc.Spec.ClusterName + "-wait-bootstrap"
	backoffLimit := int32(0)
	deadline := int64(10800)

	waitJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: clusterNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "wait-bootstrap",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "wait",
							Image:           "quay.io/cjanisze/sos-installer-helper:4.22",
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/bin/bash", "-c"},
							Args: []string{`set -ex
mkdir -p /tmp/work/config/auth
cp /state/.openshift_install_state.json /tmp/work/config/
cp /state/kubeconfig /tmp/work/config/auth/kubeconfig
cp /state/kubeadmin-password /tmp/work/config/auth/kubeadmin-password

openshift-install agent wait-for bootstrap-complete --dir=/tmp/work/config --log-level=info
echo "Bootstrap complete"
`},
							Env: []corev1.EnvVar{
								{Name: "HOME", Value: "/tmp"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "state", MountPath: "/state", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "state",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: nc.Spec.ClusterName + "-installer-state",
								},
							},
						},
					},
				},
			},
		},
	}

	// CRITICAL: the installer inside this job resolves the NESTED api name
	// (api.<cluster>.<base>) which only the custom DNS pod (primary UDN) knows.
	// With host cluster DNS every API check fails name resolution and the job
	// logs "cluster bootstrap did not complete" forever, even after the cluster
	// is fully bootstrapped (proven live: r1-r3 all false-failed this way; a
	// clone with dnsPolicy None + nameserver <dns pod IP> completed in <90s).
	applyUDNDNS(waitJob, nc)

	if err := s.client.CreateJob(ctx, waitJob); err != nil {
		return fmt.Errorf("creating wait-bootstrap job: %w", err)
	}

	log.Info("waiting for bootstrap to complete", "job", jobName, "namespace", clusterNs)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 3*time.Hour); err != nil {
		// The wait job is disposable observability: install state lives in the
		// installer-state secret. If it dies (deadline, node evict, transient
		// API unreachable) recreate it ONCE before failing the cluster —
		// proven live when r1 false-failed and r4 finished in <90s.
		if logs, lerr := s.client.JobPodLogs(ctx, clusterNs, jobName); lerr == nil {
			tail := logs
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			log.Error("bootstrap wait attempt 1 failed; recreating once", "job", jobName, "logsTail", tail)
		} else {
			log.Error("bootstrap wait attempt 1 failed; recreating once", "job", jobName, "error", err.Error())
		}
		_ = s.client.DeleteJob(ctx, clusterNs, jobName)
		retryName := jobName + "-r2"
		waitJob.Name = retryName
		waitJob.Labels["oct-shift-on-shift-virt/retry"] = "1"
		if err := s.client.CreateJob(ctx, waitJob); err != nil {
			return fmt.Errorf("recreating wait-bootstrap job: %w", err)
		}
		if err := s.client.WaitForJobCompletion(ctx, clusterNs, retryName, 2*time.Hour); err != nil {
			return fmt.Errorf("bootstrap wait failed after retry: %w", err)
		}
	}

	log.Info("bootstrap complete")
	return nil
}

// applyUDNDNS pins a job's pod to the cluster-namespace custom DNS server
// (cUDN IP recorded by deployDNS) and searches the nested base domain.
func applyUDNDNS(job *batchv1.Job, nc *crd.NestedCluster) {
	if nc.Status.DNSIPCUDN == "" {
		return
	}
	sp := &job.Spec.Template.Spec
	sp.DNSPolicy = corev1.DNSNone
	sp.DNSConfig = &corev1.PodDNSConfig{
		Nameservers: []string{nc.Status.DNSIPCUDN},
		Searches:    []string{nc.FQDN()},
	}
}

func (s *StepExecutor) waitInstall(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	clusterNs := nc.Spec.Namespace

	jobName := nc.Spec.ClusterName + "-wait-install"
	backoffLimit := int32(0)
	deadline := int64(10800)

	waitJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: clusterNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "wait-install",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "wait",
							Image:           "quay.io/cjanisze/sos-installer-helper:4.22",
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/bin/bash", "-c"},
							Args: []string{`set -ex
mkdir -p /tmp/work/config/auth
cp /state/.openshift_install_state.json /tmp/work/config/
cp /state/kubeconfig /tmp/work/config/auth/kubeconfig
cp /state/kubeadmin-password /tmp/work/config/auth/kubeadmin-password

openshift-install agent wait-for install-complete --dir=/tmp/work/config --log-level=info

echo "=== KUBECONFIG ==="
cat /tmp/work/config/auth/kubeconfig
echo "=== KUBEADMIN ==="
cat /tmp/work/config/auth/kubeadmin-password
echo "=== DONE ==="
`},
							Env: []corev1.EnvVar{
								{Name: "HOME", Value: "/tmp"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "state", MountPath: "/state", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "state",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: nc.Spec.ClusterName + "-installer-state",
								},
							},
						},
					},
				},
			},
		},
	}

	// See waitBootstrap: same host-DNS blindness applies to install-complete.
	applyUDNDNS(waitJob, nc)

	if err := s.client.CreateJob(ctx, waitJob); err != nil {
		return fmt.Errorf("creating wait-install job: %w", err)
	}

	log.Info("waiting for install to complete", "job", jobName, "namespace", clusterNs)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 3*time.Hour); err != nil {
		log.Error("install wait attempt 1 failed; recreating once", "job", jobName, "error", err.Error())
		_ = s.client.DeleteJob(ctx, clusterNs, jobName)
		retryName := jobName + "-r2"
		waitJob.Name = retryName
		waitJob.Labels["oct-shift-on-shift-virt/retry"] = "1"
		if err := s.client.CreateJob(ctx, waitJob); err != nil {
			return fmt.Errorf("recreating wait-install job: %w", err)
		}
		if err := s.client.WaitForJobCompletion(ctx, clusterNs, retryName, 2*time.Hour); err != nil {
			return fmt.Errorf("install wait failed after retry: %w", err)
		}
	}

	log.Info("installation complete")
	return nil
}

func (s *StepExecutor) postInstall(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("postinstall")
	clusterNs := nc.Spec.Namespace

	labelSelector := "oct-shift-on-shift-virt/cluster=" + nc.Spec.ClusterName + ",oct-shift-on-shift-virt/component=wait-install"
	pods, err := s.client.Clientset.CoreV1().Pods(clusterNs).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for wait-install (selector %s)", labelSelector)
	}
	// Newest attempt wins (a retried job -r2 carries the authoritative output).
	newest := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}

	logBytes, err := s.client.Clientset.CoreV1().Pods(clusterNs).GetLogs(newest.Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("getting wait-install job logs: %w", err)
	}

	logStr := string(logBytes)

	var kubeconfigData, kubeadminData string
	if idx := strings.Index(logStr, "=== KUBECONFIG ==="); idx >= 0 {
		after := logStr[idx+len("=== KUBECONFIG ==="):]
		if end := strings.Index(after, "=== KUBEADMIN ==="); end >= 0 {
			kubeconfigData = strings.TrimSpace(after[:end])
		}
	}
	if idx := strings.Index(logStr, "=== KUBEADMIN ==="); idx >= 0 {
		after := logStr[idx+len("=== KUBEADMIN ==="):]
		if end := strings.Index(after, "=== DONE ==="); end >= 0 {
			kubeadminData = strings.TrimSpace(after[:end])
		}
	}

	if kubeconfigData != "" {
		secretName := nc.Spec.ClusterName + "-kubeconfig"
		if err := s.client.CreateOpaqueSecret(ctx, clusterNs, secretName, map[string][]byte{
			"kubeconfig": []byte(kubeconfigData),
		}); err != nil {
			return fmt.Errorf("creating kubeconfig secret: %w", err)
		}
		nc.Status.KubeconfigRef = secretName
	}

	if kubeadminData != "" {
		secretName := nc.Spec.ClusterName + "-kubeadmin"
		if err := s.client.CreateOpaqueSecret(ctx, clusterNs, secretName, map[string][]byte{
			"password": []byte(kubeadminData),
		}); err != nil {
			return fmt.Errorf("creating kubeadmin secret: %w", err)
		}
		nc.Status.KubeadminRef = secretName
	}

	// Eject the agent ISO from every node VM template. Left attached
	// (bootOrder 2 cdrom), any future crash-reboot falls back into the
	// discovery shell — the "installation cannot proceed" console fossil —
	// instead of the installed OS. Invisible to the running cluster.
	for _, n := range nc.Status.Nodes {
		if n.Name == "" {
			continue
		}
		if err := s.client.RemoveISOFromVM(ctx, clusterNs, n.Name); err != nil {
			log.Info("ISO eject skipped", "vm", n.Name, "error", err.Error())
		} else {
			log.Info("agent ISO ejected from VM template", "vm", n.Name)
		}
	}

	// Host-cluster exposure (Services/Endpoints/passthrough Routes).
	// Non-fatal: the nested cluster is healthy regardless; exposure can be
	// refreshed by re-running PostInstall.
	if err := s.exposeCluster(ctx, nc); err != nil {
		log.Info("exposure degraded (cluster still healthy)", "error", err.Error())
	}

	nc.Status.APIEndpoint = nc.APIEndpoint()
	nc.Status.ConsoleURL = nc.ConsoleEndpoint()
	nc.Status.IngressDomain = nc.IngressWildcard()

	log.Info("post-install complete",
		"api", nc.Status.APIEndpoint,
		"console", nc.Status.ConsoleURL)

	return nil
}

func (s *StepExecutor) readFileFromPVC(ctx context.Context, namespace, pvcName, filePath string) ([]byte, error) {
	jobName := pvcName + "-read-" + fmt.Sprintf("%d", time.Now().Unix())
	backoffLimit := int32(0)
	deadline := int64(120)

	readJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "reader",
							Image:   "docker.io/library/alpine:3.20",
							Command: []string{"cat", filePath},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := s.client.CreateJob(ctx, readJob); err != nil {
		return nil, err
	}
	defer s.client.DeleteJob(ctx, namespace, jobName)

	if err := s.client.WaitForJobCompletion(ctx, namespace, jobName, 2*time.Minute); err != nil {
		return nil, err
	}

	pods, err := s.client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s", jobName)
	}

	logs, err := s.client.Clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("getting logs: %w", err)
	}
	return logs, nil
}

func (s *StepExecutor) DeleteCluster(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName)
	crNs := nc.Namespace
	clusterNs := nc.Spec.Namespace
	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	for _, node := range nc.Status.Nodes {
		if err := s.client.DeleteVM(ctx, clusterNs, node.Name); err != nil {
			log.Error("failed to delete VM", "node", node.Name, "error", err.Error())
		}
	}

	s.client.DynamicClient.Resource(deployGVR).Namespace(clusterNs).Delete(ctx, nc.Spec.ClusterName+"-haproxy", metav1.DeleteOptions{})
	s.client.Clientset.CoreV1().ServiceAccounts(clusterNs).Delete(ctx, nc.Spec.ClusterName+"-haproxy", metav1.DeleteOptions{})
	s.client.DeleteClusterRoleBinding(ctx, "sos-haproxy-anyuid-"+nc.Spec.ClusterName)

	s.client.DynamicClient.Resource(deployGVR).Namespace(crNs).Delete(ctx, nc.Spec.ClusterName+"-iso-server", metav1.DeleteOptions{})

	// Clean up DNS resources (deployed in the target namespace alongside VMs)
	s.client.DynamicClient.Resource(deployGVR).Namespace(clusterNs).Delete(ctx, nc.Spec.ClusterName+"-dns", metav1.DeleteOptions{})
	svcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	s.client.DynamicClient.Resource(svcGVR).Namespace(clusterNs).Delete(ctx, nc.Spec.ClusterName+"-dns", metav1.DeleteOptions{})
	s.client.Clientset.CoreV1().ConfigMaps(clusterNs).Delete(ctx, nc.Spec.ClusterName+"-dns-script", metav1.DeleteOptions{})

	s.client.DeleteJob(ctx, crNs, nc.Spec.ClusterName+"-generate-iso")
	s.client.DeleteJob(ctx, clusterNs, nc.Spec.ClusterName+"-wait-bootstrap")
	s.client.DeleteJob(ctx, clusterNs, nc.Spec.ClusterName+"-wait-install")

	s.client.DynamicClient.Resource(svcGVR).Namespace(crNs).Delete(ctx, nc.Spec.ClusterName+"-iso-server", metav1.DeleteOptions{})

	for _, name := range []string{
		nc.Spec.ClusterName + "-install-config",
		nc.Spec.ClusterName + "-iso-server-config",
	} {
		s.client.Clientset.CoreV1().Secrets(crNs).Delete(ctx, name, metav1.DeleteOptions{})
	}
	s.client.Clientset.CoreV1().PersistentVolumeClaims(crNs).Delete(ctx, nc.Spec.ClusterName+"-installer", metav1.DeleteOptions{})

	// Clean up snapshots (block-source snapshot lives in the operator ns and
	// is named -iso-block-snap; the old -iso-snap name never existed)
	snapName := nc.Spec.ClusterName + "-iso-block-snap"
	dstSnapName := nc.Spec.ClusterName + "-iso"
	s.client.DeleteVolumeSnapshot(ctx, crNs, snapName)
	s.client.DeleteVolumeSnapshot(ctx, clusterNs, dstSnapName)
	s.client.DeleteVolumeSnapshotContent(ctx, dstSnapName+"-content")
	s.client.Clientset.CoreV1().PersistentVolumeClaims(crNs).Delete(ctx, nc.Spec.ClusterName+"-iso-block", metav1.DeleteOptions{})

	s.client.DeleteCUDN(ctx, nc.Spec.ClusterName)
	if err := s.client.DeleteNamespaceForce(ctx, clusterNs, 3*time.Minute); err != nil {
		return fmt.Errorf("deleting cluster namespace: %w", err)
	}

	log.Info("cluster resources deleted")
	return nil
}

func (s *StepExecutor) buildNodeList(nc *crd.NestedCluster) []crd.NodeNameEntry {
	if len(nc.Spec.NodeNames) > 0 {
		return nc.Spec.NodeNames
	}

	var nodes []crd.NodeNameEntry
	for i := 0; i < nc.Spec.Masters.Count; i++ {
		nodes = append(nodes, crd.NodeNameEntry{
			Name: fmt.Sprintf("%s-master-%d", nc.Spec.ClusterName, i),
			Role: "master",
		})
	}
	for i := 0; i < nc.Spec.Workers.Count; i++ {
		nodes = append(nodes, crd.NodeNameEntry{
			Name: fmt.Sprintf("%s-worker-%d", nc.Spec.ClusterName, i),
			Role: "worker",
		})
	}
	return nodes
}

func (s *StepExecutor) sizingForRole(nc *crd.NestedCluster, role string) crd.NodeSizing {
	switch role {
	case "master":
		return nc.Spec.Masters
	case "worker":
		return nc.Spec.Workers
	case "infra":
		return nc.Spec.Infra
	default:
		return nc.Spec.Workers
	}
}

func toInterfaceMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}
