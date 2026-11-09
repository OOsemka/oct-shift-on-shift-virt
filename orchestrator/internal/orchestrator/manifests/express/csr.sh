# Shared by the Express scripts (sourced). approve_csrs approves the pending
# certificate requests of the cluster's own nodes, and nothing else: the
# nodes in $NODES, the expected signer and subject, and the expected
# requester. A master's snapshot ages; after about a day the kubelets' first
# certificates have expired, so cloned nodes re-bootstrap and keep asking for
# client, serving and OVN node certificates for a few minutes after boot.
# Nothing approves them on these platforms after the install. Uses $N.
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
