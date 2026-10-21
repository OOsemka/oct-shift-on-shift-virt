#!/bin/bash
# Installs OpenShift Virtualization inside the nested cluster and makes the
# kubevirt-csi class usable by CDI (findings F21/F23 in the storage plan).
# Env: VIP DOMAIN INFRA_VSC(optional). Mounts: /nested/kubeconfig, /m.
. /m/lib.sh

$N apply -f /m/cnv.yaml >/dev/null
echo "OK OpenShift Virtualization subscription created"

csv_ok() {
  local csv
  csv=$($N get subscription kubevirt-hyperconverged -n openshift-cnv -o jsonpath='{.status.installedCSV}')
  [ -n "$csv" ] && [ "$($N get csv "$csv" -n openshift-cnv -o jsonpath='{.status.phase}')" = Succeeded ]
}
poll 1800 csv_ok || fatal "the OpenShift Virtualization operator did not install within 30 minutes (check Subscription kubevirt-hyperconverged in openshift-cnv)"
echo "OK operator installed"

poll 300 $N get crd hyperconvergeds.hco.kubevirt.io || fatal "HyperConverged CRD missing"
cat <<YAML | $N apply -f - >/dev/null
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata: {name: kubevirt-hyperconverged, namespace: openshift-cnv}
spec: {}
YAML
hco_ok() { [ "$($N get hyperconverged kubevirt-hyperconverged -n openshift-cnv -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')" = True ]; }
poll 1800 hco_ok || fatal "HyperConverged did not become Available within 30 minutes"
echo "OK HyperConverged available"

# CDI ships no profile defaults for csi.kubevirt.io (upstream CDI#4323).
poll 300 $N get storageprofile kubevirt-csi || fatal "StorageProfile kubevirt-csi never appeared"
patch='{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Block"},{"accessModes":["ReadWriteOnce"],"volumeMode":"Block"},{"accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem"}]'
if [ -z "${INFRA_VSC:-}" ]; then patch="$patch"',"cloneStrategy":"copy"'; fi
$N patch storageprofile kubevirt-csi --type=merge -p "$patch}}" >/dev/null
$N annotate storageclass kubevirt-csi --overwrite storageclass.kubevirt.io/is-default-virt-class=true >/dev/null
echo "OK StorageProfile kubevirt-csi patched; kubevirt-csi is the default virtualization class"
echo "VIRT_OK"
