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
	PhaseGenerateISO     ClusterPhase = "GenerateISO"
	PhaseMountISO        ClusterPhase = "MountISO"
	PhasePowerOn         ClusterPhase = "PowerOn"
	PhaseWaitBootstrap   ClusterPhase = "WaitBootstrap"
	PhaseWaitInstall     ClusterPhase = "WaitInstall"
	PhasePostInstall     ClusterPhase = "PostInstall"
	PhaseReady           ClusterPhase = "Ready"
	PhaseFailed          ClusterPhase = "Failed"
	PhaseDeleting        ClusterPhase = "Deleting"
)

var PhaseOrder = []ClusterPhase{
	PhaseCreateNamespace,
	PhaseCreateNetwork,
	PhaseCreateSecrets,
	PhaseCreateVMs,
	PhaseCollectIPs,
	PhaseDeployHAProxy,
	PhaseGenerateISO,
	PhaseMountISO,
	PhasePowerOn,
	PhaseWaitBootstrap,
	PhaseWaitInstall,
	PhasePostInstall,
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
	ClusterName            string               `json:"clusterName"`
	BaseDomain             string               `json:"baseDomain"`
	Namespace              string               `json:"namespace"`
	OCPVersion             string               `json:"ocpVersion"`
	Topology               string               `json:"topology"`
	Masters                NodeSizing           `json:"masters"`
	Workers                NodeSizing           `json:"workers,omitempty"`
	Infra                  NodeSizing           `json:"infra,omitempty"`
	NodeNames              []NodeNameEntry      `json:"nodeNames,omitempty"`
	Network                NetworkConfig        `json:"network"`
	ExternalAccess         ExternalAccessConfig `json:"externalAccess"`
	StorageClassName       string               `json:"storageClassName"`
	PullSecret             string               `json:"pullSecret,omitempty"`
	SSHKey                 string               `json:"sshKey,omitempty"`
	PullSecretRef          SecretRef            `json:"pullSecretRef"`
	SSHKeyRef              SecretRef            `json:"sshKeyRef"`
	InstallConfigOverrides string               `json:"installConfigOverrides,omitempty"`
	AgentConfigOverrides   string               `json:"agentConfigOverrides,omitempty"`
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
}

type ExternalAccessConfig struct {
	NADName   string `json:"nadName"`
	APIIP     string `json:"apiIP"`
	IngressIP string `json:"ingressIP"`
}

type SecretRef struct {
	Name string `json:"name"`
}

type NestedClusterStatus struct {
	Phase           ClusterPhase      `json:"phase,omitempty"`
	Message         string            `json:"message,omitempty"`
	StartedAt       *metav1.Time      `json:"startedAt,omitempty"`
	CompletedAt     *metav1.Time      `json:"completedAt,omitempty"`
	APIEndpoint     string            `json:"apiEndpoint,omitempty"`
	ConsoleURL      string            `json:"consoleURL,omitempty"`
	IngressDomain   string            `json:"ingressDomain,omitempty"`
	KubeconfigRef   string            `json:"kubeconfigSecretRef,omitempty"`
	KubeadminRef    string            `json:"kubeadminSecretRef,omitempty"`
	Nodes           []NodeStatus      `json:"nodes,omitempty"`
	PhaseHistory    []PhaseTransition `json:"phaseHistory,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
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
