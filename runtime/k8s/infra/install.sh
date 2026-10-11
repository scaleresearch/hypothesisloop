#!/usr/bin/env bash
# Installs or upgrades the HypothesisLoop runtime (chart/: node-agent DaemonSet for per-node
# metrics, cluster-agent Deployment, the only component with credentials to this cluster's API)
# on a target Kubernetes cluster where training jobs run. Run once per cluster; re-running
# upgrades it in place. The chart installs the hypothesisloop-jobs namespace and the
# PriorityClasses of job pods too. --take-ownership makes a jobs namespace that an agent from
# before the chart created the release's.
#
# Parameters (env vars; see chart/values.yaml for what each value means):
#   CLUSTER_NAME       -> clusterName (required)
#   API_URL            -> apiURL (required)
#   METRICS_URL        -> metricsURL (required)
#   RUNTIME_TOKEN      -> runtimeToken (required). Handed to helm on stdin, so it is never in a
#                         process's arguments.
#   AUTOSCALER_ENABLED -> autoscalerEnabled (default: false)
#   REGISTRY_URL       -> images.registry. Must already be reachable from every node; providing
#                         that reachability is the cluster provisioner's job (required)
#   TAG                -> images.tag, the TAG `make images` pushed (required)
#   KUBECONFIG_PATH    kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT       kubectl context to target (default: current-context)
#   BUILDER            -> builder.enabled. true also builds environments in this cluster, on a
#                         rootless BuildKit beside the cluster-agent (default: false)
#   BUILDER_NODE_IMAGES -> builder.nodeImages.containerdSocket. The containerd socket of the
#                         builder's node (k3s: /run/k3s/containerd/containerd.sock): each build is
#                         loaded there too, so a job on that node does not pull it (default: none)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:?CLUSTER_NAME must name this cluster (make cluster-agent-up CLUSTER=<name>)}"
API_URL="${API_URL:?API_URL must be the control-plane API base URL, reachable from inside a pod in this cluster}"
METRICS_URL="${METRICS_URL:?METRICS_URL must be the metrics-service base URL, reachable from every node in this cluster}"
RUNTIME_TOKEN="${RUNTIME_TOKEN:?RUNTIME_TOKEN must be the runtime token this cluster was given when it was registered}"
AUTOSCALER_ENABLED="${AUTOSCALER_ENABLED:-false}"
REGISTRY_URL="${REGISTRY_URL:?REGISTRY_URL is required: the registry every node pulls the runtime images from}"
TAG="${TAG:?TAG must name the image tag make images pushed}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
BUILDER="${BUILDER:-false}"
[[ "${BUILDER}" == true || "${BUILDER}" == false ]] || { echo "ERROR: BUILDER must be true or false, not '${BUILDER}'" >&2; exit 1; }
BUILDER_NODE_IMAGES="${BUILDER_NODE_IMAGES:-}"
[[ -z "${BUILDER_NODE_IMAGES}" || "${BUILDER}" == true ]] || { echo "ERROR: BUILDER_NODE_IMAGES loads what the builder builds, and BUILDER is not true" >&2; exit 1; }

command -v helm >/dev/null || { echo "ERROR: helm is required (https://helm.sh/docs/intro/install/)" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

echo "==> Installing cluster agent on cluster '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT}, API: ${API_URL})"
helm upgrade --install hypothesisloop-cluster-agent "${SCRIPT_DIR}/chart" \
  --kube-context "${KUBE_CONTEXT}" --namespace hypothesisloop --create-namespace --take-ownership \
  --set-string clusterName="${CLUSTER_NAME}" \
  --set-string apiURL="${API_URL}" \
  --set-string metricsURL="${METRICS_URL}" \
  --set-string autoscalerEnabled="${AUTOSCALER_ENABLED}" \
  --set-string images.registry="${REGISTRY_URL}" \
  --set-string images.tag="${TAG}" \
  --set builder.enabled="${BUILDER}" \
  --set-string builder.nodeImages.containerdSocket="${BUILDER_NODE_IMAGES}" \
  --values - <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES

# helm reports success once the objects exist; a DaemonSet whose pods never start on a NotReady
# node is only caught by waiting for the rollout itself.
ROLLOUT_TIMEOUT=5m
workloads=(deployment/hypothesisloop-cluster-agent daemonset/hypothesisloop-node-agent)
for workload in "${workloads[@]}"; do
  echo "==> Waiting for ${workload} to roll out (timeout ${ROLLOUT_TIMEOUT})"
  if ! kubectl --context "${KUBE_CONTEXT}" -n hypothesisloop rollout status "${workload}" --timeout="${ROLLOUT_TIMEOUT}"; then
    echo "ERROR: ${workload} did not finish rolling out within ${ROLLOUT_TIMEOUT}; its pods and the nodes they are on:" >&2
    kubectl --context "${KUBE_CONTEXT}" -n hypothesisloop get pods -l "app=${workload#*/}" -o wide >&2
    exit 1
  fi
done
echo "==> Cluster agent on '${CLUSTER_NAME}' is rolled out"
