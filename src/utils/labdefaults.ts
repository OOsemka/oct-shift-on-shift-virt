/**
 * Lab defaults (one profile) and the quick compact cluster. Mirrors
 * orchestrator/internal/crd/labdefaults.go and quick.go.
 *
 * Admins save the defaults into the Secret sos-lab-defaults with their own
 * Kubernetes permissions. The orchestrator reads it: GET /api/v1/defaults
 * reports what is set (never the pull secret itself), and empty credentials in
 * a create request are filled from it server-side.
 */
import { consoleFetchJSON, K8sModel } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from '../components/InstallTab/types';

const PROXY_BASE = '/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator';
export const DEFAULTS_NAMESPACE = 'oct-shift-on-shift-virt';
export const DEFAULTS_SECRET = 'sos-lab-defaults';

export const SecretModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'secrets',
  abbr: 'S',
  label: 'Secret',
  labelPlural: 'Secrets',
  namespaced: true,
};

export type LabSettings = {
  baseDomain?: string;
  ocpVersion?: string;
  storageClassName?: string;
  ipPool?: string;
  masterCores?: number;
  masterMemoryGi?: number;
  masterDiskGi?: number;
  /** Quick clusters get a bastion desktop unless the card says otherwise. */
  bastionGraphical?: boolean;
  /** Expose a host StorageClass to quick clusters through KubeVirt CSI. */
  nestedStorage?: boolean;
  /** Host class to expose; empty = the host's default virtualization class. */
  nestedStorageClass?: string;
  /** GiB the nested cluster may use; 0/empty = 100. */
  nestedStorageAllowanceGi?: number;
  /** Install OpenShift Virtualization in quick clusters (implies nested storage). */
  nestedVirtualization?: boolean;
  /** Quick clusters publish the API and console through host routes. */
  publishHostRoutes?: boolean;
  /** Quick clusters also publish all their application routes. */
  publishAppRoutes?: boolean;
};

/** The only settings a quick-cluster request may override. */
export type QuickOverrides = {
  sshPublicKey?: string;
  graphical?: boolean;
  desktopUsername?: string;
  desktopPassword?: string;
  /** Reach console/API through host-cluster routes instead of external DNS. */
  hostRoutes?: boolean;
  /** Publish all application routes under the host's *.apps. */
  publishAppRoutes?: boolean;
  /** Clone from this master (Express) instead of installing. */
  master?: string;
};

export type LabDefaults = {
  pullSecretSet: boolean;
  pullSecretProblem?: string;
  sshPublicKey?: string;
  sshKeyFingerprint?: string;
  sshKeyProblem?: string;
  settings: LabSettings;
  settingsProblem?: string;
};

export type QuickPlan = { spec: ClusterConfig; problems: string[] };

export const fetchLabDefaults = (): Promise<LabDefaults> =>
  consoleFetchJSON(`${PROXY_BASE}/api/v1/defaults`) as Promise<LabDefaults>;

export const fetchQuickPlan = (name: string, master = ''): Promise<QuickPlan> =>
  consoleFetchJSON(
    `${PROXY_BASE}/api/v1/quick-clusters/preview?name=${encodeURIComponent(name)}${master ? `&master=${encodeURIComponent(master)}` : ''}`,
  ) as Promise<QuickPlan>;

export const createQuickCluster = (clusterName: string, overrides: QuickOverrides = {}): Promise<unknown> =>
  consoleFetchJSON.post(`${PROXY_BASE}/api/v1/quick-clusters`, { clusterName, ...overrides });

/** Same checks as the orchestrator (crd.ValidatePullSecret / ValidateSSHPublicKey). */
export function pullSecretProblem(raw: string): string | undefined {
  try {
    const ps = JSON.parse(raw.trim());
    if (!ps || typeof ps.auths !== 'object' || Object.keys(ps.auths).length === 0) {
      return 'The pull secret has no registries under "auths".';
    }
    return undefined;
  } catch {
    return 'The pull secret is not valid JSON.';
  }
}

export function sshKeyProblem(key: string): string | undefined {
  if (key.includes('PRIVATE KEY')) return 'This is a private key; paste the public key (the .pub file).';
  const f = key.trim().split(/\s+/);
  if (f.length < 2) return 'Expected "<type> <key> [comment]".';
  if (!/^(ssh-|ecdsa-|sk-)/.test(f[0])) return 'Not an SSH public key type.';
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(f[1]) || f[1].length < 20) return 'The key data is not valid base64.';
  return undefined;
}

/** Extract a readable error from a console fetch failure. */
export function fetchErrorText(e: unknown): string {
  const body = (e as { json?: { error?: string; message?: string } })?.json;
  return body?.error || body?.message || (e instanceof Error ? e.message : String(e));
}
