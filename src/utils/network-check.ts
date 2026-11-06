/**
 * Network checks for the Create Cluster wizard. They mirror the orchestrator's
 * ValidateNetwork (orchestrator/internal/crd/validate.go), which refuses the
 * same settings server-side; keep the two in step.
 *
 * Mistakes these stop, all seen in real use: a blank external IP
 * (the HAProxy pod got the annotation "ips":["/24"] and never started), the
 * internal VIPs typed in as external IPs, and settings changed in a later step
 * after an earlier step looked fine.
 */
import { ClusterConfig, HostNetwork, HostNodeAddress } from '../components/InstallTab/types';

export type NetworkField =
  | 'cudnSubnet'
  | 'clusterNetworkCIDR'
  | 'serviceNetworkCIDR'
  | 'nadName'
  | 'apiIP'
  | 'ingressIP'
  | 'gateway'
  | 'prefixLength'
  | 'networkType';

export type NetworkProblem = { field: NetworkField; key: string; values?: Record<string, string> };

export const DEFAULT_EXTERNAL_PREFIX = 24;
/** Offsets of the internal VIPs from the internal network address (base+10, base+11). */
const API_VIP_OFFSET = 10;
const INGRESS_VIP_OFFSET = 11;
/** The orchestrator places masters at base+100.., so the internal network must be /24 or larger. */
const MAX_INTERNAL_PREFIX = 24;

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

function numToIp(n: number): string {
  return [24, 16, 8, 0].map((s) => Math.floor(n / 2 ** s) % 256).join('.');
}

type Cidr = { base: number; prefix: number; size: number; text: string };

function parseCidr(cidr: string): Cidr | undefined {
  const [ip, bits, extra] = cidr.trim().split('/');
  if (extra !== undefined || bits === undefined || !/^\d{1,2}$/.test(bits)) return undefined;
  const prefix = Number(bits);
  const n = ipToNum(ip);
  if (n === undefined || prefix > 32) return undefined;
  const size = 2 ** (32 - prefix);
  const base = Math.floor(n / size) * size;
  return { base, prefix, size, text: `${numToIp(base)}/${prefix}` };
}

function contains(c: Cidr, ipNum: number): boolean {
  return ipNum >= c.base && ipNum < c.base + c.size;
}

/** Internal API/Ingress VIPs for an internal network, or undefined if it is not a usable CIDR. */
export function internalVIPs(cudnSubnet: string): { apiVIP: string; ingressVIP: string } | undefined {
  const c = parseCidr(cudnSubnet);
  if (!c || c.prefix > MAX_INTERNAL_PREFIX) return undefined;
  return { apiVIP: numToIp(c.base + API_VIP_OFFSET), ingressVIP: numToIp(c.base + INGRESS_VIP_OFFSET) };
}

/**
 * How the internal network collides with the host cluster's node network, or
 * undefined. OVN-Kubernetes requires UDN subnets not to overlap the node
 * network: on a host whose nodes were inside the default 10.100.0.0/16, pods
 * and VMs on the cUDN had no internet and the install never started.
 * Mirrors crd.CheckHostNetwork (orchestrator/internal/crd/hostnetwork.go).
 */
export function hostNetworkProblem(
  cudnSubnet: string,
  hostAddresses: HostNodeAddress[] | undefined,
  suggestedSubnet?: string,
): NetworkProblem | undefined {
  const cudn = parseCidr(cudnSubnet);
  if (!cudn || !hostAddresses?.length) return undefined;
  const suggestion = suggestedSubnet && suggestedSubnet !== cudn.text ? suggestedSubnet : '';
  for (const h of hostAddresses) {
    const n = ipToNum(h.address);
    if (n !== undefined && contains(cudn, n)) {
      const values = { node: h.node, ip: h.address, suggestion };
      return suggestion
        ? {
            field: 'cudnSubnet',
            key: 'Contains host node {{node}} address {{ip}}. The internal network must not overlap the host cluster node network, or the cluster has no internet access. Use a free range such as {{suggestion}}.',
            values,
          }
        : {
            field: 'cudnSubnet',
            key: 'Contains host node {{node}} address {{ip}}. The internal network must not overlap the host cluster node network, or the cluster has no internet access.',
            values,
          };
    }
  }
  for (const h of hostAddresses) {
    const sub = h.subnet ? parseCidr(h.subnet) : undefined;
    if (sub && cudn.base < sub.base + sub.size && sub.base < cudn.base + cudn.size) {
      const values = { node: h.node, cidr: sub.text, suggestion };
      return suggestion
        ? {
            field: 'cudnSubnet',
            key: 'Overlaps host node {{node}} network {{cidr}}. The internal network must not overlap the host cluster node network, or the cluster has no internet access. Use a free range such as {{suggestion}}.',
            values,
          }
        : {
            field: 'cudnSubnet',
            key: 'Overlaps host node {{node}} network {{cidr}}. The internal network must not overlap the host cluster node network, or the cluster has no internet access.',
            values,
          };
    }
  }
  return undefined;
}

/**
 * Problems on the Network step. hostNetwork comes from GET /api/v1/host-network;
 * when it is unavailable the host overlap check is skipped here (the
 * orchestrator still enforces it).
 */
export function networkProblems(net: ClusterConfig['network'], hostNetwork?: HostNetwork | null): NetworkProblem[] {
  const out: NetworkProblem[] = [];
  const cudn = parseCidr(net.cudnSubnet);
  if (!cudn) {
    out.push({ field: 'cudnSubnet', key: 'Enter an IPv4 CIDR such as 10.100.0.0/16.' });
  } else if (cudn.prefix > MAX_INTERNAL_PREFIX) {
    out.push({ field: 'cudnSubnet', key: 'The internal network must be a /24 or larger (for example /16).' });
  } else {
    const hostProblem = hostNetworkProblem(net.cudnSubnet, hostNetwork?.nodeAddresses, hostNetwork?.suggestedCudnSubnet);
    if (hostProblem) out.push(hostProblem);
  }
  const pod = parseCidr(net.clusterNetworkCIDR);
  const svc = parseCidr(net.serviceNetworkCIDR);
  if (!pod) out.push({ field: 'clusterNetworkCIDR', key: 'Enter an IPv4 CIDR such as 10.128.0.0/14.' });
  if (!svc) out.push({ field: 'serviceNetworkCIDR', key: 'Enter an IPv4 CIDR such as 172.30.0.0/16.' });
  const overlaps = (a: Cidr, b: Cidr) => a.base < b.base + b.size && b.base < a.base + a.size;
  if (cudn && pod && overlaps(cudn, pod)) {
    out.push({ field: 'clusterNetworkCIDR', key: 'Overlaps the internal network {{cidr}}.', values: { cidr: cudn.text } });
  }
  if (cudn && svc && overlaps(cudn, svc)) {
    out.push({ field: 'serviceNetworkCIDR', key: 'Overlaps the internal network {{cidr}}.', values: { cidr: cudn.text } });
  }
  if (pod && svc && overlaps(pod, svc)) {
    out.push({ field: 'serviceNetworkCIDR', key: 'Overlaps the cluster network {{cidr}}.', values: { cidr: pod.text } });
  }
  return out;
}

/**
 * First OpenShift 4.x minor whose agent installer accepts Calico/Cilium
 * (orchestrator cni.MinOCPMinor).
 */
export const MIN_THIRD_PARTY_CNI_MINOR = 22;

/** OpenShift minor of "4.22" / "4.22.3", or undefined. */
export function ocpMinor(version: string): number | undefined {
  const m = /^4\.(\d+)(\.|$)/.exec(version.trim());
  return m ? Number(m[1]) : undefined;
}

/** Problems with the network plugin choice (Network step; it depends on the OpenShift version). */
export function networkPluginProblems(config: ClusterConfig): NetworkProblem[] {
  const nt = config.network.networkType || 'OVNKubernetes';
  if (nt === 'OVNKubernetes') return [];
  const minor = ocpMinor(config.ocpVersion);
  if (minor !== undefined && minor < MIN_THIRD_PARTY_CNI_MINOR) {
    return [
      {
        field: 'networkType',
        key: '{{plugin}} needs OpenShift 4.{{min}} or later. Pick OVN-Kubernetes or change the OpenShift version.',
        values: { plugin: nt, min: String(MIN_THIRD_PARTY_CNI_MINOR) },
      },
    ];
  }
  return [];
}

/** Problems on the External Access step. */
export function externalAccessProblems(config: ClusterConfig): NetworkProblem[] {
  const out: NetworkProblem[] = [];
  const ea = config.externalAccess;
  if (!ea.nadName.trim()) {
    out.push({ field: 'nadName', key: 'Select the network (NAD) of the external VLAN.' });
  }
  const prefix = ea.prefixLength;
  const prefixOk = Number.isInteger(prefix) && prefix >= 8 && prefix <= 30;
  if (!prefixOk) {
    out.push({ field: 'prefixLength', key: 'Enter a prefix length from 8 to 30.' });
  }

  const cudn = parseCidr(config.network.cudnSubnet);
  const vips = internalVIPs(config.network.cudnSubnet);
  const others = [config.network.clusterNetworkCIDR, config.network.serviceNetworkCIDR]
    .map(parseCidr)
    .filter((c): c is Cidr => Boolean(c));

  const seen = new Map<number, NetworkField>();
  let vlan: Cidr | undefined;
  for (const field of ['apiIP', 'ingressIP'] as const) {
    const value = ea[field].trim();
    if (!value) {
      out.push({ field, key: 'Required. Type a free address on the external VLAN; it is not assigned automatically.' });
      continue;
    }
    const n = ipToNum(value);
    if (n === undefined) {
      out.push({ field, key: 'Enter an IPv4 address such as 198.51.100.10.' });
      continue;
    }
    if (vips && (value === vips.apiVIP || value === vips.ingressVIP)) {
      out.push({
        field,
        key: '{{ip}} is an internal VIP. Use a free address on the external VLAN instead.',
        values: { ip: value },
      });
    } else if (cudn && contains(cudn, n)) {
      out.push({
        field,
        key: '{{ip}} is inside the internal network {{cidr}}. Use a free address on the external VLAN.',
        values: { ip: value, cidr: cudn.text },
      });
    }
    const other = others.find((c) => contains(c, n));
    if (other) {
      out.push({
        field,
        key: '{{ip}} is inside the nested cluster network {{cidr}}.',
        values: { ip: value, cidr: other.text },
      });
    }
    if (seen.has(n)) {
      out.push({ field, key: 'API IP and Ingress IP must be different addresses.' });
    }
    seen.set(n, field);
    if (prefixOk) {
      const sub = parseCidr(`${value}/${prefix}`) as Cidr;
      if (n === sub.base || n === sub.base + sub.size - 1) {
        out.push({
          field,
          key: '{{ip}} is the network or broadcast address of {{cidr}}.',
          values: { ip: value, cidr: sub.text },
        });
      }
      if (!vlan) {
        vlan = sub;
      } else if (!contains(vlan, n)) {
        out.push({
          field,
          key: 'Not in the same VLAN subnet as the API IP ({{cidr}}). Check the address or the prefix length.',
          values: { cidr: vlan.text },
        });
      }
    }
  }
  // The gateway: the bastion answers clients outside the VLAN through it.
  const gw = (ea.gateway || '').trim();
  if (!gw) {
    out.push({ field: 'gateway', key: 'Required. Without the VLAN gateway the cluster answers only clients on the VLAN itself.' });
  } else {
    const g = ipToNum(gw);
    if (g === undefined) {
      out.push({ field: 'gateway', key: 'Enter an IPv4 address such as 198.51.100.1.' });
    } else if (seen.has(g)) {
      out.push({ field: 'gateway', key: 'The gateway must not be the API or Ingress IP.' });
    } else if (vlan && !contains(vlan, g)) {
      out.push({ field: 'gateway', key: 'The gateway is outside the VLAN subnet {{cidr}}.', values: { cidr: vlan.text } });
    }
  }
  return out;
}

/** The usual gateway guess for an address: the first host of its subnet. */
export function suggestGateway(ip: string, prefix: number): string {
  const n = ipToNum(ip);
  if (n === undefined || !(prefix >= 8 && prefix <= 30)) return '';
  const size = 2 ** (32 - prefix);
  return numToIp(Math.floor(n / size) * size + 1);
}
