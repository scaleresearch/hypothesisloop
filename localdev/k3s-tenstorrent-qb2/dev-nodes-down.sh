#!/usr/bin/env bash
# Tears down the fake accelerator node containers created by dev-nodes-up.sh and detaches this
# Tenstorrent host's own node, returning it to control-plane-only so nothing further schedules
# onto it — e.g. after a test run, so the box is fully available for real training work again.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
export NODE_PODMAN="sudo podman"

CONTEXT="${TT_CONTEXT:-k3s-tt}"
NODE="${TT_NODE:-tt-quietbox}"

for name in $(sudo podman ps -a --filter name=fake- --format '{{.Names}}' 2>/dev/null); do
  echo "==> Removing node ${name}..."
  lib_destroy_node "${CONTEXT}" "${name}"
done

if [[ "${DESTROYING_CLUSTER:-0}" == 1 ]]; then
  exit 0
fi
if ! kubectl config get-contexts "${CONTEXT}" --no-headers >/dev/null 2>&1; then
  echo "ERROR: Kubernetes context ${CONTEXT} does not exist" >&2
  exit 2
fi
kubectl --context "${CONTEXT}" version >/dev/null
if kubectl --context "${CONTEXT}" get node "${NODE}" >/dev/null 2>&1; then
  lib_detach_node "${CONTEXT}" "${NODE}"
  echo "==> ${NODE} detached (control-plane only)."
fi
