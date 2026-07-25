#!/usr/bin/env bash
# Attaches this host as a worker, runs the portable e2e suite, then detaches it again — whether
# the suite passes or fails — so the box reverts to donating zero capacity to workloads and is
# fully available for real training work. tests/run.sh itself stays environment-agnostic; this
# wrapper is the one place that knows "on tt-quietbox, provisioning means dev-nodes-up.sh."
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

t0=$(date +%s)
bash "${DIR}/dev-nodes-up.sh"
echo "==> dev-nodes-up: $(( $(date +%s) - t0 ))s"
trap 'bash "${DIR}/dev-nodes-down.sh"' EXIT

t0=$(date +%s)
# RUN_HARDWARE_TESTS=1: this host has real Tenstorrent silicon, so include the
# hardware-only scenario tests/run.sh otherwise skips (macOS has no such hardware).
RUN_HARDWARE_TESTS=1 bash "${DIR}/../../tests/run.sh" "$@"; rc=$?
echo "==> tests/run.sh: $(( $(date +%s) - t0 ))s"
exit "$rc"
