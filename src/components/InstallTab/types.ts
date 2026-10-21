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
}

export interface ExternalAccessConfig {
  nadName: string;
  apiIP: string;
  ingressIP: string;
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
  },
  externalAccess: {
    nadName: '',
    apiIP: '',
    ingressIP: '',
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
