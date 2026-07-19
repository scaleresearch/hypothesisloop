#!/usr/bin/env bash
# Full teardown: uninstalls the tt-operator Helm release, then k3s itself.
# Does NOT touch the host's tt-kmd install (apt/DKMS-managed, outside k3s's
# control — see install.sh's comment on why tt-k8s-driver-manager is
# disabled) or the Tenstorrent hardware/firmware in any way.
set -euo pipefail

CONTEXT_NAME="k3s-tt"
TT_OPERATOR_NAMESPACE="${TT_OPERATOR_NAMESPACE:-tt-operator-system}"

if kubectl --context "${CONTEXT_NAME}" get nodes &>/dev/null; then
  echo "==> Uninstalling tt-operator Helm release..."
  helm uninstall tt-operator --kube-context "${CONTEXT_NAME}" \
    --namespace "${TT_OPERATOR_NAMESPACE}" 2>/dev/null || true
fi

echo "==> Uninstalling k3s..."
sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true

kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-cluster  "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-user     "${CONTEXT_NAME}" 2>/dev/null || true
# rename-context (in install.sh) only renames the context, not the cluster/user
# objects underneath — those are still literally named "default". Clean them
# up too so a future install.sh never finds a stale "default" object with a
# CA from this now-destroyed cluster to collide with the next one's.
kubectl config delete-cluster default 2>/dev/null || true
kubectl config delete-user    default 2>/dev/null || true

echo "==> Tenstorrent cluster destroyed. Host tt-kmd install is untouched."
