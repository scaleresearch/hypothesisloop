#!/usr/bin/env bash
# Tear down the OpenResearch "cluster agent" bundle from a target Kubernetes
# cluster: the openresearch-node-agent DaemonSet and the openresearch-cluster-agent
# Deployment + its RBAC. There is no external operator to leave behind — the
# openresearch-jobs namespace and PriorityClasses cluster-agent created stay
# (harmless if the bundle is reinstalled later).
#
# Parameters (env vars, all optional):
#   CLUSTER_NAME      — label identifying this target cluster (default: local)
#   KUBECONFIG_PATH   — kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT      — kubectl context to target (default: current-context)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-local}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export KUBECONFIG="${KUBECONFIG_PATH}"

if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

kctl() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

echo "==> Removing cluster-agent bundle from cluster '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT})"

sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g" "${SCRIPT_DIR}/node-agent-daemonset.yaml" \
  | kctl delete -f - --ignore-not-found

sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g; s|__CONTROLPLANE_URL__|unused|g; s|__REGISTRY_URL__|unused|g" \
  "${SCRIPT_DIR}/cluster-agent-deployment.yaml" \
  | kctl delete -f - --ignore-not-found

echo "==> Cluster agent bundle removed from '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT})."
