#!/usr/bin/env bash
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONTEXT_NAME="k3s-local"

pkill -f "ssh.*6443:localhost:6443" 2>/dev/null || true

# The dev/test node containers (see dev-nodes-up.sh) are their own podman containers — remove them
# before tearing down the server, or they'd be orphaned (and re-register against a fresh server
# on the next `make k3s-up`, confusingly reusing their old identities).
DESTROYING_CLUSTER=1 bash "${DIR}/dev-nodes-down.sh"

if [[ "$(uname)" == "Darwin" ]]; then
  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
  ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
    "sudo systemctl stop k3s 2>/dev/null; sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null" || true
else
  sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true
fi

kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
# install.sh names the cluster/user objects differently per OS: on Darwin they're renamed to
# CONTEXT_NAME directly (sed rewrite of the raw kubeconfig); on native Linux it uses `kubectl
# config rename-context`, which only renames the context and leaves cluster/user as "default"
# underneath. Delete both names — whichever doesn't apply is just a harmless no-op.
kubectl config delete-cluster "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-user    "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-cluster default 2>/dev/null || true
kubectl config delete-user    default 2>/dev/null || true

echo "==> Cluster destroyed."
