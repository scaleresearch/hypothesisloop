#!/usr/bin/env bash
# Counterpart to k3s-macos/reload.sh for this host: k3s runs natively on Linux here (no
# podman-machine VM to hop into), so images just need `sudo k3s ctr images import -`
# directly instead of an SSH round-trip. Rebuilds every image from current source, imports
# the ones the cluster actually runs into k3s's containerd, force-recreates the
# control-plane containers, and bounces the cluster-agent/node-agent pods so they pull the
# freshly-imported image instead of running stale code.
set -euo pipefail

CONTEXT_NAME="k3s-tt"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGES=(hypothesisloop-node-agent hypothesisloop-cluster-agent hypothesisloop-workload)

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

echo "==> Rebuilding all images..."
(cd "${SCRIPT_DIR}/../.." && make images)

echo "==> Importing images into k3s..."
for img in "${IMAGES[@]}"; do
  if podman image exists "localhost/${img}:latest" 2>/dev/null; then
    podman save "localhost/${img}:latest" | sudo k3s ctr images import -
  fi
done

echo "==> Recreating control-plane containers..."
bash "${SCRIPT_DIR}/../../controlplane/infra/podman.sh" reload >/dev/null
wait_for 20 1 "control-service to accept connections" \
  curl -sf -o /dev/null "http://localhost:8082/experiments"

if kubectl --context "${CONTEXT_NAME}" get deploy/hypothesisloop-cluster-agent -n hypothesisloop &>/dev/null; then
  echo "==> Restarting cluster-agent/node-agent pods..."
  kubectl --context "${CONTEXT_NAME}" -n hypothesisloop delete pods --all >/dev/null
  wait_for 30 2 "cluster-agent pod ready" \
    kubectl --context "${CONTEXT_NAME}" -n hypothesisloop wait --for=condition=Ready \
      pod -l app=hypothesisloop-cluster-agent --timeout=1s
  wait_for 30 2 "node-agent pods ready" \
    kubectl --context "${CONTEXT_NAME}" -n hypothesisloop wait --for=condition=Ready \
      pod -l app=hypothesisloop-node-agent --timeout=1s
fi

echo "==> Reload complete."
kubectl --context "${CONTEXT_NAME}" -n hypothesisloop get pods 2>/dev/null || true
