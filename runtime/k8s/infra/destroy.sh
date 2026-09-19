#!/usr/bin/env bash
# Uninstalls the HypothesisLoop runtime that install.sh installed. The hypothesisloop-jobs
# namespace and PriorityClasses cluster-agent created stay (harmless if it is reinstalled later).
#
# Parameters (env vars, all optional):
#   KUBECONFIG_PATH   kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT      kubectl context to target (default: current-context)
set -euo pipefail

KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

echo "==> Removing cluster agent (context: ${KUBE_CONTEXT})"
helm uninstall hypothesisloop-cluster-agent --kube-context "${KUBE_CONTEXT}" \
  --namespace hypothesisloop --ignore-not-found
