#!/usr/bin/env bash
# Provisions the dev/test nodes, runs the portable pytest e2e suite, then tears the nodes back
# down — whether the suite passes or fails. The pytest suite itself stays environment-agnostic
# (it only ever queries the live cluster's own reported capacity); this wrapper is the one place
# that knows "on macOS, provisioning means dev-nodes-up.sh."
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

t0=$(date +%s)
bash "${DIR}/dev-nodes-up.sh"
echo "==> dev-nodes-up: $(( $(date +%s) - t0 ))s"
trap 'bash "${DIR}/dev-nodes-down.sh"' EXIT

t0=$(date +%s)
(cd "${DIR}/../../tests" && uv run pytest e2e -m "not hardware" "$@"); rc=$?
echo "==> pytest e2e: $(( $(date +%s) - t0 ))s"
exit "$rc"
