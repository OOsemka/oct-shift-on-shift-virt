#!/usr/bin/env bash
# Repository rules that the build does not catch. Run: yarn check-repo
#  - PatternFly 6 only: no PatternFly CSS imports (the console provides the styles).
#  - Nothing private in tracked files: home directory paths, OpenShift tokens,
#    JWTs or private keys.
set -euo pipefail
fail=0

report() {
  echo "check-repo: $1"
  echo "$2" | sed 's/^/  /'
  fail=1
}

pf_css=$(git grep -nE "@patternfly/(patternfly|react-[a-z]+/dist/styles)|patternfly[^'\"]*\.css" -- 'src/*' || true)
[ -n "$pf_css" ] && report "PatternFly CSS imports are not allowed (the console loads PatternFly 6):" "$pf_css"

private=$(git grep -nIE "(/home/[a-z][a-z0-9_-]+/|/Users/[A-Za-z][A-Za-z0-9_-]+/|sha256~[A-Za-z0-9_-]{20,}|eyJhbGciOi[A-Za-z0-9_-]{10,}|BEGIN [A-Z ]*PRIVATE KEY)" -- . ':!scripts/check-repo.sh' ':!yarn.lock' ':!.yarn/*' || true)
[ -n "$private" ] && report "private data (home paths, tokens or keys) in tracked files:" "$private"

[ "$fail" -eq 0 ] && echo "check-repo: ok"
exit "$fail"
