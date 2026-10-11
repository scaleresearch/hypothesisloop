#!/usr/bin/env bash
# Provisions the fake nodes, runs the portable pytest e2e suite, then removes them again —
# whether the suite passes or fails — so the box is fully available for real training work.
# The pytest suite itself stays
# environment-agnostic; this wrapper is the one place that knows "on a Tenstorrent host,
# provisioning means dev-nodes-up.sh."
#
# The host is the OWNED_COMPUTE_CLUSTERS entry of .env whose context is TT_CONTEXT: TT_CLUSTER is
# its name and TT_NODE its node (lib_host_node). With no such entry the cluster is named after the
# node.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
TT_CONTEXT="${TT_CONTEXT:-k3s-tt}"
cluster="$(lib_owned_cluster_at "${TT_CONTEXT}")"
TT_NODE="$(lib_host_node "${TT_CONTEXT}")"
TT_CLUSTER="${cluster%% *}"
TT_CLUSTER="${TT_CLUSTER:-${TT_NODE}}"
export TT_CONTEXT TT_NODE TT_CLUSTER

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
