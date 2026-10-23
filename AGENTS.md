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

**Current version:** `0.2.1` (package.json / consolePlugin.version).

## Architecture

| Piece | Role |
| --- | --- |
| Console dynamic plugin (`src/`, `console-extensions.json`) | Install wizard and Manage tab (cluster list + detail). PatternFly 6. |
| Plugin nginx container (`Containerfile`, `deploy/`) | Serves webpack `dist/`. ConsolePlugin **oct-shift-on-shift-virt**. |
| Orchestrator (`orchestrator/`) | Go service behind the console proxy. REST API for the UI; watches `NestedCluster` CRs and drives the phases. |
| `NestedCluster` CRD (`deploy/crd.yaml`) | `octools.io/v1alpha1`. Spec, phase, phase history, per-node status, conditions, credential references. |
| Per-cluster namespace (`spec.namespace`, wizard default `<cluster>-ns`) | Master VMs, the bastion VM, the primary Layer2 cUDN, two HAProxy pods, ISO disks, phase Jobs, and (optional) the CSI controller. |
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
| **DeployHAProxy** | Two HAProxy pods (api, ingress) on the cUDN, dual-homed onto the external NAD with the wizard's static IPs. |
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

## Bastion

One VM per cluster (CentOS Stream 9 DataSource): nested DNS, MCS relay, SSH as `core`. With *Graphical bastion* it also installs Xfce + lightdm (EPEL) and a Firefox policy whose homepage is the nested console. The desktop installs from the `sos-desktop-install` oneshot unit, which waits for DNS (resolv.conf is 127.0.0.1 and `dns.py` only arrives with DeployDNS) and retries until it succeeds. USB tablet input keeps the cursor aligned in the VM console.

## Security constraints (do not regress)

- Never change the host cluster's nodes, MachineConfigs or shared operators' configuration.
- Nothing privileged on the host; no new SCC grants beyond the per-cluster `anyuid` bindings, which teardown removes (a leftover binding grants anyuid to whoever recreates that namespace + SA).
- The orchestrator can only grant what it holds (RBAC escalation prevention). `oc auth can-i` false-negatives on `subresources.kubevirt.io`; test a Role with a server-side dry run instead.
- Credentials written by Jobs go straight into Secrets — never `oc apply` a Secret (the token would land in the last-applied annotation) and never print tokens to logs.

## Teardown

`DeleteCluster` removes VMs, Jobs, the API Route (operator namespace), ISO snapshots/PVCs, cluster-scoped bindings (anyuid, CSI pv-reader), the cUDN, then force-deletes the namespace. The cleanup finalizer is released on both deletion paths; a 2-minute resync re-drives any stranded deletion.

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
| `oct-shift-on-shift-virt-orchestrator` | **0.2.1** | `quay.io/cjanisze/oct-shift-on-shift-virt-orchestrator` |
| `sos-installer-helper` | **4.22** | `quay.io/cjanisze/sos-installer-helper` (Jobs: `oc`, `openshift-install`, `python3`) |
| `sos-ssh-helper` | **latest** | `quay.io/cjanisze/sos-ssh-helper` (SSH Jobs; override `SSH_HELPER_IMAGE`) |

## Do-not-break list

Do **not** change unless you are deliberately migrating a live cluster:

- Route: `/community-tools/compute/shift-on-shift-virt`
- Plugin ID `oct-shift-on-shift-virt`
- CRD `nestedclusters.octools.io`

## Known limitations

- Nested ingress/apps are not exposed through host Routes (the nested router dispatches on Host); use DNS or the bastion desktop.
- Masters are not live-migratable until one restart after PostInstall ejects the ISO.
- Live migration of a master holding a hot-plugged nested volume is not yet validated.

## Verify

```bash
yarn install
yarn build
cd orchestrator && go vet ./... && go test ./...
```

Do not `oc apply` or push images unless asked.
