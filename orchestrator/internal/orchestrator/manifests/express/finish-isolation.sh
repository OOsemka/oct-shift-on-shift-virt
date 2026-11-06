#!/bin/bash
# Express: once the API server has rolled out the new issuer, remove trust in
# the master's API-facing CAs. CUTOFF lies between the master's CAs and the
# clone's new signers. The kubelet client CA is left alone: the kubelets get
# the new one through the machine-config operator, and purging before that
# cuts the API server off from them (it is internal-only anyway).
source /m/lib.sh
rolled_out() {
  $N get kubeapiserver cluster -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)["status"]; l = s["latestAvailableRevision"]
sys.exit(0 if all(n.get("currentRevision") == l for n in s["nodeStatuses"]) else 1)' &&
  [ "$($N get co kube-apiserver -o jsonpath='{.status.conditions[?(@.type=="Progressing")].status}')" = "False" ]
}
sleep 60
poll 2400 rolled_out || fatal "kube-apiserver did not finish rolling out the new issuer within 40 minutes"
echo "OK kube-apiserver rolled out"
for cm in openshift-config-managed/kube-apiserver-aggregator-client-ca \
          openshift-kube-apiserver-operator/kube-control-plane-signer-ca \
          openshift-kube-apiserver-operator/loadbalancer-serving-ca \
          openshift-kube-apiserver-operator/localhost-recovery-serving-ca \
          openshift-kube-apiserver-operator/localhost-serving-ca \
          openshift-kube-apiserver-operator/node-system-admin-ca \
          openshift-kube-apiserver-operator/service-network-serving-ca; do
  $N adm ocp-certificates remove-old-trust -n "${cm%/*}" "configmap/${cm#*/}" --created-before "$CUTOFF" >/dev/null \
    || fatal "could not remove old trust from $cm"
done
echo "OK trust in the master's API CAs removed"
healthy() { [ "$($N get co --no-headers | awk '$3!="True"||$5!="False"' | wc -l)" -eq 0 ]; }
poll 900 healthy || fatal "cluster operators unhealthy after removing the old trust"
echo ISOLATED_OK
