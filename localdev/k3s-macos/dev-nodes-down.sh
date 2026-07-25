#!/usr/bin/env bash
# Tears down the dev/test node containers created by dev-nodes-up.sh, leaving the control-plane
# node (tainted no-workload by install.sh) and the cluster itself intact. For a full teardown
# use destroy.sh instead.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"

CONTEXT_NAME="k3s-local"

for name in $(podman ps -a --filter name=fake- --format '{{.Names}}' 2>/dev/null); do
  echo "==> Removing node ${name}..."
  lib_destroy_node "${CONTEXT_NAME}" "${name}"
done
