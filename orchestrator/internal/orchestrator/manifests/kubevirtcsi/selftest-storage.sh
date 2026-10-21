#!/bin/bash
# End-to-end check of nested storage, run after the host controller is up:
# a pod writes to a fresh kubevirt-csi PVC, a second pod (new attachment)
# reads it back, then everything is deleted again.
# Env: VIP DOMAIN IMG_CLI. Mounts: /nested/kubeconfig, /m.
. /m/lib.sh
NS=kubevirt-csi-selftest
cleanup() { $N delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

$N delete namespace "$NS" --ignore-not-found --wait=true --timeout=5m >/dev/null
$N create namespace "$NS" >/dev/null

pod() { # name command
cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS}
spec:
  restartPolicy: Never
  containers:
  - name: t
    image: ${IMG_CLI}
    command: ["/bin/sh", "-c", "$2"]
    volumeMounts: [{name: d, mountPath: /data}]
    securityContext:
      allowPrivilegeEscalation: false
      runAsNonRoot: true
      capabilities: {drop: [ALL]}
      seccompProfile: {type: RuntimeDefault}
  volumes: [{name: d, persistentVolumeClaim: {claimName: selftest}}]
YAML
}
succeeded() { [ "$($N get pod "$1" -n "$NS" -o jsonpath='{.status.phase}')" = Succeeded ]; }
phase() { $N get pod "$1" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || true; }

cat <<YAML | $N apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: selftest, namespace: $NS}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: kubevirt-csi
  resources: {requests: {storage: 1Gi}}
YAML

pod writer "echo sos-storage-ok > /data/probe && sync" | $N apply -f - >/dev/null
poll 600 succeeded writer || { $N describe pod writer -n "$NS" | tail -20; $N describe pvc selftest -n "$NS" | tail -10; fatal "writer pod did not succeed (phase: $(phase writer))"; }
echo "OK provisioned, attached and wrote a volume"
$N delete pod writer -n "$NS" --wait=true --timeout=5m >/dev/null

pod reader "cat /data/probe" | $N apply -f - >/dev/null
poll 600 succeeded reader || { $N describe pod reader -n "$NS" | tail -20; fatal "reader pod did not succeed (phase: $(phase reader))"; }
[ "$($N logs reader -n "$NS")" = sos-storage-ok ] || fatal "data written by the first pod was not read back"
echo "OK data survived detach and re-attach"

$N delete namespace "$NS" --wait=true --timeout=5m >/dev/null || fatal "self-test volume was not released (namespace still deleting)"
trap - EXIT
echo "STORAGE_SELFTEST_OK"
