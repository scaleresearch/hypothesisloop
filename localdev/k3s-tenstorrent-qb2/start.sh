#!/usr/bin/env bash
# Resume the Tenstorrent k3s stack after stop.sh. Assumes it was previously
# provisioned with install.sh — if no cluster exists yet, run that instead.
set -euo pipefail

CONTEXT_NAME="k3s-tt"
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

echo "==> Starting k3s..."
sudo systemctl start k3s

echo "==> Waiting for node..."
wait_for 40 3 "node ready" kubectl --context "${CONTEXT_NAME}" get nodes
kubectl --context "${CONTEXT_NAME}" wait node --all --for=condition=Ready --timeout=120s
kubectl config use-context "${CONTEXT_NAME}" >/dev/null

echo "==> Tenstorrent stack started."
bash "${SCRIPT_DIR}/status.sh"
