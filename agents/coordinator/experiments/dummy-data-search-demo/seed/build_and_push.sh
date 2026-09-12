#!/usr/bin/env bash
# Builds and pushes the dummy-data-search-demo job image, then renders seed/job.yaml's
# __WORKLOAD_IMAGE__ placeholder into a submit-ready copy. Every job here is config-only (env
# vars: HIDDEN_LAYERS, ACTIVATION, LR, BATCH_SIZE, EPOCHS, L2, N_SAMPLES, NOISE, N_FEATURES,
# CLASS_SEP, CURRICULUM) — this build step only needs to run once, up front; no agent ever needs
# to rebuild this image for a hyperparameter/architecture/data sweep.
set -euo pipefail

# localhost:5000 resolves on the coordinator's own host but NOT inside a cluster node's network
# namespace (k3s node registries.yaml mirrors the LAN IP, e.g. 192.168.1.76:5000, never
# localhost) -- an image pushed under localhost:5000 pulls fine from here but ImagePullBackOffs
# on every real job pod. Same rule as CODE_REPO_URL/data_store.endpoint in setup.md step 1.
REGISTRY="${REGISTRY:?set REGISTRY to the host LAN IP:5000, e.g. 192.168.1.76:5000 -- localhost:5000 is unreachable from inside a job pod}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo latest)}"
IMAGE="${REGISTRY}/hypothesisloop-dummy-data-search-demo-workload:${TAG}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "build_and_push.sh: building $IMAGE"
podman build -f "$REPO_ROOT/seed/Dockerfile.train" -t "$IMAGE" "$REPO_ROOT/seed"

echo "build_and_push.sh: pushing to $REGISTRY"
podman push --tls-verify="${REGISTRY_TLS_VERIFY:-false}" "$IMAGE"

for src in "$REPO_ROOT"/seed/job*.yaml; do
  [[ -f "$src" ]] || continue
  grep -q "__WORKLOAD_IMAGE__" "$src" || continue
  rendered="${src%.yaml}.rendered.yaml"
  sed "s|__WORKLOAD_IMAGE__|${IMAGE}|g" "$src" > "$rendered"
  echo "build_and_push.sh: rendered $(basename "$rendered") -- submit with:"
  echo "  hl job submit --agent <id> $rendered"
done
