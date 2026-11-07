#!/bin/bash
# Express: replace every credential a clone shares with its master.
# /creds: admin-ca.crt, admin.crt, admin.key (new system:admin), kubeadmin-hash, issuer.
# Never prints a secret; prints only the API CA bundle (public certificates).
# Re-runnable: once the master's admin certificate is rejected, it continues
# with the clone's own.
set -uo pipefail
export HOME=/tmp
fatal() { echo "FATAL: $*"; exit 1; }
poll() { local secs=$1; shift; local end=$((SECONDS + secs)); until "$@" >/dev/null 2>&1; do [ $SECONDS -ge $end ] && return 1; sleep 10; done; }

# Two kubeconfigs on the bastion's cUDN address, TLS verified as api.<domain>:
# OLD = the master's admin cert, NEW = the clone's.
OLD=/tmp/old.kc; NEW=/tmp/new.kc
install -m 600 /nested/kubeconfig "$OLD"
CL=$(oc --kubeconfig "$OLD" config view -o jsonpath='{.clusters[0].name}')
U=$(oc --kubeconfig "$OLD" config view -o jsonpath='{.contexts[0].context.user}')
oc --kubeconfig "$OLD" config set-cluster "$CL" --server="https://${VIP}:6443" >/dev/null
oc --kubeconfig "$OLD" config set "clusters.${CL}.tls-server-name" "api.${DOMAIN}" >/dev/null
cp "$OLD" "$NEW"
oc --kubeconfig "$NEW" config set-credentials "$U" --client-certificate=/creds/admin.crt --client-key=/creds/admin.key --embed-certs >/dev/null

# set_ca FILE: trust this CA bundle in both kubeconfigs.
set_ca() { for k in "$OLD" "$NEW"; do oc --kubeconfig "$k" config set "clusters.${CL}.certificate-authority-data" "$(base64 -w0 "$1")" >/dev/null; done; }

# A re-run after the serving certificates were re-issued: the master's CA no
# longer verifies the API. Read the public CA bundle without verification,
# then accept it only if the server's certificate chains to it.
if ! oc --kubeconfig "$NEW" get --raw /readyz >/dev/null 2>&1 && ! oc --kubeconfig "$OLD" get --raw /readyz >/dev/null 2>&1; then
  for k in "$NEW" "$OLD"; do
    oc --kubeconfig "$k" --insecure-skip-tls-verify=true -n openshift-config-managed get configmap kube-apiserver-server-ca \
      -o jsonpath='{.data.ca-bundle\.crt}' > /tmp/ca.pem 2>/dev/null && [ -s /tmp/ca.pem ] && break
  done
  [ -s /tmp/ca.pem ] || fatal "cannot reach the clone's API"
  echo | openssl s_client -connect "${VIP}:6443" -servername "api.${DOMAIN}" -CAfile /tmp/ca.pem -verify_return_error >/dev/null 2>&1 \
    || fatal "the API certificate does not chain to the cluster's CA bundle"
  set_ca /tmp/ca.pem
fi

if oc --kubeconfig "$OLD" whoami >/dev/null 2>&1; then
  N="oc --kubeconfig $OLD"
  # kubeadmin: new password hash.
  $N -n kube-system patch secret kubeadmin --type=merge \
    -p "{\"data\":{\"kubeadmin\":\"$(base64 -w0 /creds/kubeadmin-hash)\"}}" >/dev/null || fatal "could not replace the kubeadmin password"
  echo "OK kubeadmin replaced"
  # Admin kubeconfig: a new client CA, so the master's admin kubeconfig stops working.
  $N -n openshift-config create configmap admin-kubeconfig-client-ca --from-file=ca-bundle.crt=/creds/admin-ca.crt \
    --dry-run=client -o yaml | $N replace -f - >/dev/null || fatal "could not replace admin-kubeconfig-client-ca"
fi
poll 300 oc --kubeconfig "$NEW" whoami || fatal "the new admin certificate was not accepted"
N="oc --kubeconfig $NEW"
echo "OK new admin client CA active"
if $N get secret kubeadmin -n kube-system -o jsonpath='{.data.kubeadmin}' | grep -q "$(base64 -w0 /creds/kubeadmin-hash)"; then :; else
  $N -n kube-system patch secret kubeadmin --type=merge \
    -p "{\"data\":{\"kubeadmin\":\"$(base64 -w0 /creds/kubeadmin-hash)\"}}" >/dev/null || fatal "could not replace the kubeadmin password"
fi

$N delete oauthaccesstokens --all >/dev/null 2>&1 || true
$N patch clusterversion version --type=merge \
  -p "{\"spec\":{\"clusterID\":\"$(python3 -c 'import uuid; print(uuid.uuid4())')\"}}" >/dev/null || fatal "could not set a new cluster ID"
echo "OK OAuth tokens deleted, new cluster ID"

# Service-account tokens: a unique issuer. The old one stays trusted until
# finish-isolation.sh drops it: pods hold tokens with the old issuer until
# the kubelet refreshes them, and dropping it during the rollout locks out
# the operators that roll out the API servers (seen in testing).
ISS=$(cat /creds/issuer)
$N patch authentication.config cluster --type=merge -p "{\"spec\":{\"serviceAccountIssuer\":\"$ISS\"}}" >/dev/null || fatal "could not set the issuer"
first_issuer() { [ "$($N get kubeapiserver cluster -o jsonpath='{.status.serviceAccountIssuers[0].name}')" = "$ISS" ]; }
poll 300 first_issuer || fatal "kube-apiserver operator did not pick up the new issuer"
echo "OK service-account issuer $ISS"

# New client-auth signers the API server trusts (controller manager and
# scheduler identities, front-proxy, node-system-admin): with the master's
# keys, an admin of one clone could forge credentials its siblings accept.
# Serving and kubelet CAs are NOT rotated: their keys only allow posing as a
# server, and rotating them needs node reconfiguration (it left nodes
# NotReady in testing). Leaf certificates are re-issued from the new signers.
START=$(date +%s)
$N adm ocp-certificates regenerate-top-level -n openshift-kube-apiserver-operator \
  secret/kube-control-plane-signer secret/aggregator-client-signer secret/node-system-admin-signer >/dev/null \
  || fatal "regenerate-top-level failed"
new_signer() {
  local s nb
  for s in kube-control-plane-signer aggregator-client-signer node-system-admin-signer; do
    nb=$($N -n openshift-kube-apiserver-operator get secret "$s" -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -startdate | cut -d= -f2)
    [ "$(date -d "$nb" +%s)" -ge $((START - 120)) ] || return 1
  done
}
poll 600 new_signer || fatal "the client signers were not regenerated"
sleep 60
$N adm ocp-certificates regenerate-leaf -A --all secrets >/dev/null || fatal "regenerate-leaf failed"
echo "OK client signers regenerated, leaf certificates re-issued"
poll 600 $N whoami || fatal "the API is not reachable after re-issuing"

# The API CA bundle for the clone's kubeconfig (public certificates only).
$N -n openshift-config-managed get configmap kube-apiserver-server-ca -o jsonpath='{.data.ca-bundle\.crt}' > /tmp/bundle.pem
echo "=== CA ==="
base64 -w0 /tmp/bundle.pem
echo
echo "=== CA END ==="
echo SANITIZE_OK
