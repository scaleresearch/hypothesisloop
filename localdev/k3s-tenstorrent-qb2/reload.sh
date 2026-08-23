#!/usr/bin/env bash
# Counterpart to k3s-macos/reload.sh for this host: rebuilds every image and pushes it to the
# local registry under a fresh git-SHA tag, re-applies the node-agent/cluster-agent manifests
# pinned to that new tag (so the pull actually happens under imagePullPolicy: IfNotPresent),
# force-recreates the control-plane containers, and bounces the pods so they run current code.
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

echo "==> Rebuilding and pushing all images..."
(cd "${SCRIPT_DIR}/../.." && make images)
TAG="$(cd "${SCRIPT_DIR}/../.." && git rev-parse --short HEAD)"

HOST_IP="$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if ($i=="src") print $(i+1); exit}')"
REGISTRY_HOST_IP="$(cat "${SCRIPT_DIR}/../.registry-host-ip")"
CLUSTER_NAME="tt-quietbox" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  API_URL="http://${HOST_IP}:8081" METRICS_URL="http://${HOST_IP}:8084" \
  REGISTRY_URL="${REGISTRY_HOST_IP}:5000" TAG="${TAG}" \
  bash "${SCRIPT_DIR}/../../runtime/k8s/infra/install.sh"

echo "==> Recreating control-plane containers..."
TAG="${TAG}" podman compose -f "${SCRIPT_DIR}/../../localdev/controlplane/docker-compose.yml" \
  up -d --force-recreate control-service metrics-service >/dev/null
wait_for 20 1 "control-service to accept connections" \
  curl -sf -o /dev/null "http://localhost:8081/health"

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
