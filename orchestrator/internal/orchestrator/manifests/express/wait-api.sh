#!/bin/bash
# Express: wait until a cloned cluster's API and operators are healthy.
# Runs with the master's admin kubeconfig (the clone's is created by Sanitize).
# lib.sh exits if the API is not up yet, so poll /readyz first with the same
# kubeconfig setup (cUDN address of the bastion, TLS verified as api.<domain>).
export HOME=/tmp
K=/tmp/wait.kubeconfig; install -m 600 /nested/kubeconfig "$K"
CL=$(oc --kubeconfig "$K" config view -o jsonpath='{.clusters[0].name}')
oc --kubeconfig "$K" config set-cluster "$CL" --server="https://${VIP}:6443" >/dev/null
oc --kubeconfig "$K" config set "clusters.${CL}.tls-server-name" "api.${DOMAIN}" >/dev/null
end=$((SECONDS + 900))
until oc --kubeconfig "$K" get --raw /readyz >/dev/null 2>&1; do
  [ $SECONDS -ge $end ] && { echo "FATAL: the cloned API did not answer within 15 minutes"; exit 1; }
  sleep 5
done
echo "OK cloned API answering"
source /m/lib.sh

# A master's snapshot ages: the kubelets' first, short-lived certificates may
# have expired, so cloned nodes re-bootstrap and ask for new client, serving
# and OVN node certificates. Nothing approves them on these platforms after
# the install, so approve exactly those: the cluster's own node names, the
# expected signer and subject, and the expected requester.
approve_csrs() {
  $N get csr -o json | NODES="$NODES" python3 -c '
import base64, json, os, subprocess, sys
nodes = set(os.environ["NODES"].split())
for c in json.load(sys.stdin)["items"]:
    if c.get("status", {}).get("conditions"):
        continue
    spec = c["spec"]
    subj = subprocess.run(["openssl", "req", "-noout", "-subject"], input=base64.b64decode(spec["request"]),
                          capture_output=True).stdout.decode().replace(" ", "")
    user, signer = spec.get("username", ""), spec["signerName"]
    ok = False
    for n in nodes:
        node_subj = "O=system:nodes,CN=system:node:" + n in subj or ("CN=system:node:" + n in subj and "O=system:nodes" in subj)
        if signer == "kubernetes.io/kube-apiserver-client-kubelet" and node_subj and \
                user in ("system:serviceaccount:openshift-machine-config-operator:node-bootstrapper", "system:node:" + n):
            ok = True
        if signer == "kubernetes.io/kubelet-serving" and node_subj and user == "system:node:" + n:
            ok = True
        if signer == "kubernetes.io/kube-apiserver-client" and "CN=system:ovn-node:" + n in subj and \
                "O=system:ovn-nodes" in subj and user == "system:node:" + n:
            ok = True
    if ok:
        print(c["metadata"]["name"])' | while read -r csr; do
    $N adm certificate approve "$csr" >/dev/null && echo "OK approved $csr"
  done
}
NODES=$($N get nodes -o jsonpath='{.items[*].metadata.name}')
settled() {
  approve_csrs
  local bad total
  total=$($N get co --no-headers 2>/dev/null | wc -l)
  bad=$($N get co --no-headers 2>/dev/null | awk '$3!="True"||$5!="False"' | wc -l)
  [ "$total" -gt 20 ] && [ "$bad" -eq 0 ]
}
poll 1200 settled || fatal "cluster operators of the clone did not become available within 20 minutes"
$N get nodes --no-headers | awk '{print "node", $1, $2}'
echo CLONE_API_READY
