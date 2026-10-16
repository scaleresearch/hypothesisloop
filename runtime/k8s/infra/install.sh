#!/usr/bin/env bash
# Installs or upgrades the HypothesisLoop runtime of one Kubernetes cluster from this checkout's
# chart (chart/). Re-running it upgrades in place. A cluster without a checkout uses the command
# the UI or `hl cluster add` prints, which installs the same chart from the registry.
#
# Usage: install.sh [--dry-run]
#   --dry-run  check the prerequisites, render the chart and print the plan, then check from
#              inside the cluster, with a short-lived pod in the namespace default that runs the
#              agent's image, that a node pulls both images, that the pod reaches the API and the
#              metrics service, and that the API takes the runtime token. Nothing else is changed.
#
# Settings (env vars; chart/values.yaml says what each value means):
#   CLUSTER_NAME        -> clusterName (required)
#   API_URL             -> apiURL (required). Pods resolve it with the cluster's DNS, so a
#                          control plane on a tailnet is named by its IP, not its tailnet name.
#   METRICS_URL         -> metricsURL (required), named the same way
#   RUNTIME_TOKEN       -> runtimeToken (required). Handed to helm on stdin, so it is never in a
#                          process's arguments.
#   REGISTRY_URL        -> images.registry (required), reachable from every node
#   TAG                 -> images.tag (required), the TAG `make images` pushed
#   AUTOSCALER_ENABLED  -> autoscalerEnabled (default: false)
#   BUILDER             -> builder.enabled (default: false). true also builds environments in this
#                          cluster, on a rootless BuildKit beside the cluster-agent
#   BUILDER_NODE_IMAGES -> builder.nodeImages.containerdSocket. The containerd socket of the
#                          builder's node (k3s: /run/k3s/containerd/containerd.sock) (default: none)
#   INSECURE_REGISTRY   -> insecureRegistry.enabled (default: false). true has every node's
#                          containerd pull from REGISTRY_URL over plain HTTP, for a registry with
#                          no TLS such as the local dev registry
#   KUBECONFIG_PATH     kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT        kubectl context to target (default: current-context) -> kubeContext
set -euo pipefail

RELEASE=hypothesisloop-cluster-agent
NAMESPACE=hypothesisloop
JOBS_NAMESPACE=hypothesisloop-jobs
# How long the dry run's check pod may take to pull its images and run.
CHECK_TIMEOUT_SECONDS="${CHECK_TIMEOUT_SECONDS:-180}"

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
INSECURE_REGISTRY="${INSECURE_REGISTRY:-false}"
[[ "${INSECURE_REGISTRY}" == true || "${INSECURE_REGISTRY}" == false ]] || { echo "ERROR: INSECURE_REGISTRY must be true or false, not '${INSECURE_REGISTRY}'" >&2; exit 1; }

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

# plural <count> <noun>: "1 node", "2 nodes".
plural() {
  if [[ "$1" == 1 ]]; then echo "1 $2"; else echo "$1 $2s"; fi
}

server_version="$("${kube[@]}" version -o json | sed -n '/"serverVersion"/,/}/s/.*"gitVersion": *"\([^"]*\)".*/\1/p')"
[[ -n "${server_version}" ]] || { echo "ERROR: the cluster of context ${KUBE_CONTEXT} does not answer" >&2; exit 1; }
node_count="$("${kube[@]}" get nodes --no-headers | wc -l | tr -d ' ')"
accelerators="$("${kube[@]}" get nodes -o jsonpath='{range .items[*]}{.status.allocatable}{"\n"}{end}' \
  | grep -oE '"[a-z0-9.-]+\.[a-z]+/[a-z0-9_.-]+":"[0-9]+"' \
  | awk -F'"' '{total[$2] += $4; nodes[$2]++} END {for (r in total) printf "      %s: %d on %d node%s\n", r, total[r], nodes[r], (nodes[r] == 1 ? "" : "s")}' | sort || true)"
device_classes="$("${kube[@]}" get deviceclasses -o name 2>/dev/null | sed 's|.*/||' | tr '\n' ' ' || true)"
# One line per ResourceSlice: its driver, its node and how many devices it publishes.
dra_devices="$("${kube[@]}" get resourceslices -o go-template='{{range .items}}{{.spec.driver}} {{or .spec.nodeName "-"}} {{with .spec.devices}}{{len .}}{{else}}0{{end}}{{"\n"}}{{end}}' 2>/dev/null \
  | awk 'NF == 3 {total[$1] += $3; if (!seen[$1 " " $2]++) nodes[$1]++} END {for (d in total) printf "      %s: %d device%s on %d node%s\n", d, total[d], (total[d] == 1 ? "" : "s"), nodes[d], (nodes[d] == 1 ? "" : "s")}' | sort || true)"
# The release's revision now, empty when it is not installed.
revision="$(helm list --kube-context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" --filter "^${RELEASE}\$" -o json 2>/dev/null \
  | sed -n 's/.*"revision": *"\{0,1\}\([0-9][0-9]*\).*/\1/p' | head -1 || true)"

chart_values=(
  --set-string clusterName="${CLUSTER_NAME}"
  --set-string apiURL="${API_URL}"
  --set-string metricsURL="${METRICS_URL}"
  --set-string autoscalerEnabled="${AUTOSCALER_ENABLED}"
  --set-string images.registry="${REGISTRY_URL}"
  --set-string images.tag="${TAG}"
  --set builder.enabled="${BUILDER}"
  --set-string builder.nodeImages.containerdSocket="${BUILDER_NODE_IMAGES}"
  --set insecureRegistry.enabled="${INSECURE_REGISTRY}"
  --set-string kubeContext="${KUBE_CONTEXT}"
  --values -
)

if [[ -n "${revision}" ]]; then
  release_line="Upgrades:   helm release ${RELEASE} in namespace ${NAMESPACE}, revision ${revision} to $((revision + 1))"
  objects_verb="Updates:   "
else
  release_line="Release:    helm release ${RELEASE} in namespace ${NAMESPACE}, not installed yet"
  objects_verb="Creates:   "
fi
cat <<PLAN
==> Plan for cluster '${CLUSTER_NAME}'
    Context:    ${KUBE_CONTEXT} (Kubernetes ${server_version}, $(plural "${node_count}" node))
    ${release_line}
    Images:     ${REGISTRY_URL}/hypothesisloop-cluster-agent:${TAG}
                ${REGISTRY_URL}/hypothesisloop-node-agent:${TAG}
    ${objects_verb} namespace ${NAMESPACE}: Deployment ${RELEASE}, DaemonSet hypothesisloop-node-agent on every node,
                  Secret hypothesisloop-runtime-token$([[ "${BUILDER}" == true ]] && echo ", builder cache volume")$([[ "${INSECURE_REGISTRY}" == true ]] && echo ",
                  DaemonSet hypothesisloop-insecure-registry: containerd pulls ${REGISTRY_URL%%/*} over plain HTTP")
                namespace ${JOBS_NAMESPACE}, where jobs run
                PriorityClasses hypothesisloop-jobs-guaranteed and hypothesisloop-jobs-burst
    RBAC:       create and delete Jobs, pods, Services, Secrets and ResourceClaimTemplates in ${JOBS_NAMESPACE}
                read nodes, pods and DRA devices across the cluster
    Outbound:   ${API_URL} and ${METRICS_URL}, and the registry. Nothing inbound.
    Accelerators seen on the nodes:
${accelerators:-      no device plugin resources}
      DRA device classes: ${device_classes:-none}
    DRA devices published:
${dra_devices:-      none}
    Uninstall:  helm --kube-context ${KUBE_CONTEXT} uninstall ${RELEASE} -n ${NAMESPACE} (or destroy.sh)
PLAN

# check_from_inside runs the agent's image once on a node of the cluster, as the install's pods
# will: the node pulls both images, and the agent's check subcommand asks the API's and the
# metrics service's /healthz and an endpoint that only answers this cluster's token. The token
# reaches the pod through a Secret written from stdin, never an argument. Both are deleted after.
check_from_inside() {
  local name phase waiting node logs state deadline
  name="hypothesisloop-install-check-$(date +%s)-${RANDOM}"
  # shellcheck disable=SC2064 # the name is fixed now
  trap "\"\${kube[@]}\" -n default delete pod,secret ${name} --ignore-not-found --wait=false >/dev/null 2>&1 || true" EXIT
  "${kube[@]}" apply -f - >/dev/null <<MANIFEST
apiVersion: v1
kind: Secret
metadata:
  name: ${name}
  namespace: default
stringData:
  token: "${RUNTIME_TOKEN}"
---
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: default
  labels:
    app: hypothesisloop-install-check
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  initContainers:
    - name: node-agent-image
      image: ${REGISTRY_URL}/hypothesisloop-node-agent:${TAG}
      imagePullPolicy: IfNotPresent
      command: ["true"]
  containers:
    - name: check
      image: ${REGISTRY_URL}/hypothesisloop-cluster-agent:${TAG}
      imagePullPolicy: IfNotPresent
      command: ["/cluster-agent", "check"]
      env:
        - {name: CLUSTER_NAME, value: "${CLUSTER_NAME}"}
        - {name: API_URL, value: "${API_URL}"}
        - {name: METRICS_URL, value: "${METRICS_URL}"}
        - name: RUNTIME_TOKEN
          valueFrom: {secretKeyRef: {name: ${name}, key: token}}
MANIFEST
  echo "==> Checking from inside the cluster (pod default/${name}, up to ${CHECK_TIMEOUT_SECONDS}s)"
  deadline=$(( $(date +%s) + CHECK_TIMEOUT_SECONDS ))
  while :; do
    state="$("${kube[@]}" -n default get pod "${name}" -o jsonpath='{.status.phase}|{.spec.nodeName}|{range .status.initContainerStatuses[*]}{.state.waiting.reason}:{.state.waiting.message};{end}{range .status.containerStatuses[*]}{.state.waiting.reason}:{.state.waiting.message};{end}' 2>/dev/null || true)"
    phase="${state%%|*}" node="${state#*|}" waiting="${node#*|}" node="${node%%|*}"
    case "${waiting}" in
      *ErrImagePull:* | *ImagePullBackOff:* | *InvalidImageName:*)
        echo "    FAIL images: node ${node:-?} cannot pull them: ${waiting//;/ }" >&2
        if [[ "${INSECURE_REGISTRY}" == false && "${REGISTRY_URL}" =~ ^[^/]+:[0-9]+ ]]; then
          echo "         A registry of plain HTTP needs INSECURE_REGISTRY=true, whose node config a dry run does not install." >&2
        fi
        return 1
        ;;
    esac
    case "${phase}" in
      Succeeded | Failed) break ;;
    esac
    if (( $(date +%s) >= deadline )); then
      echo "    FAIL the check pod did not finish within ${CHECK_TIMEOUT_SECONDS}s (phase ${phase:-unknown}${waiting:+, ${waiting//;/ }}):" >&2
      "${kube[@]}" -n default get events --field-selector "involvedObject.name=${name}" >&2 || true
      return 1
    fi
    sleep 2
  done
  echo "    ok   images: node ${node} pulled ${REGISTRY_URL}/hypothesisloop-{cluster,node}-agent:${TAG}"
  logs="$("${kube[@]}" -n default logs "${name}" -c check 2>&1 || true)"
  printf '%s\n' "${logs}" | sed 's/^/    /'
  [[ "${phase}" == Succeeded ]]
}

if [[ "${DRY_RUN}" == true ]]; then
  helm template "${RELEASE}" "${SCRIPT_DIR}/chart" --namespace "${NAMESPACE}" --kube-version "${server_version}" \
    "${chart_values[@]}" >/dev/null <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES
  echo "==> The chart renders with these values"
  if ! check_from_inside; then
    echo "==> Dry run: the install would not work as it stands (above). Nothing was changed." >&2
    exit 1
  fi
  echo "==> Dry run: every check passed. Nothing was changed."
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
