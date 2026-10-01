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
  pullSecret: string;
  sshKey: string;
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

export interface NestedClusterStatus {
  phase: string;
  message: string;
  apiEndpoint: string;
  consoleURL: string;
  ingressDomain: string;
  nodes: NodeStatus[];
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
  nodeNames: [],
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
  pullSecret: '',
  sshKey: '',
  installConfigOverrides: '',
  agentConfigOverrides: '',
};
