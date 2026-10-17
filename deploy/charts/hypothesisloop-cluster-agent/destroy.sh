#!/usr/bin/env bash
# Detaches the cluster and uninstalls the HypothesisLoop runtime that install.sh installed. A hook
# first removes the cluster from the control plane, where its jobs go back to the queue, and stops
# what it runs here; then the release goes, with the hypothesisloop-jobs namespace and the
# PriorityClasses. The name must be registered again before the runtime is installed again.
#
# Settings (env vars, all optional):
#   KUBECONFIG_PATH   kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT      kubectl context to target (default: current-context)
set -euo pipefail

KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

export KUBECONFIG="${KUBECONFIG_PATH}"
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

echo "==> Detaching the cluster and removing the runtime (context: ${KUBE_CONTEXT})"
echo "    Removes: release hypothesisloop-cluster-agent, namespace hypothesisloop-jobs and every job in it,"
echo "             PriorityClasses hypothesisloop-jobs-guaranteed and hypothesisloop-jobs-burst"
helm uninstall hypothesisloop-cluster-agent --kube-context "${KUBE_CONTEXT}" \
  --namespace hypothesisloop --ignore-not-found
echo "==> Done. The namespace hypothesisloop stays: kubectl --context ${KUBE_CONTEXT} delete namespace hypothesisloop"
