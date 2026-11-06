#!/bin/bash
# End-to-end check of the nested cluster's pod network, run once after the
# install (PostInstall), whatever the network plugin. Non-fatal: the result
# lands on the NetworkReady condition. It exists because a cluster can reach
# Ready with a broken network: on a Cilium test install CoreDNS had lost its
# API watch, so new services never resolved, while every operator was Available.
#
# Checks: all nodes Ready; network operator Available, not Degraded; a client
# pod reaches a server pod on another node; a service created now resolves
# by name and answers; the kubernetes API service answers /readyz.
# Env: VIP DOMAIN. Mounts: /nested/kubeconfig, /ps, /m (lib.sh, this script).
. /m/lib.sh
NS=sos-network-selftest
cleanup() { $N delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

# --- 1. nodes and the network operator ---
notready=$($N get nodes --no-headers | awk '$2 !~ /^Ready/ {print $1}' | tr '\n' ' ')
[ -z "$notready" ] || fatal "nodes not Ready: $notready"
avail=$($N get clusteroperator network -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')
degr=$($N get clusteroperator network -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}')
[ "$avail" = True ] && [ "$degr" != True ] || fatal "network operator Available=$avail Degraded=$degr"
plugin=$($N get network.config cluster -o jsonpath='{.status.networkType}')
echo "OK nodes Ready, network operator Available (networkType ${plugin:-unknown})"

# --- 2. image: Red Hat network-tools from the nested release payload ---
REL=$($N get clusterversion version -o jsonpath='{.status.desired.image}')
IMG=$(oc adm release info "$REL" -a /ps/.dockerconfigjson --image-for=network-tools 2>/dev/null || true)
case "$IMG" in *@sha256:*) ;; *) fatal "no network-tools image in the nested payload";; esac

# --- 3. two schedulable nodes (workers, or masters on a compact cluster) ---
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
# Created only now, so resolving it proves cluster DNS still follows the API.
$N -n "$NS" expose pod server --name=selftest --port=80 --target-port=8080 >/dev/null

code() { $N -n "$NS" exec client -- curl -sk -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null; }
http_ok() { [ "$(code "$1")" = 200 ]; }

# --- 4. the checks ---
http_ok "http://${SRV_IP}:8080/" \
  || fatal "pod-to-pod failed: client on $CLI_NODE cannot reach the server pod ${SRV_IP} on $SRV_NODE"
echo "OK pod-to-pod across nodes ($CLI_NODE -> $SRV_NODE)"

poll 90 http_ok "http://selftest.${NS}.svc.cluster.local/" \
  || fatal "a new service does not resolve or answer by name (selftest.${NS}.svc) within 90s: cluster DNS is not following the API (check the dns-default pods and the network plugin's policy drops)"
echo "OK new service resolves and answers by name"

http_ok "https://kubernetes.default.svc/readyz" \
  || fatal "pods cannot reach the kubernetes API service (https://kubernetes.default.svc/readyz)"
echo "OK kubernetes API service reachable from pods"

echo "SUMMARY ${plugin:-network}: pod-to-pod across nodes, new service DNS, API service OK"
