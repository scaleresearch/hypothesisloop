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
  # Starts a real bare-metal agent on this host and launches containers directly on it — the same
  # host running the control plane and the k3s server. Sharing it with four other scenarios makes
  # its own timings a function of their load rather than of the code under test.
  bare-node-agent
  acceptable-accelerator-types
  resource-disbalance-evict
  concurrent-admission-race
  node-and-daemonset-faults
  connectivity-loss
  burst-fair-round-robin
  # Saturates an entire accelerator type (all 8 A100s) to make a guaranteed job preempt a burst
  # one. Any other scenario holding a single A100 -- including one the scheduler legitimately
  # placed there as an acceptable alternate -- means the setup never reaches saturation and the
  # scenario fails having tested nothing. Also in SLOW_TESTS, so it stays opt-in.
  preemption-requeue
)

# Capacity/preemption scenarios deliberately hold real resources across multiple scheduler ticks.
# quota-exhaustion is here for the same reason: it has to let a job actually overrun its estimate,
# since observed consumption is the only thing that can exhaust a budget. never-reported-metrics
# has to outlive two full silence windows, which the deployed floor sets in minutes.
# distributed-jobs joins them because it runs a real two-rank job and then a full
# delete-and-recreate drift repair, which is several scheduler ticks of waiting on whatever else
# the suite is doing.
SLOW_TESTS=(
  never-reported-metrics
  capacity-safety
  mixed-admission
  preemption-requeue
  running-cost-live
  quota-exhaustion
  distributed-jobs
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
preflight_workload_image_present || exit 2

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
    # The two lists compose: exclusivity says how a scenario must run, SLOW_TESTS says whether
    # this invocation runs it at all. A scenario in both stays opt-in and still runs alone.
    if ! is_slow "$name" || [[ -n "${RUN_SLOW:-}" ]]; then
      exclusive_set+=("$name")
    fi
  elif is_slow "$name"; then
    [[ -n "${RUN_SLOW:-}" ]] && slow_set+=("$name")
  else
    fast_set+=("$name")
  fi
done

# Two scenarios leave the cluster in a state the next one has to recover from, so they run at the
# end and in this order: node-and-daemonset-faults kills and readmits nodes, whose pods are still
# rescheduling after the idle barrier reads the cluster as quiet; connectivity-loss disconnects
# the cluster-agent outright. Anything that needs a settled cluster -- resource-disbalance-evict
# reads a once-per-cluster-per-tick eviction verdict -- must not sit behind them.
LAST_EXCLUSIVE=(node-and-daemonset-faults connectivity-loss)
reordered=()
for name in ${exclusive_set[@]+"${exclusive_set[@]}"}; do
  skip=0
  for late in "${LAST_EXCLUSIVE[@]}"; do [[ "$name" == "$late" ]] && skip=1; done
  [[ "$skip" -eq 1 ]] || reordered+=("$name")
done
for late in "${LAST_EXCLUSIVE[@]}"; do
  for name in ${exclusive_set[@]+"${exclusive_set[@]}"}; do
    [[ "$name" == "$late" ]] && reordered+=("$late")
  done
done
exclusive_set=(${reordered[@]+"${reordered[@]}"})

# One deterministic ceiling for every scenario. A scenario that needs a larger special case is
# too slow or is hiding an unreliable assertion and must be fixed at its source.
SCENARIO_TIMEOUT_SECONDS="${SCENARIO_TIMEOUT_SECONDS:-240}"
# What the SLOW classification means. Those scenarios hold real resources across multiple scheduler
# ticks by design, so they are slower than the ceiling above by construction rather than by
# accident — and until now the classification chose which group they ran in without giving them any
# more time to run in, so a busy cluster killed them mid-assertion.
SLOW_SCENARIO_TIMEOUT_SECONDS="${SLOW_SCENARIO_TIMEOUT_SECONDS:-600}"
# Exported so a scenario can budget against the same ceiling it will be killed at, instead of
# discovering it as a SIGTERM mid-assertion (see scenario_seconds_left in lib/common.sh).
export SCENARIO_TIMEOUT_SECONDS

# A scenario whose subject *is* a platform timing window can only be as fast as the window it
# exercises. never-reported-metrics has to keep a job alive past two full silence windows, and the
# deployed floor sets those in minutes — shortening the job would not make the test faster, it
# would make it stop testing anything. The SLOW group gets its own ceiling for the same reason in
# miniature; everything else stays on the shared one.
scenario_timeout() {
  case "$1" in
    never-reported-metrics) echo 1500 ;;
    # Three sequential phases, each of which waits out a real platform window: heartbeat
    # freshness, capacity staleness, then reconvergence after reconnect. Shortening any of them
    # would not make the scenario faster, it would stop it testing the window it exists for.
    connectivity-loss) echo 480 ;;
    *) if is_slow "$1"; then echo "$SLOW_SCENARIO_TIMEOUT_SECONDS"; else echo "$SCENARIO_TIMEOUT_SECONDS"; fi ;;
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

START=$(date +%s)
run_parallel_group() {
  local label="$1"; shift
  local names=(${@+"$@"}) pids=() name pid
  [[ "${#names[@]}" -gt 0 ]] || return 0
  echo "==> Running ${#names[@]} ${label} scenario(s) concurrently: ${names[*]}"
  for name in "${names[@]}"; do run_one "$name" & pids+=("$!"); done
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

# A scenario is only cluster-exclusive if the cluster is actually idle when it starts. The groups
# above finish when their scripts exit, which is before the jobs they left behind have released
# their accelerators — so an exclusive scenario that needs a whole flavor free (saturation
# preconditions) was racing the previous group's teardown, and failed on inventory it should have
# had. Wait for the cluster to report nothing busy before handing it over.
wait_cluster_idle() {
  local api="${API_URL:-http://localhost:8081}" deadline=$(( $(date +%s) + 120 )) busy
  while [[ "$(date +%s)" -lt "$deadline" ]]; do
    busy=$(curl -sf -m 10 "${api}/internal/clusters" 2>/dev/null \
      | grep -o '"accelerator_busy":[0-9]\+' | awk -F: '{t += $2} END {print t + 0}')
    [[ "${busy:-1}" == "0" ]] && return 0
    sleep 5
  done
  echo "  [warn] cluster still reports ${busy:-?} accelerator(s) busy; continuing anyway" >&2
}

if [[ "${#exclusive_set[@]}" -gt 0 ]]; then
  echo "==> Running ${#exclusive_set[@]} cluster-exclusive scenario(s) sequentially: ${exclusive_set[*]}"
  for name in ${exclusive_set[@]+"${exclusive_set[@]}"}; do
    wait_cluster_idle
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
