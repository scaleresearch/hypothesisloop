#!/usr/bin/env bash
# True concurrency (not "back-to-back sequential") race: N guaranteed jobs are fired at the
# scheduler at the same instant, all racing for one GPU type's capacity, where the sum of
# their requests exceeds what the node actually has. Unlike capacity-safety.sh (which submits
# sequentially and checks quota debit isn't doubled), this scenario exercises the actual
# concurrent-write path — multiple submitJob calls landing inside the same or adjacent
# scheduler ticks — which is exactly where a reservation-write race would show up as
# over-admission. API-only, parallel-safe (H200 is otherwise unused by other scenarios).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

GPU_TYPE="H200"
GPU_COUNT_PER_JOB=4
NODE_GPU_CAPACITY=8
N_JOBS=3   # 3*4=12 requested against 8 available: exactly 2 can fit, 1 must lose the race.
EXPECT_ADMITTED=2

AGENTS=()
for i in $(seq 1 "$N_JOBS"); do
  a="agent-race-${i}-${RUN_ID}"
  AGENTS+=("$a")
  register_agent "$a"
done
PE_ID=$(create_platform_experiment "concurrent-race-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  ==> firing ${N_JOBS} guaranteed jobs (${GPU_COUNT_PER_JOB}x${GPU_TYPE} each = $((N_JOBS * GPU_COUNT_PER_JOB)) requested, ${NODE_GPU_CAPACITY} available) at the same instant..."
OUT_DIR="$TMPDIR_T/race"
mkdir -p "$OUT_DIR"
PIDS=()
for i in $(seq 1 "$N_JOBS"); do
  (
    submit_job "$PE_ID" "${AGENTS[$((i - 1))]}" "guaranteed" "0.05" "$GPU_TYPE" "$GPU_COUNT_PER_JOB" \
      > "$OUT_DIR/job_$i.id" 2> "$OUT_DIR/job_$i.err"
  ) &
  PIDS+=("$!")
done
FAILED_SUBMITS=0
for pid in "${PIDS[@]}"; do
  wait "$pid" || FAILED_SUBMITS=$((FAILED_SUBMITS + 1))
done
[[ "$FAILED_SUBMITS" -eq 0 ]] \
  && pass "all $N_JOBS concurrent submissions were accepted at the HTTP layer" \
  || fail "$FAILED_SUBMITS of $N_JOBS concurrent submissions failed outright — see $OUT_DIR/*.err"

JOBS=()
for i in $(seq 1 "$N_JOBS"); do
  jid="$(cat "$OUT_DIR/job_$i.id" 2>/dev/null || true)"
  [[ -n "$jid" ]] && JOBS+=("$jid")
done

# Give the scheduler enough ticks to make its admission decisions. QUEUED is deliberately
# NOT treated as settled here — every job starts QUEUED, so counting it as "done" would let
# this exit on the very first poll, before the scheduler has actually had a chance to admit
# anything. Only RUNNING/ADMITTED (won the race) or REJECTED (a real terminal rejection, not
# from us closing the PE early) count; a job still QUEUED when the budget runs out is the
# genuine "lost the race, still waiting" outcome this scenario checks for.
settled() {
  local n=0 j s
  for j in "${JOBS[@]}"; do
    s=$(get_status "$j")
    [[ "$s" == "RUNNING" || "$s" == "ADMITTED" || "$s" == "REJECTED" ]] && n=$((n + 1))
  done
  [[ "$n" -ge "${#JOBS[@]}" ]]
}
wait_until "all raced jobs reach a settled admission decision" 45 1 settled || true

ADMITTED_COUNT=0
QUEUED_COUNT=0
declare -a ADMITTED_JOBS=()
for j in "${JOBS[@]}"; do
  s=$(get_status "$j")
  echo "  $j -> $s"
  if [[ "$s" == "RUNNING" || "$s" == "ADMITTED" ]]; then
    ADMITTED_COUNT=$((ADMITTED_COUNT + 1))
    ADMITTED_JOBS+=("$j")
  elif [[ "$s" == "QUEUED" ]]; then
    QUEUED_COUNT=$((QUEUED_COUNT + 1))
  fi
done

# The core race-safety invariant: however the scheduler interleaves concurrent submitJob
# calls, it must never admit more total GPU count than the node physically has. This is the
# assertion that would catch the reservation-write-race class of bug (fixed in loop_preempt.go
# per findings.md, but only ever exercised there by sequential back-to-back submission).
TOTAL_ADMITTED_GPUS=$((ADMITTED_COUNT * GPU_COUNT_PER_JOB))
[[ "$TOTAL_ADMITTED_GPUS" -le "$NODE_GPU_CAPACITY" ]] \
  && pass "admitted total (${ADMITTED_COUNT} jobs = ${TOTAL_ADMITTED_GPUS} GPUs) does not exceed physical capacity (${NODE_GPU_CAPACITY} GPUs) under true concurrency" \
  || fail "OVER-ADMISSION under concurrent submission: ${ADMITTED_COUNT} jobs (${TOTAL_ADMITTED_GPUS} GPUs) admitted against only ${NODE_GPU_CAPACITY} GPUs available — reservation race"

[[ "$ADMITTED_COUNT" -eq "$EXPECT_ADMITTED" ]] \
  && pass "exactly $EXPECT_ADMITTED of $N_JOBS raced jobs admitted, as capacity allows" \
  || echo "  [WARN] expected exactly $EXPECT_ADMITTED admitted, got $ADMITTED_COUNT (not necessarily a bug — cluster capacity/timing may differ; the hard invariant above is what matters)"

[[ "$QUEUED_COUNT" -ge 1 ]] \
  && pass "at least one over-subscribed job correctly lost the race and stayed QUEUED/non-admitted" \
  || fail "no job was left QUEUED even though requests (${N_JOBS}x${GPU_COUNT_PER_JOB}=$((N_JOBS * GPU_COUNT_PER_JOB))) exceed capacity (${NODE_GPU_CAPACITY}) — every job appears admitted, which is impossible if capacity accounting is correct"

for j in "${ADMITTED_JOBS[@]:-}"; do
  [[ -z "$j" ]] && continue
  [[ "$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,QUEUED" 90 || true)" == "COMPLETED" ]] && file_finding "$j"
done

close_platform_experiment "$PE_ID"
finish
