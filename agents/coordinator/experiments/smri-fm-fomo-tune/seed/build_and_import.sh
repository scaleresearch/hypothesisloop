#!/usr/bin/env bash
# The structural-change path: you edited Python under tenstorrent/src/fomo_tune_tt/ (a new
# pooling, a different transform, a new head) and need a job image that contains it.
#
# This is deliberately cheap, and it is the ONLY build step this experiment has. Everything
# heavy -- tt-metal, tt-train, the python env, every C++ artifact -- is already built inside the
# pinned base image seed/Dockerfile.workload starts from, so a code change rebuilds exactly one
# thin COPY layer. There is no compile, no clone, no cold cache.
#
# A hyperparameter change does NOT need this: env vars in seed/job.task5.yaml are read at
# runtime, so a sweep is a submit, not a build.
#
# Two steps, and the second is the one people forget: podman and k3s have separate image stores,
# so an image that exists in podman is invisible to the cluster and the job dies with
# "image_pull_failed / dial tcp 127.0.0.1:443: connection refused" -- which reads like a registry
# outage rather than a missing import.
set -euo pipefail

TAG="${TAG:-localhost/hypothesisloop-smri-fm-fomo-tune-workload:latest}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "build_and_import.sh: building $TAG (one thin layer over the pinned base)"
podman build -f "$REPO_ROOT/seed/Dockerfile.workload" -t "$TAG" "$REPO_ROOT"

# Record the digest you just built. Comparing job results across waves is only meaningful if you
# know they ran the same image (checklist item 9) -- `:latest` alone does not tell you that.
echo "build_and_import.sh: built image id $(podman inspect "$TAG" --format '{{.Id}}')"

echo "build_and_import.sh: importing into k3s containerd (podman's store is not the cluster's)"
podman save "$TAG" | sudo k3s ctr images import -

sudo k3s ctr images ls -q | grep -F "$TAG" \
  || { echo "build_and_import.sh: FATAL: $TAG is not in containerd after import" >&2; exit 1; }
echo "build_and_import.sh: done -- $TAG is visible to the cluster"
