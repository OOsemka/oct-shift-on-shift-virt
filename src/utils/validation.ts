export function validateClusterName(name: string): string | undefined {
  if (!name) return 'Cluster name is required';
  if (name !== name.toLowerCase()) return 'Must be lowercase';
  if (!/^[a-z][a-z0-9-]*[a-z0-9]$/.test(name) && name.length > 1)
    return 'Must start with a letter, contain only lowercase letters, numbers, and hyphens';
  if (name.length === 1 && !/^[a-z]$/.test(name))
    return 'Must start with a letter';
  if (name.length > 63) return 'Must be 63 characters or fewer';
  if (name.includes('--')) return 'Must not contain consecutive hyphens';
  return undefined;
}

export function validateDomain(domain: string): string | undefined {
  if (!domain) return 'Base domain is required';
  if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/.test(domain))
    return 'Must be a valid FQDN (e.g. example.com)';
  return undefined;
}

export function validateNamespace(ns: string): string | undefined {
  if (!ns) return 'Namespace is required';
  if (!/^[a-z0-9][a-z0-9-]*[a-z0-9]$/.test(ns) && ns.length > 1)
    return 'Must be a valid Kubernetes namespace name';
  if (ns.length === 1 && !/^[a-z0-9]$/.test(ns))
    return 'Must start with a letter or number';
  if (ns.length > 63) return 'Must be 63 characters or fewer';
  if (ns.startsWith('openshift-') || ns.startsWith('kube-'))
    return 'Must not start with openshift- or kube-';
  return undefined;
}

export function validateCIDR(cidr: string): string | undefined {
  if (!cidr) return 'CIDR is required';
  const parts = cidr.split('/');
  if (parts.length !== 2) return 'Must be in CIDR notation (e.g. 10.0.0.0/16)';
  const ipErr = validateIP(parts[0]);
  if (ipErr) return ipErr;
  const prefix = parseInt(parts[1], 10);
  if (isNaN(prefix) || prefix < 0 || prefix > 32) return 'Prefix must be between 0 and 32';
  return undefined;
}

export function validateIP(ip: string): string | undefined {
  if (!ip) return 'IP address is required';
  const octets = ip.split('.');
  if (octets.length !== 4) return 'Must be a valid IPv4 address';
  for (const octet of octets) {
    const n = parseInt(octet, 10);
    if (isNaN(n) || n < 0 || n > 255 || octet !== String(n)) return 'Must be a valid IPv4 address';
  }
  return undefined;
}

export function validateIPInSubnet(ip: string, cidr: string): string | undefined {
  const ipErr = validateIP(ip);
  if (ipErr) return ipErr;
  const cidrErr = validateCIDR(cidr);
  if (cidrErr) return undefined;

  const ipNum = ipToNumber(ip);
  const [netAddr, prefix] = cidr.split('/');
  const netNum = ipToNumber(netAddr);
  const mask = (0xffffffff << (32 - parseInt(prefix, 10))) >>> 0;

  if ((ipNum & mask) !== (netNum & mask)) return 'IP must be within the subnet';
  return undefined;
}

function ipToNumber(ip: string): number {
  const parts = ip.split('.').map(Number);
  return ((parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]) >>> 0;
}

export function validatePullSecret(secret: string): string | undefined {
  if (!secret) return 'Pull secret is required';
  try {
    const parsed = JSON.parse(secret);
    if (!parsed.auths || typeof parsed.auths !== 'object')
      return 'Pull secret must contain an "auths" object';
  } catch {
    return 'Must be valid JSON';
  }
  return undefined;
}

export function validateSSHKey(key: string): string | undefined {
  if (!key) return 'SSH public key is required';
  const trimmed = key.trim();
  if (!trimmed.startsWith('ssh-rsa ') && !trimmed.startsWith('ssh-ed25519 ') && !trimmed.startsWith('ecdsa-sha2-'))
    return 'Must be a valid SSH public key (ssh-rsa, ssh-ed25519, or ecdsa-sha2)';
  return undefined;
}
