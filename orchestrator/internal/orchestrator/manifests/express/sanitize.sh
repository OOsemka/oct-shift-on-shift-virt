#!/bin/bash
# Express: replace every credential a clone shares with its master.
# /creds: admin-ca.crt, admin.crt, admin.key (new system:admin), kubeadmin-hash, issuer.
# Never prints a secret; prints only the API CA bundle (public certificates).
source /m/lib.sh

# kubeadmin: new password hash.
$N -n kube-system patch secret kubeadmin --type=merge \
  -p "{\"data\":{\"kubeadmin\":\"$(base64 -w0 /creds/kubeadmin-hash)\"}}" >/dev/null || fatal "could not replace the kubeadmin password"
echo "OK kubeadmin replaced"

# Admin kubeconfig: a new client CA, so the master's admin kubeconfig stops working.
$N -n openshift-config create configmap admin-kubeconfig-client-ca --from-file=ca-bundle.crt=/creds/admin-ca.crt \
  --dry-run=client -o yaml | $N replace -f - >/dev/null || fatal "could not replace admin-kubeconfig-client-ca"
cp "$NKC" /tmp/new.kc
U=$(oc --kubeconfig /tmp/new.kc config view -o jsonpath='{.contexts[0].context.user}')
oc --kubeconfig /tmp/new.kc config set-credentials "$U" --client-certificate=/creds/admin.crt --client-key=/creds/admin.key --embed-certs >/dev/null
poll 300 oc --kubeconfig /tmp/new.kc whoami || fatal "the new admin certificate was not accepted"
N="oc --kubeconfig /tmp/new.kc"
echo "OK new admin client CA active"

$N delete oauthaccesstokens --all >/dev/null 2>&1 || true
$N patch clusterversion version --type=merge \
  -p "{\"spec\":{\"clusterID\":\"$(python3 -c 'import uuid; print(uuid.uuid4())')\"}}" >/dev/null || fatal "could not set a new cluster ID"
echo "OK OAuth tokens deleted, new cluster ID"

# Service-account tokens: a unique issuer, and no grace period for the old one
# (OpenShift keeps the previous issuer trusted for 24 h unless pruned).
ISS=$(cat /creds/issuer)
$N patch authentication.config cluster --type=merge -p "{\"spec\":{\"serviceAccountIssuer\":\"$ISS\"}}" >/dev/null || fatal "could not set the issuer"
first_issuer() { [ "$($N get kubeapiserver cluster -o jsonpath='{.status.serviceAccountIssuers[0].name}')" = "$ISS" ]; }
poll 300 first_issuer || fatal "kube-apiserver operator did not pick up the new issuer"
$N patch kubeapiserver cluster --subresource=status --type=json \
  -p "[{\"op\":\"replace\",\"path\":\"/status/serviceAccountIssuers\",\"value\":[{\"name\":\"$ISS\"}]}]" >/dev/null || fatal "could not drop the old issuer"
echo "OK service-account issuer $ISS (old issuer dropped)"

# New internal signers, then leaf certificates from them (hot-reloaded).
START=$(date +%s)
$N adm ocp-certificates regenerate-top-level -A --all secrets >/dev/null || fatal "regenerate-top-level failed"
new_signer() {
  local nb; nb=$($N -n openshift-kube-apiserver-operator get secret kube-control-plane-signer -o jsonpath='{.data.tls\.crt}' \
    | base64 -d | openssl x509 -noout -startdate | cut -d= -f2)
  [ "$(date -d "$nb" +%s)" -ge $((START - 120)) ]
}
poll 600 new_signer || fatal "the signers were not regenerated"
sleep 30
$N adm ocp-certificates regenerate-leaf -A --all secrets >/dev/null || fatal "regenerate-leaf failed"
echo "OK signers regenerated, leaf certificates re-issued"

# The API CA bundle for the clone's kubeconfig (public certificates only).
sleep 30
echo "=== CA ==="
$N -n openshift-config-managed get configmap kube-apiserver-server-ca -o jsonpath='{.data.ca-bundle\.crt}' | base64 -w0
echo
echo "=== CA END ==="
echo SANITIZE_OK
