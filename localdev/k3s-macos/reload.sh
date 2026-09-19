#!/usr/bin/env bash
# Run by `make reload` once it has rebuilt and pushed every image under TAG: re-installs the
# node-agent/cluster-agent pinned to TAG, force-recreates the control-plane containers and
# bounces the pods so they run the new images. Every node already pulls from the registry via
# the registries.yaml install.sh/dev-nodes-up.sh wrote. Every wait polls for the actual condition.
set -euo pipefail

CONTEXT_NAME="k3s-local"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/node.sh"

TAG="${TAG:?run make reload, which builds the images and exports their TAG}"

# node-agent/cluster-agent run as manifests pinned to a tag (runtime/k8s/infra/install.sh's
# TAG), so a plain pod restart alone would keep pulling the OLD tag under IfNotPresent -- the
# manifest itself must be re-applied with the new TAG for a delete+recreate to fetch anything.
HOST_ADDRESS="$(cat "${SCRIPT_DIR}/../.host-address")"
CLUSTER_NAME="local" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  REGISTRY_URL="${HOST_ADDRESS}:5000" TAG="${TAG}" \
  bash "${SCRIPT_DIR}/../../runtime/k8s/infra/install.sh"

echo "==> Recreating control-plane containers..."
TAG="${TAG}" podman compose -f "${SCRIPT_DIR}/../../localdev/controlplane/docker-compose.yml" \
  up -d --force-recreate control-service metrics-service >/dev/null
wait_for 20 1 "control-service to accept connections" \
curl -sf -o /dev/null "http://localhost:${API_HOST_PORT:-8081}/health"

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
