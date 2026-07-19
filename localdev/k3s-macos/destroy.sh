#!/usr/bin/env bash
set -euo pipefail

CONTEXT_NAME="k3s-local"

pkill -f "ssh.*6443:localhost:6443" 2>/dev/null || true

# The extra fake-accelerator nodes (see add-fake-nodes.sh) are their own podman containers — remove
# them before tearing down the server, or they'd be orphaned (and re-register against a
# fresh server on the next `make k3s-up`, confusingly reusing their old identities).
FAKE_NODE_CONTAINERS="$(podman ps -aq --filter name=fake-accelerator-node 2>/dev/null || true)"
if [[ -n "${FAKE_NODE_CONTAINERS}" ]]; then
  podman rm -f ${FAKE_NODE_CONTAINERS} >/dev/null
fi

if [[ "$(uname)" == "Darwin" ]]; then
  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
  ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
    "sudo systemctl stop k3s 2>/dev/null; sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null" || true
else
  sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true
fi

kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-cluster  "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-user     "${CONTEXT_NAME}" 2>/dev/null || true

echo "==> Cluster destroyed."
