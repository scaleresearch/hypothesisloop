#!/usr/bin/env bash
# Installs or upgrades the HypothesisLoop runtime of one Kubernetes cluster from this checkout's
# chart (chart/). Re-running it upgrades in place. A cluster without a checkout uses the command
# the UI or `hl cluster add` prints, which installs the same chart from the registry.
#
# Usage: install.sh [--dry-run]
#   --dry-run  check the prerequisites, render the chart and print the plan; change nothing.
#
# Settings (env vars; chart/values.yaml says what each value means):
#   CLUSTER_NAME        -> clusterName (required)
#   API_URL             -> apiURL (required)
#   METRICS_URL         -> metricsURL (required)
#   RUNTIME_TOKEN       -> runtimeToken (required). Handed to helm on stdin, so it is never in a
#                          process's arguments.
#   REGISTRY_URL        -> images.registry (required), reachable from every node
#   TAG                 -> images.tag (required), the TAG `make images` pushed
#   AUTOSCALER_ENABLED  -> autoscalerEnabled (default: false)
#   BUILDER             -> builder.enabled (default: false). true also builds environments in this
#                          cluster, on a rootless BuildKit beside the cluster-agent
#   BUILDER_NODE_IMAGES -> builder.nodeImages.containerdSocket. The containerd socket of the
#                          builder's node (k3s: /run/k3s/containerd/containerd.sock) (default: none)
#   KUBECONFIG_PATH     kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT        kubectl context to target (default: current-context)
set -euo pipefail

RELEASE=hypothesisloop-cluster-agent
NAMESPACE=hypothesisloop
JOBS_NAMESPACE=hypothesisloop-jobs

DRY_RUN=false
case "${1:-}" in
  "") ;;
  --dry-run) DRY_RUN=true ;;
  -h | --help) sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p;}' "$0"; exit 0 ;;
  *) echo "ERROR: unknown argument '$1' (usage: install.sh [--dry-run])" >&2; exit 2 ;;
esac

CLUSTER_NAME="${CLUSTER_NAME:?CLUSTER_NAME must name this cluster as it was registered (hl cluster add NAME)}"
API_URL="${API_URL:?API_URL must be the control-plane API base URL, reachable from inside a pod in this cluster}"
METRICS_URL="${METRICS_URL:?METRICS_URL must be the metrics-service base URL, reachable from every node in this cluster}"
RUNTIME_TOKEN="${RUNTIME_TOKEN:?RUNTIME_TOKEN must be the runtime token this cluster was given when it was registered}"
REGISTRY_URL="${REGISTRY_URL:?REGISTRY_URL must be the registry the agent images are pulled from, as every node reaches it}"
TAG="${TAG:?TAG must name the image tag make images pushed}"
AUTOSCALER_ENABLED="${AUTOSCALER_ENABLED:-false}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
BUILDER="${BUILDER:-false}"
[[ "${BUILDER}" == true || "${BUILDER}" == false ]] || { echo "ERROR: BUILDER must be true or false, not '${BUILDER}'" >&2; exit 1; }
BUILDER_NODE_IMAGES="${BUILDER_NODE_IMAGES:-}"
[[ -z "${BUILDER_NODE_IMAGES}" || "${BUILDER}" == true ]] || { echo "ERROR: BUILDER_NODE_IMAGES loads what the builder builds, and BUILDER is not true" >&2; exit 1; }

for tool in helm kubectl; do
  command -v "${tool}" >/dev/null || { echo "ERROR: ${tool} is required" >&2; exit 1; }
done
# --take-ownership came in helm 3.17.
helm_version="$(helm version --template '{{.Version}}')"
if [[ "${helm_version}" =~ ^v3\.([0-9]+)\. ]] && (( BASH_REMATCH[1] < 17 )); then
  echo "ERROR: helm ${helm_version} is too old; 3.17 or newer is required" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi
kube=(kubectl --context "${KUBE_CONTEXT}")

server_version="$("${kube[@]}" version -o json | sed -n '/"serverVersion"/,/}/s/.*"gitVersion": *"\([^"]*\)".*/\1/p')"
[[ -n "${server_version}" ]] || { echo "ERROR: the cluster of context ${KUBE_CONTEXT} does not answer" >&2; exit 1; }
node_count="$("${kube[@]}" get nodes --no-headers | wc -l | tr -d ' ')"
accelerators="$("${kube[@]}" get nodes -o jsonpath='{range .items[*]}{.status.allocatable}{"\n"}{end}' \
  | grep -oE '"[a-z0-9.-]+\.[a-z]+/[a-z0-9_.-]+":"[0-9]+"' \
  | awk -F'"' '{total[$2] += $4; nodes[$2]++} END {for (r in total) printf "      %s: %d on %d nodes\n", r, total[r], nodes[r]}' | sort || true)"
device_classes="$("${kube[@]}" get deviceclasses -o name 2>/dev/null | sed 's|.*/||' | tr '\n' ' ' || true)"

chart_values=(
  --set-string clusterName="${CLUSTER_NAME}"
  --set-string apiURL="${API_URL}"
  --set-string metricsURL="${METRICS_URL}"
  --set-string autoscalerEnabled="${AUTOSCALER_ENABLED}"
  --set-string images.registry="${REGISTRY_URL}"
  --set-string images.tag="${TAG}"
  --set builder.enabled="${BUILDER}"
  --set-string builder.nodeImages.containerdSocket="${BUILDER_NODE_IMAGES}"
  --values -
)

cat <<PLAN
==> Plan for cluster '${CLUSTER_NAME}'
    Context:    ${KUBE_CONTEXT} (Kubernetes ${server_version}, ${node_count} nodes)
    Release:    helm release ${RELEASE} in namespace ${NAMESPACE}
    Images:     ${REGISTRY_URL}/hypothesisloop-cluster-agent:${TAG}
                ${REGISTRY_URL}/hypothesisloop-node-agent:${TAG}
    Creates:    namespace ${NAMESPACE}: Deployment ${RELEASE}, DaemonSet hypothesisloop-node-agent on every node,
                  Secret hypothesisloop-runtime-token$([[ "${BUILDER}" == true ]] && echo ", builder cache volume")
                namespace ${JOBS_NAMESPACE}, where jobs run
                PriorityClasses hypothesisloop-jobs-guaranteed and hypothesisloop-jobs-burst
    RBAC:       create and delete Jobs, pods, Services, Secrets and ResourceClaimTemplates in ${JOBS_NAMESPACE}
                read nodes, pods and DRA devices across the cluster
    Outbound:   ${API_URL} and ${METRICS_URL}, and the registry. Nothing inbound.
    Accelerators seen on the nodes:
${accelerators:-      no device plugin resources}
      DRA device classes: ${device_classes:-none}
    Uninstall:  helm uninstall ${RELEASE} -n ${NAMESPACE} (or destroy.sh)
PLAN

if [[ "${DRY_RUN}" == true ]]; then
  helm template "${RELEASE}" "${SCRIPT_DIR}/chart" --namespace "${NAMESPACE}" --kube-version "${server_version}" \
    "${chart_values[@]}" >/dev/null <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES
  echo "==> Dry run: the chart renders with these values. Nothing was changed."
  exit 0
fi

echo "==> Installing"
# --take-ownership adopts a jobs namespace that an agent from before the chart created.
helm upgrade --install "${RELEASE}" "${SCRIPT_DIR}/chart" \
  --kube-context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" --create-namespace --take-ownership \
  "${chart_values[@]}" <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES

# helm reports success once the objects exist; a DaemonSet whose pods never start on a NotReady
# node is only caught by waiting for the rollout itself.
ROLLOUT_TIMEOUT=5m
for workload in "deployment/${RELEASE}" daemonset/hypothesisloop-node-agent; do
  echo "==> Waiting for ${workload} to roll out (timeout ${ROLLOUT_TIMEOUT})"
  if ! "${kube[@]}" -n "${NAMESPACE}" rollout status "${workload}" --timeout="${ROLLOUT_TIMEOUT}"; then
    echo "ERROR: ${workload} did not finish rolling out within ${ROLLOUT_TIMEOUT}; its pods and the nodes they are on:" >&2
    "${kube[@]}" -n "${NAMESPACE}" get pods -l "app=${workload#*/}" -o wide >&2
    exit 1
  fi
done
echo "==> Cluster '${CLUSTER_NAME}' is installed. It shows Connected in the UI (Compute Resources) within a minute."
echo "    Logs: kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} logs deploy/${RELEASE}"
