# Image registry on nested storage (after kubevirt-csi passed its self-test).
# Agent-based installs on platform none leave the registry Removed (there is
# no storage OpenShift can assume), so builds fail with "Output image could
# not be resolved". With nested storage there is: a ReadWriteOnce
# kubevirt-csi volume, one replica, Recreate rollouts (kubevirt-csi offers RWX
# only for raw block). A registry someone already configured is left alone.
source /m/lib.sh
GI=${REGISTRY_GI:-100}
state=$($N get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.spec.managementState}')
storage=$($N get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.spec.storage}')
if [ "$state" != "Removed" ] || { [ -n "$storage" ] && [ "$storage" != "{}" ]; }; then
  echo "REGISTRY_SKIPPED already configured (managementState $state)"
  exit 0
fi
if ! $N -n openshift-image-registry get pvc image-registry-storage >/dev/null 2>&1; then
  cat <<YAML | $N create -f - >/dev/null || fatal "could not create the registry volume"
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: image-registry-storage
  namespace: openshift-image-registry
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: kubevirt-csi
  resources: {requests: {storage: ${GI}Gi}}
YAML
fi
$N patch configs.imageregistry.operator.openshift.io cluster --type=merge -p \
  '{"spec":{"managementState":"Managed","replicas":1,"rolloutStrategy":"Recreate","storage":{"pvc":{"claim":"image-registry-storage"}}}}' >/dev/null \
  || fatal "could not enable the image registry"
available() { [ "$($N -n openshift-image-registry get deploy image-registry -o jsonpath='{.status.availableReplicas}' 2>/dev/null)" = 1 ]; }
poll 600 available || fatal "the image registry did not become available within 10 minutes"
# The build controller (openshift-controller-manager) reads the registry
# address at start-up. Its pods restart when the registry is enabled, but can
# come up before their configuration has the address; builds then stay New
# with InvalidOutputReference. Once the address is in its configuration,
# restart it once.
ocm_has_registry() {
  $N -n openshift-controller-manager get configmap config -o jsonpath='{.data.config\.yaml}' 2>/dev/null \
    | grep -q '"internalRegistryHostname":"image-registry'
}
poll 600 ocm_has_registry || fatal "the build controller configuration never got the registry address"
$N -n openshift-controller-manager rollout restart deploy/controller-manager >/dev/null || fatal "could not restart the build controller"
$N -n openshift-controller-manager rollout status deploy/controller-manager --timeout=300s >/dev/null || fatal "the build controller did not restart"
echo "REGISTRY_OK ${GI}Gi"
