#!/usr/bin/env bash
# Installs or upgrades the HypothesisLoop runtime (chart/: node-agent DaemonSet for per-node
# metrics, cluster-agent Deployment, the only component with credentials to this cluster's API)
# on a target Kubernetes cluster where training jobs run. Run once per cluster; re-running
# upgrades it in place. The hypothesisloop-jobs namespace and PriorityClasses are created by
# cluster-agent itself on startup.
#
# Parameters (env vars; see chart/values.yaml for what each value means):
#   CLUSTER_NAME       -> clusterName (required)
#   API_URL            -> apiURL (required)
#   METRICS_URL        -> metricsURL (required)
#   RUNTIME_TOKEN      -> runtimeToken (required)
#   AUTOSCALER_ENABLED -> autoscalerEnabled (default: false)
#   REGISTRY_URL       -> images.registry. Must already be reachable from every node; providing
#                         that reachability is the cluster provisioner's job (default: localhost:5000)
#   TAG                -> images.tag, the TAG `make images` pushed (required)
#   KUBECONFIG_PATH    kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT       kubectl context to target (default: current-context)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:?CLUSTER_NAME must name this cluster (make cluster-agent-up CLUSTER=<name>)}"
API_URL="${API_URL:?API_URL must be the control-plane API base URL, reachable from inside a pod in this cluster}"
METRICS_URL="${METRICS_URL:?METRICS_URL must be the metrics-service base URL, reachable from every node in this cluster}"
RUNTIME_TOKEN="${RUNTIME_TOKEN:?RUNTIME_TOKEN must be the runtime token this cluster was given when it was registered}"
AUTOSCALER_ENABLED="${AUTOSCALER_ENABLED:-false}"
REGISTRY_URL="${REGISTRY_URL:-localhost:5000}"
TAG="${TAG:?TAG must name the image tag make images pushed}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

command -v helm >/dev/null || { echo "ERROR: helm is required (https://helm.sh/docs/intro/install/)" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

echo "==> Installing cluster agent on cluster '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT}, API: ${API_URL})"
helm upgrade --install hypothesisloop-cluster-agent "${SCRIPT_DIR}/chart" \
  --kube-context "${KUBE_CONTEXT}" --namespace hypothesisloop --create-namespace \
  --set-string clusterName="${CLUSTER_NAME}" \
  --set-string apiURL="${API_URL}" \
  --set-string metricsURL="${METRICS_URL}" \
  --set-string runtimeToken="${RUNTIME_TOKEN}" \
  --set-string autoscalerEnabled="${AUTOSCALER_ENABLED}" \
  --set-string images.registry="${REGISTRY_URL}" \
  --set-string images.tag="${TAG}"
