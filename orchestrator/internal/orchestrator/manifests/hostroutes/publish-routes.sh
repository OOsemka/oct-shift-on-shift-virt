#!/bin/bash
# Publishes the nested console, OAuth and API under host-side route names
# (externalAccess.hostRoutes). Runs in the cluster namespace with the nested
# kubeconfig (/nested) and the leaf certificates (/certs).
# Env: CLUSTER DOMAIN VIP CONSOLE_HOST OAUTH_HOST API_HOST
source /m/lib.sh

for v in CONSOLE_HOST OAUTH_HOST API_HOST; do
  [ -n "${!v:-}" ] || fatal "$v is not set"
done

# upsert_tls NAME CERT KEY: create or replace a TLS Secret in openshift-config.
# Never "oc apply" a Secret (it would copy the key into an annotation).
upsert_tls() {
  local name=$1 crt=$2 key=$3
  if $N -n openshift-config get secret "$name" >/dev/null 2>&1; then
    $N -n openshift-config create secret tls "$name" --cert="$crt" --key="$key" \
      --dry-run=client -o yaml | $N replace -f - >/dev/null || fatal "could not update secret $name"
  else
    $N -n openshift-config create secret tls "$name" --cert="$crt" --key="$key" >/dev/null \
      || fatal "could not create secret $name"
  fi
  echo "OK secret openshift-config/$name"
}
upsert_tls sos-external-routes /certs/routes.crt /certs/routes.key
upsert_tls sos-external-api /certs/api.crt /certs/api.key

before_rev=$($N get kubeapiserver cluster -o jsonpath='{.status.latestAvailableRevision}' 2>/dev/null || echo 0)

# componentRoutes: replace only the console and oauth-openshift entries.
cur=$($N get ingress.config cluster -o json) || fatal "cannot read ingress.config"
patch=$(CUR="$cur" python3 -c '
import json, os
spec = json.loads(os.environ["CUR"]).get("spec", {})
keep = [r for r in spec.get("componentRoutes") or []
        if (r.get("namespace"), r.get("name")) not in
           (("openshift-console", "console"), ("openshift-authentication", "oauth-openshift"))]
keep += [
  {"name": "console", "namespace": "openshift-console", "hostname": os.environ["CONSOLE_HOST"],
   "servingCertKeyPairSecret": {"name": "sos-external-routes"}},
  {"name": "oauth-openshift", "namespace": "openshift-authentication", "hostname": os.environ["OAUTH_HOST"],
   "servingCertKeyPairSecret": {"name": "sos-external-routes"}},
]
print(json.dumps({"spec": {"componentRoutes": keep}}))
')
$N patch ingress.config cluster --type=merge -p "$patch" >/dev/null || fatal "could not patch ingress.config componentRoutes"
echo "OK componentRoutes: console=$CONSOLE_HOST oauth=$OAUTH_HOST"

# namedCertificates: replace any entry that names API_HOST, keep the rest.
cur=$($N get apiserver cluster -o json) || fatal "cannot read apiserver config"
api_patch=$(CUR="$cur" python3 -c '
import json, os
spec = json.loads(os.environ["CUR"]).get("spec", {})
host = os.environ["API_HOST"]
named = [c for c in (spec.get("servingCerts") or {}).get("namedCertificates") or []
         if host not in (c.get("names") or [])]
named.append({"names": [host], "servingCertificate": {"name": "sos-external-api"}})
print(json.dumps({"spec": {"servingCerts": {"namedCertificates": named}}}))
')
$N patch apiserver cluster --type=merge -p "$api_patch" >/dev/null || fatal "could not patch apiserver namedCertificates"
echo "OK apiserver namedCertificates: $API_HOST"

# The console keeps its default route (now a redirect) and serves the custom
# name from a separate console-custom route; the console URL in its status is
# the reliable signal. The OAuth route itself switches to the new name.
console_url_is() { [ "$($N get console.config cluster -o jsonpath='{.status.consoleURL}' 2>/dev/null)" = "https://$1" ]; }
route_is() { [ "$($N -n "$1" get route "$2" -o jsonpath='{.spec.host}' 2>/dev/null)" = "$3" ]; }
poll 600 console_url_is "$CONSOLE_HOST" || fatal "console did not switch to $CONSOLE_HOST"
poll 600 route_is openshift-authentication oauth-openshift "$OAUTH_HOST" || fatal "OAuth route did not switch to $OAUTH_HOST"
echo "OK nested console and OAuth routes use the host-side names"

co_settled() {
  local s
  s=$($N get clusteroperator "$1" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}')
  [[ "$s" == *"Available=True"* && "$s" == *"Progressing=False"* && "$s" == *"Degraded=False"* ]]
}
poll 900 co_settled console || fatal "console operator did not settle"
poll 900 co_settled authentication || fatal "authentication operator did not settle"
echo "OK console and authentication operators settled"

# The API certificate needs a kube-apiserver rollout (a new revision on every
# control-plane node, ~10-15 min). The routes already work, so a slow rollout
# is a warning, not a failure.
new_revision() { [ "$($N get kubeapiserver cluster -o jsonpath='{.status.latestAvailableRevision}')" -gt "$before_rev" ]; }
rolled_out() {
  $N get kubeapiserver cluster -o json | python3 -c '
import json, sys
st = json.load(sys.stdin)["status"]
latest = st.get("latestAvailableRevision")
sys.exit(0 if all(n.get("currentRevision") == latest for n in st.get("nodeStatuses", [])) else 1)'
}
if poll 180 new_revision; then
  echo "kube-apiserver rolling out the API certificate..."
fi
if poll 1080 rolled_out && poll 300 co_settled kube-apiserver; then
  echo "OK kube-apiserver serves the host-side API certificate"
else
  echo "WARN kube-apiserver rollout still in progress; the API route works, its certificate follows when the rollout ends"
fi
echo HOSTROUTES_OK
