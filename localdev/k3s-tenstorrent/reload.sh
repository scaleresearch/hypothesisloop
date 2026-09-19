#!/usr/bin/env bash
# Counterpart to k3s-macos/reload.sh for this host, run by `make tt-reload` once it has rebuilt
# and pushed every image under TAG: re-installs the node-agent/cluster-agent pinned to TAG (so the
# pull actually happens under imagePullPolicy: IfNotPresent), force-recreates the control-plane
# containers, and bounces the pods so they run current code.
set -euo pipefail

CONTEXT_NAME="k3s-tt"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/node.sh"

TAG="${TAG:?run make tt-reload, which builds the images and exports their TAG}"

HOST_ADDRESS="$(cat "${SCRIPT_DIR}/../.host-address")"
CLUSTER_NAME="box-a" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  API_URL="http://${HOST_ADDRESS}:8081" METRICS_URL="http://${HOST_ADDRESS}:8084" \
  REGISTRY_URL="${HOST_ADDRESS}:5000" TAG="${TAG}" \
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
