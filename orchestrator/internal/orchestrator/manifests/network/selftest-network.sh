#!/bin/bash
# End-to-end check of the nested cluster's pod network, run once after the
# install (PostInstall), whatever the network plugin. Non-fatal: the result
# lands on the NetworkReady condition. It exists because a cluster can reach
# Ready with a broken network: on a Cilium test install CoreDNS had lost its
# API watch, so new services never resolved, while every operator was Available.
#
# Checks, in order, each with its own failure message:
#   1. all nodes Ready; network operator Available, not Degraded
#   2. openshift-dns ds/dns-default rolled out, every pod Ready
#   3. a client pod reaches a server pod on another node
#   4. the kubernetes API service answers /readyz by ClusterIP, then by name
#   5. a service created now answers by ClusterIP (service routing)
#   6. its name resolves in the client pod (cluster DNS follows the API)
#   7. it answers HTTP by name
# On any failure a bounded block of "DIAG " lines follows the FATAL line
# (network operator, Cilium status and policy drops, CoreDNS errors,
# dns-default pods); the orchestrator keeps its tail in its log and a short
# form in the condition, since the Job is deleted afterwards.
# Env: VIP DOMAIN. Mounts: /nested/kubeconfig, /ps, /m (lib.sh, this script).
. /m/lib.sh
NS=sos-network-selftest
DNS_SEL=dns.operator.openshift.io/daemonset-dns=default
DNS_WAIT=240 # dns-default may still be rolling out right after the install
SVC_WAIT=180 # window for the service created during the test
plugin=""
cleanup() { $N delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

# dns-default pods, one per line: name node phase ready/containers
dns_status() {
  timeout 20 $N -n openshift-dns get pods -l "$DNS_SEL" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{" "}{.status.phase}{" "}{.status.containerStatuses[*].ready}{"\n"}{end}' 2>/dev/null \
    | awk 'NF>=3 {r=0; for (i=4; i<=NF; i++) if ($i=="true") r++; printf "%s %s %s %d/%d\n", $1, $2, $3, r, NF-3}' || true
}
dns_unready() { dns_status | awk '{split($4, a, "/"); if (a[2]==0 || a[1]!=a[2]) print}'; }
# join stdin lines with " | "
join_lines() { awk 'NR>1 {printf " | "} {printf "%s", $0}'; }

# Cilium agent status and a short policy-drop sample, preferably on the node
# of an unready dns-default pod (where the drops to the API servers show up).
cilium_diag() {
  local node agent=""
  node=$(dns_unready | awk '{print $2; exit}')
  [ -z "$node" ] || agent=$(timeout 20 $N -n cilium get pods -l k8s-app=cilium --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -n "$agent" ] || agent=$(timeout 20 $N -n cilium get pods -l k8s-app=cilium -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -n "$agent" ] || { echo "cilium: no agent pod found"; return 0; }
  echo "cilium-dbg status --brief ($agent):"
  timeout 20 $N -n cilium exec "$agent" -c cilium-agent -- cilium-dbg status --brief 2>&1 | tail -n 5
  echo "cilium policy drops (10s sample on $agent):"
  timeout 12 $N -n cilium exec "$agent" -c cilium-agent -- cilium-dbg monitor --type drop 2>&1 | grep 'Policy denied' | head -n 10
}

# Best-effort evidence printed after a FATAL line, every line prefixed "DIAG ".
# Bounded in time (every call under timeout) and size (lines cut, block
# capped). Most useful last: the orchestrator keeps the tail of the block.
diagnostics() {
  set +e +o pipefail
  {
    echo "---- diagnostics ----"
    echo "network operator:"
    timeout 20 $N get clusteroperator network -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason} {.message}{"\n"}{end}' 2>&1
    local nt=${plugin:-} pods
    [ -n "$nt" ] || nt=$(timeout 20 $N get network.config cluster -o jsonpath='{.spec.networkType}' 2>/dev/null)
    if [ "$nt" = Cilium ] || timeout 20 $N get namespace cilium >/dev/null 2>&1; then cilium_diag; fi
    # CoreDNS errors of the unready dns-default pods (all pods if none is unready)
    pods=$(dns_unready | awk '{print $1}')
    [ -n "$pods" ] || pods=$(dns_status | awk '{print $1}')
    for p in $(echo "$pods" | head -n 4); do
      echo "$p dns log (error|Failed|timeout, last 20):"
      timeout 20 $N -n openshift-dns logs "$p" -c dns --tail=500 2>&1 | grep -E 'error|Failed|timeout' | tail -n 20
    done
    echo "dns-default pods (name node phase ready/containers):"
    dns_status
  } 2>&1 | cut -c1-240 | tail -n 150 | sed 's/^/DIAG /'
}
fatal() { echo "FATAL: $*"; diagnostics; exit 1; }

# --- 1. nodes and the network operator ---
notready=$($N get nodes --no-headers | awk '$2 !~ /^Ready/ {print $1}' | tr '\n' ' ')
[ -z "$notready" ] || fatal "nodes not Ready: $notready"
avail=$($N get clusteroperator network -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')
degr=$($N get clusteroperator network -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}')
[ "$avail" = True ] && [ "$degr" != True ] || fatal "network operator Available=$avail Degraded=$degr"
plugin=$($N get network.config cluster -o jsonpath='{.status.networkType}')
echo "OK nodes Ready, network operator Available (networkType ${plugin:-unknown})"

# --- 2. cluster DNS: dns-default rolled out, every pod Ready ---
# A CoreDNS that lost its API watch stays unready (seen with Cilium policy
# drops to the API servers), so this names the pod before any lookup fails.
dns_ready() {
  local want upd ready
  read -r want upd ready < <($N -n openshift-dns get ds dns-default \
    -o jsonpath='{.status.desiredNumberScheduled} {.status.updatedNumberScheduled} {.status.numberReady}{"\n"}' 2>/dev/null || echo)
  [ -n "${want:-}" ] && [ "$want" -gt 0 ] && [ "${upd:-}" = "$want" ] && [ "${ready:-}" = "$want" ] && [ -z "$(dns_unready)" ]
}
if ! poll "$DNS_WAIT" dns_ready; then
  bad=$(dns_unready | head -n 3)
  first=$(echo "$bad" | awk 'NR==1 {print $1}')
  if [ -n "$first" ]; then
    last=$(timeout 20 $N -n openshift-dns logs "$first" -c dns --tail=5 2>&1 | cut -c1-200 | join_lines || true)
    fatal "dns-default not ready within ${DNS_WAIT}s: $(echo "$bad" | awk '{print $1, $4}' | join_lines) (ready/containers); last dns log lines of $first: ${last:-none}"
  fi
  counts=$($N -n openshift-dns get ds dns-default -o jsonpath='desired={.status.desiredNumberScheduled} updated={.status.updatedNumberScheduled} ready={.status.numberReady}' 2>&1 || true)
  fatal "dns-default not rolled out within ${DNS_WAIT}s: $counts"
fi
echo "OK dns-default rolled out, all pods Ready"

# --- 3. image: Red Hat network-tools from the nested release payload ---
REL=$($N get clusterversion version -o jsonpath='{.status.desired.image}')
IMG=$(oc adm release info "$REL" -a /ps/.dockerconfigjson --image-for=network-tools 2>/dev/null || true)
case "$IMG" in *@sha256:*) ;; *) fatal "no network-tools image in the nested payload";; esac

# --- 4. two schedulable nodes (workers, or masters on a compact cluster) ---
mapfile -t nodes < <($N get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.taints[*].effect}{"\n"}{end}' \
  | awk '$0 !~ /NoSchedule|NoExecute/ {print $1}')
[ ${#nodes[@]} -ge 1 ] || fatal "no schedulable node"
SRV_NODE=${nodes[0]}; CLI_NODE=${nodes[1]:-${nodes[0]}}
[ "$SRV_NODE" != "$CLI_NODE" ] || echo "NOTE only one schedulable node: pod-to-pod is tested on one node"

$N delete namespace "$NS" --ignore-not-found --wait=true --timeout=3m >/dev/null
$N create namespace "$NS" >/dev/null
pod() { # name node command...
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {app: $1}}
spec:
  nodeName: $2
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: t
    image: ${IMG}
    command: ["/bin/sh", "-c", "$3"]
    securityContext:
      allowPrivilegeEscalation: false
      runAsNonRoot: true
      capabilities: {drop: [ALL]}
      seccompProfile: {type: RuntimeDefault}
YAML
}
pod server "$SRV_NODE" "exec python3 -m http.server 8080" | $N apply -f - >/dev/null
pod client "$CLI_NODE" "exec sleep 900" | $N apply -f - >/dev/null
$N -n "$NS" wait --for=condition=Ready pod/server pod/client --timeout=5m >/dev/null \
  || fatal "test pods not Ready in 5m: $($N -n "$NS" get pods --no-headers | tr -s ' ' | tr '\n' ';')"
SRV_IP=$($N -n "$NS" get pod server -o jsonpath='{.status.podIP}')

code() { $N -n "$NS" exec client -- curl -sk -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null; }
http_ok() { [ "$(code "$1")" = 200 ]; }
# resolves_to NAME IP: NAME resolves to IP inside the client pod. getent
# (glibc) is in every RHEL-based image, network-tools included.
resolves_to() { $N -n "$NS" exec client -- getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | grep -qxF "$2"; }

# --- 5. pod-to-pod ---
http_ok "http://${SRV_IP}:8080/" \
  || fatal "pod-to-pod failed: client on $CLI_NODE cannot reach the server pod ${SRV_IP} on $SRV_NODE"
echo "OK pod-to-pod across nodes ($CLI_NODE -> $SRV_NODE)"

# --- 6. the kubernetes API service, by ClusterIP then by name ---
API_CIP=$($N -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}')
poll 60 http_ok "https://${API_CIP}/readyz" \
  || fatal "pods cannot reach the kubernetes API service by ClusterIP (https://${API_CIP}/readyz) within 60s: service routing to the API servers is broken (kube-proxy or the network plugin's replacement, or a policy drop)"
poll 60 http_ok "https://kubernetes.default.svc/readyz" \
  || fatal "the kubernetes API answers by ClusterIP ${API_CIP} but not by name (https://kubernetes.default.svc/readyz) within 60s: cluster DNS does not answer pods"
echo "OK kubernetes API service reachable from pods (ClusterIP ${API_CIP} and by name)"

# --- 7. a service created now: ClusterIP, then DNS, then HTTP by name ---
# Created only now, so resolving it proves cluster DNS still follows the API.
$N -n "$NS" expose pod server --name=selftest --port=80 --target-port=8080 >/dev/null
SVC_CIP=$($N -n "$NS" get svc selftest -o jsonpath='{.spec.clusterIP}')
SVC_NAME="selftest.${NS}.svc.cluster.local"
svc_end=$((SECONDS + SVC_WAIT))
left() { local l=$((svc_end - SECONDS)); if [ "$l" -gt 10 ]; then echo "$l"; else echo 10; fi; }

poll "$(left)" http_ok "http://${SVC_CIP}/" \
  || fatal "service routing broken: the new service's ClusterIP ${SVC_CIP}:80 does not answer within ${SVC_WAIT}s, although its server pod answers directly (kube-proxy or the network plugin's replacement)"
echo "OK new service answers by ClusterIP ${SVC_CIP}"

poll "$(left)" resolves_to "$SVC_NAME" "$SVC_CIP" \
  || fatal "DNS not following the API: the new service answers by ClusterIP ${SVC_CIP}, but ${SVC_NAME} does not resolve to it in a pod within ${SVC_WAIT}s (CoreDNS in dns-default does not see new services: check its API watch and the network plugin's policy drops)"
echo "OK new service name resolves to ${SVC_CIP}"

poll "$(left)" http_ok "http://${SVC_NAME}/" \
  || fatal "the new service resolves to ${SVC_CIP} and answers by ClusterIP, but HTTP by name (http://${SVC_NAME}/) fails within ${SVC_WAIT}s"
echo "OK new service answers by name"

echo "SUMMARY ${plugin:-network}: dns-default Ready, pod-to-pod across nodes, API service, new service routing + DNS OK"
