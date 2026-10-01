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

export const fetchKubeadmin = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/kubeadmin`);

export const fetchClusterLogs = (name: string): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/logs`);

export const fetchStorageClasses = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/storageclasses`);

export const fetchNADs = (namespaces?: string[]): Promise<unknown> => {
  const params = namespaces?.length ? `?namespaces=${namespaces.map(encodeURIComponent).join(',')}` : '';
  return consoleFetchJSON(`${PROXY_BASE}/api/v1/nads${params}`);
};

export const fetchOCPVersions = (): Promise<unknown> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/ocp-versions`);

export const previewConfig = (config: ClusterConfig): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/clusters/preview-config`, config);
