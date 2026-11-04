## What and why

<!-- What does this change, and what problem does it solve? Link an issue if there is one. -->

## How it was tested

<!-- For example: "yarn verify", plus what you clicked on which OpenShift version.
     If it changes what gets applied to the cluster, paste the generated YAML (from the page's YAML preview). -->

- OpenShift version(s):
- Tested on a cluster: yes / no

## Checklist

- [ ] `yarn verify` passes (type check, translation keys, repository rules, build)
- [ ] UI strings go through `t('...')` and are in `locales/en/` (`yarn check-i18n --fix` adds them)
- [ ] PatternFly 6 components only, no PatternFly CSS imports
- [ ] No environment-specific values (hostnames, IPs, VLAN IDs, NIC or bond names, tokens)
- [ ] README / AGENTS.md updated if behavior changed
- [ ] Version, image tags and catalog rows **not** changed (maintainers do that at release time)
