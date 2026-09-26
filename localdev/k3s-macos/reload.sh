#!/usr/bin/env bash
# Run by `make reload` once it has rebuilt and pushed every image under TAG: re-installs the
# node-agent/cluster-agent of the local cluster pinned to TAG and bounces their pods so they run
# the new images. The control plane is no part of this: it runs from the tree (make dev-api).
# Every node already pulls from the registry via the registries.yaml install.sh/dev-nodes-up.sh
# wrote. Every wait polls for the actual condition.
set -euo pipefail

CONTEXT_NAME="k3s-local"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/node.sh"
source "${SCRIPT_DIR}/../lib/auth.sh"

TAG="${TAG:?run make reload, which builds the images and exports their TAG}"

# node-agent/cluster-agent run as manifests pinned to a tag (runtime/k8s/infra/install.sh's
# TAG), so a plain pod restart alone would keep pulling the OLD tag under IfNotPresent -- the
# manifest itself must be re-applied with the new TAG for a delete+recreate to fetch anything.
HOST_ADDRESS="$(cat "${SCRIPT_DIR}/../.host-address")"
CLUSTER_NAME="local" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  API_URL="http://host.containers.internal:8081" METRICS_URL="http://host.containers.internal:8084" \
  REGISTRY_URL="${HOST_ADDRESS}:5000" TAG="${TAG}" RUNTIME_TOKEN="$(lib_runtime_token local)" \
  bash "${SCRIPT_DIR}/../../runtime/k8s/infra/install.sh"

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
