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
settled() {
  local bad total
  total=$($N get co --no-headers 2>/dev/null | wc -l)
  bad=$($N get co --no-headers 2>/dev/null | awk '$3!="True"||$5!="False"' | wc -l)
  [ "$total" -gt 20 ] && [ "$bad" -eq 0 ]
}
poll 1200 settled || fatal "cluster operators of the clone did not become available within 20 minutes"
$N get nodes --no-headers | awk '{print "node", $1, $2}'
echo CLONE_API_READY
