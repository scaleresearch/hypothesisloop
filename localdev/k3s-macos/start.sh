#!/usr/bin/env bash
# Resume local dev after stop.sh: starts the podman machine, starts k3s back
# up inside it (or natively on Linux), re-opens the API port-forward, and
# restarts the control-plane containers. Assumes the cluster was previously
# provisioned with install.sh — if no cluster exists yet, run
# `make k3s-up` / `make full-up` instead.
set -euo pipefail

CONTEXT_NAME="k3s-local"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

wait_for() {
  local max="$1" delay="$2" desc="$3"; shift 3
  for i in $(seq 1 "${max}"); do
    if "$@" &>/dev/null; then return 0; fi
    if [[ "${i}" -eq "${max}" ]]; then
      echo "ERROR: timed out waiting for ${desc}"; exit 1
    fi
    sleep "${delay}"
  done
}

if [[ "$(uname)" == "Darwin" ]]; then
  if ! podman machine list --format '{{.Running}}' 2>/dev/null | grep -q "true"; then
    echo "==> Starting podman machine..."
    podman machine start
  fi

  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"

  echo "==> Starting k3s inside podman machine..."
  ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
    "sudo systemctl start k3s"

  echo "==> Waiting for k3s API..."
  wait_for 40 3 "k3s kubeconfig" ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no \
    core@localhost "sudo systemctl is-active k3s >/dev/null"

  echo "==> Re-opening k3s API port-forward..."
  pkill -f "ssh.*6443:localhost:6443" 2>/dev/null || true
  ssh -N -f -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -L 6443:localhost:6443 core@localhost
  sleep 1
else
  echo "==> Starting k3s..."
  sudo systemctl start k3s
fi

echo "==> Waiting for node..."
wait_for 40 3 "node ready" kubectl --context "${CONTEXT_NAME}" get nodes
kubectl --context "${CONTEXT_NAME}" wait node --all --for=condition=Ready --timeout=120s
kubectl config use-context "${CONTEXT_NAME}" >/dev/null

echo "==> Starting control plane..."
podman compose -f "${SCRIPT_DIR}/../../localdev/controlplane/docker-compose.yml" start
wait_for 20 1 "control-service to accept connections" \
  curl -sf -o /dev/null "http://localhost:8081/health"
wait_for 20 1 "metrics-service to accept connections" \
  curl -sf -o /dev/null "http://localhost:8084/health"

echo "==> Local dev started. Context: ${CONTEXT_NAME}"
kubectl --context "${CONTEXT_NAME}" get nodes
