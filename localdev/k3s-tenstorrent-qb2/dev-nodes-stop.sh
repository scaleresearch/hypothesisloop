#!/usr/bin/env bash
# Powers off the fake accelerator nodes without destroying them: each is cordoned, drained, its
# container stopped and its Node object removed, so the cluster stops advertising their capacity.
# dev-nodes-up.sh brings them back. Use dev-nodes-down.sh to remove the containers as well.
#
# The fake nodes exist only to give scheduling scenarios more than one host to place work on —
# they advertise NVIDIA product labels but have no accelerator behind them. Leaving them running
# when they are not under test means the cluster advertises capacity that cannot execute
# anything, which is exactly the trap this script exists to avoid. This host's own real
# Tenstorrent node (tt-quietbox) is never touched.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
export NODE_PODMAN="sudo podman"

CONTEXT="${TT_CONTEXT:-k3s-tt}"

if ! kubectl config get-contexts "${CONTEXT}" --no-headers >/dev/null 2>&1; then
  echo "ERROR: Kubernetes context ${CONTEXT} does not exist" >&2
  exit 2
fi

found=0
for name in $(sudo podman ps -a --filter name=fake- --format '{{.Names}}' 2>/dev/null); do
  found=1
  echo "==> Powering off node ${name}..."
  lib_stop_node "${CONTEXT}" "${name}"
done

if (( found == 0 )); then
  echo "==> No fake nodes to power off."
  exit 0
fi

echo "==> Fake nodes powered off. Bring them back with dev-nodes-up.sh."
kubectl --context "${CONTEXT}" get nodes -L nvidia.com/gpu.product
