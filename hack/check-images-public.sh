#!/usr/bin/env bash
# Every image the plugin, the orchestrator or its phase Jobs use must pull
# anonymously: community installs have no pull secret. A dev cluster that
# carries a registry secret hides a private image (it happened with the SSH
# helper), so check from the outside. Run before every release.
set -uo pipefail
cd "$(dirname "$0")/.."
imgs=$( { grep -rhoE '"(quay\.io|docker\.io|registry\.[a-z.]+|ghcr\.io)/[a-z0-9._/-]+(:[A-Za-z0-9._-]+)?"' orchestrator/internal orchestrator/cmd --include=*.go | grep -v '_test.go' ;
          grep -hoE 'image: *[^ ]+' deploy/install.yaml | sed 's/image: *//' ; } | tr -d '"' | grep -vE '^quay\.io/x/' | sort -u)
fail=0
for i in $imgs; do
  if timeout 60 skopeo inspect --no-creds "docker://$i" >/dev/null 2>&1; then echo "OK      $i"
  else echo "PRIVATE $i (or missing)"; fail=1; fi
done
exit $fail
