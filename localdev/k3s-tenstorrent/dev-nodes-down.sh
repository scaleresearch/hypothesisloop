#!/usr/bin/env bash
# Tears down the fake accelerator node containers created by dev-nodes-up.sh. This host's own
# node is left as it is.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
export NODE_PODMAN="sudo podman"

CONTEXT="${TT_CONTEXT:-k3s-tt}"

for name in $(sudo podman ps -a --filter name=fake- --format '{{.Names}}' 2>/dev/null); do
  echo "==> Removing node ${name}..."
  lib_destroy_node "${CONTEXT}" "${name}"
done
