#!/usr/bin/env bash
# Burst-tier preemption: a guaranteed-tier job saturating a accelerator type preempts a running
# burst-tier job, which requeues (QUEUED) and is re-admitted later — unlike terminal
# eviction (see eviction-terminal.sh). API-only, parallel-safe as long as ACCELERATOR_TYPE below is
# unique to this scenario (avoid clashing with another scenario's contention target).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
ACCELERATOR_COUNT_PER_BURST=4
BURST_JOBS_N=2
AGENTS=("agent-preempt-a-${RUN_ID}" "agent-preempt-b-${RUN_ID}" "agent-preempt-c-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Budget deliberately generous: a stage boundary trips once budget_accelerator_hours consumed
# reaches the first stage's share of the ladder (see controller/stages.go), unrelated to what
# this scenario tests. A100 (high acch_rate) with accelerator_count=4 burst jobs could, under
# scheduling delay, cross that boundary and cut an agent mid-scenario — give it enough headroom
# that can't happen.
PE_ID=$(create_platform_experiment "preemption-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  ==> filling all ${ACCELERATOR_TYPE} capacity with ${BURST_JOBS_N} burst jobs of ${ACCELERATOR_COUNT_PER_BURST} accelerators each..."
declare -a BURST_JOBS
for i in $(seq 1 "$BURST_JOBS_N"); do
  BJ=$(submit_job "$PE_ID" "${AGENTS[$((i - 1))]}" "burst" "0.017" "$ACCELERATOR_TYPE" "$ACCELERATOR_COUNT_PER_BURST")
  BURST_JOBS+=("$BJ")
done

burst_jobs_admitted() {
  local n=0 bj
  for bj in "${BURST_JOBS[@]}"; do
    s=$(get_status "$bj")
    [[ "$s" == "RUNNING" || "$s" == "ADMITTED" ]] && n=$((n + 1))
  done
  [[ "$n" -ge "$BURST_JOBS_N" ]]
}
if ! wait_until "burst jobs admitted onto $ACCELERATOR_TYPE" "$ADMISSION_BUDGET_SECONDS" 1 burst_jobs_admitted; then
  fail "burst jobs did not reach RUNNING/ADMITTED; preemption setup failed"
  close_platform_experiment "$PE_ID"
  finish
fi

JOB4=$(submit_job "$PE_ID" "${AGENTS[2]}" "guaranteed" "0.017" "$ACCELERATOR_TYPE")
echo "  submitted guaranteed job pinned to ${ACCELERATOR_TYPE} (should preempt a burst victim): $JOB4"

PREEMPTED=""
for i in $(seq 1 30); do
  for BJ in "${BURST_JOBS[@]}"; do
    [[ "$(get_status "$BJ")" == "QUEUED" ]] && { PREEMPTED="$BJ"; break 2; }
  done
  sleep 1
done

if [[ -n "$PREEMPTED" ]]; then
  pass "burst job $PREEMPTED was preempted back to QUEUED by guaranteed job $JOB4"
  READMISSION_BUDGET=$((ADMISSION_BUDGET_SECONDS + $(completion_wait_tries 0.017)))
  VFINAL=$(wait_for_status "$PREEMPTED" "RUNNING,COMPLETED,FAILED,EVICTED" "$READMISSION_BUDGET" || true)
  if [[ "$VFINAL" == "RUNNING" || "$VFINAL" == "COMPLETED" ]]; then
    pass "$PREEMPTED was re-admitted and ran again after preemption (final=$VFINAL)"
  else
    fail "$PREEMPTED never came back after preemption (final=$VFINAL)"
    # Diagnostics: which terminal path claimed the victim, and what the cluster looked like.
    echo "  -- victim state --"
    curl -s "$SCHED_URL/experiments/$PREEMPTED" | py "
import json,sys
e=json.load(sys.stdin)
for k in ('status','eviction_reason','not_admitted_reason','capacity_tier',
          'estimated_duration_hours','estimated_cost_acch','accelerator_type',
          'accelerator_count','created_at','updated_at'):
    print(f'     {k}={e.get(k)!r}')
"
    echo "  -- all ${ACCELERATOR_TYPE} holders right now --"
    curl -s "$SCHED_URL/experiments" | py "
import json,sys
rows=json.load(sys.stdin)
t='$ACCELERATOR_TYPE'
for e in rows:
    if e.get('accelerator_type')==t and e['status'] in ('RUNNING','ADMITTED','QUEUED'):
        print(f\"     {e['id']} {e['agent_id']} {e['status']} n={e['accelerator_count']} tier={e['capacity_tier']} nar={e.get('not_admitted_reason')!r}\")
"
    echo "  -- node allocatable/used --"
    kubectl get nodes -l 'nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe' \
      -o jsonpath='{range .items[*]}     {.metadata.name} allocatable={.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' 2>/dev/null || true
    kubectl -n "$JOB_NS" get pods -o wide 2>/dev/null | head -20
  fi
else
  fail "no preemption observed even with ${ACCELERATOR_TYPE} capacity nominally saturated (${BURST_JOBS_N}x${ACCELERATOR_COUNT_PER_BURST} accelerators) — investigate admission accounting"
fi

close_platform_experiment "$PE_ID"
for j in "${BURST_JOBS[@]}" "$JOB4"; do
  s=$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,REJECTED" 30 || true)
  [[ "$s" == "COMPLETED" || "$s" == "FAILED" || "$s" == "EVICTED" || "$s" == "REJECTED" ]] \
    || fail "Part 1 cleanup did not make $j terminal (status=$s)"
  wait_until "Part 1 workload $j is removed" 30 1 job_resource_absent "$j" \
    || fail "Part 1 workload $j remained in Kubernetes"
done

# --- Part 2: true-concurrency preemption race -------------------------------------------
# Part 1 staggers submissions (burst first, wait for RUNNING, then the guaranteed preemptor)
# so ordering is deterministic. This leaves a real interleaving untested: burst saturation
# and the guaranteed preemptor arriving in the same/adjacent scheduler ticks, where a
# preemption decision and a fresh admission decision could race. Reuses A100 (Part 1 has
# already wound down its jobs, so holds none of that capacity) since a parallel `tests/run.sh`
# run only isolates capacity per scenario FILE, not per section within one.
echo "  ==> Part 2: firing burst saturation + guaranteed preemptor at the same instant..."
AGENTS2=("agent-race2-a-${RUN_ID}" "agent-race2-b-${RUN_ID}" "agent-race2-c-${RUN_ID}")
for a in "${AGENTS2[@]}"; do register_agent "$a"; done
PE2_ID=$(create_platform_experiment "preemption-race2-${RUN_ID}" 50.0 "${#AGENTS2[@]}")
signup_and_start "$PE2_ID" "${AGENTS2[@]}"

OUT_DIR2="$TMPDIR_T/race2"
mkdir -p "$OUT_DIR2"
PIDS2=()
# 2 burst jobs (4 accelerators each = 8, full node) + 1 guaranteed job (4 accelerators), all fired at once —
# unlike Part 1, nothing here waits for the burst jobs to land first.
(submit_job "$PE2_ID" "${AGENTS2[0]}" "burst" "0.03" "$ACCELERATOR_TYPE" "$ACCELERATOR_COUNT_PER_BURST" \
  > "$OUT_DIR2/burst_a.id" 2> "$OUT_DIR2/burst_a.err") & PIDS2+=("$!")
(submit_job "$PE2_ID" "${AGENTS2[1]}" "burst" "0.03" "$ACCELERATOR_TYPE" "$ACCELERATOR_COUNT_PER_BURST" \
  > "$OUT_DIR2/burst_b.id" 2> "$OUT_DIR2/burst_b.err") & PIDS2+=("$!")
(submit_job "$PE2_ID" "${AGENTS2[2]}" "guaranteed" "0.03" "$ACCELERATOR_TYPE" \
  > "$OUT_DIR2/guaranteed.id" 2> "$OUT_DIR2/guaranteed.err") & PIDS2+=("$!")
RACE2_SUBMIT_FAILED=0
for pid in "${PIDS2[@]}"; do wait "$pid" || RACE2_SUBMIT_FAILED=$((RACE2_SUBMIT_FAILED + 1)); done
[[ "$RACE2_SUBMIT_FAILED" -eq 0 ]] \
  && pass "Part 2: all 3 concurrently-fired submissions were accepted at the HTTP layer" \
  || fail "Part 2: $RACE2_SUBMIT_FAILED of 3 concurrent submissions failed outright — see $OUT_DIR2/*.err"

BURST2_A="$(cat "$OUT_DIR2/burst_a.id" 2>/dev/null || true)"
BURST2_B="$(cat "$OUT_DIR2/burst_b.id" 2>/dev/null || true)"
GUARANTEED2="$(cat "$OUT_DIR2/guaranteed.id" 2>/dev/null || true)"

# Safety invariant sampled throughout the race window: guaranteed + running-burst accelerator
# count on this node must never exceed physical capacity, regardless of tick interleaving.
OVER_CAPACITY_SEEN=0
running_accelerator_total() {
  local total=0 j s gc
  for j in "$BURST2_A" "$BURST2_B" "$GUARANTEED2"; do
    [[ -z "$j" ]] && continue
    s=$(get_status "$j")
    if [[ "$s" == "RUNNING" || "$s" == "ADMITTED" ]]; then
      gc=$(get_field "$j" accelerator_count)
      total=$((total + ${gc:-0}))
    fi
  done
  echo "$total"
}
for _ in $(seq 1 45); do
  t=$(running_accelerator_total)
  [[ "$t" -gt 8 ]] && { OVER_CAPACITY_SEEN=1; echo "  [FAIL-SAMPLE] observed ${t} accelerators concurrently RUNNING/ADMITTED on an 8-accelerator node"; }
  # Exit early once the guaranteed job has settled — no need to keep sampling once the race
  # this loop exists to catch is over.
  gs=$(get_status "$GUARANTEED2")
  [[ "$gs" == "RUNNING" || "$gs" == "COMPLETED" || "$gs" == "FAILED" || "$gs" == "REJECTED" ]] && break
  sleep 1
done
[[ "$OVER_CAPACITY_SEEN" -eq 0 ]] \
  && pass "Part 2: never observed more than 8 accelerators concurrently RUNNING/ADMITTED under the concurrent-arrival race" \
  || fail "Part 2: over-capacity admission observed during concurrent burst+guaranteed arrival"

# Guaranteed-tier non-starvation: even when it arrives at the exact same instant as burst
# jobs racing for the same capacity, the guaranteed job must still win eventually (guaranteed
# outranks burst — that's the whole point of the tier), not get stuck behind burst jobs that
# happened to submit their HTTP request nanoseconds earlier.
GFINAL=$(wait_for_status "$GUARANTEED2" "RUNNING,COMPLETED" 60 || true)
[[ "$GFINAL" == "RUNNING" || "$GFINAL" == "COMPLETED" ]] \
  && pass "Part 2: guaranteed job reached $GFINAL despite arriving concurrently with burst saturation — no starvation" \
  || fail "Part 2: guaranteed job never ran (final=$GFINAL) even though it should outrank/preempt concurrently-arriving burst jobs"

close_platform_experiment "$PE2_ID"
for j in "$BURST2_A" "$BURST2_B" "$GUARANTEED2"; do
  [[ -z "$j" ]] && continue
  s=$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,REJECTED" 30 || true)
  [[ "$s" == "COMPLETED" || "$s" == "FAILED" || "$s" == "EVICTED" || "$s" == "REJECTED" ]] \
    || fail "Part 2 cleanup did not make $j terminal (status=$s)"
  wait_until "Part 2 workload $j is removed" 30 1 job_resource_absent "$j" \
    || fail "Part 2 workload $j remained in Kubernetes"
done
finish
