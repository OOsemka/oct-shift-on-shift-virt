#!/bin/bash
# Express: once the control plane has rolled out the new issuer and the
# certificates from the new client signers, remove trust in the master's
# client-auth CAs. CUTOFF lies between the master's CAs and the clone's new
# signers. Serving and kubelet CAs are not rotated (see sanitize.sh).
source /m/lib.sh
rolled_out() {
  local r
  for r in kubeapiserver kubecontrollermanager kubescheduler; do
    $N get "$r" cluster -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)["status"]; l = s["latestAvailableRevision"]
sys.exit(0 if all(n.get("currentRevision") == l for n in s["nodeStatuses"]) else 1)' || return 1
  done
  [ "$($N get co --no-headers | awk '$3!="True"||$4!="False"||$5!="False"' | wc -l)" -eq 0 ]
}
sleep 60
poll 3600 rolled_out || fatal "the control plane did not finish rolling out the new issuer within an hour"
echo "OK control plane rolled out (new issuer)"

# Pods refresh their service-account tokens within about an hour (bound
# token lifetime); only then can the old, shared issuer be dropped.
WAIT_UNTIL=$((ISSUER_AT + 65 * 60))
while [ "$(date +%s)" -lt $WAIT_UNTIL ]; do sleep 60; done
ISS=$($N get authentication.config cluster -o jsonpath='{.spec.serviceAccountIssuer}')
if [ "$($N get kubeapiserver cluster -o jsonpath='{.status.serviceAccountIssuers[*].name}')" != "$ISS" ]; then
  $N patch kubeapiserver cluster --subresource=status --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/status/serviceAccountIssuers\",\"value\":[{\"name\":\"$ISS\"}]}]" >/dev/null || fatal "could not drop the old issuer"
  echo "OK old service-account issuer dropped"
  sleep 120
  poll 3600 rolled_out || fatal "the control plane did not finish rolling out without the old issuer"
fi
echo "OK control plane rolled out (old issuer dropped)"
for cm in openshift-config-managed/kube-apiserver-aggregator-client-ca \
          openshift-kube-apiserver-operator/kube-control-plane-signer-ca \
          openshift-kube-apiserver-operator/node-system-admin-ca; do
  $N adm ocp-certificates remove-old-trust -n "${cm%/*}" "configmap/${cm#*/}" --created-before "$CUTOFF" >/dev/null \
    || fatal "could not remove old trust from $cm"
done
echo "OK trust in the master's client-auth CAs removed"
healthy() { [ "$($N get co --no-headers | awk '$3!="True"||$5!="False"' | wc -l)" -eq 0 ]; }
poll 900 healthy || fatal "cluster operators unhealthy after removing the old trust"
echo ISOLATED_OK
