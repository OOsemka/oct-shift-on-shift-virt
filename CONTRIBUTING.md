# Contributing to Shift-on-Shift Virt

Thanks for helping. Shift-on-Shift Virt is part of OpenShift Community Tools (OCT), a
community project that is not officially supported by Red Hat. Bug fixes, new
options and docs improvements are all welcome. For larger changes, open an
issue first so we can agree on the approach before you spend time on it.

## Setup

Requirements: Node.js 20 and Corepack (it provides the Yarn version pinned in
`package.json`) and Go (version in `orchestrator/go.mod`) for the companion service.

```bash
corepack enable
yarn install
yarn verify    # type check, translation keys, repository rules, production build
```

For `orchestrator/`:

```bash
cd orchestrator && go vet ./... && go test ./...
```

The same checks run in GitHub Actions on every pull request. A pull request
can be merged when they pass and a maintainer has reviewed it.

## Test on your own cluster

Use a lab cluster you can afford to break.

Build your image and push it to a registry you own (make the repository public,
or add a pull secret):

```bash
podman build -t quay.io/<you>/oct-shift-on-shift-virt:dev -f Containerfile .
podman push quay.io/<you>/oct-shift-on-shift-virt:dev
```

**If Shift-on-Shift Virt is already installed from the storefront**, swap in your image:

```bash
oc set image deploy/oct-shift-on-shift-virt -n oct-shift-on-shift-virt plugin=quay.io/<you>/oct-shift-on-shift-virt:dev
```

**Otherwise** deploy the YAML in this repo with your image:

```bash
sed 's|quay.io/cjanisze/oct-shift-on-shift-virt:[^ ]*|quay.io/<you>/oct-shift-on-shift-virt:dev|' deploy/install.yaml | oc apply -f -
```

and enable the plugin in the console (skip if the storefront already enabled it):

```bash
oc patch console.operator cluster --type=json \
  -p '[{"op":"add","path":"/spec/plugins/-","value":"oct-shift-on-shift-virt"}]'
```

The page is at **Community Tools → Compute → Shift-on-Shift Virt** (`/community-tools/compute/shift-on-shift-virt`). After pushing a new `:dev`
image, restart the plugin pod (`oc rollout restart deploy/oct-shift-on-shift-virt -n oct-shift-on-shift-virt`)
and reload the console.

**Orchestrator (`oct-shift-on-shift-virt-orchestrator`)** (`orchestrator/`): build it with
`podman build -t quay.io/<you>/oct-shift-on-shift-virt-orchestrator:dev -f orchestrator/Containerfile orchestrator/` and push it. It runs as Deployment `sos-orchestrator`: `oc set image deploy/sos-orchestrator -n oct-shift-on-shift-virt orchestrator=quay.io/<you>/oct-shift-on-shift-virt-orchestrator:dev`.

Not able to test on a cluster? Say so in the pull request and describe what you
checked instead (for example the page's YAML preview, or output of the functions
you changed). A maintainer tests it before the release.

## Rules

- **PatternFly 6** components only. Do not import PatternFly CSS; the console provides it.
- **Translations:** every UI string goes through `t('...')` and has an entry in
  `locales/en/plugin__oct-shift-on-shift-virt.json`. `yarn check-i18n --fix` adds missing ones.
- **No environment-specific values:** no hostnames, IPs, VLAN IDs, NIC names,
  StorageClasses, cluster URLs or tokens in code or docs. Read live cluster state
  and let the user choose.
- Never change the host cluster's nodes, MachineConfigs or shared operators' configuration.
- Nothing privileged on the host; no SCC grants beyond the per-cluster `anyuid` bindings, which teardown removes.
- The orchestrator can only grant what it holds (RBAC escalation prevention). Test new Roles with a server-side dry run.
- Credentials written by Jobs go straight into Secrets: never `oc apply` a Secret and never print tokens to logs.
- Keep the route, plugin ID and the `nestedclusters.octools.io` CRD unchanged unless you are deliberately migrating.
- **Wording:** this is a community project; do not describe it as official Red Hat software.
- [AGENTS.md](AGENTS.md) describes the design in detail. It is also the guide
  for AI coding assistants, which are welcome; please review what they produce.

## Releases (maintainers)

Leave these to the maintainers. Do not change them in a pull request:

- the version in `package.json` (both places), README, AGENTS.md and the deploy YAML
- container images (`<version>-ocp4.22` and `-ocp4.21`, including the companion image; published tags are never overwritten)
- git tags and the storefront catalog rows

## License

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0), the license of this project.
