#!/bin/bash
# Nested side of ConfigureStorage. Runs as a Job in the cluster's host
# namespace: that is the only place that can reach the nested API (the
# orchestrator pod cannot — the nested cluster lives on an isolated network).
#
# Env: CLUSTER HOST_NS DOMAIN VIP INFRA_SC INFRA_VSC(optional) RWX_BLOCK(true|false) VM_MAP("ip=vm ...")
# Mounts: /nested/kubeconfig (nested admin), /ps/.dockerconfigjson, /m (templates)
#
# Prints "IMAGE VAR=ref" lines for the orchestrator. Never prints the token.
. /m/lib.sh

# --- 1. images: Red Hat builds from the nested cluster's own release payload ---
REL=$($N get clusterversion version -o jsonpath='{.status.desired.image}')
[ -n "$REL" ] || fatal "cannot read the nested release image"
for pair in IMG_DRIVER=kubevirt-csi-driver IMG_PROVISIONER=csi-external-provisioner \
            IMG_ATTACHER=csi-external-attacher IMG_RESIZER=csi-external-resizer \
            IMG_SNAPSHOTTER=csi-external-snapshotter IMG_REGISTRAR=csi-node-driver-registrar \
            IMG_LIVENESS=csi-livenessprobe IMG_CLI=cli; do
  var=${pair%%=*}; name=${pair#*=}
  ref=$(oc adm release info "$REL" -a /ps/.dockerconfigjson --image-for="$name" 2>/dev/null || true)
  case "$ref" in *@sha256:*) ;; *) fatal "no payload image for $name";; esac
  export "$var=$ref"
  echo "IMAGE $var=$ref"
done

# --- 2. node -> VM annotations, BEFORE the node plugin starts (read once at start) ---
# Nested nodes have no providerID (platform none); match by InternalIP, not name.
changed=0
while read -r node ip; do
  [ -n "$node" ] || continue
  vm=""
  for pair in $VM_MAP; do [ "${pair%%=*}" = "$ip" ] && vm=${pair#*=}; done
  if [ -z "$vm" ]; then echo "WARN node $node ($ip): no matching VM — volumes cannot attach there"; continue; fi
  cur=$($N get node "$node" -o jsonpath='{.metadata.annotations.csi\.kubevirt\.io/infra-vm-name}')
  if [ "$cur" != "$vm" ]; then
    $N annotate node "$node" --overwrite "csi.kubevirt.io/infra-vm-name=$vm" "csi.kubevirt.io/infra-vm-namespace=$HOST_NS" >/dev/null
    changed=1
  fi
  echo "OK node $node -> VM $HOST_NS/$vm"
done < <($N get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')

# --- 3. render and apply ---
render() {
  python3 -c 'import os,sys,string;sys.stdout.write(string.Template(open(sys.argv[1]).read()).safe_substitute(os.environ))' "$1" > "$2"
  # safe_substitute leaves unknown ${VAR}s alone (the DaemonSet legitimately uses $(KUBE_NODE_NAME)),
  # so check explicitly that nothing of ours is left unrendered.
  if grep -qE '\$\{[A-Z_]+\}' "$2"; then fatal "unrendered placeholder in $1: $(grep -oE '\$\{[A-Z_]+\}' "$2" | sort -u | tr '\n' ' ')"; fi
}
render /m/nested.yaml.tmpl /tmp/nested.yaml
# StorageClass parameters are immutable: recreate the class when the host
# class or the RWX Block decision changed. Bound volumes are unaffected.
cur=$($N get storageclass kubevirt-csi -o jsonpath='{.parameters.infraStorageClassName}/{.parameters.rwxBlock}' 2>/dev/null || true)
if [ -n "$cur" ] && [ "$cur" != "${INFRA_SC}/${RWX_BLOCK}" ]; then
  $N delete storageclass kubevirt-csi >/dev/null
  echo "OK recreating StorageClass kubevirt-csi ($cur -> ${INFRA_SC}/${RWX_BLOCK})"
fi
$N apply -f /tmp/nested.yaml >/dev/null
echo "OK nested driver objects applied"
if [ -n "${INFRA_VSC:-}" ]; then
  render /m/nested-snapclass.yaml.tmpl /tmp/snapclass.yaml
  # parameters are immutable: recreate when the mapped host class changed
  cur=$($N get volumesnapshotclass kubevirt-csi-snapclass -o jsonpath='{.parameters.infraSnapshotClassName}' 2>/dev/null || true)
  [ -n "$cur" ] && [ "$cur" != "$INFRA_VSC" ] && $N delete volumesnapshotclass kubevirt-csi-snapclass >/dev/null
  $N apply -f /tmp/snapclass.yaml >/dev/null
  echo "OK snapshot class -> host $INFRA_VSC"
else
  echo "WARN host has no VolumeSnapshotClass for $INFRA_SC's provisioner: snapshots disabled"
fi

[ "$changed" = 1 ] && $N rollout restart ds/kubevirt-csi-node -n kubevirt-csi-driver >/dev/null 2>&1 || true
$N rollout status ds/kubevirt-csi-node -n kubevirt-csi-driver --timeout=10m >/dev/null || fatal "node plugin DaemonSet not ready"
echo "OK node plugin ready on all nodes"

# --- 4. controller credential: dedicated SA token, written straight to the host Secret ---
TOK=""
for i in $(seq 1 60); do
  TOK=$($N get secret kubevirt-csi-controller-token -n kubevirt-csi-driver -o jsonpath='{.data.token}' 2>/dev/null || true)
  [ -n "$TOK" ] && break; sleep 5
done
[ -n "$TOK" ] || fatal "controller token never populated"
CA=$($N get secret kubevirt-csi-controller-token -n kubevirt-csi-driver -o jsonpath='{.data.ca\.crt}')
umask 077
cat > /tmp/tenant.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters:
- name: nested
  cluster:
    server: https://${VIP}:6443
    tls-server-name: api-int.${DOMAIN}
    certificate-authority-data: ${CA}
users:
- name: kubevirt-csi-controller
  user:
    token: $(echo "$TOK" | base64 -d)
contexts:
- name: nested
  context: {cluster: nested, user: kubevirt-csi-controller}
current-context: nested
EOF
unset TOK
# Host side uses this Pod's own ServiceAccount (no kubeconfig). create-or-set-data
# avoids 'oc apply', which would copy the token into a last-applied annotation.
if oc get secret kvcluster-kubeconfig -n "$HOST_NS" >/dev/null 2>&1; then
  oc set data secret/kvcluster-kubeconfig -n "$HOST_NS" --from-file=value=/tmp/tenant.kubeconfig >/dev/null
else
  oc create secret generic kvcluster-kubeconfig -n "$HOST_NS" --from-file=value=/tmp/tenant.kubeconfig >/dev/null
  oc label secret kvcluster-kubeconfig -n "$HOST_NS" "oct-shift-on-shift-virt/cluster=$CLUSTER" >/dev/null
fi
rm -f /tmp/tenant.kubeconfig
echo "OK controller credential stored in $HOST_NS/kvcluster-kubeconfig"
echo "STORAGE_NESTED_OK"
