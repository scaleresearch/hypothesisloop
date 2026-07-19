#!/usr/bin/env bash
# Shared bash test-lib: source this (then api.sh / cluster.sh as needed) from any
# scenario under tests/scenarios/. Every scenario gets its own RUN_ID so concurrent
# scenarios never collide on agent IDs, job IDs or platform-experiment names.
set -euo pipefail

QUOTA_URL="${QUOTA_URL:-http://localhost:8081}"
SCHED_URL="${SCHED_URL:-http://localhost:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://localhost:8083}"
PROM_URL="${PROM_URL:-http://localhost:4000/v1/prometheus}"
JOB_NS="${JOB_NS:-openresearch-jobs}"
CLUSTER_NS="${CLUSTER_NS:-openresearch}"

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_DIR="$(cd "${LIB_DIR}/.." && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workloads/generic/job.yaml}"

# PID makes RUN_ID unique even if two scenarios start in the same second.
RUN_ID="$(date +%s)-$$"

TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

py() { python3 -c "$@"; }

FAILED=0
pass() { echo "  [PASS] $*"; }
fail() { echo "  [FAIL] $*"; FAILED=1; }

# wait_until DESC TRIES SLEEP CHECK_CMD...  — polls CHECK_CMD (a command, not a string) until
# it exits 0 or TRIES is exhausted. Every "wait for X" in this suite reduces to this.
wait_until() {
  local desc="$1" tries="$2" sleep_s="$3"; shift 3
  for ((i = 1; i <= tries; i++)); do
    if "$@"; then return 0; fi
    sleep "$sleep_s"
  done
  echo "  [TIMEOUT] $desc (${tries}x${sleep_s}s)" >&2
  return 1
}

# Call at the end of every scenario script; exits 1 if any pass()/fail() call recorded a failure.
finish() {
  if [[ "$FAILED" == "1" ]]; then
    echo "==> RESULT: FAILED"
    exit 1
  fi
  echo "==> RESULT: PASSED"
}
