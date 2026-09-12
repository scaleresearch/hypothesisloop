#!/usr/bin/env bash
# Builds and pushes this experiment's job image (seed/Dockerfile.workload) and rewrites
# job.yaml's placeholder image ref to the pushed, digest-pinned tag. This is the only build step
# a config-only hyperparameter sweep needs -- OPT_NAME/OPT_LR/etc are runtime env vars (job.yaml),
# never baked into the image. Re-run this only after a change to optimizers.py/train.py/
# Dockerfile.workload itself.
set -euo pipefail

# localhost:5000 resolves on the coordinator's own host but not inside a cluster node's network
# namespace -- must be the host LAN IP (same rule as CODE_REPO_URL in setup.md step 1).
REGISTRY="${REGISTRY:?set REGISTRY to the host LAN IP:5000, e.g. 192.168.1.76:5000 -- localhost:5000 is unreachable from inside a job pod}"
TAG="${TAG:-$(git rev-parse --short HEAD)}"
IMAGE="${REGISTRY}/hypothesisloop-shampoo-family-optimizers-workload:${TAG}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "build_and_push.sh: building $IMAGE"
podman build -f "$REPO_ROOT/seed/Dockerfile.workload" -t "$IMAGE" "$REPO_ROOT/seed"

echo "build_and_push.sh: built image id $(podman inspect "$IMAGE" --format '{{.Id}}')"

echo "build_and_push.sh: pushing to $REGISTRY"
podman push --tls-verify="${REGISTRY_TLS_VERIFY:-false}" "$IMAGE"

echo "build_and_push.sh: rewriting seed/job.yaml image ref to $IMAGE"
sed -i.bak "s#image: localhost:5000/hypothesisloop-shampoo-family-optimizers-workload:.*#image: ${IMAGE}#" \
    "$REPO_ROOT/seed/job.yaml"
rm -f "$REPO_ROOT/seed/job.yaml.bak"

echo "build_and_push.sh: done. Smoke-test before spawning any agents:"
echo "  podman pull --tls-verify=false ${IMAGE}   # from any cluster node"
