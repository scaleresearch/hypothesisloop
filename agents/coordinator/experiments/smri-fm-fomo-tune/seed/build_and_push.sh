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
# Push, not import: podman and k3s used to have separate image stores with no registry between
# them (an image built here was invisible to the cluster until sideloaded by hand) -- now this
# builds against the same registry every cluster node already trusts (see
# localdev/lib/node.sh's registries.yaml, or REGISTRY_URL for a remote cluster) and the job pulls
# it normally, no per-machine import step left to forget.
set -euo pipefail

# localhost:5000 resolves on the coordinator's own host but NOT inside a cluster node's network
# namespace (k3s node registries.yaml mirrors the host LAN IP, e.g. 192.168.1.76:5000, never
# localhost) -- an image pushed under localhost:5000 pulls fine from here but ImagePullBackOffs on
# every real job pod. Same rule as CODE_REPO_URL/data_store.endpoint in setup.md step 1.
REGISTRY="${REGISTRY:?set REGISTRY to the host LAN IP:5000, e.g. 192.168.1.76:5000 -- localhost:5000 is unreachable from inside a job pod}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
IMAGE="${REGISTRY}/hypothesisloop-smri-fm-fomo-tune-workload:${TAG}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "build_and_push.sh: building $IMAGE (one thin layer over the pinned base)"
podman build -f "$REPO_ROOT/seed/Dockerfile.workload" -t "$IMAGE" "$REPO_ROOT"

# Record the digest you just built. Comparing job results across waves is only meaningful if you
# know they ran the same image (checklist item 9) -- a mutable tag alone does not tell you that,
# which is why every rendered job.yaml below pins this exact tag, not `latest`.
echo "build_and_push.sh: built image id $(podman inspect "$IMAGE" --format '{{.Id}}')"

echo "build_and_push.sh: pushing to $REGISTRY"
podman push --tls-verify="${REGISTRY_TLS_VERIFY:-false}" "$IMAGE"

# job.yaml/job.task5.yaml are tracked with a placeholder image ref -- the resolved tag is a
# build-time fact, not something to commit (a stale committed tag is exactly the kind of drift
# that made the original disk-GC incident hard to diagnose). Each gets rendered alongside it,
# gitignored, and that rendered copy is what you actually submit.
for src in "$REPO_ROOT"/seed/job*.yaml; do
  [[ -f "$src" ]] || continue
  grep -q "__WORKLOAD_IMAGE__" "$src" || continue
  rendered="${src%.yaml}.rendered.yaml"
  sed "s|__WORKLOAD_IMAGE__|${IMAGE}|g" "$src" > "$rendered"
  echo "build_and_push.sh: rendered $(basename "$rendered") -- submit with:"
  echo "  hl job submit --agent <id> $rendered"
done
