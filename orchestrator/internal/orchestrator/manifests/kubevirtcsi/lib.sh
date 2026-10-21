# Shared by the storage/virtualization job scripts (sourced, not executed).
# Builds a nested kubeconfig that reaches the API through the cUDN VIP while
# still verifying TLS against the real api.<domain> name.
set -euo pipefail
export HOME=/tmp
fatal() { echo "FATAL: $*"; exit 1; }

NKC=/tmp/nested.kubeconfig
install -m 600 /nested/kubeconfig "$NKC"
CL=$(oc --kubeconfig "$NKC" config view -o jsonpath='{.clusters[0].name}')
oc --kubeconfig "$NKC" config set-cluster "$CL" --server="https://${VIP}:6443" >/dev/null
oc --kubeconfig "$NKC" config set "clusters.${CL}.tls-server-name" "api.${DOMAIN}" >/dev/null
N="oc --kubeconfig $NKC"
$N get --raw /readyz >/dev/null || fatal "nested API not reachable at ${VIP}:6443"
echo "OK nested API reachable (TLS verified as api.${DOMAIN})"

# poll CMD... until it succeeds or SECS elapse: poll SECS cmd args
poll() {
  local secs=$1; shift
  local end=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -ge $end ] && return 1
    sleep 10
  done
}
