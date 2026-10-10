#!/usr/bin/env bash
# Provisions the fake nodes, runs the portable pytest e2e suite, then removes them again —
# whether the suite passes or fails — so the box is fully available for real training work.
# The pytest suite itself stays
# environment-agnostic; this wrapper is the one place that knows "on a Tenstorrent host,
# provisioning means dev-nodes-up.sh."
#
# TT_NODE names the host's node (lib_tt_node) and TT_CLUSTER the cluster it is registered as,
# the node's name unless the environment or .env sets it.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
TT_NODE="$(lib_tt_node "${TT_CONTEXT:-k3s-tt}")"
if [[ -z "${TT_CLUSTER:-}" && -f "${LIB_NODE_REPO}/.env" ]]; then
  TT_CLUSTER="$(set +u; . "${LIB_NODE_REPO}/.env"; printf '%s' "${TT_CLUSTER:-}")"
fi
TT_CLUSTER="${TT_CLUSTER:-${TT_NODE}}"
export TT_NODE TT_CLUSTER

t0=$(date +%s)
bash "${DIR}/dev-nodes-up.sh"
echo "==> dev-nodes-up: $(( $(date +%s) - t0 ))s"
trap 'bash "${DIR}/dev-nodes-down.sh"' EXIT

t0=$(date +%s)
# No marker deselection: this host has real Tenstorrent silicon and owns every exclusive
# cluster resource the suite needs, so it runs everything -- including the hardware-only and
# exclusive tests the quick lane otherwise excludes.
(cd "${DIR}/../../tests" && uv run pytest e2e "$@"); rc=$?
echo "==> pytest e2e (full suite): $(( $(date +%s) - t0 ))s"
exit "$rc"
