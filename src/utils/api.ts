import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from './types';

const PROXY_BASE = '/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator';

export const fetchClusters = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters`);

export const fetchCluster = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}`);

export const createCluster = (config: ClusterConfig): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/clusters`, config);

export const deleteCluster = (name: string): Promise<unknown> =>
  consoleFetchJSON.delete(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}`);

export const fetchClusterStatus = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/status`);

export const fetchClusterNodes = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/nodes`);

export const fetchKubeconfig = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/kubeconfig`);

/** Public CA that signs the host-route certificates (externalAccess.hostRoutes). */
export const fetchExternalCA = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/external-ca`);

export const fetchKubeadmin = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/kubeadmin`);

export const fetchClusterLogs = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/logs`);

/**
 * Day 2: add nested storage, or OpenShift Virtualization (which includes nested
 * storage and restarts the node VMs one at a time), to a Ready cluster.
 */
export const enableStorage = (
  name: string,
  body: { virtualization: boolean; infraStorageClassName?: string; allowanceGi?: number },
): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/enable-storage`, body);

export const fetchStorageClasses = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/storageclasses`);

export const fetchNADs = (namespaces?: string[]): Promise<unknown> => {
  const params = namespaces?.length ? `?namespaces=${namespaces.map(encodeURIComponent).join(',')}` : '';
  return consoleFetchJSON(`${PROXY_BASE}/api/v1/nads${params}`);
};

export const fetchOCPVersions = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/ocp-versions`);

export const fetchNetworkPlugins = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/network-plugins`);

export const fetchHostNetwork = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/host-network`);

export const previewConfig =(config: ClusterConfig): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/clusters/preview-config`, config);
