# S-O-S architecture options (2026-10-15)

Two open architectural questions, each written up with what is **proven live**
on AZ1 versus what is **inferred**. Nothing here is implemented yet.

Companion to `SOS-E2E-STATUS.md` (run history and defect log). Written after
runs 16/17 reached `Ready` unassisted (3/3 masters, 34/34 ClusterOperators).

---

# Part 1 — Consolidating the helper VMs

Today every nested cluster gets **three** helper VMs in its namespace, all of
them the same small CentOS Stream 9 image:

| VM | Service | Ports | Required? |
| --- | --- | --- | --- |
| `<cluster>-dns-vm` | `dns.py` for the nested zone | 53/udp+tcp | **Yes** |
| `<cluster>-mcs-relay` | TCP relay to masters' MCS | 12262, 12264 | **Yes, conditionally** |
| `<cluster>-jumpbox` | sshd for operators | 22 | No — diagnostics only |

### Why DNS must be a VM with a stable IP
Nothing else resolves `api/api-int/*.apps.<cluster>.<base>`. It must be
claim-stable because GenerateISO bakes `nameserver=<ip>` into the agent ISO as
a kernel argument, and because an unclaimed Deployment pod drifts its cUDN
lease — that drift stranded two masters in an earlier run (`.18 -> .52`).

### Why the MCS relay exists
The hypervisors' nftables reject TCP SYN to 22623/22624 for host- and
pod-originated traffic. HAProxy is a **pod**, so it cannot reach the masters'
MCS directly. The relay is a **VM**, and VM->VM traffic never traverses those
hooks, so it listens on 12262/12264 (not rejected) and forwards to 22623.
The constraint is the *pod->VM hop*, not MCS itself — which is why option 3
below removes the need for the relay entirely.

### The only real obstacle to one VM: ordering
`MCSRelay` runs **before** `DeployHAProxy` deliberately, so HAProxy's first
generated config already contains the MCS backend. A later config change rolls
the HAProxy pod, which changes its cUDN DHCP lease, which silently stales the
DNS records (the lease-drift bug class). But the DNS records *need* that lease,
which only exists *after* HAProxy starts. One VM cannot currently bake both at
cloud-init time.

### Options

**Option 1 — one bastion, DNS records pushed over SSH. (RECOMMENDED FIRST)**
Create `<cluster>-bastion` early (at the current MCSRelay point) running the
relay + sshd + a DNS daemon with empty records; `DeployDNS` then writes
`/etc/sos/dns-records` over SSH and restarts the service. Reuses machinery
already proven in ConfigureNodes (ops SA, `sos-ssh-helper`, cluster key).
*3 helper VMs -> 1.* Saves ~2 vCPU, ~4 Gi RAM, 64 Gi of RBD clones per cluster.
Risk: low. One new moving part (the record push).

**Option 2 — one bastion, HAProxy pods get preseeded IPAMClaims.**
Make the HAProxy VIPs deterministic before anything starts, so the bastion can
bake DNS records at creation. Also kills the lease-drift class permanently.
Status: documented as GA for Layer2 primary UDN but **unproven here**. Live
check today: the only IPAMClaims present are the six auto-created
`<vm>.default` ones; the HAProxy pods get their cUDN IP implicitly with no
claim. An earlier attempt failed only because it used
`k8s.v1.cni.cncf.io/networks` (which OVN rejects for a primary network)
instead of `v1.multus-cni.io/default-network` with `ipam-claim-reference`.

**Option 3 — one bastion that also replaces the HAProxy pods.**
The relay VM already fronts 6443/22623/22624 (commit 2c8c324). Move all TCP
proxying onto the bastion and delete both HAProxy Deployments:
*3 VMs + 2 pods -> 1 VM.* Every hop becomes VM->VM, so the pod->VM:22623
firewall constraint disappears; the VIP is a claim-stable VM address rather
than a drifting pod lease; DNS records are known at plan time; zero HAProxy
reloads. Costs: single point of failure for the nested control-plane VIP, and
the loss of HAProxy health checking (already removed deliberately — the
pod->VM L4 checks flapped and blackholed the VIP, run 12).

**Decision: implement option 1 first, then option 3 once validated.** Not both
at once — a failure in a combined change would not say which half caused it.

### IMPLEMENTED 2026-10-15 — and the SSH push turned out to be unnecessary

Option 1 shipped, but *better* than specified: no record push is needed.

The ordering constraint was never really about the relay VM existing — it was
about HAProxy's first config needing the relay's ADDRESS. Since the
deterministic ipPlan already reserves that address up front (preseeded
IPAMClaim created in CreateVMs, landed in 7cb2347), HAProxy can be pointed at
the bastion before it boots. So:

* `MCSRelay` phase no longer creates anything — it pins
  `status.mcsRelayIP = plan.Bastion` so DeployHAProxy's FIRST config is
  already correct (`reserveBastionIP`).
* `DeployDNS` phase creates ONE `<cluster>-bastion` VM with DNS records AND
  MCS backends AND the SSH key all baked at cloud-init, because by then the
  HAProxy leases are known (`deployBastion`).

Net: **3 helper VMs -> 1**, one IPAMClaim instead of three, no SSH push, no
extra phase, no CRD change, and still zero HAProxy reloads. `dns_vm.go` keeps
only the VIP resolvers and `indentBlock`; `mcsrelay.go` keeps the HAProxy
rewire plus the relay script as a const; `jumpbox.go` is deleted. The bastion
keeps the old mcs-relay offset (base+12) so existing clusters' claims stay
valid. Pod-mode DNS (`DNS_MODE != "vm"`) is untouched as a fallback.

Validated offline before shipping: rendered cloud-config parses as YAML, both
embedded Python programs parse, both SSH keys are present, and the DNS
records/zone/AAAA-fix and relay master IPs are all asserted present.

#### CORRECTION — the SSH push IS needed after all (caught live, first run)

The claim that "the ordering constraint dissolves" was WRONG, and the first
GUI run proved it within three minutes:

```
haproxy-ingress pod lease : 10.100.0.12
bastion IPAMClaim         : 10.100.0.12/16     <- direct collision
```

**A preseeded IPAMClaim does not remove an address from OVN's dynamic pool.**
It only pins the address once it is bound to a running workload. The old
MCSRelay phase kept base+12 safe purely as a side effect of *creating the VM
there* — the VM physically occupied the address before any HAProxy pod
existed. Recording the address in status reserves nothing.

So option 1 is implemented as originally specified:
* `MCSRelay` -> `deployBastionEarly`: creates the bastion (MCS relay + sshd)
  and waits for it, so it owns base+12 before DeployHAProxy runs.
* `DeployDNS` -> `configureBastionDNS`: renders dns.py from the now-known
  HAProxy leases and pushes it over SSH from a Job in the cluster namespace,
  then PROVES the server answers — an A record matching the API VIP *and* a
  prompt NOERROR for AAAA (a dropped AAAA is invisible to getent but hangs
  every AF_UNSPEC client). It also fails loudly if a HAProxy lease has landed
  on the bastion's address.
Still 3 helper VMs -> 1; the cost is one extra Job, not an extra VM.

Note this collision class disappears entirely under option 3: with no HAProxy
pods there is no dynamic lease to collide with, and the DNS records are the
bastion's own address.

---

# Part 2 — Exposing the nested API/ingress via host Services and Routes

The goal: reach the nested cluster's API and ingress through the **host**
cluster's Routes, so no corporate DNS entries are needed.

## What was measured today (all live, on the healthy `sos-nightly` cluster)

1. **A selector-based Service in the UDN namespace works at the API level.**
   Creating a Service with `selector: component=haproxy-api` auto-populated
   EndpointSlices with the HAProxy pod's **default-network** IP and allocated a
   ClusterIP:
   ```
   addresses=["10.131.0.62"]  ready=true  ports=6443   clusterIP=172.30.14.150
   ```
   No manual Endpoints and **no `endpoints/restricted` RBAC** — so the
   privileged grant that run 17 asked for is not actually needed.

2. **…but those endpoints carry no traffic.** From a default-network pod:
   ```
   https://172.30.14.150:6443/healthz  -> 000   (ClusterIP)
   https://10.131.0.62:6443/healthz    -> 000   (pod IP direct)
   nc -z 10.131.0.62 6443              -> CLOSED/filtered
   ping 10.131.0.62                    -> no reply
   ```
   A primary-UDN namespace isolates its pods **inbound**: they can egress via
   eth0, but nothing outside the namespace can reach them on it. So pointing a
   host Service at the HAProxy pods can never work — with or without RBAC. The
   endpoints-restriction error in run 17 was only the first wall.

3. **The nested VLAN addresses are unreachable from the cluster too.**
   From a default-network pod: `172.21.75.103:6443` -> 000,
   `172.21.75.104:443` -> 000.
   From an ingress **node** (routers are `hostNetwork: true`, so this is the
   path a Route would take):
   ```
   ip route get 172.21.75.103 -> via 172.21.66.1 dev br-ex
   curl https://172.21.75.103:6443/healthz -> 000
   ```
   The nodes sit on 172.21.66.0/24; vlan2111 is a separate L2 they have no
   interface on, and the gateway does not forward to it. (The workstation
   reaches 172.21.75.x over the corporate/VPN path, which is not available to
   the cluster.)

**Conclusion: from the host cluster — pods *and* nodes — there is currently no
working path into a nested cluster.** Any Route-based exposure therefore
requires a new bridging component. This is a correction to the earlier design
note that assumed the HAProxy pods' pod-network IPs were a usable bridge.

## Options

**Option E0 — corporate DNS (works today, nothing to build).**
Two A records per cluster:
```
api.<cluster>.<base>     A  <externalAccess.apiIP>       # 172.21.75.x
*.apps.<cluster>.<base>  A  <externalAccess.ingressIP>
```
Everything is native: `oc login`, console, OAuth redirects, and TLS all use the
cluster's real hostnames with valid certs. The wizard's review step already
prints exactly these records. Requires the ability to add DNS entries.

**Option E1 — edge-proxy pod in a normal namespace + Service + Routes.**
Add a small HAProxy/nginx Deployment in `oct-shift-on-shift-virt` (NOT a UDN
namespace, so it is reachable normally), dual-homed:
* `eth0` pod network — reachable by a selector Service and by the hostNetwork
  router;
* `net1` on a CopyNAD'd `vlan2111` — reaches the nested `apiIP`/`ingressIP`.

Then `Service (selector) -> edge proxy -> VLAN -> nested HAProxy -> masters`,
with passthrough Routes in front. Uses only proven primitives (CopyNAD already
exists; selector Services proven above; no privileged RBAC).

Honest scope of what E1 delivers:
* **API: yes.** Passthrough preserves client certificates, so the admin
  kubeconfig works. The nested serving cert has no SAN for the route hostname,
  so either `--insecure-skip-tls-verify`, or add the route hostname via the
  nested `APIServer.spec.servingCerts.namedCertificates` and ship the CA in the
  distributed kubeconfig (clean, more work).
* **Console: only with extra work.** The console redirects to
  `oauth-openshift.apps.<nested>`, which a browser outside cannot resolve. Fix
  by setting the nested `Ingress.spec.componentRoutes` for `console` and
  `oauth-openshift` to the host-side route hostnames plus a serving cert.
* **Arbitrary nested apps: no.** A passthrough Route selects the backend by
  SNI, and the nested router then matches the same hostname. Unless a nested
  app's Route hostname equals the host-side hostname, it returns 503. There is
  no general wildcard rewrite.

**Option E2 — exposure from the bastion VM.** Does not help on its own: the
bastion lives in the cluster namespace (it must, to be on the cUDN), so the
host router still cannot reach it. It would still need E1's edge proxy. A VM in
a normal namespace cannot attach to another namespace's cUDN (multus
cross-namespace attach is blocked).

**Option E3 — NodePort/LoadBalancer.** Same dead end: the endpoints are the
unreachable UDN pods.

## Recommendation
Keep **E0** as the supported path — it is already implemented, needs no new
components, and is the only option where TLS, OAuth and arbitrary apps all work
properly. Build **E1** only if DNS control is genuinely unavailable, and ship it
labelled as what it is: an API-access convenience, with console support
requiring `componentRoutes` and app access out of scope.

Either way, the `endpoints/restricted` grant discussed earlier should **not** be
requested — the selector-Service finding removes the need for it, and the live
ClusterRole should have the three read-only rules only.

---

# Part 3 — API via Route + a graphical bastion (2026-10-15, PROVEN then implemented)

## The proxy question, answered by a live prototype

A proxy in between does work, and I built one end to end before writing any
product code. From my workstation, with no nested DNS and no VLAN access:

```
$ oc --kubeconfig <server: https://sos-nightly-api.apps.az1.vpc.openinfra.lab:443> get nodes
sos-nightly-master-0   Ready   control-plane,master,worker   v1.35.6
sos-nightly-master-1   Ready   control-plane,master,worker   v1.35.6
sos-nightly-master-2   Ready   control-plane,master,worker   v1.35.6
ClusterOperators: total=34 Available=34
```

Chain: `Route (passthrough) -> Service (selector) -> proxy pod -> VLAN ->
nested HAProxy -> masters`.

### Why a proxy is REQUIRED (not just convenient)
* A nested cluster's pods sit in a primary-UDN namespace, which blocks
  inbound from outside the namespace. Measured: default-network pod ->
  haproxy pod IP = no TCP, no ICMP.
* The ingress nodes have no interface on vlan2111 (they are on
  172.21.66.0/24). Measured from an ingress node:
  `curl https://172.21.75.103:6443/healthz` -> 000.
So neither a Service's endpoints nor the hostNetwork router can reach a
nested cluster directly. A pod in a NORMAL namespace, dual-homed onto the
pod network and the VLAN, is reachable from both sides and bridges them.
Cross-namespace NAD reference (`default/vlan2111`) works — no CopyNAD needed.

### Design decisions
* **One SHARED edge proxy, not one per cluster.** vlan2111 has NO IPAM
  (`ipam: <none>`, localnet), so every VLAN attachment needs a STATIC
  address. One proxy per cluster would burn a third VLAN IP per cluster and
  add a wizard field. A single proxy routes by TLS SNI
  (`req_ssl_sni`), so N clusters cost one VLAN IP total and zero wizard
  fields. Per cluster the orchestrator adds only a Route and a backend.
* **Passthrough, not reencrypt.** Passthrough preserves client certificates,
  so the admin kubeconfig works unchanged apart from its server URL.
  Reencrypt would give a trusted host cert but break client-cert auth and
  force token auth, which drags in nested OAuth (see below).
* **Honest TLS caveat:** the nested apiserver presents its own cert
  (`CN=172.30.0.1`, no SAN for the route host), so clients need
  `--insecure-skip-tls-verify`. The clean fix is to add the route hostname
  via the nested `APIServer.spec.servingCerts.namedCertificates`; not done
  yet.

### Why ingress/apps is NOT exposed the same way
Measured against the nested ingress VLAN IP:
```
https://172.21.75.104:443/                                        -> 503
  same request + Host: console-openshift-console.apps.<nested>    -> 200
```
The nested router dispatches on Host/SNI. A passthrough Route carries the
HOST-side hostname, which the nested router does not know -> 503. Serving
apps would require the proxy to terminate TLS and rewrite Host, and even then
the console redirects the browser to `oauth-openshift.apps.<nested>`, which
does not resolve outside. That needs nested
`Ingress.spec.componentRoutes` + a serving cert. Out of scope here, and the
reason the graphical bastion is the better answer for console access.

## The graphical bastion

The bastion already sits on the cUDN AND runs the nested DNS, so from inside
it every nested name resolves and the console works natively — no routes, no
hostname rewriting, no OAuth problem. Adding a desktop turns it into the
console access path.

**Access method: the OpenShift console's own VM console (KubeVirt VNC).**
This needs no network exposure at all — it goes through the KubeVirt API, not
through the cUDN — so there is no extra VLAN IP, no Route, and no new attack
surface. The operator is already in the OpenShift UI, so it is one click from
the VM list.

Decisions:
* XFCE (X11) rather than Wayland: VNC through the KubeVirt console is an X11
  path, and XFCE is the lightest desktop that gives a real browser.
* The desktop install runs in the BACKGROUND from cloud-init. It is a few
  hundred MB over UDN egress and must never hold up the DeployDNS phase.
* RAM goes 2Gi -> 4Gi and 1 -> 2 vCPU when graphical is enabled, otherwise a
  browser is unusable.
* resolv.conf on the bastion is pinned to 127.0.0.1 so the browser resolves
  the nested zone from the DNS server running on the same box.
* Credentials come from the wizard. The password is stored as a SHA-512 crypt
  hash in the cloud-init secret, never as plaintext; when not supplied, one is
  generated and surfaced in a Secret rather than silently set to a default.

Net effect, side by side with external DNS:
1. `oc` against the nested API through the host Route (no DNS needed).
2. The nested console in a browser on the bastion, via the OpenShift VM console.
3. Corporate DNS to the VLAN VIPs still works exactly as before.
