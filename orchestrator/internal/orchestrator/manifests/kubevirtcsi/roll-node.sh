# Day-2 rolling restart helper (phase EnableStorage), one node at a time:
#   MODE=bootid   print the node's current boot ID (BOOTID=...)
#   MODE=drain    cordon and drain NODE, then print its boot ID
#   MODE=ready    after the node's VM restarted: wait for a new boot ID and
#                 Ready, uncordon, and wait until etcd and kube-apiserver settle
#   MODE=uncordon uncordon NODE (after a failure)
#   MODE=uncordonmarked uncordon every node this helper cordoned (marked at
#                 drain time), e.g. one left cordoned when the orchestrator
#                 restarted between the VM start and MODE=ready. Nodes cordoned
#                 by someone else are left alone.
# Simultaneous restart (all nodes at once, no drain):
#   MODE=bootids  print every node's boot ID (BOOTID <node> <id>, one per line)
#   MODE=allready after all VMs restarted: wait until every node in BOOT_IDS
#                 ("node=id node=id ...") booted again and is Ready, then wait
#                 until etcd and kube-apiserver settle
# Env: VIP DOMAIN NODE MODE BOOT_ID(ready) BOOT_IDS(allready) API_WAIT(optional). Mounts: /nested/kubeconfig, /m.
source /m/lib.sh
MARK=oct-shift-on-shift-virt/cordoned-for-restart
bootid() { $N get node "$NODE" -o jsonpath='{.status.nodeInfo.bootID}'; }
uncordon_node() { $N adm uncordon "$1" && $N annotate node "$1" "$MARK-" >/dev/null; }
settled() {
  for co in etcd kube-apiserver; do
    s=$($N get co "$co" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}') || return 1
    case "$s" in *Available=True*) ;; *) return 1 ;; esac
    case "$s" in *Degraded=True*|*Progressing=True*) return 1 ;; esac
  done
}
case "$MODE" in
bootid)
  echo "BOOTID=$(bootid)" ;;
drain)
  $N annotate node "$NODE" --overwrite "$MARK=true" >/dev/null
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
  uncordon_node "$NODE"
  poll 2400 settled || fatal "etcd and kube-apiserver did not settle within 40 minutes after $NODE restarted"
  echo "OK $NODE booted again, Ready and uncordoned; the control plane has settled" ;;
uncordon)
  uncordon_node "$NODE" || true ;;
uncordonmarked)
  for n in $($N get nodes -o go-template='{{range .items}}{{if eq (index .metadata.annotations "oct-shift-on-shift-virt/cordoned-for-restart") "true"}}{{.metadata.name}} {{end}}{{end}}'); do
    uncordon_node "$n" && echo "OK $n uncordoned (left cordoned by an interrupted restart)"
  done ;;
bootids)
  $N get nodes -o jsonpath='{range .items[*]}BOOTID {.metadata.name} {.status.nodeInfo.bootID}{"\n"}{end}' ;;
allready)
  [ -n "${BOOT_IDS:-}" ] || fatal "BOOT_IDS not set"
  for pair in $BOOT_IDS; do
    NODE=${pair%%=*}; BOOT_ID=${pair#*=}
    rebooted() { b=$(bootid) && [ -n "$b" ] && [ "$b" != "$BOOT_ID" ]; }
    poll 2400 rebooted || fatal "$NODE did not boot again within 40 minutes after the restart"
    ready() { [ "$($N get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
    poll 2400 ready || fatal "$NODE did not become Ready within 40 minutes after the restart"
    echo "OK $NODE booted again and is Ready"
  done
  poll 2400 settled || fatal "etcd and kube-apiserver did not settle within 40 minutes after the restart"
  echo "OK all nodes booted again and are Ready; the control plane has settled" ;;
*)
  fatal "unknown MODE $MODE" ;;
esac
