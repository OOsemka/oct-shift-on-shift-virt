# Day-2 rolling restart helper (phase EnableStorage), one node at a time:
#   MODE=bootid   print the node's current boot ID (BOOTID=...)
#   MODE=drain    cordon and drain NODE, then print its boot ID
#   MODE=ready    after the node's VM restarted: wait for a new boot ID and
#                 Ready, uncordon, and wait until etcd and kube-apiserver settle
#   MODE=uncordon uncordon NODE (after a failed drain)
# Env: VIP DOMAIN NODE MODE BOOT_ID(ready) API_WAIT(optional). Mounts: /nested/kubeconfig, /m.
source /m/lib.sh
bootid() { $N get node "$NODE" -o jsonpath='{.status.nodeInfo.bootID}'; }
case "$MODE" in
bootid)
  echo "BOOTID=$(bootid)" ;;
drain)
  $N adm cordon "$NODE"
  $N adm drain "$NODE" --ignore-daemonsets --delete-emptydir-data --force --timeout=30m \
    || fatal "could not drain $NODE within 30 minutes (a PodDisruptionBudget may block it)"
  echo "OK $NODE drained"
  echo "BOOTID=$(bootid)" ;;
ready)
  [ -n "${BOOT_ID:-}" ] || fatal "BOOT_ID not set"
  rebooted() { b=$(bootid) && [ -n "$b" ] && [ "$b" != "$BOOT_ID" ]; }
  poll 2400 rebooted || fatal "$NODE did not boot again within 40 minutes after its VM restarted"
  ready() { [ "$($N get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
  poll 2400 ready || fatal "$NODE did not become Ready within 40 minutes after its restart"
  $N adm uncordon "$NODE"
  settled() {
    for co in etcd kube-apiserver; do
      s=$($N get co "$co" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}') || return 1
      case "$s" in *Available=True*) ;; *) return 1 ;; esac
      case "$s" in *Degraded=True*|*Progressing=True*) return 1 ;; esac
    done
  }
  poll 2400 settled || fatal "etcd and kube-apiserver did not settle within 40 minutes after $NODE restarted"
  echo "OK $NODE booted again, Ready and uncordoned; the control plane has settled" ;;
uncordon)
  $N adm uncordon "$NODE" || true ;;
*)
  fatal "unknown MODE $MODE" ;;
esac
