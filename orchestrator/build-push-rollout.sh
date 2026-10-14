#!/usr/bin/env bash
# Build the orchestrator binary, bake it into the image, push, and roll the
# operator deployment — atomically. Containerfile.prebuilt.local COPYs the
# prebuilt ./sos-orchestrator binary, so `go build ./...` alone does NOT
# update what gets shipped (this burned a full debug cycle once).
set -euo pipefail
cd "$(dirname "$0")"
gofmt -w internal/ cmd/ 2>/dev/null || true
go vet ./...
CGO_ENABLED=0 go build -ldflags "-s -w" -o sos-orchestrator ./cmd
podman build -t quay.io/cjanisze/sos-orchestrator:latest -f Containerfile.prebuilt.local . | tail -1
podman push quay.io/cjanisze/sos-orchestrator:latest | tail -1
oc rollout restart deploy/sos-orchestrator -n oct-shift-on-shift-virt
oc rollout status deploy/sos-orchestrator -n oct-shift-on-shift-virt --timeout=120s
echo "BUILT_PUSHED_ROLLED_OUT $(sha256sum sos-orchestrator | cut -c1-16)"
