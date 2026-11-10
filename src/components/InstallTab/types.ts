export interface NodeSizing {
  count: number;
  cpuCores: number;
  memoryGi: number;
  diskGi: number;
}

export interface NodeNameEntry {
  name: string;
  role: 'master' | 'worker' | 'infra';
}

export interface NetworkConfig {
  cudnSubnet: string;
  apiVIP: string;
  ingressVIP: string;
  clusterNetworkCIDR: string;
  serviceNetworkCIDR: string;
  /** Network plugin of the nested cluster; Calico and Cilium need OpenShift 4.22+. */
  networkType: NetworkType;
  /** Calico/Cilium release; empty means the newest offered (always empty for OVN-Kubernetes). */
  cniVersion: string;
}

export type NetworkType = 'OVNKubernetes' | 'Calico' | 'Cilium';

/** One choice from GET /api/v1/network-plugins (orchestrator internal/cni). */
export interface NetworkPlugin {
  networkType: NetworkType;
  displayName: string;
  /** Newest first; absent for OVN-Kubernetes (it ships with OpenShift). */
  versions?: string[];
  /** First OpenShift version whose agent installer accepts it, e.g. "4.22". */
  minOCPVersion?: string;
}

/** One host cluster node address from GET /api/v1/host-network (orchestrator crd.HostNodeAddress). */
export interface HostNodeAddress {
  node: string;
  address: string;
  /** The node's network with prefix (from OVN-Kubernetes), when known. */
  subnet?: string;
}

/** GET /api/v1/host-network: host node addresses and a free default internal network. */
export interface HostNetwork {
  nodeAddresses: HostNodeAddress[];
  /** Empty when no default candidate is free. */
  suggestedCudnSubnet: string;
}

export interface ExternalAccessConfig {
  nadName: string;
  /** Free static addresses on the external VLAN, typed in by the user (never assigned automatically). */
  apiIP: string;
  ingressIP: string;
  /** Prefix length of the external VLAN subnet. */
  prefixLength: number;
  /** VLAN gateway: the bastion answers clients outside the VLAN through it. */
  gateway: string;
  /** IP pool the addresses were picked from; empty when entered by hand. */
  ipPool: string;
  /** Publish the API and console through host-cluster Routes (no DNS needed). */
  hostRoutes?: boolean;
  /** Publish all application routes under the host's *.apps (needs hostRoutes). */
  publishAppRoutes?: boolean;
}

/**
 * The bastion is the single helper VM (nested DNS + MCS relay + SSH). With
 * graphical enabled it also gets a desktop, so the nested console can be
 * opened in a browser ON the bastion through the OpenShift VM console — the
 * bastion already resolves the nested zone, so this needs no Route and no
 * extra network exposure.
 *
 * The password is write-only: the orchestrator copies it into a Secret and
 * clears it from the CR.
 */
export interface BastionConfig {
  graphical: boolean;
  username: string;
  password: string;
}

/**
 * Nested storage: the kubevirt csi-driver gives the nested cluster a default
 * StorageClass backed by a host StorageClass. Virtualization installs
 * OpenShift Virtualization in the nested cluster and implies storage. An
 * empty infraStorageClassName lets the orchestrator pick the host default.
 */
export interface NestedStorageConfig {
  enabled: boolean;
  infraStorageClassName: string;
  allowanceGi: number;
}

export interface VirtualizationConfig {
  enabled: boolean;
}

export interface ClusterConfig {
  clusterName: string;
  baseDomain: string;
  namespace: string;
  ocpVersion: string;
  topology: 'compact' | 'ha' | 'custom';
  masters: NodeSizing;
  workers: NodeSizing;
  infra: NodeSizing;
  nodeNames: NodeNameEntry[];
  network: NetworkConfig;
  externalAccess: ExternalAccessConfig;
  storageClassName: string;
  storage: NestedStorageConfig;
  virtualization: VirtualizationConfig;
  pullSecret: string;
  sshKey: string;
  bastion: BastionConfig;
  installConfigOverrides: string;
  agentConfigOverrides: string;
}

export interface NodeStatus {
  name: string;
  role: string;
  vmStatus: string;
  powerState: string;
  bootPhase: string;
  agentStatus: string;
  ip: string;
}

export interface ClusterCondition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason: string;
  message: string;
}

export interface NestedClusterStatus {
  phase: string;
  message: string;
  apiEndpoint: string;
  consoleURL: string;
  ingressDomain: string;
  nodes: NodeStatus[];
  conditions?: ClusterCondition[];
  installConfig?: string;
  agentConfig?: string;
  /** Host-side URLs when externalAccess.hostRoutes is on. */
  hostConsoleURL?: string;
  hostAPIURL?: string;
}

/** Build node name entries for a given topology and sizing. */
export function generateNodeNames(
  clusterName: string,
  topology: ClusterConfig['topology'],
  masters: NodeSizing,
  workers: NodeSizing,
): NodeNameEntry[] {
  if (topology === 'custom') return [];
  const prefix = clusterName || 'cluster';
  const names: NodeNameEntry[] = Array.from({ length: masters.count }, (_, i) => ({
    name: `${prefix}-master-${i}`,
    role: 'master' as const,
  }));
  if (topology === 'ha') {
    for (let i = 0; i < workers.count; i++) {
      names.push({ name: `${prefix}-worker-${i}`, role: 'worker' });
    }
  }
  return names;
}

export const DEFAULT_CLUSTER_CONFIG: ClusterConfig = {
  clusterName: '',
  baseDomain: '',
  namespace: '',
  ocpVersion: '',
  topology: 'compact',
  masters: { count: 3, cpuCores: 8, memoryGi: 32, diskGi: 120 },
  workers: { count: 0, cpuCores: 4, memoryGi: 16, diskGi: 120 },
  infra: { count: 0, cpuCores: 4, memoryGi: 16, diskGi: 120 },
  nodeNames: [
    { name: 'cluster-master-0', role: 'master' },
    { name: 'cluster-master-1', role: 'master' },
    { name: 'cluster-master-2', role: 'master' },
  ],
  network: {
    cudnSubnet: '10.100.0.0/16',
    apiVIP: '10.100.0.10',
    ingressVIP: '10.100.0.11',
    clusterNetworkCIDR: '10.128.0.0/14',
    serviceNetworkCIDR: '172.30.0.0/16',
    networkType: 'OVNKubernetes',
    cniVersion: '',
  },
  externalAccess: {
    nadName: '',
    apiIP: '',
    ingressIP: '',
    prefixLength: 24,
    gateway: '',
    ipPool: '',
    hostRoutes: false,
  },
  storageClassName: '',
  storage: { enabled: false, infraStorageClassName: '', allowanceGi: 100 },
  virtualization: { enabled: false },
  pullSecret: '',
  sshKey: '',
  bastion: { graphical: false, username: 'core', password: '' },
  installConfigOverrides: '',
  agentConfigOverrides: '',
};
