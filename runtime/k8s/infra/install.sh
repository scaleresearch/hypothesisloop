#!/usr/bin/env bash
# Installs or upgrades the HypothesisLoop runtime (chart/: node-agent DaemonSet for per-node
# metrics, cluster-agent Deployment, the only component with credentials to this cluster's API)
# on a target Kubernetes cluster where training jobs run. Run once per cluster; re-running
# upgrades it in place. The chart installs the hypothesisloop-jobs namespace and the
# PriorityClasses of job pods too.
#
# Parameters (env vars; see chart/values.yaml for what each value means):
#   CLUSTER_NAME       -> clusterName (required)
#   API_URL            -> apiURL (required)
#   METRICS_URL        -> metricsURL (required)
#   RUNTIME_TOKEN      -> runtimeToken (required). Handed to helm on stdin, so it is never in a
#                         process's arguments.
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

# A cluster installed before the chart owned the jobs namespace has one its agent created, and
# helm installs over an object only once it is marked as this release's.
if kubectl --context "${KUBE_CONTEXT}" get namespace hypothesisloop-jobs >/dev/null 2>&1; then
  kubectl --context "${KUBE_CONTEXT}" label namespace hypothesisloop-jobs --overwrite app.kubernetes.io/managed-by=Helm >/dev/null
  kubectl --context "${KUBE_CONTEXT}" annotate namespace hypothesisloop-jobs --overwrite \
    meta.helm.sh/release-name=hypothesisloop-cluster-agent meta.helm.sh/release-namespace=hypothesisloop >/dev/null
fi

echo "==> Installing cluster agent on cluster '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT}, API: ${API_URL})"
helm upgrade --install hypothesisloop-cluster-agent "${SCRIPT_DIR}/chart" \
  --kube-context "${KUBE_CONTEXT}" --namespace hypothesisloop --create-namespace \
  --set-string clusterName="${CLUSTER_NAME}" \
  --set-string apiURL="${API_URL}" \
  --set-string metricsURL="${METRICS_URL}" \
  --set-string autoscalerEnabled="${AUTOSCALER_ENABLED}" \
  --set-string images.registry="${REGISTRY_URL}" \
  --set-string images.tag="${TAG}" \
  --values - <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES
