#!/usr/bin/env bash
# Runs every scenario under tests/scenarios/. API-only scenarios run concurrently (they each
# get their own RUN_ID-namespaced agents/platform-experiments, so they don't collide); the
# few that mutate cluster-wide state (a real node, the cluster-agent Deployment, the
# node-agent DaemonSet) run sequentially afterward so they don't fight each other.
#
# Usage:
#   bash tests/run.sh                      # everything
#   bash tests/run.sh node-death eviction  # only scenarios whose filename matches these
#   ONLY_FAST=1 bash tests/run.sh          # skip CLUSTER_EXCLUSIVE scenarios (fast, no kubectl)
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Mutate real cluster/node/daemonset state — must not run concurrently with each other.
CLUSTER_EXCLUSIVE=(
  node-and-daemonset-faults
  connectivity-loss
)

is_exclusive() {
  local name="$1" e
  for e in "${CLUSTER_EXCLUSIVE[@]}"; do [[ "$name" == "$e" ]] && return 0; done
  return 1
}

all_scenarios=()
for f in "$DIR"/scenarios/*.sh; do
  all_scenarios+=("$(basename "$f" .sh)")
done

filters=("$@")
matches() {
  local name="$1"
  [[ "${#filters[@]}" -eq 0 ]] && return 0
  local f
  for f in "${filters[@]}"; do [[ "$name" == *"$f"* ]] && return 0; done
  return 1
}

LOG_DIR="$(mktemp -d)"
trap 'rm -rf "$LOG_DIR"' EXIT

declare -a parallel_set exclusive_set
for name in "${all_scenarios[@]}"; do
  matches "$name" || continue
  if is_exclusive "$name"; then exclusive_set+=("$name"); else parallel_set+=("$name"); fi
done
[[ -n "${ONLY_FAST:-}" ]] && exclusive_set=()

run_one() {
  local name="$1"
  bash "$DIR/scenarios/${name}.sh" > "$LOG_DIR/${name}.log" 2>&1
  echo "$?" > "$LOG_DIR/${name}.rc"
}

# Every scenario in parallel_set runs its own workload pods across the same handful of fake
# nodes, which all share one podman VM's real (not k8s-reported) CPU pool. Firing all of them
# at once routinely oversubscribes that shared pool on a modest dev machine: pods get
# throttled hard enough that admission/preemption/scheduling-tick timeouts fire for reasons
# that have nothing to do with scheduler correctness (verified: capacity-safety,
# distributed-jobs, and preemption-requeue all pass individually but flake under the full
# 13-way concurrent run). Batching trades wall-clock time for a load level the VM can
# actually sustain, without touching any scenario's own timeouts/assertions.
PARALLEL_BATCH_SIZE="${PARALLEL_BATCH_SIZE:-5}"
START=$(date +%s)
echo "==> Running ${#parallel_set[@]} scenario(s), ${PARALLEL_BATCH_SIZE} at a time: ${parallel_set[*]:-<none>}"
batch=()
for name in "${parallel_set[@]:-}"; do
  [[ -z "$name" ]] && continue
  batch+=("$name")
  if [[ "${#batch[@]}" -ge "${PARALLEL_BATCH_SIZE}" ]]; then
    pids=()
    for n in "${batch[@]}"; do run_one "$n" & pids+=("$!"); done
    for pid in "${pids[@]}"; do wait "$pid"; done
    batch=()
  fi
done
if [[ "${#batch[@]}" -gt 0 ]]; then
  pids=()
  for n in "${batch[@]}"; do run_one "$n" & pids+=("$!"); done
  for pid in "${pids[@]}"; do wait "$pid"; done
fi

# cluster-agent Ready check for the CLUSTER_EXCLUSIVE loop below — deliberately NOT sourcing
# tests/lib/common.sh here (it sets `set -e`, which would change this script's own top-level
# error handling); just the one kubectl query these two lines need.
cluster_agent_ready() {
  [[ "$(kubectl -n openresearch get deployment/openresearch-cluster-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" == "1" ]]
}

if [[ "${#exclusive_set[@]}" -gt 0 ]]; then
  echo "==> Running ${#exclusive_set[@]} cluster-exclusive scenario(s) sequentially: ${exclusive_set[*]}"
  for name in "${exclusive_set[@]}"; do
    # connectivity-loss.sh deliberately disconnects cluster-agent; its own cleanup should
    # already leave it reconnected, but don't just trust that — a scenario that starts with
    # cluster-agent still down would have its very first submission silently misattributed to
    # this scenario's own bug rather than the previous one's cleanup.
    if ! cluster_agent_ready; then
      echo "  [WARN] cluster-agent not connected before ${name} — waiting up to 30s"
      for _ in $(seq 1 30); do cluster_agent_ready && break; sleep 1; done
      cluster_agent_ready || echo "  [WARN] cluster-agent still not connected — ${name} will likely fail at its first submission"
    fi
    run_one "$name"
  done
fi
ELAPSED=$(( $(date +%s) - START ))

echo ""
echo "=========================================================="
echo "RESULTS (${ELAPSED}s)"
echo "=========================================================="
FAILED=0
for name in "${parallel_set[@]:-}" "${exclusive_set[@]:-}"; do
  [[ -z "$name" ]] && continue
  rc=$(cat "$LOG_DIR/${name}.rc" 2>/dev/null || echo 1)
  if [[ "$rc" == "0" ]]; then
    echo "  [PASS] $name"
  else
    echo "  [FAIL] $name"
    FAILED=1
  fi
done

if [[ "$FAILED" == "1" ]]; then
  echo ""
  echo "==> Full output for failed scenarios:"
  for name in "${parallel_set[@]:-}" "${exclusive_set[@]:-}"; do
    [[ -z "$name" ]] && continue
    rc=$(cat "$LOG_DIR/${name}.rc" 2>/dev/null || echo 1)
    if [[ "$rc" != "0" ]]; then
      echo ""
      echo "---- $name ----"
      cat "$LOG_DIR/${name}.log"
    fi
  done
  echo ""
  echo "==> RESULT: one or more scenarios FAILED."
  exit 1
fi
echo ""
echo "==> RESULT: all scenarios passed."
