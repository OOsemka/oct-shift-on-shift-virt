# Shift-on-Shift Virt (OpenShift Community Tools)

**Community project. Not officially supported by Red Hat.**

Standalone OpenShift Console plugin that deploys full, standalone OpenShift clusters on KubeVirt virtual machines — OpenShift-on-OpenShift, not HyperShift. A wizard collects the cluster definition; an orchestrator drives an agent-based install end to end and hands back a kubeconfig.

- **Plugin ID:** `oct-shift-on-shift-virt`
- **Version:** `0.2.0`
- **Image:** `quay.io/cjanisze/oct-shift-on-shift-virt:0.2.0-ocp4.22` and `:0.2.0-ocp4.21` (`<semver>-ocp<major.minor>`; always publish both minors)
- **Orchestrator image:** `quay.io/cjanisze/oct-shift-on-shift-virt-orchestrator:0.2.0-ocp4.22` and `:0.2.0-ocp4.21` (companion)
- **Git branch:** `main` tracks the newest supported minor (4.22); an `ocp-X.Y` branch is cut only when PatternFly or console APIs diverge

Validated on OpenShift **4.22** (PatternFly 6), installing nested OpenShift 4.22. Open from **Community Tools → Compute** after the storefront ([oct-storefront](https://github.com/OOsemka/oct-storefront)) and this plugin are enabled.

## Features

- **Install wizard** — Cluster Basics, Topology, VM Sizing, Node Naming, Network, External Access, Storage, Credentials (including the optional graphical bastion), then Review & Edit Configs for the generated `install-config` / `agent-config`.
- **Fully automated agent-based install** — no manual steps on the nested nodes; progress, a deployment log and per-node lifecycle are shown on the cluster page.
- **Isolated networking** — each nested cluster gets its own namespace on a Layer2 primary user-defined network, with deterministic node addresses reserved up front.
- **One bastion VM per cluster** — nested DNS, the Machine Config Server relay and SSH access. Optionally an XFCE desktop whose browser opens straight on the nested console.
- **Live-migratable masters** — VMs use the `l2bridge` network binding and RWX Block root disks.
- **Optional API route** — publish the nested API through the host cluster's router, no extra DNS needed.
- **Credentials on the cluster page** — download the kubeconfig, reveal the kubeadmin password, copy an `oc login` command.
- **Nested storage (optional)** — the KubeVirt CSI driver gives the nested cluster a default `kubevirt-csi` StorageClass backed by one host StorageClass, with snapshots when the host has a matching VolumeSnapshotClass. All images come from the nested release payload. A storage quota on the cluster namespace caps host usage.
- **Nested OpenShift Virtualization (optional)** — installed and configured to use nested storage, so the nested cluster can run VMs. Plan for ~180 GiB of extra host storage for its boot images.
- **Clean teardown** — deleting a cluster removes its VMs, disks, snapshots and namespace.

## Routes

- `/community-tools/compute/shift-on-shift-virt` — Install and Manage tabs
- `/community-tools/compute/shift-on-shift-virt/cluster/:name` — cluster detail

## Architecture

| Component | Description |
| --- | --- |
| Console plugin (`src/`, `console-extensions.json`) | Install wizard, cluster list and detail page. PatternFly 6. |
| Plugin nginx (`Containerfile`, `deploy/`) | Serves the webpack `dist/`. ConsolePlugin **oct-shift-on-shift-virt**. |
| Orchestrator (`orchestrator/`) | Go service behind the console proxy (`/api/proxy/plugin/oct-shift-on-shift-virt/orchestrator`). Watches `NestedCluster` resources and drives the install phases. |
| `NestedCluster` CRD (`deploy/crd.yaml`) | `octools.io/v1alpha1`. Spec, phase, phase history, per-node status, credential references. |
| Per-cluster namespace | Master VMs, the bastion VM, the user-defined network, two HAProxy pods (API and ingress VIPs), ISO disks. |
| Edge proxy (optional) | One shared pod in the plugin namespace that routes nested API traffic by TLS SNI. |

### Install phases

`CreateNamespace → CreateNetwork → CreateSecrets → CreateVMs → CollectIPs → MCSRelay → DeployHAProxy → DeployDNS → GenerateISO → MountISO → PowerOn → ConfigureNodes → WaitBootstrap → WaitInstall → PostInstall → ConfigureStorage → ConfigureVirtualization → Ready`

The last two run only when selected in the wizard. They are non-fatal: a failure is reported on the `StorageReady` / `VirtualizationReady` condition and the cluster still reaches Ready.

Each phase is idempotent and the current phase is stored on the resource, so the orchestrator resumes after a restart.

## Requirements

- **OpenShift Virtualization**, with the `l2bridge` network binding plugin registered:
  `oc get kubevirt -A -o jsonpath='{.items[0].spec.configuration.network.binding}'`
- A **StorageClass that binds ReadWriteMany Block volumes** (nested root disks; needed for live migration).
- An **external network** (a NetworkAttachmentDefinition) plus two free static addresses on it, for the nested API and ingress VIPs.
- The `centos-stream9` DataSource in `openshift-virtualization-os-images` (bastion base image; override with `BASTION_BASE_IMAGE`).
- A Red Hat **pull secret** and an SSH public key. The orchestrator generates its own automation key pair; your key stays authorised on the nodes.
- Capacity for the masters you size in the wizard, plus the bastion (1 vCPU / 2 GiB, or 2 vCPU / 4 GiB with the desktop).

## Nested storage and virtualization (optional)

Two checkboxes on the wizard's Storage step:

- **Persistent storage for the nested cluster** — installs the KubeVirt CSI driver in split mode. The nested cluster gets a default `kubevirt-csi` StorageClass. Each nested PVC becomes a DataVolume on the host StorageClass you pick, hot-plugged into the node VM that needs it.
- **OpenShift Virtualization in the nested cluster** — also installs OpenShift Virtualization from `redhat-operators` and points CDI at `kubevirt-csi`. Implies storage.

Requirements on the host:

- KubeVirt **hot-plug volumes** (`HotplugVolumes`) enabled — the default with current OpenShift Virtualization.
- Any StorageClass works for nested RWO volumes (Block or Filesystem). Nested **RWX Block**, and so live migration of VMs inside the nested cluster, is offered only when the selected host class supports **RWX Block** (its CDI StorageProfile says so). A Filesystem-only class such as CephFS gives RWO only: sharing one file-backed disk between two nested nodes fails and pauses the VM that had it.
- Optional: a VolumeSnapshotClass for the same provisioner, which enables nested snapshots and smart clones.

What it does on the host, and nothing more:

- A controller Deployment in the cluster namespace, non-root, with a namespace-scoped Role. The one cluster-scoped grant is `get persistentvolumes`, used for volume expansion.
- The nested cluster can use **only** the selected host class.
- A ResourceQuota on the namespace caps its total storage: the VM disks plus the allowance from the wizard, plus 180 GiB for virtualization boot images.

Results appear on the cluster page (`StorageReady`, `VirtualizationReady`). A failure there never fails the cluster install.

## Accessing a nested cluster

The cluster page shows all three options once the cluster is Ready.

1. **DNS** — create `api.<cluster>.<baseDomain>` and `*.apps.<cluster>.<baseDomain>` A records pointing at the external API and ingress IPs. Everything works natively: `oc`, console, OAuth, valid certificates.
2. **Host route (API only, optional)** — set `EDGE_PROXY_VLAN_IP` and `EDGE_PROXY_NAD` on the orchestrator. Each cluster then gets a passthrough Route to its API. Use `--insecure-skip-tls-verify`: the nested certificate does not carry the route hostname.
3. **Bastion desktop** — enable *Graphical bastion* in the wizard, then open **Virtualization → VirtualMachines → `<cluster>-bastion` → Console**. The bastion resolves the nested zone, so its browser reaches the nested console directly. The login is stored in the `<cluster>-bastion-password` Secret.

## Configuration

Orchestrator environment variables (all optional unless noted):

| Variable | Purpose |
| --- | --- |
| `DNS_MODE` | `vm` runs nested DNS on the bastion. **Required** — other values select a legacy pod-based DNS. |
| `EDGE_PROXY_VLAN_IP`, `EDGE_PROXY_NAD` | Enable the API host route. Static address and `<namespace>/<name>` of the external NAD. Both empty = off. |
| `EDGE_PROXY_IMAGE` | Edge proxy image (default HAProxy). |
| `BASTION_BASE_IMAGE` | DataSource for the bastion (default `centos-stream9`). |
| `SSH_HELPER_IMAGE` | Image for in-namespace SSH jobs. |

## Known limitations

- **Nested ingress/apps are not exposed through host Routes.** The nested router dispatches on the Host header, so use DNS or the bastion desktop for the console and apps.
- **Masters are not live-migratable while the install ISO is attached.** PostInstall ejects it from the VM template, so masters show *Restart required*; after one restart they are migratable.
- A bastion with a second (external) NIC has not been tested for live migration.
- The API route hostname is generated by the router (`<cluster>-api-<namespace>.<appsDomain>`).

## Install

Use the storefront **Add** action, or apply manually (cluster-admin; do not apply unless asked):

```bash
oc apply -f deploy/crd.yaml
oc apply -f deploy/install.yaml
```

Then enable `oct-shift-on-shift-virt` in `consoles.operator.openshift.io/cluster` `spec.plugins`. **Add success is not Ready** — confirm both the plugin and `sos-orchestrator` Deployments are Running.

## Contributing — cluster-portable code

Do **not** hardcode environment-specific values: StorageClass names, VLANs, CIDRs, NAD names, lab domains, node names or cluster URLs. The wizard collects them, or the orchestrator reads them from the cluster; optional overrides default to empty. Agents: `.cursor/rules/oct-no-env-hardcoding.mdc`. Do not commit pull secrets, kubeconfigs or tokens.

## Build

```bash
yarn install
yarn build

cd orchestrator
go vet ./... && go test ./...
CGO_ENABLED=0 go build -o sos-orchestrator ./cmd
```

`orchestrator/build-push-rollout.sh` builds, pushes and rolls out the orchestrator in one step — it pushes images and restarts a deployment, so only run it against your own registry and cluster.
