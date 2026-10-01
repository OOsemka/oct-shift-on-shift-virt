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

CreateNamespace → CreateNetwork → CreateSecrets → CreateVMs → CollectIPs → DeployHAProxy → GenerateISO → MountISO → PowerOn → WaitBootstrap → WaitInstall → PostInstall → Ready

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
| **GenerateISO** | Runs `openshift-install agent create image` in a Job (in the orchestrator namespace, which has internet access). Copies kubeconfig/password to cluster namespace. |
| **MountISO** | Converts ISO to block PVC, creates VolumeSnapshot, clones via static VolumeSnapshotContent to cluster namespace, creates per-VM ISO PVCs, attaches as CDROM. |
| **PowerOn** | Starts all VMs. BIOS boot order: rootdisk=1, iso=2. Empty rootdisk causes BIOS fallthrough to ISO on first boot; after RHCOS is written, rootdisk boots. |
| **WaitBootstrap** | Runs `openshift-install agent wait-for bootstrap-complete` in a Job in the cluster namespace. |
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
| `<cluster>-haproxy-api` | 6443, 22623 | API server + Machine Config Server load balancer |
| `<cluster>-haproxy-ingress` | 443, 80 | Ingress load balancer |

Each pod gets its own OVN-assigned cUDN IP. The OCP installer requires `apiVIPs ≠ ingressVIPs`, hence two separate pods. Backends are the master node IPs collected in the CollectIPs phase.

### External access

HAProxy pods are dual-homed: cUDN (primary) + VLAN NAD (secondary via Multus annotation). The VLAN interface gets a static IP from `spec.externalAccess.apiIP` / `ingressIP`. External clients reach the cluster through these VLAN IPs.

### Install-config platform

**Current: `platform: none`**. This avoids the agent-based installer's VIP "already in use" ARP validation, which blocks installation when HAProxy pods already hold the VIP IPs on the cUDN. With `platform: none`, no keepalived is deployed inside the nested cluster.

**Known issue (unresolved):** With `platform: none`, the assisted-service inside the agent ISO may not start or may require additional configuration (DNS for `api-int.<cluster>.<baseDomain>`, or `networking.userManagedNetworking: true`). The last test run showed VMs booted RHCOS but the assisted-service ports (8090, 6443, 22623) were not listening on the rendezvous node. This needs investigation — see "Current blockers" below.

**Previously tried:**
- `platform: baremetal` with `apiVIPs`/`ingressVIPs` set to HAProxy cUDN IPs → **blocked** by `api vips <IP> is already in use in cidr` validation.
- `platform: baremetal` + `loadBalancer.type: UserManaged` → **still blocked** by the same ARP-based VIP validation.

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

## Current blockers (as of 2026-10-01)

1. **Agent-based installer with `platform: none`**: VMs boot RHCOS but the assisted-service does not start (ports 8090/6443/22623 are connection-refused on the rendezvous node). Next steps to investigate:
   - Try adding `networking.userManagedNetworking: true` to the install-config (may be required for `platform: none` in some OCP versions).
   - Set up DNS resolution inside VMs for `api-int.<cluster>.<baseDomain>` pointing to the HAProxy API cUDN IP (via NMState `networkConfig` in agent-config, or a CoreDNS pod in the cUDN namespace).
   - Try `platform: baremetal` without VIPs initially: deploy HAProxy with `replicas: 0`, let agents validate VIPs are free, then scale HAProxy up after install starts. Risk: OVN may assign different IPs on scale-up.
   - Check if `platform: external` (OCP 4.14+) bypasses the VIP ARP check.
   - Check the VNC console of the rendezvous node to see what the agent UI/logs show.

2. **Podman UID mapping on build host**: `podman build` with multi-stage Dockerfile fails with "potentially insufficient UIDs or GIDs available in user namespace" because `/etc/subuid` has `chrisjaniszewski` but the login user is `cjanisze`. Workaround: cross-compile the Go binary locally and use a `FROM scratch` Containerfile that only copies the binary + CA certs.

3. **Quay repo visibility**: `quay.io/cjanisze/sos-orchestrator` is private. The orchestrator deployment uses an `imagePullSecrets` reference to `quay-pull-secret` (created from podman login credentials). To make it public, log in to quay.io web UI → Repository Settings → Make Public.

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

## Test cluster state (test1)

| Field | Value |
| --- | --- |
| CR | `nestedcluster/test1` in `oct-shift-on-shift-virt` |
| Namespace | `sos-test1` |
| cUDN CIDR | `10.200.0.0/24` |
| VMs | `test1-master-0` (.11), `test1-master-1` (.10), `test1-master-2` (.9) — Running |
| HAProxy API | `test1-haproxy-api` (cUDN IP .15) |
| HAProxy Ingress | `test1-haproxy-ingress` (cUDN IP .14) |
| External API IP | `172.21.117.100` (VLAN 2117 via NAD `chrisj/vlan2117`) |
| External Ingress IP | `172.21.117.101` |
| Phase | `WaitBootstrap` (stuck — agent not initializing with `platform: none`) |
| Orchestrator | Scaled to 0 |
| StorageClass | `odf-storagecluster-ceph-rbd-virtualization` |
| OCP version | 4.18 |

To resume: fix the `platform: none` issue (or switch approach), then `oc scale deploy/sos-orchestrator -n oct-shift-on-shift-virt --replicas=1`.

To clean up test1 completely:
```bash
oc scale deploy/sos-orchestrator -n oct-shift-on-shift-virt --replicas=0
# Remove VMs (have finalizers)
for vm in $(oc get vm -n sos-test1 -o name); do
  oc patch $vm -n sos-test1 --type=merge -p '{"metadata":{"finalizers":null}}'
  oc delete $vm -n sos-test1 --force
done
oc delete job --all -n sos-test1 --force
oc delete deploy --all -n sos-test1 --force
oc delete pvc --all -n sos-test1 --force
oc delete datavolume --all -n sos-test1
oc delete ns sos-test1
oc delete clusteruserdefinednetwork test1
oc delete clusterrolebinding sos-haproxy-anyuid-test1
# Orchestrator namespace resources
oc delete secret -n oct-shift-on-shift-virt -l app.kubernetes.io/managed-by=oct-shift-on-shift-virt
oc delete job --all -n oct-shift-on-shift-virt
oc delete pvc --all -n oct-shift-on-shift-virt
oc delete volumesnapshotcontent test1-iso-content
# Reset CR
oc patch nestedcluster test1 -n oct-shift-on-shift-virt --type=merge --subresource=status \
  -p '{"status":{"phase":"Pending","message":"","nodes":null,"phaseHistory":null,"startedAt":null}}'
```

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

## Verify

```bash
yarn install
yarn build
cd orchestrator && go build ./cmd/
```

Do not `oc apply` or push images unless asked.
