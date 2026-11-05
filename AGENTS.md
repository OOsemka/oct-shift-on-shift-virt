# AGENTS.md — Shift-on-Shift Virt (OCT extension)

This is **OpenShift Community Tools (OCT)**, a **community project**, not an official Red Hat supported product. Do not describe it as official Red Hat software.

This repository is the **Shift-on-Shift Virt** ConsolePlugin — deploy standalone OpenShift clusters on KubeVirt virtual machines (full OCP-on-OCP, not HyperShift). It is **not** the OCT storefront. Catalog hubs live in `oct-storefront`.

## Identifiers

| | Value |
| --- | --- |
| Plugin ID / ConsolePlugin / `package.json` `consolePlugin.name` | **`oct-shift-on-shift-virt`** |
| Frontend image | `quay.io/cjanisze/oct-shift-on-shift-virt:<semver>-ocp<major.minor>` |
| Orchestrator image (release) | `quay.io/cjanisze/oct-shift-on-shift-virt-orchestrator:<semver>-ocp<major.minor>` |
| Orchestrator image (dev loop) | `quay.io/cjanisze/sos-orchestrator:latest` (`orchestrator/build-push-rollout.sh`) |
| i18n | `plugin__oct-shift-on-shift-virt` |
| Route | `/community-tools/compute/shift-on-shift-virt` |
| Proxy | `/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator` |

Display name is **Shift-on-Shift Virt**.

**Current version:** `0.3.2` (package.json / consolePlugin.version).

## Architecture

| Piece | Role |
| --- | --- |
| Console dynamic plugin (`src/`, `console-extensions.json`) | Install wizard and Manage tab (cluster list + detail). PatternFly 6. |
| Plugin nginx container (`Containerfile`, `deploy/`) | Serves webpack `dist/`. ConsolePlugin **oct-shift-on-shift-virt**. |
| Orchestrator (`orchestrator/`) | Go service behind the console proxy. REST API for the UI; watches `NestedCluster` CRs and drives the phases. |
| `NestedCluster` CRD (`deploy/crd.yaml`) | `octools.io/v1alpha1`. Spec, phase, phase history, per-node status, conditions, credential references. |
| `IPPool` CRD (`deploy/crd.yaml`) | `octools.io/v1alpha1`. Free external VLAN addresses offered in the wizard (see IP pools). |
| Per-cluster namespace (`spec.namespace`, wizard default `<cluster>-ns`) | Master VMs, the bastion VM (also the API/Ingress load balancer for new clusters), the primary Layer2 cUDN, two HAProxy pods (clusters created before the bastion load balancer only), ISO disks, phase Jobs, and (optional) the CSI controller. |
| Edge proxy (optional) | One shared HAProxy in the operator namespace, SNI-routing nested API traffic behind passthrough Routes. Off unless `EDGE_PROXY_VLAN_IP` + `EDGE_PROXY_NAD` are set. |

## Orchestration phases

`CreateNamespace → CreateNetwork → CreateSecrets → CreateVMs → CollectIPs → MCSRelay → DeployHAProxy → DeployDNS → GenerateISO → MountISO → PowerOn → ConfigureNodes → WaitBootstrap → WaitInstall → PostInstall → ConfigureStorage → ConfigureVirtualization → Ready`

Each phase is idempotent; `.status.phase` is the resume point after an orchestrator restart. To re-run a phase on a live cluster, patch `.status.phase` (status subresource) — the engine starts from there. `status.phase` values are constrained by the CRD enum: a value outside it makes every status write fail and freezes the CR.

| Phase | What it does |
| --- | --- |
| **CreateNamespace / CreateNetwork** | Namespace labelled for a primary UDN (immutable, must be set at creation) and its Layer2 `ClusterUserDefinedNetwork`. |
| **CreateSecrets** | Pull secret, SSH key Secret. The GUI only supplies a public key, so the orchestrator generates an automation key pair; the user's key stays authorised. |
| **CreateVMs / CollectIPs** | Master VMs with the `l2bridge` binding and RWX **Block** root disks (explicit `volumeMode`), so they are live-migratable. Deterministic IPs via preseeded IPAMClaims (masters at base+100..). |
| **MCSRelay** | Creates the single **bastion** VM early, so it occupies base+12 before any pod can take it (an IPAMClaim does not remove an address from OVN's dynamic pool). |
| **DeployHAProxy** | Bastion load balancer (`status.loadBalancer: bastion`, all new clusters): records the bastion's cluster address as the API/Ingress target; no pods. Older clusters: two HAProxy pods (api, ingress) on the cUDN, dual-homed onto the external NAD with the wizard's static IPs. |
| **DeployDNS** | Pushes `dns.py` to the bastion: authoritative for the nested zone, forwards the rest. MCS (22623/22624) is relayed through the bastion on 12262/12264. |
| **GenerateISO / MountISO** | Agent ISO built in a Job in the operator namespace, converted to Block, distributed to the cluster namespace by snapshot handle, attached as a cdrom (RWO by design). |
| **PowerOn / ConfigureNodes** | Boot; an in-namespace Job pins node DNS to the bastion, finds the real rendezvous node and keeps a node-guard loop running. Skipped when the kubeconfig Secret exists (cluster already installed). |
| **WaitBootstrap / WaitInstall** | `openshift-install agent wait-for` Jobs in the cluster namespace. |
| **PostInstall** | Stores kubeconfig + kubeadmin Secrets, ejects the ISO from the VM templates (masters then show *Restart required*; one restart makes them migratable), publishes optional host Routes. |
| **ConfigureStorage** (opt-in) | Nested storage — see below. Non-fatal: result on condition `StorageReady`. |
| **ConfigureVirtualization** (opt-in) | Installs OpenShift Virtualization in the nested cluster, patches the `kubevirt-csi` StorageProfile. Non-fatal: condition `VirtualizationReady`. |

## Nested storage (`spec.storage`, `spec.virtualization`)

Upstream **kubevirt/csi-driver in split mode** (`orchestrator/internal/orchestrator/storage.go`, `manifests/kubevirtcsi/`):

- **Host side** (cluster namespace): controller Deployment (restricted-v2, non-root), SA `kubevirt-csi` with a namespace-scoped Role (DataVolumes, VM/VMI get, add/removevolume, VolumeSnapshots, PVC get/patch). The only cluster-scoped grant is the shared `oct-sos-kubevirt-csi-pv-reader` ClusterRole (`get persistentvolumes`, needed for expansion), bound per cluster.
- **Nested side**: node DaemonSet, `kubevirt-csi` default StorageClass, optional `kubevirt-csi-snapclass`. Applied by a Job in the cluster namespace (the orchestrator pod cannot reach nested IPs).
- **Images**: from the nested cluster's own release payload (`oc adm release info --image-for`), pinned by digest. No third-party registries.
- **Enforcement**: `infraStorageClassEnforcement` is set explicitly (upstream defaults to allow-all): exactly one host class, no host default. Snapshots need **both** the host `storageSnapshotMapping` and the nested class parameter `infraSnapshotClassName`; the host VolumeSnapshotClass is matched by driver (the host default class may belong to another driver).
- **Nodes** have no providerID, so they are annotated `csi.kubevirt.io/infra-vm-name/-namespace` (matched by InternalIP) **before** the node plugin starts.
- **Nested profile**: built from the host StorageProfile of the infra class (`nestedClaimPropertySets`). RWO Block + RWO Filesystem always (the driver never sets an infra volume mode, so they work on any host class); RWX Block only if the host class has RWX Block, because on a Filesystem-only host class a second hotplug of the same disk fails and pauses the first VM. The nested StorageClass carries `rwxBlock: "true"|"false"` for the same decision.
- **Quota**: ResourceQuota `<cluster>-storage` on `requests.storage` = existing non-CSI PVCs + allowance (default 100 GiB) + 180 GiB if virtualization is on.
- **Self-test** Job: PVC → write → new pod (re-attach) → read → delete. `StorageReady=True` only after it passes.
- CDI has no StorageProfile defaults for `csi.kubevirt.io` (upstream kubevirt/containerized-data-importer#4323); the snapshotter RoleBinding is missing upstream (kubevirt/csi-driver#398) — both handled here.

## Network plugin (`spec.network.networkType`, `cniVersion`)

OVNKubernetes (default, empty) or the open-source **Calico** / **Cilium**, chosen on the wizard's Network step together with a version (latest or previous release). Code: `orchestrator/internal/cni/`.

- **Only OCP 4.22+**: the agent installer passes `networkType` to assisted-service, whose `network_type` enum gained `Calico`/`Cilium` in the 4.22 payload (4.18-4.21 accept only OVNKubernetes). `cni.Validate` (called from `ValidateNetwork`) and `networkPluginProblems` in `src/utils/network-check.ts` refuse older versions. Single-node would also need OVNKubernetes; the wizard has no SNO topology.
- **Manifests are embedded**, generated by `hack/update-cni-manifests.sh` (Calico: the release's `ocp.tgz`; Cilium: `helm template` with the OpenShift settings). The offered versions are the version directories under `manifests/<plugin>/`, newest first: keep two. Per-cluster values (`api-int` host, pod CIDR) are `__SOS_*__` placeholders or `*.yaml.tmpl` files we own.
- **GenerateISO** adds them to the `<cluster>-install-config` Secret as `cni-*.yaml` keys (~50 KB) and the Job copies them into `openshift/` (custom manifests) before every `agent create image` attempt.
- **Encapsulation is required**: nested nodes are on an OVN Layer2 network with port security, so packets with pod source addresses are dropped. Calico: our Installation CR sets VXLAN with BGP off (eBPF dataplane as upstream). Cilium: `routingMode=tunnel`, VXLAN.
- **No kube-proxy**: Calico eBPF and Cilium kube-proxy replacement reach the API through `api-int` (the bastion load balancer); both ship `Network.operator.openshift.io` with `deployKubeProxy: false`.
- **Third-party images** (quay.io/tigera, quay.io/calico, quay.io/cilium) are pulled by the nested cluster only; disconnected installs need OVN-Kubernetes. Nothing changes on the host cluster.

## Bastion

One VM per cluster (CentOS Stream 9 DataSource): nested DNS, MCS relay, SSH as `core`. With *Graphical bastion* it also installs Xfce + lightdm (EPEL) and a Firefox policy whose homepage is the nested console. The desktop installs from the `sos-desktop-install` oneshot unit, which waits for DNS (resolv.conf is 127.0.0.1 and `dns.py` only arrives with DeployDNS) and retries until it succeeds. USB tablet input keeps the cursor aligned in the VM console.

## Security constraints (do not regress)

- Never change the host cluster's nodes, MachineConfigs or shared operators' configuration.
- Nothing privileged on the host; no new SCC grants beyond the per-cluster `anyuid` bindings, which teardown removes (a leftover binding grants anyuid to whoever recreates that namespace + SA).
- The orchestrator can only grant what it holds (RBAC escalation prevention). `oc auth can-i` false-negatives on `subresources.kubevirt.io`; test a Role with a server-side dry run instead.
- Credentials written by Jobs go straight into Secrets — never `oc apply` a Secret (the token would land in the last-applied annotation) and never print tokens to logs.

## Network settings guardrails

Two kinds of addresses, easy to mix up: the **internal VIPs** (`spec.network.apiVIP/ingressVIP`, base+10/+11 of the internal cUDN subnet, set automatically by the wizard) and the **external IPs** (`spec.externalAccess.apiIP/ingressIP`, free addresses on a reachable VLAN, always typed in by the user, plus the VLAN `gateway` and `prefixLength`; the bastion takes them, see below).

`crd.(*NestedClusterSpec).ValidateNetwork()` (`orchestrator/internal/crd/validate.go`) refuses settings that can only produce a broken cluster: blank or invalid external IPs (they made the annotation `"ips":["/24"]` and the HAProxy pod never started), external IPs equal to the internal VIPs or inside the internal, pod or service networks, the same address twice, addresses in different VLAN subnets or on the network/broadcast address, no NAD, an internal network smaller than /24 or overlapping the pod/service networks. It runs in the create API (HTTP 400 with the reasons) and at the start of a new cluster's reconcile (so `oc apply` is covered); existing clusters are never re-checked. The wizard runs the same rules (`src/utils/network-check.ts`): Next stays disabled on Network and External Access, and Deploy is blocked while any problem remains. Keep the two in step.

## Bastion load balancer (`status.loadBalancer: bastion`)

New clusters get no HAProxy pods. The bastion VM (`bastion.go`, `bastionlb.go`) has a second NIC (`external`, bridge binding, MAC pinned by `externalMAC(cluster)`) on the selected external network, copied into the cluster namespace (`externalNADInClusterNs`). It works the same on Linux-bridge and OVN localnet networks: the guest sets its own addresses, so the NAD needs no IPAM.

- **cloud-init:** `sos-external.sh` finds the NIC by MAC and uses NetworkManager to set `apiIP/prefix` and `ingressIP/prefix`, `ipv4.never-default`, a table-100 default route via `externalAccess.gateway`, and rules `from <apiIP>/<ingressIP> lookup 100` (rp_filter 2). Replies to any client outside the VLAN leave through the VLAN gateway; everything else keeps the cluster network's default route. Nothing lab-specific: no address ranges are hardcoded.
- **Balancer:** `lb.py` (Python, ships in the cloud image; no package install, works disconnected) is the old MCS relay generalised: 6443/22623/22624 to the masters, 443/80 to every node, 12262/12264 kept. Round-robin, next backend on a refused or timed-out connect (2 s), connect timeout cleared afterwards so watches survive. VM to VM traffic is allowed, so MCS needs no relay hop.
- **Health checks:** forwarding stays L4 pass-through; separate per-backend probes defined in `/etc/sos-lb.json` (`checks` + `frontend_checks`, rendered by `configJSON()`): API `https://<master>:6443/readyz`, MCS `https://<master>:22623/healthz` (also gates 12262) and `http://<master>:22624/healthz` (also gates 12264), all every 5 s, timeout 5 s, rise 2, fall 3; Ingress `http://<node>:1936/healthz/ready` for 80 and 443 (10 s, timeout 5 s, rise 2, fall 2). Only HTTP 200 is healthy, TLS not verified. Listeners bind before the first probe; backends start healthy. Healthy backends are tried first (round-robin), then the others, keeping the per-connection fallthrough. Only transitions go to the `sos-lb` journal (a compact cluster has one node failing 1936 for good). **Fail-open:** with no healthy backend, all are used (logged once), so the gradual agent-based install is never blocked. Logic tested by `bastionlb_py_test.go` (runs lb.py under local `python3`). lb.py only reaches a bastion through cloud-init at VM creation.
- **DNS:** `api`, `api-int` and `*.apps` resolve to the bastion's cluster address (`useBastionAsLoadBalancer` sets `APIVIPCUDN`/`IngressVIPCUDN` to it). With `platform: none` that is the standard single-LB setup.
- **DeployDNS job** also checks that both external addresses are on the bastion and that 6443/22623/22624/443/80 listen (`LB_EXTERNAL_IPS`), and fails with the bastion's journal otherwise.
- **Why not pods:** the pods' default route pointed at the cluster network, so clients outside the VLAN never got replies, and on Linux-bridge NADs (no IPAM) they got no static address at all. Fixing that in pods needs NET_ADMIN; a VM has its own routing table.
- `GetVMIIP` prefers the `default` interface so the external address is never taken for the bastion's cluster address. Clusters without the marker (created earlier) keep the HAProxy-pod path unchanged.

## Lab defaults and the quick compact cluster

One profile of lab defaults in the Secret `sos-lab-defaults` (operator namespace): `pullSecret`, `sshPublicKey`, `settings.json` (`baseDomain`, `ocpVersion`, `storageClassName`, `ipPool`, `masterCores/MemoryGi/DiskGi`). Admins write it from the **Lab Defaults** tab through the Kubernetes API with their own permissions (only changed keys are patched; the pull secret is write-only). The orchestrator only reads it.

- `GET /api/v1/defaults`: what is set (`pullSecretSet`, SSH key + fingerprint, settings); **never the pull secret**.
- `createSecrets`: empty `pullSecret`/`sshKey` in a cluster are filled from the lab defaults (pull secret: spec > `pullSecretRef` > lab defaults > legacy `pull-secret`). A pull secret sent in the spec is copied into the cluster's Secret and then **removed from the NestedCluster object** (`clearSpecPullSecret`); later phases read the Secret.
- **Quick compact cluster** (Install tab card): `GET /api/v1/quick-clusters/preview?name=` returns the exact plan (`crd.BuildQuickSpec`: 3 masters at the default size, two free addresses from the default pool, wizard network defaults) or the reasons it cannot be created; `POST /api/v1/quick-clusters` takes **only the name** and creates through the same path as the wizard (`createNestedCluster`: network checks, address availability, pool membership).
- The wizard blocks Deploy only when there is no pull secret in the form or the lab defaults.
- Profile extras: `bastionGraphical` (desktop on quick clusters by default), `nestedStorage` / `nestedStorageClass` / `nestedStorageAllowanceGi` (KubeVirt CSI) and `nestedVirtualization` (implies storage). The quick request may override only `sshPublicKey`, `graphical`, `desktopUsername` and `desktopPassword` (`crd.ApplyQuickOverrides`; desktop credentials only with a desktop; the password goes to the `-bastion-password` Secret and is cleared from the CR as in the wizard). The card pre-fills the SSH key from the profile and sends it only when edited.
- PowerOn is safe to repeat after an orchestrator restart: VMs that are Starting/Running count as started, and KubeVirt's "VM is already running" is success (`k8s.vmAlreadyStarted`).

Delete: the confirmation needs the cluster name typed. The API's status write before deleting is retried on conflict and never blocks the delete (a conflict during an install used to return "failed to initiate deletion" and delete nothing; `TestDeleteSurvivesStatusConflict`). A delete that interrupts an install is logged as "reconciliation cancelled", not as phase errors.

## IP pools (`IPPool`, IP Pools tab)

`ippools.octools.io` (`deploy/crd.yaml`, namespaced, lives in `oct-shift-on-shift-virt`): `nadName` (always `default/<name>`: pools are shared, so they never reference a NAD in a project namespace; enforced by the CRD pattern, `Validate()` and the form), `prefixLength`, `addresses` (single IPv4 or ranges `a-b`, at most 4096), optional `gateway`/`description`. Usage is never stored: an address is in use while a NestedCluster on the same NAD references it (`crd.ExternalAddressUse`), so deleting a cluster frees its addresses.

- **IP Pools tab** (`src/components/IPPoolsTab/`): lists pools with usage from `GET /api/v1/ippools` (orchestrator), and creates or deletes pools through the Kubernetes API **with the user's own RBAC** (cluster admins by default). The orchestrator only reads pools (`ippools` get/list/watch).
- **Wizard:** External Access has an "Address source" choice. A pool fills `nadName`/`prefixLength` and offers its free addresses (first two preselected); the CR records `externalAccess.ipPool`.
- **Create API:** refuses (409) external addresses another cluster already uses on the same NAD (`CheckAddressesFree`, also for manual entries), and when `ipPool` is set, network/prefix/addresses that do not match the pool (`CheckPool`).

## Teardown

`DeleteCluster` removes VMs, Jobs, the API Route (operator namespace), ISO snapshots/PVCs, cluster-scoped bindings (anyuid, CSI pv-reader), the cUDN, then force-deletes the namespace **only if this cluster created it** (label `oct-shift-on-shift-virt/cluster=<name>`, `k8s.NamespaceOwnedBy`). Create refuses an existing namespace it does not own (API 409 before anything is created; `CreateNamespace` also refuses) and system namespaces (`openshift*`, `kube-*`, `default`, the operator namespace; `crd.ValidateClusterNamespace`). Before this, a CR naming an existing namespace adopted it, and deleting the cluster wiped it. The cleanup finalizer is released on both deletion paths; a 2-minute resync re-drives any stranded deletion.


Deletion bookkeeping (`engine.go`): every reconcile has its own `reconcileRun` entry, and every teardown clears the `deleting` mark when it ends, whichever path started it (Deleting phase or finalizer). A live (non-deleting) event clears a stale mark. Before this, a cluster deleted and re-created with the same name could not be deleted again until its install timed out. Covered by `engine_delete_test.go`.
## Key code files

| File | What it does |
| --- | --- |
| `orchestrator/internal/orchestrator/steps.go` | Phase dispatch and most phase implementations, `DeleteCluster`. |
| `orchestrator/internal/orchestrator/engine.go` | Watch/reconcile loop, finalizer handling, deletion resync. |
| `orchestrator/internal/orchestrator/bastion.go` | Bastion VM, cloud-init user-data, DNS push. |
| `orchestrator/internal/orchestrator/configurenodes.go` | ConfigureNodes Job, node-guard, ops ServiceAccount. |
| `orchestrator/internal/orchestrator/storage.go` | ConfigureStorage / ConfigureVirtualization. |
| `orchestrator/internal/orchestrator/expose.go` | Optional edge proxy + passthrough Routes. |
| `orchestrator/internal/k8s/vm.go` | VM spec (l2bridge, RWX Block disks, ISO attach/eject). |
| `orchestrator/internal/crd/types.go` | Spec/status types, phase constants, `PhaseOrder`. |
| `orchestrator/internal/api/handlers.go` | REST API used by the UI. |
| `orchestrator/internal/cni/` | Calico / Cilium manifests (embedded) and validation; `GET /api/v1/network-plugins`. |

## RBAC

`deploy/install.yaml` ClusterRole `oct-shift-on-shift-virt-orchestrator` is the source of truth; the storefront bundle is generated from it. Storage added: `subresources.kubevirt.io` add/removevolume (update), `persistentvolumes` get, `roles`/`rolebindings`, `resourcequotas`.

## Build

```bash
yarn install && yarn build
cd orchestrator && go vet ./... && go test ./...
```

The build host's rootless podman cannot run multi-stage builds (subuid mapping), so the orchestrator binary is built locally and copied into the image (`Containerfile.prebuilt.local`). Dev loop: `orchestrator/build-push-rollout.sh` (pushes `sos-orchestrator:latest` and restarts the Deployment — only against your own registry/cluster). Test images on the cluster, not with local `podman run`.

## OpenShift and extension versions

Two axes in the catalog: git tag **`v0.x.x`** (semver) and optional branch **`ocp-X.Y`** when PatternFly or APIs diverge. Image tags **always** `<semver>-ocp<major.minor>` (e.g. `0.2.0-ocp4.22`). **Always publish both** `<semver>-ocp4.21` and `<semver>-ocp4.22` (same digest if bits match) — for the plugin **and** the orchestrator. Never overwrite a published tag.

- Git: `main` tracks the newest supported minor (currently **4.22**).
- PatternFly 6 on 4.22; do not mix PF majors on one branch.

## Navigation

This plugin **does not** register the Community Tools section or hubs. Open from the storefront **Compute** tile or `/community-tools/compute/shift-on-shift-virt`.

## No environment-specific hardcoding

Never bake in lab hostnames, StorageClasses, NAD names, VLANs, CIDRs, or similar. All cluster-specific values come from user input in the wizard or are read from the cluster at runtime. See `.cursor/rules/oct-no-env-hardcoding.mdc`.

## PatternFly 6

Import from `@patternfly/react-core` ^6. Do **not** import PatternFly CSS. Prefix new CSS `sos-`. Include `CommunityDisclaimer` on tool pages.

## Companion images

| Image | Current version | Quay repo |
| --- | --- | --- |
| `oct-shift-on-shift-virt-orchestrator` | **0.3.2** | `quay.io/cjanisze/oct-shift-on-shift-virt-orchestrator` |
| `sos-installer-helper` | **4.22** | `quay.io/cjanisze/sos-installer-helper` (Jobs: `oc`, `openshift-install`, `python3`) |
| `sos-ssh-helper` | **1.0.0** | `quay.io/cjanisze/sos-ssh-helper` (SSH Jobs; override `SSH_HELPER_IMAGE`). Must stay public: run `hack/check-images-public.sh` before releases. |

## Do-not-break list

Do **not** change unless you are deliberately migrating a live cluster:

- Route: `/community-tools/compute/shift-on-shift-virt`
- Plugin ID `oct-shift-on-shift-virt`
- CRD `nestedclusters.octools.io`

## Releases and how they reach clusters

- Release: bump `package.json` (both fields), README/AGENTS versions and `deploy/install.yaml` images; publish plugin **and** orchestrator for both minors; tag `v<semver>`. See `.cursor/rules/oct-release-checklist.mdc`.
- Clusters install this tool from the **bundle compiled into the storefront** (`oct-storefront/catalog/deploy/oct-shift-on-shift-virt.yaml` = `deploy/crd.yaml` + `---` + `deploy/install.yaml`). Because the bundle has companions (orchestrator, CRD), every release here needs a storefront release with the regenerated bundle, and clusters must update the storefront first. Storefronts since 1.4.7 hold the update back otherwise.
- Storefront upgrades converge the CRD and RBAC and add new orchestrator env vars, but never change existing env values. A behaviour that must change on existing installs needs a new env var or a migration.
- Dev loop: the dev cluster runs `sos-orchestrator:latest` (`orchestrator/build-push-rollout.sh`, which restarts the orchestrator on the **current `oc` context** — check it first) and the plugin from a separate `:dev` tag. Never push dev builds to a published `<semver>-ocp<minor>` tag.

## Upstream items to track

- **kubevirt/csi-driver #398** (split-mode controller cannot patch VolumeSnapshotContents) is fixed upstream by #399. Keep our workaround ClusterRoleBinding `kubevirt-csi-controller-snapshotter` (in `manifests/kubevirtcsi/nested.yaml.tmpl`) until a csi-driver release with that fix is in the OpenShift payload we pull images from.
- **kubevirt/containerized-data-importer #4323** (no StorageProfile defaults for `csi.kubevirt.io`) is open; the proposal there is RWO defaults plus an `rwxBlock` StorageClass opt-in. ConfigureVirtualization writes the nested StorageProfile itself and the nested StorageClass already carries `rwxBlock`, so nothing changes here if it lands.

## Known limitations

- Nested ingress/apps are not exposed through host Routes (the nested router dispatches on Host); use DNS or the bastion desktop.
- Masters are not live-migratable until one restart after PostInstall ejects the ISO.
- Live migration of a master holding a hot-plugged nested volume, and online resize through the automated install, are not yet validated (milestone M2 in the maintainers' plan; expected to work, not tested).

## Verify

```bash
yarn install
yarn verify   # ts-check, check-i18n, check-repo, build (same as CI)
cd orchestrator && go vet ./... && go test ./...
```

`yarn check-i18n --fix` adds missing translation keys. Contributor workflow and the maintainer-only release steps: [CONTRIBUTING.md](CONTRIBUTING.md).

Do not `oc apply` or push images unless asked.
