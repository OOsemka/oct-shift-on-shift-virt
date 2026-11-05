/**
 * IP pools: free addresses on an external VLAN that admins prepare, so people
 * creating clusters pick from a list. Mirrors orchestrator/internal/crd/ippool.go.
 * Usage is computed by the orchestrator from the existing clusters.
 */
import { consoleFetchJSON, K8sModel } from '@openshift-console/dynamic-plugin-sdk';

const PROXY_BASE = '/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator';
export const POOL_NAMESPACE = 'oct-shift-on-shift-virt';
export const MAX_POOL_ADDRESSES = 4096;
/** Pools are shared, so their network must be a NAD in this namespace (never a project namespace). */
export const POOL_NAD_NAMESPACE = 'default';

export const IPPoolModel: K8sModel = {
  apiGroup: 'octools.io',
  apiVersion: 'v1alpha1',
  kind: 'IPPool',
  plural: 'ippools',
  abbr: 'IPP',
  label: 'IP Pool',
  labelPlural: 'IP Pools',
  namespaced: true,
};

export type AddressUse = { cluster: string; role: 'API' | 'Ingress' | 'Jumpbox' };
export type PoolAddress = { ip: string; usedBy?: AddressUse };
export type IPPoolItem = {
  name: string;
  nadName: string;
  prefixLength: number;
  gateway?: string;
  description?: string;
  addresses: PoolAddress[];
  total: number;
  free: number;
  problem?: string;
};

export const fetchIPPools = async (): Promise<IPPoolItem[]> => {
  const res = (await consoleFetchJSON(`${PROXY_BASE}/api/v1/ippools`)) as { pools?: IPPoolItem[] };
  return res.pools || [];
};

export const freeAddresses = (pool: IPPoolItem): string[] =>
  pool.problem ? [] : pool.addresses.filter((a) => !a.usedBy).map((a) => a.ip);

function ipToNum(ip: string): number | undefined {
  const parts = ip.trim().split('.');
  if (parts.length !== 4) return undefined;
  let n = 0;
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p) || Number(p) > 255 || p !== String(Number(p))) return undefined;
    n = n * 256 + Number(p);
  }
  return n;
}

const numToIp = (n: number) => [24, 16, 8, 0].map((s) => Math.floor(n / 2 ** s) % 256).join('.');

export type PoolForm = {
  name: string;
  nadName: string;
  prefixLength: number;
  addresses: string;
  gateway: string;
  description: string;
};

export type PoolProblem = { field: keyof PoolForm; key: string; values?: Record<string, string> };

/** Address entries from the form: one per line, or separated by commas or spaces. */
export const addressEntries = (text: string): string[] =>
  text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);

/** Expands entries to sorted unique addresses, or returns the first bad entry. */
export function expandAddresses(entries: string[]): { addresses: string[]; bad?: string; tooMany?: boolean } {
  const seen = new Set<number>();
  for (const entry of entries) {
    const [from, to = from, extra] = entry.split('-');
    const lo = ipToNum(from);
    const hi = ipToNum(to);
    if (extra !== undefined || lo === undefined || hi === undefined || lo > hi) return { addresses: [], bad: entry };
    if (hi - lo + 1 + seen.size > MAX_POOL_ADDRESSES) return { addresses: [], tooMany: true };
    for (let n = lo; n <= hi; n++) seen.add(n);
  }
  return { addresses: Array.from(seen).sort((a, b) => a - b).map(numToIp) };
}

const DNS1123 = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

export function poolProblems(form: PoolForm, existingNames: string[]): PoolProblem[] {
  const out: PoolProblem[] = [];
  const name = form.name.trim();
  if (!name || name.length > 63 || !DNS1123.test(name)) {
    out.push({ field: 'name', key: 'Use lowercase letters, digits and hyphens (up to 63 characters).' });
  } else if (existingNames.includes(name)) {
    out.push({ field: 'name', key: 'A pool named {{name}} already exists.', values: { name } });
  }
  if (!form.nadName) {
    out.push({ field: 'nadName', key: 'Select the network (NAD) of the external VLAN.' });
  } else if (!form.nadName.startsWith(`${POOL_NAD_NAMESPACE}/`)) {
    out.push({
      field: 'nadName',
      key: 'Pools are shared, so their network must be a NAD in the {{ns}} namespace.',
      values: { ns: POOL_NAD_NAMESPACE },
    });
  }
  const prefixOk = Number.isInteger(form.prefixLength) && form.prefixLength >= 8 && form.prefixLength <= 30;
  if (!prefixOk) out.push({ field: 'prefixLength', key: 'Enter a prefix length from 8 to 30.' });

  const { addresses, bad, tooMany } = expandAddresses(addressEntries(form.addresses));
  if (bad) {
    out.push({ field: 'addresses', key: '{{entry}} is not an IPv4 address or range such as 192.0.2.10-192.0.2.40.', values: { entry: bad } });
  } else if (tooMany) {
    out.push({ field: 'addresses', key: 'A pool may hold at most {{max}} addresses.', values: { max: String(MAX_POOL_ADDRESSES) } });
  } else if (addresses.length === 0) {
    out.push({ field: 'addresses', key: 'Add at least one address or range.' });
  } else if (prefixOk) {
    const size = 2 ** (32 - form.prefixLength);
    const base = Math.floor((ipToNum(addresses[0]) as number) / size) * size;
    const subnet = `${numToIp(base)}/${form.prefixLength}`;
    const inSubnet = (n: number) => n >= base && n < base + size;
    const outside = addresses.find((a) => !inSubnet(ipToNum(a) as number));
    if (outside) {
      out.push({ field: 'addresses', key: '{{ip}} is not in the same subnet as the first address ({{subnet}}).', values: { ip: outside, subnet } });
    }
    const edge = addresses.find((a) => {
      const n = ipToNum(a) as number;
      return n === base || n === base + size - 1;
    });
    if (edge) {
      out.push({ field: 'addresses', key: '{{ip}} is the network or broadcast address of {{subnet}}.', values: { ip: edge, subnet } });
    }
    const gw = form.gateway.trim();
    if (gw) {
      const g = ipToNum(gw);
      if (g === undefined) out.push({ field: 'gateway', key: 'Enter an IPv4 address or leave it empty.' });
      else if (!inSubnet(g)) out.push({ field: 'gateway', key: 'The gateway is outside {{subnet}}.', values: { subnet } });
      else if (addresses.includes(numToIp(g))) out.push({ field: 'gateway', key: 'The gateway must not be one of the pool addresses.' });
    }
  }
  return out;
}

export const buildIPPool = (form: PoolForm) => ({
  apiVersion: 'octools.io/v1alpha1',
  kind: 'IPPool',
  metadata: { name: form.name.trim(), namespace: POOL_NAMESPACE },
  spec: {
    nadName: form.nadName,
    prefixLength: form.prefixLength,
    addresses: addressEntries(form.addresses),
    ...(form.gateway.trim() ? { gateway: form.gateway.trim() } : {}),
    ...(form.description.trim() ? { description: form.description.trim() } : {}),
  },
});
