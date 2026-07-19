#!/usr/bin/env bash
# Quick health check for the tt-operator stack installed by install.sh:
# node readiness, NFD device labels, operator components, and the
# DRA-published device inventory. Read-only — safe to run anytime.
set -euo pipefail

CONTEXT_NAME="k3s-tt"
TT_OPERATOR_NAMESPACE="${TT_OPERATOR_NAMESPACE:-tt-operator-system}"

kctl() { kubectl --context "${CONTEXT_NAME}" "$@"; }

if ! kctl get nodes &>/dev/null; then
  echo "ERROR: cluster context '${CONTEXT_NAME}' is not reachable — run localdev/k3s-tenstorrent-qb2/install.sh first."
  exit 1
fi

echo "==> Nodes:"
kctl get nodes -L feature.node.kubernetes.io/pci-1200_1e52.present,driver.tenstorrent.com/kmd-version,driver.tenstorrent.com/install-mode

echo
echo "==> tt-operator components (namespace ${TT_OPERATOR_NAMESPACE}):"
kctl -n "${TT_OPERATOR_NAMESPACE}" get deploy,ds 2>/dev/null || echo "    (none found)"

echo
echo "==> Tenstorrent devices published via Dynamic Resource Allocation:"
kctl get resourceslices -o custom-columns='NAME:.metadata.name,POOL:.spec.pool.generation,DEVICES:.spec.devices[*].name' 2>/dev/null \
  || echo "    (no ResourceSlices yet — DRA driver may still be resolving fabric topology)"

echo
echo "==> Host-side tt-smi device list:"
TT_SMI="$(command -v tt-smi || true)"
[[ -z "${TT_SMI}" && -x "${HOME}/.tenstorrent-venv/bin/tt-smi" ]] && TT_SMI="${HOME}/.tenstorrent-venv/bin/tt-smi"
if [[ -n "${TT_SMI}" ]]; then
  "${TT_SMI}" -ls
else
  echo "    tt-smi not found on PATH or in ~/.tenstorrent-venv."
fi
