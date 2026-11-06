#!/usr/bin/env python3
"""Print the Calico component images a Tigera operator release deploys.

Reads the operator's pkg/components/calico.go (at the operator tag shipped in
Calico's OpenShift bundle) and prints, one per line, "<image> <tag>" for every
entry of components.CalicoImages, plus a first line "release <CalicoRelease>".
An operator.tigera.io ImageSet must be named calico-<CalicoRelease> and may
only list these images (prefixed calico/); see the operator's
pkg/controller/utils/imageset/imageset.go.

FIPS variants share the image name of the normal build (only the tag differs)
and an ImageSet holds one digest per image name, so they are skipped: the
nested clusters do not run Calico in FIPS mode.

Used by hack/update-cni-manifests.sh. Usage: calico-images.py calico.go
"""
import re
import sys

src = open(sys.argv[1]).read()

release = re.search(r'CalicoRelease\s+string\s*=\s*"([^"]+)"', src)
if not release:
    sys.exit("CalicoRelease not found")

components = {}
for m in re.finditer(r'(Component\w+)\s*=\s*Component\{(.*?)\n\t\}', src, re.S):
    image = re.search(r'Image:\s*"([^"]+)"', m.group(2))
    version = re.search(r'Version:\s*"([^"]+)"', m.group(2))
    if image and version:
        components[m.group(1)] = (image.group(1), version.group(1))

listing = re.search(r'CalicoImages\s*=\s*\[\]Component\{(.*?)\n\t\}', src, re.S)
if not listing:
    sys.exit("CalicoImages not found")

print("release", release.group(1))
seen = set()
for name in re.findall(r'(Component\w+)', listing.group(1)):
    if name not in components:
        sys.exit(f"{name} is listed in CalicoImages but its definition was not parsed")
    image, version = components[name]
    if version.endswith("-fips") or image in seen:
        continue
    seen.add(image)
    print(image, version)
