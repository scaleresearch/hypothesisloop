#!/usr/bin/env bash
# Pause local dev without tearing anything down: stops the control-plane
# containers, stops k3s inside the podman machine (or natively on Linux), and
# closes the API port-forward. All cluster/container state is preserved on
# disk — `start.sh` brings everything back as it was. For a full teardown
# that deletes the cluster, use destroy.sh instead.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/../../controlplane/infra/docker-compose.yaml"

echo "==> Stopping control plane..."
podman compose -f "${COMPOSE_FILE}" stop 2>/dev/null || true

echo "==> Closing k3s API port-forward..."
pkill -f "ssh.*6443:localhost:6443" 2>/dev/null || true

if [[ "$(uname)" == "Darwin" ]]; then
  if podman machine list --format '{{.Running}}' 2>/dev/null | grep -q "true"; then
    SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
    SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
    echo "==> Stopping k3s inside podman machine..."
    ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
      "sudo systemctl stop k3s" 2>/dev/null || true
  fi

  echo "==> Stopping podman machine..."
  podman machine stop 2>/dev/null || true
else
  echo "==> Stopping k3s..."
  sudo systemctl stop k3s 2>/dev/null || true
fi

echo "==> Local dev stopped. Run start.sh to bring it back up."
