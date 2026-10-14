# AGENTS.md — Shift-on-Shift Virt (OCT extension)

This is **OpenShift Community Tools (OCT)**, a **community project**, not an official Red Hat supported product. Do not describe it as official Red Hat software.

This repository is the **Shift-on-Shift Virt** ConsolePlugin — deploy standalone OpenShift clusters on KubeVirt virtual machines (full OCP-on-OCP, not HyperShift). It is **not** the OCT storefront. Catalog hubs live in `oct-storefront`.

## Identifiers

| | Value |
| --- | --- |
| Plugin ID / ConsolePlugin / `package.json` `consolePlugin.name` | **`oct-shift-on-shift-virt`** |
| Frontend image | `quay.io/cjanisze/oct-shift-on-shift-virt:<semver>-ocp<major.minor>` |
| Orchestrator image | `quay.io/cjanisze/sos-orchestrator:latest` |
| i18n | `plugin__oct-shift-on-shift-virt` |
| Route | `/community-tools/compute/shift-on-shift-virt` |
| Proxy | `/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator` |

Display name is **Shift-on-Shift Virt**.

**Current version:** `0.1.0` (package.json / consolePlugin.version).

## Architecture

| Piece | Role |
| --- | --- |
| Console dynamic plugin (`src/`, `console-extensions.json`) | Two-tab UI: Install (wizard) and Manage (deployed clusters). PatternFly 6. |
| Plugin nginx container (`Containerfile`, `deploy/`) | Serves webpack `dist/`. ConsolePlugin **oct-shift-on-shift-virt**. |
| Orchestrator sidecar (`orchestrator/`) | Go HTTP service. REST API for cluster lifecycle. Watches `NestedCluster` CRs and drives orchestration phases. |
| NestedCluster CRD (`deploy/crd.yaml`) | `octools.io/v1alpha1`. Tracks spec, status, phase history, per-node status, credentials. |
| Per-cluster namespace (`sos-<clusterName>`) | KubeVirt VMs, cUDN, HAProxy LB pods, ISO PVCs. |

## What this plugin owns

- Main page at `/community-tools/compute/shift-on-shift-virt` with Install and Manage tabs
- Cluster detail at `/community-tools/compute/shift-on-shift-virt/cluster/:name`
- `NestedCluster` CRD (`octools.io/v1alpha1`)
- Orchestrator API at `/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator/api/v1/...`
- Per-cluster namespaces with: KubeVirt VMs, cUDN, HAProxy LB pods

## Orchestration phases

CreateNamespace → CreateNetwork → CreateSecrets → CreateVMs → CollectIPs → DeployHAProxy → DeployDNS → GenerateISO → MountISO → PowerOn → WaitBootstrap → WaitInstall → PostInstall → Ready

Each phase is idempotent. State is recorded in the NestedCluster CR `.status.phase`. On pod restart the reconciler resumes from the last phase. `startReconcile` has a mutex that skips duplicate reconciles for the same cluster key — when patching the CR phase manually, scale the orchestrator to 0 first, patch, then scale to 1.

### Phase details

| Phase | What it does |
| --- | --- |
| **CreateNamespace** | Creates `sos-<name>` namespace with label `k8s.ovn.org/primary-user-defined-network: ""` (immutable, must be set at creation). |
| **CreateNetwork** | Creates `ClusterUserDefinedNetwork` for L2 Primary UDN with the `cudnSubnet` CIDR. Also creates a NAD in the namespace. |
| **CreateSecrets** | Creates pull-secret and SSH key secrets in the cluster namespace. |
| **CreateVMs** | Creates KubeVirt VMs with bridge binding on the default (cUDN) interface. VMs start automatically to get DHCP IPs. |
| **CollectIPs** | Waits for VMIs to become Ready, reads cUDN IPs from `k8s.v1.cni.cncf.io/network-status` annotation, then stops VMs for ISO attachment. |
| **DeployHAProxy** | Deploys **two** HAProxy pods (api + ingress) with cUDN + VLAN dual-homing. Waits for each pod's cUDN IP. Stores those IPs as internal VIPs. |
| **DeployDNS** | Deploys a Python DNS server pod in the target namespace (cUDN). Resolves `api.<cluster>.<domain>` and `api-int.<cluster>.<domain>` to the API HAProxy cUDN IP, `*.apps.<cluster>.<domain>` to Ingress HAProxy cUDN IP. Forwards all other queries to host cluster CoreDNS (172.30.0.10). Listens on port 5353 (Service maps 53→5353). |
| **GenerateISO** | Runs `openshift-install agent create image` in a Job (in the orchestrator namespace, which has internet access). Copies kubeconfig/password to cluster namespace. |
| **MountISO** | Converts ISO to block PVC, creates VolumeSnapshot, clones via static VolumeSnapshotContent to cluster namespace, creates per-VM ISO PVCs, attaches as CDROM. |
| **PowerOn** | Starts all VMs. BIOS boot order: rootdisk=1, iso=2. Empty rootdisk causes BIOS fallthrough to ISO on first boot; after RHCOS is written, rootdisk boots. |
| **WaitBootstrap** | Runs `openshift-install agent wait-for bootstrap-complete` in a Job in the cluster namespace. **Known issue:** Job polls the rendezvousIP from installer state which may not match the actual rendezvous node. The 1-hour timeout often triggers before the nested cluster completes bootstrap. The CR shows "Failed" but the actual install continues. **TODO:** Replace with direct assisted-service API monitoring. |
| **WaitInstall** | Runs `openshift-install agent wait-for install-complete`. |
| **PostInstall** | Extracts kubeconfig and kubeadmin password, stores in CR status. |
| **Ready** | Terminal state. |

## Networking architecture

### Primary cUDN (Cluster User-Defined Network)

VMs live on a **Primary L2 UDN** — OVN replaces the default cluster network for all pods in the `sos-<name>` namespace. This means:

- All pods (VMs, HAProxy, jobs) get an IP on the cUDN subnet (e.g. `10.200.0.0/24`).
- Pods **cannot** reach ClusterIP services (those live on the default cluster network).
- Pods **can** reach the internet via OVN egress NAT.
- The namespace label `k8s.ovn.org/primary-user-defined-network: ""` must be set **at namespace creation** — it is immutable.

**CIDR**: Currently `10.200.0.0/24` for test clusters. Do NOT use CIDRs that belong to existing VLANs (e.g. `172.21.81.0/24` is VLAN 2117).

### Internal LB (HAProxy)

OVN port security blocks keepalived gratuitous ARPs inside the cUDN — a traditional VIP will never work. Instead, the orchestrator deploys **two HAProxy pods** in the cUDN namespace:

| Pod | Ports | Purpose |
| --- | --- | --- |
| `<cluster>-haproxy-api` | 6443 | API server load balancer |
| `<cluster>-haproxy-ingress` | 443, 80 | Ingress load balancer |

Each pod gets its own OVN-assigned cUDN IP. The OCP installer requires `apiVIPs ≠ ingressVIPs`, hence two separate pods. Backends are the master node IPs collected in the CollectIPs phase.

### External access

HAProxy pods are dual-homed: cUDN (primary) + VLAN NAD (secondary via Multus annotation). The VLAN interface gets a static IP from `spec.externalAccess.apiIP` / `ingressIP`. External clients reach the cluster through these VLAN IPs.

### Port 22623 (MCS) — blocked on cUDN, not needed for agent-based install

OpenShift host nftables rules block TCP port 22623/22624 for non-service-network traffic. This prevents any pod on the cUDN from connecting to port 22623 on VM or pod IPs. The block is outbound only (pods can bind/listen on 22623 but connections to cUDN addresses are rejected).

This does NOT affect agent-based installs because the assisted-service (port 8090) handles all ignition delivery. Port 22623 is only needed post-install for adding new worker nodes.

HAProxy API config should NOT include a 22623 frontend/backend during initial install. Add it later if workers need to be added.

### DNS for nested cluster VMs

VMs get DNS from OVN-K DHCP (host cluster CoreDNS at 172.30.0.10). The nested cluster domain (e.g. `*.chris-sos-odf.chris.lab`) is unknown to cluster CoreDNS.

The orchestrator deploys a Python DNS pod in the target namespace that:
- Resolves `api.<cluster>.<domain>` → API HAProxy cUDN IP
- Resolves `api-int.<cluster>.<domain>` → API HAProxy cUDN IP
- Resolves `*.apps.<cluster>.<domain>` → Ingress HAProxy cUDN IP
- Forwards all other queries → 172.30.0.10 (host cluster CoreDNS)

VMs must be configured to use this DNS pod. NMState dns-resolver in agent-config does NOT work (OVN-K DHCP overrides it). Current workaround: SSH into VMs post-boot and prepend the DNS pod's cUDN IP to `/etc/resolv.conf`.

### Install-config platform

**`platform: none`** is required. This avoids the agent-based installer's VIP "already in use" ARP validation (HAProxy pods hold the VIP IPs on the cUDN before the installer checks). With `platform: none`, no keepalived is deployed inside the nested cluster.

The install-config overrides from the UI wizard use `<PULL_SECRET>` and `<SSH_KEY>` placeholders. The orchestrator replaces these with actual values during GenerateISO. The agent-config is always generated by the orchestrator (not from UI overrides) because it requires DHCP-assigned IPs, MACs, and rendezvousIP that are only known after CollectIPs.

### Boot order (self-service, no runtime management)

VMs are created with `rootdisk bootOrder=1, iso bootOrder=2`. On first boot the rootdisk is empty, so BIOS falls through to the ISO. After the agent writes RHCOS to the rootdisk and reboots, the rootdisk boots directly. No boot order changes needed at runtime.

### VolumeSnapshot cross-namespace ISO distribution

The agent ISO is generated in the orchestrator namespace (which has internet access for pulling release images). To get it into the cUDN namespace (which has no ClusterIP access and can't run CDI importer pods), the orchestrator:

1. Converts the ISO file PVC to a block-mode PVC (CDI import job).
2. Creates a VolumeSnapshot in the orchestrator namespace.
3. Gets the `snapshotHandle` from the VolumeSnapshotContent.
4. Creates a static VolumeSnapshotContent in the cluster namespace with `source.snapshotHandle`.
5. Creates per-VM PVCs from the snapshot.

**Stale snapshot pitfall:** VolumeSnapshotContent with `deletionPolicy: Retain` survives namespace deletion. On re-runs, the old VSC still exists but its `volumeSnapshotRef` UID doesn't match the new VolumeSnapshot, causing `SnapshotContentMisbound`. Fix: delete the old VSC before retrying MountISO, or have cleanup code delete it.

### CopyNAD netAttachDefName rewrite

When copying a NetworkAttachmentDefinition to a different namespace, the `netAttachDefName` field inside the CNI config JSON must be rewritten from `<sourceNs>/<name>` to `<destNs>/<name>`. Without this, OVN fails with "failed to find NAD key" and the pod stays in ContainerCreating. The `CopyNAD` function in `cudn.go` handles this automatically.

## Known issues and solutions

### "Waiting for all hosts to be ready" stall (UNSOLVED — primary blocker)

The `openshift-install agent wait-for bootstrap-complete` logs show "Waiting for cluster install to initialize" indefinitely, or the assisted-service reports "3 hosts expected, 0 ready, 3 not validated".

**Root cause:** The VMs boot RHCOS and the assisted-service starts on the rendezvous node (port 8090), but host validation fails. The most likely cause is DNS — the VMs cannot resolve `api.cluster.domain` and `api-int.cluster.domain` because:

1. VMs on Primary cUDN get DNS from OVN-K DHCP (172.30.0.10 = host cluster CoreDNS)
2. Host cluster CoreDNS does NOT know about the nested cluster's domain
3. NMState dns-resolver in agent-config does NOT work (OVN-K DHCP overrides it)
4. The orchestrator deploys a DNS pod on the cUDN, but VMs are not configured to use it
5. There is no automated way to update VM `/etc/resolv.conf` before the assisted-service validates hosts

**What works manually:** SSH into VMs after boot and prepend the DNS pod's cUDN IP to `/etc/resolv.conf`. After this, DNS resolves and the install progresses (test1 got 2/3 masters operational, chris-sos-odf reached 55% install / 25+ operators Available).

**What's needed for automation:**
- Option A: Add a post-PowerOn phase that waits for SSH on all VMs, then configures DNS via SSH. Requires the orchestrator to have SSH client capability (Go `golang.org/x/crypto/ssh` or exec `ssh`) and access to the SSH private key.
- Option B: Bake DNS config into the agent ISO via ignition customization (additional manifests or custom ignition). The agent-based installer supports this but the format needs investigation.
- Option C: Use a systemd unit in the agent-config's `networkConfig` that modifies resolv.conf. May work even if NMState IP config is overridden.
- Option D: Configure host cluster CoreDNS to forward the nested cluster domain to our DNS service (works but modifies shared cluster resources — risky).

**Additional DNS problems confirmed (latest run):**
- DNS pod listens on port **5353**, not 53. The Kubernetes Service maps 53→5353 via ClusterIP, but VMs connect directly to the pod's cUDN IP where only 5353 is open. Fix: either make the DNS pod listen on 53 (needs `NET_BIND_SERVICE` capability or `securityContext.capabilities`), or have VMs query port 5353.
- DNS responses may be malformed (`dig` reports "malformed message packet"). The Python DNS script's `struct.pack` format may need further fixes beyond the `">HHHH"` → `">HHHHH"` change. Consider replacing the custom Python DNS with a battle-tested solution (CoreDNS sidecar, dnsmasq binary).
- Removing ALL NMState from agent-config also removed the DNS server entry. A minimal NMState block with ONLY `dns-resolver` (no static IPs) should be tested — it may survive OVN-K DHCP even if static IPs don't.

**Recommended approach:** Option A (SSH-based DNS config) is the most reliable and doesn't touch shared resources. Add a `ConfigureNodes` phase between PowerOn and WaitBootstrap. Additionally, consider replacing the custom Python DNS script with CoreDNS or dnsmasq for reliable DNS responses.

### Guest agent reports internal VM interfaces (informational)

The QEMU guest agent inside RHCOS VMs reports ALL network interfaces to KubeVirt, including internal ones created by CRI-O/podman for bootstrap containers:
- `podman0` (bridge, 10.88.0.1) — podman's default bridge for containers
- `veth0` — veth pair endpoint connecting containers to podman0

These appear in `vmi.status.interfaces` alongside the real NIC (`enp1s0`). They are NOT host-level interfaces and were not created by the orchestrator. Their presence confirms bootstrap containers are running inside the VM.

### VMs on Primary cUDN cannot reach ClusterIP services (CONFIRMED)

KubeVirt VMs bridged to a Primary cUDN have NO access to the Kubernetes service network (172.30.0.0/16). This means:
- VMs cannot reach CoreDNS at 172.30.0.10 (even though OVN-K DHCP provides this as the DNS server)
- VMs cannot reach any ClusterIP service
- VMs CAN reach other pods/VMs on the cUDN via their cUDN IPs
- VMs CAN reach the internet via OVN egress NAT

Regular pods in the cUDN namespace CAN reach ClusterIP services via their secondary interface (eth0). This is why our DNS pod (a regular pod) can forward queries to 172.30.0.10 but VMs cannot query 172.30.0.10 directly.

The DNS pod acts as a bridge: VMs → DNS pod (cUDN IP) → 172.30.0.10 (via pod's eth0) → cluster CoreDNS.

### Port 22623 blocked cluster-wide (RESOLVED — not a blocker)

OpenShift host nodes have nftables rules that reject TCP SYN to ports 22623 and 22624 for traffic outside the service/pod CIDR. This means:
- Any pod on the cUDN **cannot** make outbound TCP connections to port 22623 on cUDN addresses
- HAProxy pods cannot proxy port 22623 to VM backends
- This is a **security feature** preventing MCS (Machine Config Server) impersonation

**Resolution:** Port 22623 is NOT needed for agent-based installs. The assisted-service (port 8090) handles all initial ignition delivery. MCS is only needed post-install when adding new nodes. The HAProxy should proxy 6443 (API) and optionally 443/80 (ingress). Port 22623 can be omitted from HAProxy config during initial install.

**Verified:** Host nftables rules on every node contain `tcp dport 22623 reject` and `tcp dport 22624 reject` in OUTPUT and FORWARD chains (exempting service/pod CIDRs). This causes TCP RST for any pod-to-pod or pod-to-VM connection on these ports across the cUDN. The block is at the host kernel level, not OVN.

### rendezvousIP mismatch (WORKAROUND — detect dynamically)

The agent-based installer's `set-node-zero.sh` checks if the local IP matches `rendezvousIP` with only a 30-second timeout. On cUDN with DHCP, the IP may not be assigned within that window, causing the wrong node to become the rendezvous.

**NMState static IPs don't work:** OVN-K DHCP overrides NMState `networkConfig` on Primary cUDN networks. The DHCP lease from OVN always wins.

**Solution (TODO):** After PowerOn, poll all node IPs for port 8090. Whichever responds first IS the actual rendezvous. Use that IP for the WaitBootstrap job's installer state. Don't assume `hosts[0]` is the rendezvous.

### DNS resolution for nested cluster domains (WORKAROUND — needs improvement)

VMs on the cUDN get DNS from OVN-K DHCP (172.30.0.10 = host cluster CoreDNS). CoreDNS doesn't know about `*.chris-sos-odf.chris.lab` or similar nested cluster domains.

**NMState dns-resolver doesn't work:** OVN-K DHCP overrides NMState dns-resolver config on Primary cUDN.

**Current workaround:** Deploy a Python DNS pod in the target namespace. It gets a cUDN IP. After VMs boot, SSH into each VM and prepend the DNS pod's cUDN IP to `/etc/resolv.conf`.

**Problem with current workaround:** The DNS pod's cUDN IP changes on restart (not using IPAMClaim). VMs get stale DNS config.

**Better solutions (TODO):**
1. Reserve a stable cUDN IP for the DNS pod via IPAMClaim annotation
2. Or: configure the agent ISO's ignition to set DNS via a systemd unit
3. Or: use cluster CoreDNS forwarding (but this is a shared resource and risky)

### HAProxy VIP instability (BUG — needs fix)

HAProxy pods get cUDN IPs via DHCP. cUDN persistent IPAM is per-pod (IPAMClaim keyed by pod name). When HAProxy is restarted (new pod), it gets a new IP, breaking DNS records and the install.

**Solution (TODO):** Use IPAMClaim annotations on HAProxy pods to reserve stable IPs, or use `excludeSubnets` on the cUDN to carve out a range for VIPs and assign them statically via Multus annotation.

### WaitBootstrap false failure (BUG — needs fix)

The `openshift-install agent wait-for bootstrap-complete` job has two problems:
1. It polls the rendezvousIP from the installer state, which may not match the actual rendezvous
2. It has a 1-hour timeout that's often too short for nested cluster installs

Both sos-test1 and chris-sos-odf showed the CR as "Failed" while the actual installation progressed successfully.

**Solution (TODO):** Replace or supplement the wait-bootstrap job with direct monitoring:
- Poll all node IPs for port 8090 (assisted-service) to find the actual rendezvous
- Query the assisted-service API for cluster/host installation progress
- Monitor the cluster API (6443) via HAProxy for readiness
- Increase timeout or make it configurable

### Podman UID mapping on build host (WORKAROUND)

`podman build` with multi-stage Dockerfile fails with "potentially insufficient UIDs or GIDs available in user namespace". Workaround: cross-compile Go binary locally, use `FROM scratch` Containerfile. See Build section.

## Key code files

| File | What it does |
| --- | --- |
| `orchestrator/internal/orchestrator/steps.go` | All phase implementations. `deployHAProxy` creates two HAProxy deployments + waits for cUDN IPs. `generateISO` runs the installer job. `generateHAProxyConfigAPI/Ingress` build HAProxy configs with master IPs as backends. |
| `orchestrator/internal/orchestrator/engine.go` | Reconcile loop. Reads phases from `PhaseOrder`, runs them sequentially. Mutex prevents duplicate reconciles. |
| `orchestrator/internal/k8s/cudn.go` | `CreateCUDN`, `CopyNAD` (with netAttachDefName rewrite), `WaitForDeploymentCUDNIP` (reads cUDN IP from network-status annotation), `parseCUDNIP`. |
| `orchestrator/internal/k8s/vm.go` | `CreateVM` (bridge binding on default interface), `AddISOToVM` (rootdisk bootOrder=1, iso bootOrder=2), `StopVM`, `StartVM`. |
| `orchestrator/internal/k8s/snapshot.go` | VolumeSnapshot CRUD for cross-namespace ISO cloning. `CreatePVCFromSnapshot` sets `volumeMode: Block`. |
| `orchestrator/internal/k8s/secrets.go` | Secret CRUD. `createSecret` now updates if already exists (was silently skipping, causing stale install-configs). |
| `orchestrator/internal/installer/config.go` | `GenerateInstallConfig` (currently `platform: none`), `GenerateAgentConfig`. |
| `orchestrator/internal/crd/types.go` | `NestedClusterSpec`, `NestedClusterStatus`, `NodeStatus`, phase constants and `PhaseOrder`. |

## Deployment history

### test1 (sos-test1-ns)

| Field | Value |
| --- | --- |
| CR | `nestedcluster/test1` in `oct-shift-on-shift-virt` |
| Namespace | `sos-test1-ns` |
| cUDN CIDR | `10.200.0.0/24` |
| OCP version | 4.22.14 |
| Phase (CR) | `Failed` (WaitBootstrap timeout) |
| **Actual state** | **2 of 3 masters operational** — bootstrap completed, CVO progressing |
| Outcome | Proved agent-based install works on cUDN. Port 22623 block discovered. master-0 never transitioned from bootstrap to regular node. |

### chris-sos-odf (chris-sos-odf-ns) — multiple attempts

| Field | Value |
| --- | --- |
| CR | `nestedcluster/chris-sos-odf` in `oct-shift-on-shift-virt` |
| Namespace | `chris-sos-odf-ns` |
| cUDN CIDR | `10.100.0.0/16` |
| OCP version | 4.22 |
| StorageClass | `odf-storagecluster-ceph-fast-pool-rbd` |
| Phase (CR) | `Failed` (WaitBootstrap timeout) on all attempts |
| **Best result** | 55% install, 25+ of 34 operators Available, 3-member etcd quorum |
| **Current status** | Stuck at "Waiting for all hosts to be ready" (DNS not configured on VMs) |

**Attempts:**
1. First attempt: discovered port 22623 blocked, rendezvousIP mismatch, DNS script struct.pack bug
2. Second attempt (with NMState fix): NMState static IPs and dns-resolver overridden by OVN-K DHCP
3. Third attempt (with fixes): No MCS in HAProxy, no NMState, dynamic rendezvous detection. Same DNS stall.
4. Manual SSH fix during attempt 2: configured DNS on VMs manually → install progressed to 55%, 25+ operators

**Primary unsolved blocker:** No automated way to configure DNS on VMs before assisted-service host validation.

## RBAC

The orchestrator ServiceAccount needs these permissions (some were applied via `oc patch` and should be persisted in deploy manifests):

- `snapshot.storage.k8s.io` — `volumesnapshots`, `volumesnapshotcontents` (create, get, list, watch, delete)
- `kubevirt.io` — `virtualmachines`, `virtualmachineinstances` (full CRUD + subresources start/stop)
- `k8s.ovn.org` — `clusteruserdefinednetworks` (create, get, delete)
- `k8s.cni.cncf.io` — `network-attachment-definitions` (create, get, list)
- `batch` — `jobs` (create, get, list, watch, delete)
- `apps` — `deployments` (create, get, list, delete)
- `""` — `namespaces`, `secrets`, `serviceaccounts`, `persistentvolumeclaims`, `pods` (full CRUD)
- `rbac.authorization.k8s.io` — `clusterrolebindings` (create, delete — for HAProxy anyuid SCC)
- `cdi.kubevirt.io` — `datavolumes` (create, get, list)

## Build (orchestrator)

The build host has a podman UID mapping issue. Use this workaround:

```bash
cd orchestrator
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o sos-orchestrator ./cmd/
# Copy CA certs for the scratch image
cp /etc/pki/tls/certs/ca-bundle.crt ./ca-certificates.crt
podman build --no-cache -t quay.io/cjanisze/sos-orchestrator:latest -f Containerfile.scratch .
podman push quay.io/cjanisze/sos-orchestrator:latest
```

The `Containerfile.scratch` is a `FROM scratch` image that just copies the binary + CA certs. The multi-stage `Containerfile` doesn't work due to the UID mapping issue.

## OpenShift and extension versions

Two axes in the catalog: git tag **`v0.x.x`** (semver) and optional branch **`ocp-X.Y`** when PatternFly or APIs diverge. Image tags **always** `<semver>-ocp<major.minor>` (e.g. `0.1.0-ocp4.22`). **Always publish both** `<semver>-ocp4.21` and `<semver>-ocp4.22` (same digest if bits match).

- Git: `main` tracks the newest supported minor (currently **4.22**).
- PatternFly 6 on 4.22; do not mix PF majors on one branch.

## Navigation

This plugin **does not** register the Community Tools section or hubs. Open from the storefront **Compute** tile or `/community-tools/compute/shift-on-shift-virt`.

## No environment-specific hardcoding

Never bake in lab hostnames, StorageClasses, NAD names, VLANs, CIDRs, or similar. All cluster-specific values come from user input in the wizard or are read from the cluster at runtime. See `.cursor/rules/oct-no-env-hardcoding.mdc`.

## PatternFly 6

Import from `@patternfly/react-core` ^6. Do **not** import PatternFly CSS. Prefix new CSS `sos-`. Include `CommunityDisclaimer` on tool pages.

## Companion images

The deploy bundle references a sidecar image with its own independent version.

| Image | Current version | Quay repo |
| --- | --- | --- |
| `sos-orchestrator` | **0.1.0** | `quay.io/cjanisze/sos-orchestrator` |

## Do-not-break list

Do **not** change unless you are deliberately migrating a live cluster:

- Route: `/community-tools/compute/shift-on-shift-virt`
- Plugin ID `oct-shift-on-shift-virt`
- CRD `nestedclusters.octools.io`

## Next steps for full e2e automation

To achieve fully automated deployment from the OCTools UI:

### P0: DNS configuration on VMs (blocks all installs)
Add a `ConfigureNodes` phase between PowerOn and WaitBootstrap that:
1. Waits for SSH to be available on all VMs (poll port 22)
2. SSHes into each VM using the cluster SSH key
3. Prepends the DNS pod's cUDN IP to `/etc/resolv.conf`
4. Verifies `host api.<cluster>.<domain>` resolves correctly

Requires: Go SSH client (`golang.org/x/crypto/ssh`), SSH private key access (currently only public key is stored in the secret — need to store the private key too, or use `virtctl ssh`).

### P1: Dynamic rendezvous handling
The `detectRendezvousNode()` helper was added but the WaitBootstrap job still uses the installer state from GenerateISO (which has the original rendezvousIP). Need to either:
- Regenerate the installer state with the actual rendezvous IP
- Or replace the `openshift-install agent wait-for` job with direct assisted-service API monitoring

### P1: HAProxy VIP stability
HAProxy pod restarts change the cUDN IP, breaking DNS and the install. Options:
- Use IPAMClaim annotations to reserve IPs (if supported for pods)
- Deploy HAProxy as a VM instead of a pod (VMs have persistent IPAM)
- Add IP change detection and DNS update logic

### P2: DNS pod IP stability
Same issue as HAProxy — DNS pod restarts change its cUDN IP, invalidating VMs' `/etc/resolv.conf`. Options:
- Use IPAMClaim for DNS pod
- Deploy DNS as a process on the rendezvous VM (no separate pod needed)

### P2: WaitBootstrap improvements
- Increase timeout (1hr is too short for nested clusters)
- Monitor assisted-service API (port 8090) for real-time progress
- Monitor cluster API (port 6443) via HAProxy for bootstrap completion
- Don't fail the CR when the job times out if the install is still progressing

### P3: Post-install automation
- Extract kubeconfig and kubeadmin password from the nested cluster
- Configure external DNS/routing for the nested cluster
- Health check the nested cluster (all operators Available, nodes Ready)

## Verify

```bash
yarn install
yarn build
cd orchestrator && go build ./cmd/
```

Do not `oc apply` or push images unless asked.
