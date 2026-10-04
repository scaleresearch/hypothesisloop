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
#                         that reachability is the cluster provisioner's job (default: localhost:5000)
#   TAG                -> images.tag, the TAG `make images` pushed (required)
#   KUBECONFIG_PATH    kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT       kubectl context to target (default: current-context)
#   BUILDER_CLIENT_DIR -> builder.*. Set, the cluster also runs the platform's image builder,
#                         reached at the host this context reaches the cluster's API at, and this
#                         directory gets what the control plane connects with: addr, ca.pem,
#                         cert.pem and key.pem. Every install makes new certificates, so the
#                         previous client's stop working (default: no builder)
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
BUILDER_CLIENT_DIR="${BUILDER_CLIENT_DIR:-}"

command -v helm >/dev/null || { echo "ERROR: helm is required (https://helm.sh/docs/intro/install/)" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

# cert NAME SUBJECT EXTENSIONS [SIGNER]: NAME.pem and NAME-key.pem in ${certs}, signed by
# SIGNER's key, or by its own without one.
cert() {
  local name="$1" subject="$2" extensions="$3" signer="${4:-}"
  openssl ecparam -name prime256v1 -genkey -noout -out "${certs}/${name}-key.pem"
  openssl req -new -key "${certs}/${name}-key.pem" -subj "/CN=${subject}" -out "${certs}/${name}.csr"
  if [[ -z "${signer}" ]]; then
    openssl x509 -req -in "${certs}/${name}.csr" -signkey "${certs}/${name}-key.pem" -sha256 -days 3650 \
      -extfile <(printf '%s\n' "${extensions}") -out "${certs}/${name}.pem"
  else
    openssl x509 -req -in "${certs}/${name}.csr" -CA "${certs}/${signer}.pem" -CAkey "${certs}/${signer}-key.pem" \
      -set_serial "0x$(openssl rand -hex 16)" -sha256 -days 3650 -extfile <(printf '%s\n' "${extensions}") -out "${certs}/${name}.pem"
  fi
}

builder_values=()
if [[ -n "${BUILDER_CLIENT_DIR}" ]]; then
  BUILDER_HOST="$(kubectl config view --context "${KUBE_CONTEXT}" --minify -o jsonpath='{.clusters[0].cluster.server}')"
  BUILDER_HOST="${BUILDER_HOST#*://}"
  BUILDER_HOST="${BUILDER_HOST%%[:/]*}"
  case "${BUILDER_HOST}" in
    "" | localhost | 127.* | "[" | host.containers.internal)
      echo "ERROR: context ${KUBE_CONTEXT} reaches the cluster's API at '${BUILDER_HOST}', where the control plane would not reach the builder; attach with a context that names the cluster's own host" >&2
      exit 1 ;;
  esac
  if [[ "${BUILDER_HOST}" =~ ^[0-9.]+$ ]]; then san="IP:${BUILDER_HOST}"; else san="DNS:${BUILDER_HOST}"; fi
  certs="$(mktemp -d)"
  trap 'rm -rf "${certs}"' EXIT
  cert ca hypothesisloop-builder 'basicConstraints=critical,CA:TRUE
keyUsage=critical,keyCertSign'
  cert server "${BUILDER_HOST}" "subjectAltName=${san}
extendedKeyUsage=serverAuth" ca
  cert client hypothesisloop-control-plane 'extendedKeyUsage=clientAuth' ca
  builder_values=(--set builder.enabled=true --set-file builder.tls.ca="${certs}/ca.pem"
    --set-file builder.tls.cert="${certs}/server.pem" --set-file builder.tls.key="${certs}/server-key.pem")
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
  ${builder_values[@]+"${builder_values[@]}"} \
  --values - <<VALUES
runtimeToken: "${RUNTIME_TOKEN}"
VALUES

# helm reports success once the objects exist; a DaemonSet whose pods never start on a NotReady
# node is only caught by waiting for the rollout itself.
ROLLOUT_TIMEOUT=5m
workloads=(deployment/hypothesisloop-cluster-agent daemonset/hypothesisloop-node-agent)
if [[ -n "${BUILDER_CLIENT_DIR}" ]]; then
  workloads+=(deployment/hypothesisloop-builder)
fi
for workload in "${workloads[@]}"; do
  echo "==> Waiting for ${workload} to roll out (timeout ${ROLLOUT_TIMEOUT})"
  if ! kubectl --context "${KUBE_CONTEXT}" -n hypothesisloop rollout status "${workload}" --timeout="${ROLLOUT_TIMEOUT}"; then
    echo "ERROR: ${workload} did not finish rolling out within ${ROLLOUT_TIMEOUT}; its pods and the nodes they are on:" >&2
    kubectl --context "${KUBE_CONTEXT}" -n hypothesisloop get pods -l "app=${workload#*/}" -o wide >&2
    exit 1
  fi
done
echo "==> Cluster agent on '${CLUSTER_NAME}' is rolled out"

if [[ -n "${BUILDER_CLIENT_DIR}" ]]; then
  port="$(kubectl --context "${KUBE_CONTEXT}" -n hypothesisloop get service hypothesisloop-builder -o jsonpath='{.spec.ports[0].nodePort}')"
  mkdir -p "${BUILDER_CLIENT_DIR}"
  chmod 700 "${BUILDER_CLIENT_DIR}"
  (umask 077 && cp "${certs}/ca.pem" "${BUILDER_CLIENT_DIR}/ca.pem" && cp "${certs}/client.pem" "${BUILDER_CLIENT_DIR}/cert.pem" \
    && cp "${certs}/client-key.pem" "${BUILDER_CLIENT_DIR}/key.pem" && printf 'tcp://%s:%s\n' "${BUILDER_HOST}" "${port}" > "${BUILDER_CLIENT_DIR}/addr")
  echo "==> Builder at $(<"${BUILDER_CLIENT_DIR}/addr"), its client credentials in ${BUILDER_CLIENT_DIR}"
fi
