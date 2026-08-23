#!/usr/bin/env bash
# Rebuilds every image from current source and pushes it to the local registry
# (localdev/controlplane/docker-compose.yml) under a fresh git-SHA tag. Every node -- k3s
# server and every fake worker container -- already trusts that registry via the
# registries.yaml install.sh/dev-nodes-up.sh wrote into it, so there is nothing left to copy by
# hand: `kubectl delete pods` below is enough to make each pod re-pull. Then force-recreates the
# control-plane containers and bounces the cluster-agent/node-agent pods so they actually run
# the freshly pushed image instead of stale code.
#
# Use this after any Go/Dockerfile change, in place of the old manual `make images` + ad hoc
# `podman exec ... ctr images import` + pod deletes — every wait below polls for the actual
# condition instead of sleeping a fixed guess, so this finishes as soon as the cluster is
# actually ready rather than after some worst-case timeout.
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

echo "==> Rebuilding and pushing all images..."
(cd "${SCRIPT_DIR}/../.." && make images)
TAG="$(cd "${SCRIPT_DIR}/../.." && git rev-parse --short HEAD)"

# node-agent/cluster-agent run as manifests pinned to a SHA tag (runtime/k8s/infra/install.sh's
# TAG), so a plain pod restart alone would keep pulling the OLD tag under IfNotPresent -- the
# manifest itself must be re-applied with the new TAG for a delete+recreate to fetch anything.
REGISTRY_HOST_IP="$(cat "${SCRIPT_DIR}/../.registry-host-ip")"
CLUSTER_NAME="local" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
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
