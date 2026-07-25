#!/usr/bin/env bash
# Powers off the dev/test node containers without destroying them: each is cordoned, drained, its
# container stopped and its Node object removed, so the cluster stops advertising their capacity.
# dev-nodes-up.sh brings them back. Use dev-nodes-down.sh to remove the containers as well.
#
# These nodes exist only to give scheduling scenarios more than one host to place work on — they
# advertise accelerator product labels but have no accelerator behind them. Leaving them running
# when they are not under test means the cluster advertises capacity that cannot execute
# anything.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"

CONTEXT_NAME="k3s-local"

found=0
for name in $(podman ps -a --filter name=fake- --format '{{.Names}}' 2>/dev/null); do
  found=1
  echo "==> Powering off node ${name}..."
  lib_stop_node "${CONTEXT_NAME}" "${name}"
done

if (( found == 0 )); then
  echo "==> No fake nodes to power off."
  exit 0
fi

echo "==> Nodes powered off. Bring them back with dev-nodes-up.sh."
