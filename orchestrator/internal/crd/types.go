package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Group    = "octools.io"
	Version  = "v1alpha1"
	Resource = "nestedclusters"
	Kind     = "NestedCluster"
)

type ClusterPhase string

const (
	PhaseEmpty           ClusterPhase = ""
	PhaseCreateNamespace ClusterPhase = "CreateNamespace"
	PhaseCreateNetwork   ClusterPhase = "CreateNetwork"
	PhaseCreateSecrets   ClusterPhase = "CreateSecrets"
	PhaseCreateVMs       ClusterPhase = "CreateVMs"
	PhaseCollectIPs      ClusterPhase = "CollectIPs"
	PhaseDeployHAProxy   ClusterPhase = "DeployHAProxy"
	PhaseDeployDNS       ClusterPhase = "DeployDNS"
	PhaseGenerateISO     ClusterPhase = "GenerateISO"
	PhaseMountISO        ClusterPhase = "MountISO"
	PhasePowerOn         ClusterPhase = "PowerOn"
	PhaseConfigureNodes  ClusterPhase = "ConfigureNodes"
	PhaseMCSRelay        ClusterPhase = "MCSRelay"
	PhaseWaitBootstrap   ClusterPhase = "WaitBootstrap"
	PhaseWaitInstall     ClusterPhase = "WaitInstall"
	PhasePostInstall     ClusterPhase = "PostInstall"
	// PhasePublishRoutes publishes the API and console through host-cluster
	// Routes (opt-in, externalAccess.hostRoutes); non-fatal.
	PhasePublishRoutes ClusterPhase = "PublishRoutes"
	// ConfigureStorage exposes a host StorageClass inside the nested cluster
	// (kubevirt csi-driver, split mode). ConfigureVirtualization installs
	// OpenShift Virtualization in the nested cluster and points CDI at that
	// storage. Both are optional and non-fatal: a failure leaves the cluster
	// Ready with a False condition rather than failing the install.
	PhaseConfigureStorage        ClusterPhase = "ConfigureStorage"
	PhaseConfigureVirtualization ClusterPhase = "ConfigureVirtualization"
	PhaseReady                   ClusterPhase = "Ready"
	PhaseFailed                  ClusterPhase = "Failed"
	PhaseDeleting                ClusterPhase = "Deleting"
)

var PhaseOrder = []ClusterPhase{
	PhaseCreateNamespace,
	PhaseCreateNetwork,
	PhaseCreateSecrets,
	PhaseCreateVMs,
	PhaseCollectIPs,
	// MCSRelay before DeployHAProxy: the relay only needs master IPs
	// (CollectIPs), boots while the ISO is generated, and lets HAProxy come
	// up ONE final config with MCS servers already in it — no live reload,
	// no pods/exec needed mid-install.
	PhaseMCSRelay,
	PhaseDeployHAProxy,
	PhaseDeployDNS,
	PhaseGenerateISO,
	PhaseMountISO,
	PhasePowerOn,
	PhaseConfigureNodes,
	PhaseWaitBootstrap,
	PhaseWaitInstall,
	PhasePostInstall,
	PhasePublishRoutes,
	PhaseConfigureStorage,
	PhaseConfigureVirtualization,
	PhaseReady,
}

type NestedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NestedClusterSpec   `json:"spec"`
	Status            NestedClusterStatus `json:"status,omitempty"`
}

type NestedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NestedCluster `json:"items"`
}

type NestedClusterSpec struct {
	ClusterName      string               `json:"clusterName"`
	BaseDomain       string               `json:"baseDomain"`
	Namespace        string               `json:"namespace"`
	OCPVersion       string               `json:"ocpVersion"`
	Topology         string               `json:"topology"`
	Masters          NodeSizing           `json:"masters"`
	Workers          NodeSizing           `json:"workers,omitempty"`
	Infra            NodeSizing           `json:"infra,omitempty"`
	NodeNames        []NodeNameEntry      `json:"nodeNames,omitempty"`
	Network          NetworkConfig        `json:"network"`
	ExternalAccess   ExternalAccessConfig `json:"externalAccess"`
	StorageClassName string               `json:"storageClassName"`
	PullSecret       string               `json:"pullSecret,omitempty"`
	SSHKey           string               `json:"sshKey,omitempty"`
	// SSHPrivateKey is the optional private half of SSHKey. Required for the
	// ConfigureNodes phase (in-cluster SSH into VMs to fix DNS after boot —
	// the primary-UDN DHCP hands VMs the host cluster's CoreDNS, which is
	// unreachable from the UDN, so it must be replaced post-boot).
	SSHPrivateKey    string    `json:"sshPrivateKey,omitempty"`
	SSHPrivateKeyRef SecretRef `json:"sshPrivateKeyRef,omitempty"`
	PullSecretRef    SecretRef `json:"pullSecretRef"`
	SSHKeyRef        SecretRef `json:"sshKeyRef"`
	// Bastion controls the single helper VM (nested DNS + MCS relay + ssh).
	// With Graphical set it also gets a desktop, so the nested console can be
	// opened in a browser ON the bastion via the OpenShift VM console — the
	// bastion already resolves the nested zone, so no Route or DNS is needed.
	Bastion BastionConfig `json:"bastion,omitempty"`
	// Storage exposes a host StorageClass to the nested cluster. Virtualization
	// installs OpenShift Virtualization in the nested cluster and requires
	// Storage (enabling it implies storage).
	Storage                StorageConfig        `json:"storage,omitempty"`
	Virtualization         VirtualizationConfig `json:"virtualization,omitempty"`
	InstallConfigOverrides string               `json:"installConfigOverrides,omitempty"`
	AgentConfigOverrides   string               `json:"agentConfigOverrides,omitempty"`
}

// StorageConfig controls nested-cluster storage backed by the host.
//
// InfraStorageClassName empty means: the host's default *virtualization*
// StorageClass, else its default StorageClass — read live, never hardcoded.
// AllowanceGi is the host storage the nested cluster may consume; it becomes
// a ResourceQuota on the cluster's namespace together with the cluster's own
// disks (and the boot images when virtualization is enabled).
type StorageConfig struct {
	Enabled               bool   `json:"enabled,omitempty"`
	InfraStorageClassName string `json:"infraStorageClassName,omitempty"`
	AllowanceGi           int    `json:"allowanceGi,omitempty"`
}

// VirtualizationConfig installs OpenShift Virtualization in the nested cluster.
type VirtualizationConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// StorageWanted reports whether nested storage should be configured.
// Virtualization needs storage, so it implies it.
func (s NestedClusterSpec) StorageWanted() bool { return s.Storage.Enabled || s.Virtualization.Enabled }

// BastionConfig is collected by the wizard. Password is write-only: it is
// hashed into the VM's cloud-init and never stored back on the CR.
type BastionConfig struct {
	Graphical bool   `json:"graphical,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

type NodeSizing struct {
	Count    int `json:"count"`
	CPUCores int `json:"cpuCores"`
	MemoryGi int `json:"memoryGi"`
	DiskGi   int `json:"diskGi"`
}

type NodeNameEntry struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type NetworkConfig struct {
	CUDNSubnet         string `json:"cudnSubnet"`
	APIVIP             string `json:"apiVIP"`
	IngressVIP         string `json:"ingressVIP"`
	ClusterNetworkCIDR string `json:"clusterNetworkCIDR,omitempty"`
	ServiceNetworkCIDR string `json:"serviceNetworkCIDR,omitempty"`
	DNSIP              string `json:"dnsIP,omitempty"`
	// NetworkType is the nested cluster's network plugin: OVNKubernetes
	// (empty, the default) or the open-source Calico or Cilium (OCP 4.22+,
	// manifests embedded in internal/cni). Fixed at install time.
	NetworkType string `json:"networkType,omitempty"`
	// CNIVersion selects the Calico/Cilium release (cni.Versions); empty
	// means the newest embedded one. Must be empty for OVNKubernetes.
	CNIVersion string `json:"cniVersion,omitempty"`
}

// DefaultClusterNetworkCIDR is the pod network when none is set.
const DefaultClusterNetworkCIDR = "10.128.0.0/14"

// PodCIDR returns the cluster (pod) network, defaulting like the CRD.
func (n NetworkConfig) PodCIDR() string {
	if n.ClusterNetworkCIDR == "" {
		return DefaultClusterNetworkCIDR
	}
	return n.ClusterNetworkCIDR
}

type ExternalAccessConfig struct {
	NADName   string `json:"nadName"`
	APIIP     string `json:"apiIP"`
	IngressIP string `json:"ingressIP"`
	// JumpboxIP gives the diagnostics jumpbox VM a second interface on the
	// NAD above with this static address (same prefix as the HAProxy
	// VIPs, PrefixLength), making it ssh-reachable from the external network. Optional:
	// empty leaves the jumpbox cUDN-only.
	JumpboxIP string `json:"jumpboxIP,omitempty"`
	// PrefixLength is the prefix of the external VLAN subnet the static
	// addresses above live in (the HAProxy pods' interfaces get <ip>/<prefix>).
	// 0 means 24, the value every earlier version hardcoded.
	PrefixLength int `json:"prefixLength,omitempty"`
	// Gateway of the external VLAN. The HAProxy pods send replies to clients
	// outside the VLAN through it; without it they answer only the VLAN
	// itself (replies left through the cluster network and never returned).
	// Required for new clusters.
	Gateway string `json:"gateway,omitempty"`
	// IPPool is the IPPool (same namespace as the NestedCluster) the addresses
	// above were picked from. Optional; when set, the API checks that NADName,
	// PrefixLength and the addresses match the pool.
	IPPool string `json:"ipPool,omitempty"`
	// HostRoutes publishes the nested API and console through Routes on the
	// host cluster (names under the host's *.apps domain), so they work
	// without DNS records for the nested domain. Needs the bastion load
	// balancer and external addresses the host nodes can reach.
	HostRoutes bool `json:"hostRoutes,omitempty"`
}

// DefaultExternalPrefixLength is used when ExternalAccessConfig.PrefixLength is 0.
const DefaultExternalPrefixLength = 24

// Prefix returns the external VLAN prefix length, defaulting to 24.
func (e ExternalAccessConfig) Prefix() int {
	if e.PrefixLength == 0 {
		return DefaultExternalPrefixLength
	}
	return e.PrefixLength
}

type SecretRef struct {
	Name string `json:"name"`
}

// LoadBalancerBastion marks clusters whose API/Ingress load balancer runs on
// the bastion VM (with the external VLAN addresses on its own NIC) instead
// of the per-cluster HAProxy pods used before.
const LoadBalancerBastion = "bastion"

type NestedClusterStatus struct {
	Phase ClusterPhase `json:"phase,omitempty"`
	// LoadBalancer is "bastion" for clusters created since the bastion
	// load balancer; empty means the older HAProxy pods.
	LoadBalancer string `json:"loadBalancer,omitempty"`
	// HostConsoleURL / HostAPIURL: the nested console and API through host
	// Routes (externalAccess.hostRoutes); set by PublishRoutes.
	HostConsoleURL string `json:"hostConsoleURL,omitempty"`
	HostAPIURL     string `json:"hostAPIURL,omitempty"`
	// ReleaseVersion/ReleaseImage: the OpenShift release spec.ocpVersion
	// resolved to at GenerateISO ("4.22" -> 4.22.22, payload by digest).
	// Recorded once, so a re-run phase installs the same release.
	ReleaseVersion string `json:"releaseVersion,omitempty"`
	ReleaseImage   string `json:"releaseImage,omitempty"`
	// InstallerImage is the payload's installer image; the wait-for Jobs run
	// it (the cluster namespace cannot download a matching binary). Empty
	// for clusters installed before: they keep the helper image.
	InstallerImage string             `json:"installerImage,omitempty"`
	Message        string             `json:"message,omitempty"`
	StartedAt      *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt    *metav1.Time       `json:"completedAt,omitempty"`
	APIEndpoint    string             `json:"apiEndpoint,omitempty"`
	ConsoleURL     string             `json:"consoleURL,omitempty"`
	IngressDomain  string             `json:"ingressDomain,omitempty"`
	KubeconfigRef  string             `json:"kubeconfigSecretRef,omitempty"`
	KubeadminRef   string             `json:"kubeadminSecretRef,omitempty"`
	InstallConfig  string             `json:"installConfig,omitempty"`
	AgentConfig    string             `json:"agentConfig,omitempty"`
	Nodes          []NodeStatus       `json:"nodes,omitempty"`
	PhaseHistory   []PhaseTransition  `json:"phaseHistory,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
	// Actual cUDN IPs collected at runtime. spec.network.apiVIP/ingressVIP are
	// wizard-supplied wishes; the cUDN DHCP assigns the real ones to the
	// HAProxy pods, and those (not the wishes) are what DNS must serve. Spec
	// edits are not persisted by the reconcile loop, so the truth lives here.
	APIVIPCUDN     string `json:"apiVIPcUDN,omitempty"`
	IngressVIPCUDN string `json:"ingressVIPcUDN,omitempty"`
	DNSIPCUDN      string `json:"dnsIPcUDN,omitempty"`
	// RendezvousIP/Node are discovered in-cluster (assisted-service on :8090).
	RendezvousIP   string `json:"rendezvousIP,omitempty"`
	RendezvousNode string `json:"rendezvousNode,omitempty"`
	// MCSRelayIP is the cUDN IP of the MCS relay VM (see MCSRelay phase).
	MCSRelayIP string `json:"mcsRelayIP,omitempty"`
	// HostAPIRoute/HostConsoleRoute are passthrough Routes published on the
	// host cluster so the nested cluster is reachable without /etc/hosts.
	HostAPIRoute     string `json:"hostAPIRoute,omitempty"`
	HostConsoleRoute string `json:"hostConsoleRoute,omitempty"`
}

type NodeStatus struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	VMStatus    string `json:"vmStatus"`
	PowerState  string `json:"powerState"`
	BootPhase   string `json:"bootPhase"`
	AgentStatus string `json:"agentStatus"`
	IP          string `json:"ip,omitempty"`
	MAC         string `json:"mac,omitempty"`
}

type PhaseTransition struct {
	Phase     ClusterPhase `json:"phase"`
	StartedAt metav1.Time  `json:"startedAt"`
	EndedAt   *metav1.Time `json:"endedAt,omitempty"`
	Message   string       `json:"message"`
	Success   bool         `json:"success"`
}

func (nc *NestedCluster) FQDN() string {
	return nc.Spec.ClusterName + "." + nc.Spec.BaseDomain
}

func (nc *NestedCluster) APIEndpoint() string {
	return "https://api." + nc.FQDN() + ":6443"
}

func (nc *NestedCluster) ConsoleEndpoint() string {
	return "https://console-openshift-console.apps." + nc.FQDN()
}

func (nc *NestedCluster) IngressWildcard() string {
	return "*.apps." + nc.FQDN()
}

func NextPhase(current ClusterPhase) ClusterPhase {
	for i, p := range PhaseOrder {
		if p == current && i+1 < len(PhaseOrder) {
			return PhaseOrder[i+1]
		}
	}
	return PhaseReady
}
