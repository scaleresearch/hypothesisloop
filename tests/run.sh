#!/usr/bin/env bash
# Portable e2e suite: runs every selected scenario once. Hardware tests need real Tenstorrent
# silicon and are excluded by default.
#
# Usage:
#   bash tests/run.sh                      # fast group only (default)
#   bash tests/run.sh node-death eviction  # only scenarios whose filename matches these
#   RUN_SLOW=1 bash tests/run.sh           # full suite: fast + slow group
#   RUN_HARDWARE_TESTS=1 bash tests/run.sh # also include HARDWARE_ONLY scenarios
#
# Scenarios default to an NVIDIA dev cluster. To run them against other silicon, name a type the
# cluster actually advertises and its acch_rate from controlplane/settings/hypothesisloop.yaml
# (see TEST_ACCELERATOR_TYPE in tests/lib/common.sh) — e.g. for a Tenstorrent host:
#   TEST_ACCELERATOR_TYPE=tenstorrent.com/chipArch=blackhole TEST_ACCH_RATE=0.5 bash tests/run.sh
# Scenarios needing inventory the host lacks (several accelerator flavors, more than one node)
# still skip or fail on their own preconditions; run them one at a time when accelerators are few,
# since the whole suite runs concurrently and contends for the same chips.
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Mutate shared cluster/node/daemonset state, or need every fake-accelerator unit of a type
# accounted for with no other scenario touching it concurrently — run sequentially, not
# concurrently. The first group is structurally slow (real node/daemonset kill+recovery, real
# disconnect/reconnect wait windows); burst-fair-round-robin only needs exclusivity because it
# pins an accelerator type's whole capacity to make admission ORDER externally observable, which
# any other concurrent user of that type (however small) would silently invalidate.
CLUSTER_EXCLUSIVE=(
  acceptable-accelerator-types
  resource-disbalance-evict
  concurrent-admission-race
  node-and-daemonset-faults
  connectivity-loss
  burst-fair-round-robin
)

# Capacity/preemption scenarios deliberately hold real resources across multiple scheduler ticks.
# quota-exhaustion is here for the same reason: it has to let a job actually overrun its estimate,
# since observed consumption is the only thing that can exhaust a budget. never-reported-metrics
# has to outlive two full silence windows, which the deployed floor sets in minutes.
SLOW_TESTS=(
  never-reported-metrics
  capacity-safety
  mixed-admission
  preemption-requeue
  running-cost-live
  quota-exhaustion
)

# Needs real accelerator hardware — excluded unless explicitly requested.
HARDWARE_ONLY=(
  tenstorrent-hardware
  nvidia-hardware
)

is_exclusive() {
  local name="$1" e
  for e in "${CLUSTER_EXCLUSIVE[@]}"; do [[ "$name" == "$e" ]] && return 0; done
  return 1
}

is_slow() {
  local name="$1" e
  for e in "${SLOW_TESTS[@]}"; do [[ "$name" == "$e" ]] && return 0; done
  return 1
}

is_hardware_only() {
  local name="$1" e
  for e in "${HARDWARE_ONLY[@]}"; do [[ "$name" == "$e" ]] && return 0; done
  return 1
}

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

# One check for the whole run: an unschedulable cluster makes every scenario fail identically and
# unhelpfully, several minutes apart. See preflight_accelerator_schedulable.
source "$DIR/lib/preflight.sh"
preflight_accelerator_schedulable || exit 2

fast_set=()
slow_set=()
exclusive_set=()
hardware_set=()
for f in "$DIR"/scenarios/*.sh; do
  name="$(basename "$f" .sh)"
  matches "$name" || continue
  if is_hardware_only "$name"; then
    [[ -n "${RUN_HARDWARE_TESTS:-}" ]] && hardware_set+=("$name")
  elif is_exclusive "$name"; then
    exclusive_set+=("$name")
  elif is_slow "$name"; then
    [[ -n "${RUN_SLOW:-}" ]] && slow_set+=("$name")
  else
    fast_set+=("$name")
  fi
done

# connectivity-loss runs last: disconnects cluster-agent, so nothing else should run alongside it.
reordered=()
has_connectivity_loss=0
for name in ${exclusive_set[@]+"${exclusive_set[@]}"}; do
  if [[ "$name" == "connectivity-loss" ]]; then has_connectivity_loss=1; continue; fi
  reordered+=("$name")
done
[[ "$has_connectivity_loss" -eq 1 ]] && reordered+=(connectivity-loss)
exclusive_set=(${reordered[@]+"${reordered[@]}"})

# One deterministic ceiling for every scenario. A scenario that needs a larger special case is
# too slow or is hiding an unreliable assertion and must be fixed at its source.
SCENARIO_TIMEOUT_SECONDS="${SCENARIO_TIMEOUT_SECONDS:-240}"
# Exported so a scenario can budget against the same ceiling it will be killed at, instead of
# discovering it as a SIGTERM mid-assertion (see scenario_seconds_left in lib/common.sh).
export SCENARIO_TIMEOUT_SECONDS

# The one exception to that ceiling: a scenario whose subject *is* a platform timing window can
# only be as fast as the window it exercises. never-reported-metrics has to keep a job alive past
# two full silence windows, and the deployed floor sets those in minutes — shortening the job
# would not make the test faster, it would make it stop testing anything. Everything else stays on
# the shared ceiling.
scenario_timeout() {
  case "$1" in
    never-reported-metrics) echo 1500 ;;
    *) echo "$SCENARIO_TIMEOUT_SECONDS" ;;
  esac
}

# GNU coreutils' timeout isn't on macOS by default (and gtimeout only if brew coreutils is
# installed) — fall back to a watchdog that kills the scenario itself after the same ceiling.
run_with_timeout() {
  local secs="$1"; shift
  if command -v timeout >/dev/null 2>&1; then timeout "$secs" "$@"; return $?; fi
  if command -v gtimeout >/dev/null 2>&1; then gtimeout "$secs" "$@"; return $?; fi
  "$@" &
  local job=$!
  ( sleep "$secs"; kill -TERM "$job" 2>/dev/null ) &
  local watchdog=$!
  local rc=0
  wait "$job" || rc=$?
  kill -TERM "$watchdog" 2>/dev/null
  wait "$watchdog" 2>/dev/null || true
  return "$rc"
}

run_one() {
  local name="$1" t0 t1 rc elapsed budget
  t0=$(date +%s)
  budget=$(scenario_timeout "$name")
  SCENARIO_TIMEOUT_SECONDS="$budget" run_with_timeout "$budget" bash "$DIR/scenarios/${name}.sh" > "$LOG_DIR/${name}.log" 2>&1
  rc=$?
  echo "$rc" > "$LOG_DIR/${name}.rc"
  t1=$(date +%s)
  elapsed=$(( t1 - t0 ))
  echo "$elapsed" > "$LOG_DIR/${name}.elapsed"
  [[ "$rc" == "0" ]] && echo "  [PASS] $name (${elapsed}s)" || echo "  [FAIL] $name (${elapsed}s, rc=$rc)"
}

# A scenario that was killed (suite timeout, ^C) may never have written its .rc/.elapsed files.
# Missing means "did not report success", which is a failure — never a silently-skipped row.
scenario_rc() { [[ -r "$LOG_DIR/$1.rc" ]] && cat "$LOG_DIR/$1.rc" || echo "1"; }
scenario_elapsed() { [[ -r "$LOG_DIR/$1.elapsed" ]] && cat "$LOG_DIR/$1.elapsed" || echo "?"; }

# How many scenarios may hold cluster capacity at once. Every scenario reserves real
# accelerators and, since admission proves a job fits a single node in every dimension, real
# per-node CPU too — so firing the whole suite at once oversubscribes the cluster and scenarios
# fail on admission timeouts that say nothing about the code. A cap trades some wall-clock for
# results that mean the same thing every run; raise it on a larger cluster.
SUITE_CONCURRENCY="${SUITE_CONCURRENCY:-5}"

START=$(date +%s)
run_parallel_group() {
  local label="$1"; shift
  local names=(${@+"$@"}) pids=() name pid inflight=0
  [[ "${#names[@]}" -gt 0 ]] || return 0
  echo "==> Running ${#names[@]} ${label} scenario(s), ${SUITE_CONCURRENCY} at a time: ${names[*]}"
  for name in "${names[@]}"; do
    run_one "$name" & pids+=("$!")
    inflight=$(( inflight + 1 ))
    if [[ "$inflight" -ge "$SUITE_CONCURRENCY" ]]; then
      wait -n 2>/dev/null || true
      inflight=$(( inflight - 1 ))
    fi
  done
  for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid"; done
}

run_parallel_group fast ${fast_set[@]+"${fast_set[@]}"}
run_parallel_group slow ${slow_set[@]+"${slow_set[@]}"}

# cluster-agent Ready check for the CLUSTER_EXCLUSIVE loop below — deliberately NOT sourcing
# tests/lib/common.sh here (it sets `set -e`, which would change this script's own top-level
# error handling); just the one kubectl query these two lines need.
cluster_agent_ready() {
  [[ "$(kubectl -n hypothesisloop get deployment/hypothesisloop-cluster-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" == "1" ]]
}

if [[ "${#exclusive_set[@]}" -gt 0 ]]; then
  echo "==> Running ${#exclusive_set[@]} cluster-exclusive scenario(s) sequentially: ${exclusive_set[*]}"
  for name in ${exclusive_set[@]+"${exclusive_set[@]}"}; do
    cluster_agent_ready || {
      echo "  [FAIL] cluster-agent is not ready before ${name}" >&2
      echo "1" > "$LOG_DIR/${name}.rc"
      echo "0" > "$LOG_DIR/${name}.elapsed"
      echo "cluster-agent is not ready before scenario start" > "$LOG_DIR/${name}.log"
      continue
    }
    run_one "$name"
  done
fi

if [[ "${#hardware_set[@]}" -gt 0 ]]; then
  echo "==> Running ${#hardware_set[@]} hardware scenario(s) sequentially: ${hardware_set[*]}"
  for name in ${hardware_set[@]+"${hardware_set[@]}"}; do run_one "$name"; done
fi
ELAPSED=$(( $(date +%s) - START ))

echo ""
echo "=========================================================="
echo "RESULTS (${ELAPSED}s)"
echo "=========================================================="
FAILED=0
for name in ${fast_set[@]+"${fast_set[@]}"} ${slow_set[@]+"${slow_set[@]}"} ${exclusive_set[@]+"${exclusive_set[@]}"} ${hardware_set[@]+"${hardware_set[@]}"}; do
  [[ -z "$name" ]] && continue
  # A scenario killed before it could write its own result files has not passed — report it as a
  # failure rather than letting the read error decide, which prints a confusing shell diagnostic
  # and leaves rc empty.
  rc=$(scenario_rc "$name")
  secs=$(scenario_elapsed "$name")
  if [[ "$rc" == "0" ]]; then
    echo "  [PASS] $name (${secs}s)"
  else
    echo "  [FAIL] $name (${secs}s)"
    FAILED=1
  fi
done

if [[ "$FAILED" == "1" ]]; then
  echo ""
  echo "==> Full output for failed scenarios:"
  for name in ${fast_set[@]+"${fast_set[@]}"} ${slow_set[@]+"${slow_set[@]}"} ${exclusive_set[@]+"${exclusive_set[@]}"} ${hardware_set[@]+"${hardware_set[@]}"}; do
    [[ -z "$name" ]] && continue
    rc=$(scenario_rc "$name")
    if [[ "$rc" != "0" ]]; then
      echo ""
      echo "---- $name ----"
      cat "$LOG_DIR/${name}.log" 2>/dev/null || echo "(no output captured: scenario did not start or was killed)"
    fi
  done
  echo ""
  echo "==> RESULT: one or more scenarios FAILED."
  exit 1
fi
echo ""
echo "==> RESULT: all scenarios passed."
