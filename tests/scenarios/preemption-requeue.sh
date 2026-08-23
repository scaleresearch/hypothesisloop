#!/usr/bin/env bash
# Burst-tier preemption: a guaranteed-tier job saturating a accelerator type preempts a running
# burst-tier job, which requeues (QUEUED) and is re-admitted later — unlike terminal
# eviction (see eviction-terminal.sh). API-only, parallel-safe as long as ACCELERATOR_TYPE below is
# unique to this scenario (avoid clashing with another scenario's contention target).
#
# Five parts, all of them preemption and requeue, which is why they share this file rather than
# being duplicated into distributed-jobs.sh or a file of their own:
#   1. a single-node burst job is preempted, its burned hours are counted while it sits QUEUED,
#      and it is re-admitted onto the flavor it already ran on.
#   2. the same, with burst saturation and the guaranteed preemptor arriving at the same instant.
#   3. a distributed burst job is preempted with NO surviving rank, and requeues at full width.
#   4. a GROUPED burst job is preempted and requeues with every group intact.
#   5. a job that checkpoints when it is told termination is coming RESUMES from that step
#      instead of from zero, is billed one run's worth across the two stints, and has its
#      declared window capped by configuration.
# Parts 3-5 need kubectl, for the pod-level facts no API can report: whether a rank survived, and
# what window the pod itself declares.
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

  # A preempted job is requeued with its estimate rescaled down to the work that remains, so the
  # hours it already burned live in neither figure unless they are settled as observed usage at
  # requeue time. Left uncounted, its agent can re-admit against budget it has already spent —
  # and the gap lasts until the job finally terminates, which may be days.
  VICTIM_AGENT=$(get_field "$PREEMPTED" agent_id)
  # Burst-tier consumption lands in the burst bucket; sum both so the check does not depend
  # on which tier the victim was admitted under.
  victim_used_any() {
    curl -sf "$API_URL/platform-experiments/${PE_ID}/quotas/${VICTIM_AGENT}" \
      | py "import sys,json; d=json.load(sys.stdin); print((d.get('used_guaranteed_acch') or 0) + (d.get('used_burst_acch') or 0))"
  }
  victim_consumption_counted() { py "import sys; sys.exit(0 if $(victim_used_any) > 0 else 1)"; }
  # Settlement is written right after the requeue commits, but both the metrics write and the
  # read back through GreptimeDB are asynchronous enough to need a short window.
  if wait_until "preempted job's burned hours appear as observed usage" 30 2 victim_consumption_counted; then
    pass "the stint $PREEMPTED already ran is counted while it sits QUEUED ($(victim_used_any) AccH)"
  else
    fail "$PREEMPTED was requeued but its consumed hours are in no consumption figure — its agent can re-admit against budget it has already spent"
  fi

  # Settlement bills lifetime observed hours at the rate the row carries, so re-admitting a
  # job that has already run onto a different flavor retroactively re-prices the stint it ran.
  VICTIM_TYPE=$(get_field "$PREEMPTED" accelerator_type)
  [[ "$VICTIM_TYPE" == "$ACCELERATOR_TYPE" ]] \
    && pass "$PREEMPTED kept the flavor it ran on across the requeue" \
    || fail "$PREEMPTED was requeued onto $VICTIM_TYPE, not the $ACCELERATOR_TYPE it already ran on — its first stint would be re-priced"
  READMISSION_BUDGET=$((ADMISSION_BUDGET_SECONDS + $(completion_wait_tries 0.017)))
  VFINAL=$(wait_for_status "$PREEMPTED" "RUNNING,COMPLETED,FAILED,EVICTED" "$READMISSION_BUDGET" || true)
  if [[ "$VFINAL" == "RUNNING" || "$VFINAL" == "COMPLETED" ]]; then
    pass "$PREEMPTED was re-admitted and ran again after preemption (final=$VFINAL)"
  else
    fail "$PREEMPTED never came back after preemption (final=$VFINAL)"
    # Diagnostics: which terminal path claimed the victim, and what the cluster looked like.
    echo "  -- victim state --"
    curl -s "$API_URL/experiments/$PREEMPTED" | py "
import json,sys
e=json.load(sys.stdin)
for k in ('status','eviction_reason','not_admitted_reason','capacity_tier',
          'estimated_duration_hours','estimated_cost_acch','accelerator_type',
          'accelerator_count','created_at','updated_at'):
    print(f'     {k}={e.get(k)!r}')
"
    echo "  -- all ${ACCELERATOR_TYPE} holders right now --"
    curl -s "$API_URL/experiments" | py "
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

# --- Parts 3-5: distributed, grouped and resumable preemption ----------------------------
# All three are preemption and requeue, so they belong in this file rather than duplicated into
# distributed-jobs.sh or a file of their own — this scenario already owns the ground, and it is
# already the one allowed to saturate A100 (see CLUSTER_EXCLUSIVE in tests/run.sh).
#
# The A100 arithmetic every part below is built on: ONE A100 host advertising 8 accelerators.
# Two consequences, and both are load-bearing:
#   * a burst gang of G accelerators forces preemption only if the guaranteed job asks for more
#     than the 8-G that are free. There is no G for which the guaranteed job both fails to fit
#     alongside the gang AND fits alongside it after the requeue, so in every part the requeued
#     gang waits for its preemptor to finish. That is inherent to preemption, not a shortcut:
#     Part 1 above already waits the same way.
#   * spread_across_hosts defaults TRUE for any multi-node job, and one host cannot satisfy a
#     hard spread across two. Every multi-node A100 job below therefore sets it false — the same
#     reason distributed-jobs.sh Part 3 does, and the same non-weakening: hard spreading is
#     asserted there, and what these parts need is ranks that must find each other.
A100_ACCH_RATE=0.375
AGENTS3=("agent-preempt-gang-${RUN_ID}" "agent-preempt-gang-g-${RUN_ID}"
         "agent-preempt-groups-${RUN_ID}" "agent-preempt-groups-g-${RUN_ID}"
         "agent-preempt-ckpt-${RUN_ID}" "agent-preempt-ckpt-g-${RUN_ID}"
         "agent-preempt-ckpt-cap-${RUN_ID}")
for a in "${AGENTS3[@]}"; do register_agent "$a"; done
# Every metric these parts read has to be declared, and each is a fact only the job can report:
# the reduced value that proves a gang re-formed at full width, and the step a resumed job began
# at. val_accuracy stays the ranking metric, as in every other scenario.
PREEMPT_METRICS='[{"key": "val_accuracy", "direction": "maximize"},
                  {"key": "world_reduced_sum", "direction": "maximize"},
                  {"key": "resume_step", "direction": "maximize"},
                  {"key": "train_step", "direction": "maximize"},
                  {"key": "checkpoint_step", "direction": "maximize"},
                  {"key": "checkpoint_write_status", "direction": "maximize"}]'
# Budget: the same 50.0 and for the same reason as Part 1 above — what is debited here is tiny
# beside it, and the headroom exists so the elimination ladder's first stage boundary (40% of
# budget, see controller/stages.go) can never trip mid-scenario and cut an agent whose job this
# part is still waiting on. What the seven jobs below actually reserve, at A100's acch_rate of
# ${A100_ACCH_RATE}: Part 3's 6-accelerator gang 0.045 + its 4-accelerator preemptor 0.030,
# Part 4's 4-accelerator grouped gang 0.030 + its 6-accelerator preemptor 0.045, Part 5's
# 1-accelerator resumable job 0.019 + its 8-accelerator preemptor 0.060 + the cap probe 0.008
# — 0.237 AccH in all, under 0.5% of the budget.
PE3_ID=$(create_platform_experiment "preemption-distributed-${RUN_ID}" 50.0 "${#AGENTS3[@]}" 10 0 "$PREEMPT_METRICS")
signup_and_start "$PE3_ID" "${AGENTS3[@]}"

# A predicate, not an inline $(...): wait_until re-invokes its command every iteration, so a
# command substitution written at the call site would be evaluated once and polled forever
# against its first result.
job_is_queued() { [[ "$(get_status "$1")" == "QUEUED" ]]; }
no_rank_survives() { [[ "$(job_pod_count "$1")" -eq 0 ]]; }

echo ""
echo "=========================================================="
echo "Part 3: a preempted gang leaves no surviving rank, and comes back at full width"
echo "=========================================================="
# G5 says preemption applies to the WHOLE job and never to part of it, and G6 says the requeue
# restores the full N-node footprint. Neither is a statement about a status field: a gang whose
# ranks 0 and 1 were deleted while rank 2 ran on still reads QUEUED, and a requeue that came back
# two ranks wide still reads RUNNING. So G5 is asserted on pods and G6 on the reduced value.
GANG_NODES=3
GANG_ACC_PER_NODE=2
GANG_ACCELERATORS=$(( GANG_NODES * GANG_ACC_PER_NODE ))   # 6 of the node's 8
# 4 > the 2 that are free with the gang running, so this job cannot be admitted without taking
# the gang's capacity. That is the whole precondition for preemption.
GANG_PREEMPTOR_ACC=4
GANG_RUN_SECONDS=20
PREEMPTOR_RUN_SECONDS=10
gang_job() { echo '{"command": ["python", "train_distributed.py"], "max_retries": 0, "topology": {"spread_across_hosts": false}}'; }

JOB_GANG=$(submit_job_ext "$PE3_ID" "${AGENTS3[0]}" "burst" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GANG_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" \
  "$GANG_ACC_PER_NODE" "$GANG_NODES" "" "" "" "" "$(gang_job)")
echo "  submitted ${GANG_NODES}-rank burst gang holding ${GANG_ACCELERATORS} A100s: $JOB_GANG"
S=$(wait_for_status "$JOB_GANG" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "the burst gang never reached RUNNING (status=$S, not_admitted_reason=$(get_field "$JOB_GANG" not_admitted_reason)) — nothing about distributed preemption was exercised"
else
  RANKS_RUNNING=$(job_pod_count "$JOB_GANG")
  [[ "$RANKS_RUNNING" -eq "$GANG_NODES" ]] \
    && pass "all ${GANG_NODES} ranks are running before the preemptor arrives" \
    || fail "$RANKS_RUNNING rank pod(s) exist, want ${GANG_NODES} — the gang was not fully placed, so 'no rank survives' would prove nothing"

  JOB_GANG_PRE=$(submit_job_ext "$PE3_ID" "${AGENTS3[1]}" "guaranteed" "0.02" "$JOB_FILE" \
    "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${PREEMPTOR_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "$GANG_PREEMPTOR_ACC" "")
  echo "  submitted guaranteed ${GANG_PREEMPTOR_ACC}-accelerator job (only $(( 8 - GANG_ACCELERATORS )) are free): $JOB_GANG_PRE"

  if wait_until "the gang is preempted back to QUEUED" 90 1 job_is_queued "$JOB_GANG"; then
    pass "the whole gang was preempted back to QUEUED — the experiment, not a rank, is the unit (G5)"
    # G5 on pods rather than on status. The old per-index behaviour this file's siblings exist to
    # rule out would leave the ranks that were not chosen still running and still holding A100s,
    # while the experiment row already read QUEUED.
    wait_until "every rank's pod is gone" 60 2 no_rank_survives "$JOB_GANG" \
      && pass "no surviving rank: 0 pods remain, so the preemption freed the gang's whole ${GANG_ACCELERATORS}-accelerator footprint (G5)" \
      || fail "$(job_pod_count "$JOB_GANG") rank pod(s) still hold A100s for a gang the platform says is QUEUED — preemption took part of the job"
  else
    fail "the gang was never preempted (status=$(get_status "$JOB_GANG")) even though the guaranteed job needs ${GANG_PREEMPTOR_ACC} of $(( 8 - GANG_ACCELERATORS )) free accelerators"
  fi

  # The requeued gang needs all ${GANG_ACCELERATORS} back, and the preemptor is holding
  # ${GANG_PREEMPTOR_ACC} of them, so it cannot be re-admitted until that job finishes — which is
  # what this waits for rather than sleeping through.
  S=$(wait_for_completion_after_running "$JOB_GANG_PRE" "0.02" "$ADMISSION_BUDGET_SECONDS" "$(( PREEMPTOR_RUN_SECONDS + 60 ))" || true)
  [[ "$S" == "COMPLETED" ]] \
    && { pass "the guaranteed preemptor ran and completed on the capacity it took"; file_finding "$JOB_GANG_PRE" "preemption e2e: guaranteed job preempted a distributed burst gang."; } \
    || fail "the guaranteed preemptor ended $S — the capacity it took was never used"

  # A requeue and re-admission is a scheduler tick plus a fresh gang placement, not a pod restart,
  # so this budgets an admission window on top of the run.
  S=$(wait_for_completion_after_running "$JOB_GANG" "0.02" "$(( ADMISSION_BUDGET_SECONDS * 2 ))" "$(( GANG_RUN_SECONDS + 90 ))" || true)
  if [[ "$S" == "COMPLETED" ]]; then
    pass "the preempted gang was re-admitted and ran to COMPLETED"
    # G6, and the only reading of it that cannot be faked. For N ranks the all_reduce total is
    # N(N-1)/2 = 3 here; a requeue that came back two ranks wide reduces to 1 and one that came
    # back alone to 0. "It ran again" is true of all three.
    GANG_REDUCED=$(metric_max "$JOB_GANG" world_reduced_sum)
    GANG_EXPECTED=$(py "print(${GANG_NODES} * (${GANG_NODES} - 1) // 2)")
    if [[ -z "$GANG_REDUCED" ]]; then
      fail "the requeued gang never reported world_reduced_sum — it completed without proving it re-formed at all"
    elif [[ "$(py "print(abs(float('$GANG_REDUCED') - ${GANG_EXPECTED}.0) < 0.001)")" == "True" ]]; then
      pass "the requeued gang reduced to $GANG_REDUCED (0+1+2) — it came back at its full ${GANG_NODES}-rank width (G6)"
    else
      fail "the requeued gang reduced to $GANG_REDUCED, want ${GANG_EXPECTED} — it came back NARROWER than it was preempted at"
    fi
    GANG_WIDTH=$(get_field "$JOB_GANG" accelerator_count)
    [[ "$GANG_WIDTH" == "$GANG_ACCELERATORS" ]] \
      && pass "the experiment still carries its ${GANG_ACCELERATORS}-accelerator footprint across the requeue" \
      || fail "after the requeue the experiment carries $GANG_WIDTH accelerators, want ${GANG_ACCELERATORS} — the rescale shrank the footprint, not just the estimate"
    file_finding "$JOB_GANG" "preemption e2e: a preempted gang requeued and re-formed at full width."
  else
    fail "the preempted gang never came back (status=$S, eviction_reason=$(get_field "$JOB_GANG" eviction_reason), not_admitted_reason=$(get_field "$JOB_GANG" not_admitted_reason))"
  fi
fi

echo ""
echo "=========================================================="
echo "Part 4: a preempted GROUPED job comes back with every group intact"
echo "=========================================================="
# Groups are a second way to express a gang, so everything Part 3 proves has to hold when the
# gang's nodes are not identical — and the failure mode is different enough to be worth its own
# job: a requeue that restored only the first group would still be a running experiment, still
# billed, and still reading COMPLETED at the end. The reduced value is again the proof, because
# it counts nodes across BOTH groups.
GROUPS_TRAINER_ACC=2
GROUPS_WORKER_ACC=1
GROUPS_WORKER_REPLICAS=2
GROUPS_ACCELERATORS=$(( GROUPS_TRAINER_ACC + GROUPS_WORKER_ACC * GROUPS_WORKER_REPLICAS ))  # 4
GROUPS_NODES=$(( 1 + GROUPS_WORKER_REPLICAS ))                                              # 3
# 6 > the 4 free while the grouped gang runs, so again the guaranteed job can only be admitted
# by taking the burst job's capacity.
GROUPS_PREEMPTOR_ACC=6
# The per-node fields job.yaml carries are deleted (null pops the key in mk_body.py) because
# domain.JobSpec.ValidateGroups rejects a spec that states a node's shape both at the top level
# and per group — one way to say a thing, no merge rules.
groups_job() {
  cat <<JSON
{"command": ["python", "train_distributed.py"],
 "max_retries": 0,
 "cpu": null, "memory": null, "storage": null, "accelerator_count": null,
 "topology": {"spread_across_hosts": false},
 "groups": [
   {"name": "trainer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi", "accelerator_count": ${GROUPS_TRAINER_ACC}},
   {"name": "worker", "replicas": ${GROUPS_WORKER_REPLICAS}, "cpu": "100m", "memory": "64Mi", "storage": "256Mi", "accelerator_count": ${GROUPS_WORKER_ACC}}
 ]}
JSON
}

JOB_GROUPS=$(submit_job_ext "$PE3_ID" "${AGENTS3[2]}" "burst" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GANG_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "" "" \
  "" "" "" "" "$(groups_job)")
echo "  submitted burst grouped job (trainer x1 @${GROUPS_TRAINER_ACC}, worker x${GROUPS_WORKER_REPLICAS} @${GROUPS_WORKER_ACC} = ${GROUPS_ACCELERATORS} A100s): $JOB_GROUPS"
S=$(wait_for_status "$JOB_GROUPS" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "the burst grouped job never reached RUNNING (status=$S, not_admitted_reason=$(get_field "$JOB_GROUPS" not_admitted_reason)) — nothing about grouped preemption was exercised"
else
  GROUP_PODS=$(job_pod_count "$JOB_GROUPS")
  [[ "$GROUP_PODS" -eq "$GROUPS_NODES" ]] \
    && pass "all ${GROUPS_NODES} replicas across both groups are running before the preemptor arrives" \
    || fail "$GROUP_PODS pod(s) exist, want ${GROUPS_NODES} — the grouped job was not fully placed"

  JOB_GROUPS_PRE=$(submit_job_ext "$PE3_ID" "${AGENTS3[3]}" "guaranteed" "0.02" "$JOB_FILE" \
    "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${PREEMPTOR_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "$GROUPS_PREEMPTOR_ACC" "")
  echo "  submitted guaranteed ${GROUPS_PREEMPTOR_ACC}-accelerator job (only $(( 8 - GROUPS_ACCELERATORS )) are free): $JOB_GROUPS_PRE"

  if wait_until "the grouped job is preempted back to QUEUED" 90 1 job_is_queued "$JOB_GROUPS"; then
    pass "preemption took all groups together — the grouped job is one experiment, one decision"
    wait_until "every replica of every group is gone" 60 2 no_rank_survives "$JOB_GROUPS" \
      && pass "no replica of either group is left holding an A100" \
      || fail "$(job_pod_count "$JOB_GROUPS") pod(s) survive a grouped job the platform says is QUEUED — preemption took only part of the set"
  else
    fail "the grouped job was never preempted (status=$(get_status "$JOB_GROUPS"))"
  fi

  S=$(wait_for_completion_after_running "$JOB_GROUPS_PRE" "0.02" "$ADMISSION_BUDGET_SECONDS" "$(( PREEMPTOR_RUN_SECONDS + 60 ))" || true)
  [[ "$S" == "COMPLETED" ]] \
    && { pass "the guaranteed preemptor ran and completed on the grouped job's capacity"; file_finding "$JOB_GROUPS_PRE" "preemption e2e: guaranteed job preempted a grouped burst job."; } \
    || fail "the guaranteed preemptor ended $S — the capacity it took was never used"

  S=$(wait_for_completion_after_running "$JOB_GROUPS" "0.02" "$(( ADMISSION_BUDGET_SECONDS * 2 ))" "$(( GANG_RUN_SECONDS + 90 ))" || true)
  if [[ "$S" == "COMPLETED" ]]; then
    pass "the preempted grouped job was re-admitted and ran to COMPLETED"
    # ${GROUPS_NODES} nodes over two groups: the only correct reduced value is 0+1+2 = 3. A
    # requeue that restored the trainer alone reduces to 0, and one that restored the trainer plus
    # a single worker to 1 — so this number, and only this number, says every group came back.
    GROUPS_REDUCED=$(metric_max "$JOB_GROUPS" world_reduced_sum)
    GROUPS_EXPECTED=$(py "print(${GROUPS_NODES} * (${GROUPS_NODES} - 1) // 2)")
    if [[ -z "$GROUPS_REDUCED" ]]; then
      fail "the requeued grouped job never reported world_reduced_sum — it completed without proving the groups re-formed"
    elif [[ "$(py "print(abs(float('$GROUPS_REDUCED') - ${GROUPS_EXPECTED}.0) < 0.001)")" == "True" ]]; then
      pass "the requeued grouped job reduced to $GROUPS_REDUCED (0+1+2) — trainer and both workers came back and joined ONE process group"
    else
      fail "the requeued grouped job reduced to $GROUPS_REDUCED, want ${GROUPS_EXPECTED} — a group did not come back, or two replicas collided on a rank"
    fi
    GROUPS_WIDTH=$(get_field "$JOB_GROUPS" accelerator_count)
    [[ "$GROUPS_WIDTH" == "$GROUPS_ACCELERATORS" ]] \
      && pass "the experiment still carries the summed ${GROUPS_ACCELERATORS}-accelerator group footprint across the requeue" \
      || fail "after the requeue the grouped experiment carries $GROUPS_WIDTH accelerators, want ${GROUPS_ACCELERATORS}"
    file_finding "$JOB_GROUPS" "preemption e2e: a preempted grouped job requeued with every group intact."
  else
    fail "the preempted grouped job never came back (status=$S, eviction_reason=$(get_field "$JOB_GROUPS" eviction_reason), not_admitted_reason=$(get_field "$JOB_GROUPS" not_admitted_reason))"
  fi
fi

echo ""
echo "=========================================================="
echo "Part 5: the checkpoint window, and a job that actually resumes"
echo "=========================================================="
# The rescale this file's Part 1 asserts on bills a preempted job for the hours it has LEFT — the
# accounting is written assuming the job resumes where it stopped. Everything below is about
# whether execution delivers that.
#
# checkpoint_train.py is the fixture. On SIGTERM it waits CHECKPOINT_WRITE_DELAY_SECONDS and only
# then writes its step to $HYPOTHESISLOOP_DATA_URI; on start-up it reads that prefix back and
# continues from what it finds. Two properties fall out of that shape, and neither can be faked:
#   * the delay is longer than the ordinary shutdown grace, so a checkpoint exists at all ONLY if
#     the job was granted its declared window. A build that signalled the job and killed it on the
#     ordinary grace leaves no checkpoint and no resumption.
#   * the prefix is keyed on the experiment id and a requeue KEEPS that id, so resumption needs no
#     platform state whatsoever — which is precisely why nothing here submits a second job.
CKPT_STEPS_TOTAL=14
CKPT_STEP_SECONDS=5
CKPT_FULL_RUN_SECONDS=$(( CKPT_STEPS_TOTAL * CKPT_STEP_SECONDS ))   # 70
# Preempted deep into the run, not early: that is what separates "resumed" from "restarted" in
# wall clock and therefore in the settled cost. Continuing means 5 steps (~25s) remain; starting
# over means all 14 (~70s) do.
CKPT_PREEMPT_AT_STEP=9
CKPT_WRITE_DELAY=8
# Declared window. Comfortably above the ${CKPT_WRITE_DELAY}s the fixture spends before writing,
# and below the configured cap, so this job's window is its own declaration rather than the
# ceiling — the cap itself is probed separately below.
CKPT_GRACE=60
# 8 > the 7 free while the 1-accelerator job runs: the smallest job on the node still forces
# preemption if the guaranteed job wants the whole node.
CKPT_PREEMPTOR_ACC=8
ckpt_job() { echo '{"command": ["python", "checkpoint_train.py"], "max_retries": 0, "checkpoint_grace_seconds": '"$CKPT_GRACE"'}'; }
CKPT_ENV="{\"STEPS_TOTAL\": \"${CKPT_STEPS_TOTAL}\", \"STEP_SECONDS\": \"${CKPT_STEP_SECONDS}\", \"CHECKPOINT_WRITE_DELAY_SECONDS\": \"${CKPT_WRITE_DELAY}\"}"

# The pod's declared grace, which is where the window physically lives: deleting a Job hands its
# pods to the garbage collector, and the collector uses whatever the POD declares.
pod_grace_seconds() {
  kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$1" \
    -o jsonpath='{.items[0].spec.terminationGracePeriodSeconds}' 2>/dev/null
}
# RUNNING is reported off the workload's own phase, so the pod list can lag it by a moment. This
# waits for the pod to be readable rather than reading an empty string and calling it a wrong
# window — the assertion is about the number, and "not there yet" is not a number.
pod_grace_readable() { [[ -n "$(pod_grace_seconds "$1")" ]]; }
# Read from the deployed settings rather than hardcoded, for the same reason
# silence_window_seconds is: a scenario that asserted against its own copy of a configured
# number would keep passing after the configuration changed underneath it.
CKPT_CAP=$(py "
import re, sys
text = open('${SCRIPT_DIR}/../controlplane/settings/hypothesisloop.yaml').read()
m = re.search(r'^\s*max_checkpoint_grace_seconds:\s*([0-9]+)', text, re.M)
if not m:
    sys.exit('max_checkpoint_grace_seconds not found in the deployed settings')
print(m.group(1))
")

CKPT_QUOTA_BEFORE=$(curl -sf "$API_URL/platform-experiments/${PE3_ID}/quotas/${AGENTS3[4]}" \
  | py "import sys,json; d=json.load(sys.stdin); print((d.get('used_guaranteed_acch') or 0) + (d.get('used_burst_acch') or 0))")
JOB_CKPT=$(submit_job_ext "$PE3_ID" "${AGENTS3[4]}" "burst" "0.05" "$JOB_FILE" "$CKPT_ENV" \
  "$ACCELERATOR_TYPE" "1" "" "" "" "" "" "$(ckpt_job)")
echo "  submitted a 1-accelerator burst job that checkpoints when told (declared window ${CKPT_GRACE}s): $JOB_CKPT"
S=$(wait_for_status "$JOB_CKPT" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "the checkpointing job never reached RUNNING (status=$S, not_admitted_reason=$(get_field "$JOB_CKPT" not_admitted_reason)) — nothing about the window or resumption was exercised"
else
  STINT1_START=$(date +%s)

  # --- 1. the declared window reached the pod
  wait_until "the checkpointing job's pod is readable" 30 1 pod_grace_readable "$JOB_CKPT" || true
  DECLARED_GRACE=$(pod_grace_seconds "$JOB_CKPT")
  [[ "$DECLARED_GRACE" == "$CKPT_GRACE" ]] \
    && pass "the pod carries the job's declared ${CKPT_GRACE}s window, not the 5s ordinary shutdown grace" \
    || fail "the pod's terminationGracePeriodSeconds is ${DECLARED_GRACE:-<unreadable>}, want ${CKPT_GRACE} — the declared window never reached the only place that can honour it"

  # --- 2. preempt it deep into the run
  # Waited on the job's OWN reported progress rather than a sleep, because what matters is the
  # step it had reached when the signal arrived — that number is the scenario's baseline for
  # everything below, and a sleep would only be a guess at it.
  ckpt_reached_step() {
    local v
    v=$(metric_max "$1" train_step)
    [[ -n "$v" ]] || return 1
    py "import sys; sys.exit(0 if float('$v') >= $2 else 1)"
  }
  if ! wait_until "the job reaches step ${CKPT_PREEMPT_AT_STEP}" "$(( (CKPT_PREEMPT_AT_STEP * CKPT_STEP_SECONDS + 60) / 2 ))" 2 \
      ckpt_reached_step "$JOB_CKPT" "$CKPT_PREEMPT_AT_STEP"; then
    fail "the job never reported reaching step ${CKPT_PREEMPT_AT_STEP} (highest seen: $(metric_max "$JOB_CKPT" train_step)) — preempting now would say nothing about resuming from depth"
  fi
  STEP_AT_PREEMPTION=$(metric_max "$JOB_CKPT" train_step)

  JOB_CKPT_PRE=$(submit_job_ext "$PE3_ID" "${AGENTS3[5]}" "guaranteed" "0.02" "$JOB_FILE" \
    "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${PREEMPTOR_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "$CKPT_PREEMPTOR_ACC" "")
  echo "  submitted guaranteed ${CKPT_PREEMPTOR_ACC}-accelerator job to preempt it at step ${STEP_AT_PREEMPTION}: $JOB_CKPT_PRE"

  if wait_until "the checkpointing job is preempted back to QUEUED" 90 1 job_is_queued "$JOB_CKPT"; then
    STINT1_ELAPSED=$(( $(date +%s) - STINT1_START ))
    pass "the checkpointing job was preempted back to QUEUED after ${STINT1_ELAPSED}s at step ${STEP_AT_PREEMPTION}"
  else
    STINT1_ELAPSED=$(( $(date +%s) - STINT1_START ))
    fail "the checkpointing job was never preempted (status=$(get_status "$JOB_CKPT"))"
  fi

  S=$(wait_for_completion_after_running "$JOB_CKPT_PRE" "0.02" "$ADMISSION_BUDGET_SECONDS" "$(( PREEMPTOR_RUN_SECONDS + 60 ))" || true)
  [[ "$S" == "COMPLETED" ]] \
    && { pass "the guaranteed preemptor ran and completed on the whole node"; file_finding "$JOB_CKPT_PRE" "preemption e2e: guaranteed job preempted a checkpointing burst job."; } \
    || fail "the guaranteed preemptor ended $S — the capacity it took was never used"

  # --- 3. the window was actually spent: a checkpoint exists
  # The fixture waits ${CKPT_WRITE_DELAY}s before writing, which is longer than the ordinary 5s
  # shutdown grace. So this object exists only if the termination was classified as `policy` AND
  # the job was given the window it declared — a build that signalled and killed on the ordinary
  # grace leaves this listing empty.
  CKPT_OBJECTS=$(experiment_data_keys "$JOB_CKPT" | grep -c 'step\.txt$' || true)
  [[ "$CKPT_OBJECTS" -eq 1 ]] \
    && pass "the preempted job had time to write its checkpoint — a policy termination gave it the window it declared" \
    || fail "the preempted job left no step.txt behind (keys: $(experiment_data_keys "$JOB_CKPT" | tr '\n' ' ')) — it was killed inside the ordinary 5s grace, so it was told nothing it could act on"

  # --- 4. it resumes from that step rather than from zero
  STINT2_START=$(date +%s)
  S=$(wait_for_completion_after_running "$JOB_CKPT" "0.05" "$(( ADMISSION_BUDGET_SECONDS * 2 ))" "$(( CKPT_FULL_RUN_SECONDS + 90 ))" || true)
  STINT2_ELAPSED=$(( $(date +%s) - STINT2_START ))
  if [[ "$S" == "COMPLETED" ]]; then
    pass "the preempted job was re-admitted and ran to COMPLETED"

    # THE assertion. resume_step is reported once per stint, before any work: 0 on a first run and
    # the checkpointed step on a resumed one. A job that came back from zero completes just as
    # happily and reports 0 twice, so "it completed" proves nothing and this number proves it all.
    RESUME_STEP=$(metric_max "$JOB_CKPT" resume_step)
    STINTS=$(metric_distinct_count "$JOB_CKPT" resume_step)
    if [[ -z "$RESUME_STEP" ]]; then
      fail "the job never reported resume_step — there is no evidence about where the second stint began"
    elif [[ "$(py "print(float('$RESUME_STEP') > 0.5)")" == "True" ]]; then
      pass "the second stint began at step ${RESUME_STEP}, not 0 — it resumed from its own checkpoint"
    else
      fail "the second stint began at step ${RESUME_STEP} — the job restarted from zero and the rescaled estimate bills for work it redid"
    fi
    [[ "$STINTS" -eq 2 ]] \
      && pass "exactly 2 stints ran (one resume_step value each), so the reading above is about a requeue rather than a restart loop" \
      || fail "$STINTS distinct resume_step value(s) reported, want 2 — the job ran a different number of stints than this scenario is reasoning about"

    # The series CONTINUED rather than restarted. One point per step, reported once: a run that
    # started over reports steps 1..${STEP_AT_PREEMPTION} a second time, so the point COUNT — not
    # the maximum, which reaches ${CKPT_STEPS_TOTAL} either way — is what tells the two apart.
    STEP_POINTS=$(metric_values "$JOB_CKPT" train_step | wc -l | tr -d ' ')
    FINAL_STEP=$(metric_max "$JOB_CKPT" train_step)
    [[ "$(py "print(abs(float('${FINAL_STEP:-0}') - ${CKPT_STEPS_TOTAL}.0) < 0.001)")" == "True" ]] \
      && pass "the series ran through to step ${FINAL_STEP} of ${CKPT_STEPS_TOTAL}" \
      || fail "the highest reported step is ${FINAL_STEP:-<never reported>}, want ${CKPT_STEPS_TOTAL}"
    # One point of slack: the step in flight when SIGTERM arrived may legitimately be reported by
    # both stints, since the checkpoint records the last COMPLETED step.
    [[ "$STEP_POINTS" -le "$(( CKPT_STEPS_TOTAL + 1 ))" ]] \
      && pass "${STEP_POINTS} step points for a ${CKPT_STEPS_TOTAL}-step run — the metric series continued across the two stints instead of restarting" \
      || fail "${STEP_POINTS} step points for a ${CKPT_STEPS_TOTAL}-step run — the early steps were reported twice, so the second stint redid work the first had already done"

    # --- 5. one run's worth of work across the two stints
    # Wall clock first, because it is what the cost is computed from. Continuing from step 9 of 14
    # means 5 steps remain, ~25s plus container start; starting over means all 14 do, ~70s plus
    # container start. The floor below sits between the two with room for a slow start on either
    # side, and is derived from the fixture's own step arithmetic rather than tuned by hand.
    CKPT_RESTART_FLOOR=$(py "print(int(${CKPT_FULL_RUN_SECONDS} * 0.75))")
    [[ "$STINT2_ELAPSED" -lt "$CKPT_RESTART_FLOOR" ]] \
      && pass "the second stint took ${STINT2_ELAPSED}s — under the ${CKPT_RESTART_FLOOR}s floor a from-zero rerun of all ${CKPT_STEPS_TOTAL} steps could not beat" \
      || fail "the second stint took ${STINT2_ELAPSED}s, at or above the ${CKPT_RESTART_FLOOR}s a full ${CKPT_FULL_RUN_SECONDS}s rerun would take — it redid the work it had checkpointed"

    # Settlement bills lifetime observed hours on 1 accelerator, so the debit must cover BOTH
    # stints: a build that dropped the preempted stint's hours (the gap Part 1 above exists to
    # catch, seen from the other end) lands below the floor, and one that redid the work lands
    # above the ceiling. The band is measured against this scenario's own wall clock, which is the
    # only runtime figure it can see, and is widened by the checkpoint window and teardown that
    # sit inside the billed interval but outside these two measurements.
    CKPT_TOTAL_RUN=$(( STINT1_ELAPSED + STINT2_ELAPSED ))
    CKPT_QUOTA_AFTER=$(curl -sf "$API_URL/platform-experiments/${PE3_ID}/quotas/${AGENTS3[4]}" \
      | py "import sys,json; d=json.load(sys.stdin); print((d.get('used_guaranteed_acch') or 0) + (d.get('used_burst_acch') or 0))")
    CKPT_DEBIT=$(py "print(round(float('${CKPT_QUOTA_AFTER:-0}') - float('${CKPT_QUOTA_BEFORE:-0}'), 6))")
    CKPT_LO=$(py "print(round(1 * max(1, ${CKPT_TOTAL_RUN} - 30) / 3600.0 * ${A100_ACCH_RATE}, 6))")
    CKPT_HI=$(py "print(round(1 * (${CKPT_TOTAL_RUN} + 45) / 3600.0 * ${A100_ACCH_RATE}, 6))")
    ONE_RUN=$(py "print(round(1 * ${CKPT_FULL_RUN_SECONDS} / 3600.0 * ${A100_ACCH_RATE}, 6))")
    [[ "$(py "print(${CKPT_LO} <= float('${CKPT_DEBIT}') <= ${CKPT_HI})")" == "True" ]] \
      && pass "settled ${CKPT_DEBIT} AccH over ${CKPT_TOTAL_RUN}s of running (band ${CKPT_LO}..${CKPT_HI}) — one run's worth of work (~${ONE_RUN} AccH), billed across both stints" \
      || fail "settled ${CKPT_DEBIT} AccH falls outside ${CKPT_LO}..${CKPT_HI} for ${CKPT_TOTAL_RUN}s on 1 accelerator — below means the preempted stint was never billed, above means the job paid twice for work it checkpointed"
    file_finding "$JOB_CKPT" "preemption e2e: a preempted job resumed from its checkpoint and finished the same run."
  else
    fail "the checkpointing job never came back (status=$S, eviction_reason=$(get_field "$JOB_CKPT" eviction_reason), not_admitted_reason=$(get_field "$JOB_CKPT" not_admitted_reason))"
  fi
fi

echo ""
echo "  --- the declared window is capped by configuration ---"
# The cap is the only thing that makes the window safe to offer: without it a job holds contended
# accelerators for as long as it claims to still be saving. It is applied by the runtime when it
# compiles the pod, so the pod's own declaration is where it is observable — and that is also the
# only place that matters, since the kubelet honours nothing else.
#
# Plain train.py, not the checkpointing fixture: this job is only ever read while RUNNING and then
# left to finish, and a fixture that delays its own exit on SIGTERM would have nothing to add here.
JOB_CAP=$(submit_job_ext "$PE3_ID" "${AGENTS3[6]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${PREEMPTOR_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "1" "" \
  "" "" "" "" '{"checkpoint_grace_seconds": 99999}')
echo "  submitted a job declaring a 99999s window against a configured cap of ${CKPT_CAP}s: $JOB_CAP"
S=$(wait_for_status "$JOB_CAP" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" == "RUNNING" ]]; then
  wait_until "the over-declaring job's pod is readable" 30 1 pod_grace_readable "$JOB_CAP" || true
  CAPPED_GRACE=$(pod_grace_seconds "$JOB_CAP")
  [[ "$CAPPED_GRACE" == "$CKPT_CAP" ]] \
    && pass "the pod carries ${CAPPED_GRACE}s — the configured cap, not the 99999s the job asked for" \
    || fail "the pod's terminationGracePeriodSeconds is ${CAPPED_GRACE:-<unreadable>}, want the configured cap of ${CKPT_CAP} — an uncapped window lets a job hold contended accelerators indefinitely by claiming it is still saving"
  S=$(wait_for_status "$JOB_CAP" "COMPLETED,FAILED,EVICTED" "$(( PREEMPTOR_RUN_SECONDS + 90 ))" || true)
  [[ "$S" == "COMPLETED" ]] && file_finding "$JOB_CAP" "preemption e2e: an over-declared checkpoint window is capped at the configured maximum."
else
  fail "the over-declaring job never reached RUNNING (status=$S) — the cap was never observable"
fi

close_platform_experiment "$PE3_ID"
for j in "${JOB_GANG:-}" "${JOB_GANG_PRE:-}" "${JOB_GROUPS:-}" "${JOB_GROUPS_PRE:-}" "${JOB_CKPT:-}" "${JOB_CKPT_PRE:-}" "${JOB_CAP:-}"; do
  [[ -z "$j" ]] && continue
  s=$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,REJECTED" 30 || true)
  [[ "$s" == "COMPLETED" || "$s" == "FAILED" || "$s" == "EVICTED" || "$s" == "REJECTED" ]] \
    || fail "Parts 3-5 cleanup did not make $j terminal (status=$s)"
  wait_until "Parts 3-5 workload $j is removed" 30 1 job_resource_absent "$j" \
    || fail "Parts 3-5 workload $j remained in Kubernetes"
done
finish
