#!/usr/bin/env bash
# Provisions the fake nodes, runs the portable pytest e2e suite, then removes them again —
# whether the suite passes or fails — so the box is fully available for real training work.
# The pytest suite itself stays
# environment-agnostic; this wrapper is the one place that knows "on box-a, provisioning
# means dev-nodes-up.sh."
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
