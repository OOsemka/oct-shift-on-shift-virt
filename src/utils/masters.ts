/**
 * Express: masters are installed clusters turned into templates (shut down,
 * disks snapshotted); clones are created from them in minutes.
 */
import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';

const PROXY_BASE = '/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator';

export type MasterItem = {
  name: string;
  ocpVersion: string;
  releaseVersion?: string;
  masters: { count: number; cpuCores: number; memoryGi: number; diskGi: number };
  internalDomain: string;
  createdAt?: string;
  clones: number;
};

export type ClusterListItem = {
  name: string;
  phase: string;
  ocpVersion: string;
  cloneOf?: string;
  masterRefusal?: string;
};

export const fetchMasters = async (): Promise<MasterItem[]> => {
  const res = (await consoleFetchJSON(`${PROXY_BASE}/api/v1/masters`)) as { items?: MasterItem[] };
  return res.items || [];
};

export const fetchClusterList = async (): Promise<ClusterListItem[]> => {
  const res = (await consoleFetchJSON(`${PROXY_BASE}/api/v1/clusters`)) as { items?: ClusterListItem[] };
  return res.items || [];
};

export const makeMaster = (name: string): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/clusters/${encodeURIComponent(name)}/make-master`, {});

/** Clusters that can become a master now: Ready, installed (not a clone), no refusal. */
export const masterCandidates = (clusters: ClusterListItem[]): ClusterListItem[] =>
  clusters.filter((c) => c.phase === 'Ready' && !c.cloneOf && !c.masterRefusal);

export const masterLabel = (m: MasterItem): string =>
  `${m.name} · OpenShift ${m.releaseVersion || m.ocpVersion} · ${m.masters.count} × ${m.masters.cpuCores} vCPU / ${m.masters.memoryGi} GiB`;
