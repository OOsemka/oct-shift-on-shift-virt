package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/installer"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	case crd.PhaseGenerateISO:
		return s.generateISO(ctx, nc)
	case crd.PhaseMountISO:
		return s.mountISO(ctx, nc)
	case crd.PhasePowerOn:
		return s.powerOn(ctx, nc)
	case crd.PhaseWaitBootstrap:
		return s.waitBootstrap(ctx, nc)
	case crd.PhaseWaitInstall:
		return s.waitInstall(ctx, nc)
	case crd.PhasePostInstall:
		return s.postInstall(ctx, nc)
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

	if nc.Spec.SSHKey != "" {
		secretName := nc.Spec.ClusterName + "-ssh-key"
		if err := s.client.CreateSSHKeySecret(ctx, ns, secretName, nc.Spec.SSHKey); err != nil {
			return fmt.Errorf("creating SSH key secret: %w", err)
		}
		nc.Spec.SSHKeyRef = crd.SecretRef{Name: secretName}
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
		ip, err := s.client.WaitForVMIReady(ctx, clusterNs, node.Name, 5*time.Minute)
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

	if err := s.client.CreateOpaqueSecret(ctx, clusterNs, apiConfigName, map[string][]byte{
		"haproxy.cfg": []byte(apiConfig),
	}); err != nil {
		return fmt.Errorf("creating haproxy api config: %w", err)
	}
	if err := s.client.CreateOpaqueSecret(ctx, clusterNs, ingressConfigName, map[string][]byte{
		"haproxy.cfg": []byte(ingressConfig),
	}); err != nil {
		return fmt.Errorf("creating haproxy ingress config: %w", err)
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

	// --- API HAProxy (ports 6443 + 22623) ---
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
		})
	_, err := s.client.DynamicClient.Resource(deployGVR).Namespace(clusterNs).
		Create(ctx, apiDeploy, metav1.CreateOptions{})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("creating haproxy-api deployment: %w", err)
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
		})
	_, err = s.client.DynamicClient.Resource(deployGVR).Namespace(clusterNs).
		Create(ctx, ingressDeploy, metav1.CreateOptions{})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("creating haproxy-ingress deployment: %w", err)
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

	nc.Spec.Network.APIVIP = apiIP
	nc.Spec.Network.IngressVIP = ingressIP
	log.Info("haproxy LBs deployed",
		"api_vip", apiIP, "ingress_vip", ingressIP,
		"external_api_ip", nc.Spec.ExternalAccess.APIIP,
		"external_ingress_ip", nc.Spec.ExternalAccess.IngressIP)

	return nil
}

func (s *StepExecutor) buildHAProxyDeployment(name, namespace, saName, configSecret, nadAnnotation string, labels map[string]interface{}, ports []interface{}) *unstructured.Unstructured {
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
	var apiServers, mcsServers string
	for i, node := range nc.Status.Nodes {
		if node.IP == "" {
			continue
		}
		apiServers += fmt.Sprintf("  server master%d %s:6443 check\n", i, node.IP)
		mcsServers += fmt.Sprintf("  server master%d %s:22623 check\n", i, node.IP)
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

backend api-server-backend
  balance roundrobin
%s
frontend machine-config-server
  bind *:22623
  default_backend machine-config-server-backend

backend machine-config-server-backend
  balance roundrobin
%s`, apiServers, mcsServers)
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

func (s *StepExecutor) generateISO(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	crNs := nc.Namespace         // CR namespace (has internet access)
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
	}

	agentConfig := installer.GenerateAgentConfig(nc.Spec, hosts)
	if nc.Spec.AgentConfigOverrides != "" {
		agentConfig = nc.Spec.AgentConfigOverrides
	}

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
							},
							Command: []string{"/bin/bash", "-c"},
							Args: []string{`set -ex
mkdir -p /output/config
cp /secrets/install-config.yaml /output/config/
cp /secrets/agent-config.yaml /output/config/

echo "Generating agent ISO..."
openshift-install agent create image --dir=/output/config --log-level=info

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

	// Step 4: Create one block PVC per VM from the snapshot
	for _, node := range nc.Status.Nodes {
		isoPVC := node.Name + "-iso"
		log.WithNode(node.Name).Info("creating ISO PVC from snapshot", "pvc", isoPVC)
		if err := s.client.CreatePVCFromSnapshot(ctx, clusterNs, isoPVC, dstSnapName,
			nc.Spec.StorageClassName, "5Gi"); err != nil {
			return fmt.Errorf("creating ISO PVC for %s: %w", node.Name, err)
		}
	}

	// Step 5: Attach ISO PVCs to VMs
	log.Info("attaching ISO to VMs")
	for _, node := range nc.Status.Nodes {
		isoPVC := node.Name + "-iso"
		if err := s.client.AddISOToVM(ctx, clusterNs, node.Name, isoPVC); err != nil {
			return fmt.Errorf("attaching ISO to %s: %w", node.Name, err)
		}
		log.WithNode(node.Name).Info("ISO attached as cdrom with boot priority")
	}

	return nil
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

func (s *StepExecutor) waitBootstrap(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	clusterNs := nc.Spec.Namespace

	jobName := nc.Spec.ClusterName + "-wait-bootstrap"
	backoffLimit := int32(0)
	deadline := int64(3600)

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

	if err := s.client.CreateJob(ctx, waitJob); err != nil {
		return fmt.Errorf("creating wait-bootstrap job: %w", err)
	}

	log.Info("waiting for bootstrap to complete", "job", jobName, "namespace", clusterNs)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 60*time.Minute); err != nil {
		return fmt.Errorf("bootstrap wait failed: %w", err)
	}

	log.Info("bootstrap complete")
	return nil
}

func (s *StepExecutor) waitInstall(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("installer")
	clusterNs := nc.Spec.Namespace

	jobName := nc.Spec.ClusterName + "-wait-install"
	backoffLimit := int32(0)
	deadline := int64(3600)

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

	if err := s.client.CreateJob(ctx, waitJob); err != nil {
		return fmt.Errorf("creating wait-install job: %w", err)
	}

	log.Info("waiting for install to complete", "job", jobName, "namespace", clusterNs)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 60*time.Minute); err != nil {
		return fmt.Errorf("install wait failed: %w", err)
	}

	log.Info("installation complete")
	return nil
}

func (s *StepExecutor) postInstall(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("postinstall")
	clusterNs := nc.Spec.Namespace

	jobName := nc.Spec.ClusterName + "-wait-install"
	pods, err := s.client.Clientset.CoreV1().Pods(clusterNs).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for wait-install job %s", jobName)
	}

	logBytes, err := s.client.Clientset.CoreV1().Pods(clusterNs).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
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

	s.client.DeleteJob(ctx, crNs, nc.Spec.ClusterName+"-generate-iso")
	s.client.DeleteJob(ctx, clusterNs, nc.Spec.ClusterName+"-wait-bootstrap")
	s.client.DeleteJob(ctx, clusterNs, nc.Spec.ClusterName+"-wait-install")

	svcGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	s.client.DynamicClient.Resource(svcGVR).Namespace(crNs).Delete(ctx, nc.Spec.ClusterName+"-iso-server", metav1.DeleteOptions{})

	for _, name := range []string{
		nc.Spec.ClusterName + "-install-config",
		nc.Spec.ClusterName + "-iso-server-config",
	} {
		s.client.Clientset.CoreV1().Secrets(crNs).Delete(ctx, name, metav1.DeleteOptions{})
	}
	s.client.Clientset.CoreV1().PersistentVolumeClaims(crNs).Delete(ctx, nc.Spec.ClusterName+"-installer", metav1.DeleteOptions{})

	// Clean up snapshots
	snapName := nc.Spec.ClusterName + "-iso-snap"
	dstSnapName := nc.Spec.ClusterName + "-iso"
	s.client.DeleteVolumeSnapshot(ctx, crNs, snapName)
	s.client.DeleteVolumeSnapshot(ctx, clusterNs, dstSnapName)
	s.client.DeleteVolumeSnapshotContent(ctx, dstSnapName+"-content")

	s.client.DeleteCUDN(ctx, nc.Spec.ClusterName)
	s.client.DeleteNamespace(ctx, clusterNs)

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
