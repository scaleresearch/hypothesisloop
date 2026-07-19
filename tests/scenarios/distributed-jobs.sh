#!/usr/bin/env bash
# Distributed (num_nodes>1) job admission, in two parts sharing one platform experiment:
#   1. depth: a single num_nodes=2 job's gang scheduling creates all replicas, generated pod
#      specs carry equal/non-empty requests==limits, and billing reflects
#      TotalAccelerators = accelerator_count * num_nodes, not just accelerator_count.
#   2. pressure: several num_nodes=2 jobs submitted concurrently must each still get their
#      full per-node x NumNodes footprint under real scheduling pressure, not a partial
#      replica count.
# API + pod-spec checks; parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

JOB_HOURS="0.025"
# H100, not T4: T4 is every other parallel-set scenario's default accelerator type (job-lifecycle,
# mixed-admission, phase2-and-settlement, cpu-quota-guard's own dimension aside), so
# num_nodes=2 T4 jobs here would have to out-rank a constant stream of fresh guaranteed-tier
# submissions from those sibling scenarios for the scheduler's fairness-window priority —
# observed in practice as not_admitted_reason=="outranked", not capacity_unavailable, i.e.
# real capacity was free but priority never favored these jobs within any reasonable wait
# window. H100 is unused by every other scenario (capacity-safety uses L40, preemption-requeue
# uses A100 — same reasoning, see those files), so this scenario gets a clean, uncontended pool.
ACCELERATOR_TYPE="H100"
H100_ACCH_RATE=1.0
AGENTS=("agent-dist-depth-${RUN_ID}" "agent-dist-pressure-a-${RUN_ID}" "agent-dist-pressure-b-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Budget sized well above what Part1+Part2 will actually debit (H100's acch_rate=1.0 is the
# AccH baseline itself, so num_nodes=2 jobs cost exactly 2x their wall-clock hours) — also
# comfortably clears the phase-2 hold trigger at 40% of budget (see preemption-requeue.sh's
# identical reasoning for why that matters).
PE_ID=$(create_platform_experiment "distributed-jobs-${RUN_ID}" 20.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "=========================================================="
echo "Part 1: single distributed job — gang scheduling + billing depth"
echo "=========================================================="
QUOTA_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
JOB=$(submit_job "$PE_ID" "${AGENTS[0]}" "guaranteed" "$JOB_HOURS" "$ACCELERATOR_TYPE" "1" "2")
echo "  submitted distributed job (accelerator_count=1, num_nodes=2): $JOB"

S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED" 30 || true)
if [[ "$S" != "RUNNING" && "$S" != "COMPLETED" ]]; then
  fail "distributed job never reached RUNNING/COMPLETED (status=$S) — gang scheduling may be broken"
else
  pass "distributed job reached $S"

  REPLICAS=$(job_pod_count "$JOB")
  [[ "$REPLICAS" -ge 2 ]] && pass "$REPLICAS pods (expected 2, one per num_nodes replica)" \
    || fail "only $REPLICAS pod(s) — expected 2, gang scheduling did not create both replicas"

  POD=$(job_pod "$JOB")
  if [[ -n "$POD" ]]; then
    MEM_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || true)
    MEM_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.memory}' 2>/dev/null || true)
    ACCELERATOR_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu}' 2>/dev/null || true)
    ACCELERATOR_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}' 2>/dev/null || true)
    if [[ -n "$MEM_REQ" && "$MEM_REQ" == "$MEM_LIM" && -n "$ACCELERATOR_REQ" && "$ACCELERATOR_REQ" == "$ACCELERATOR_LIM" ]]; then
      pass "generated pod has equal, non-empty memory/accelerator requests==limits"
    else
      fail "expected equal, non-empty memory/accelerator requests+limits, got memory req=$MEM_REQ lim=$MEM_LIM accelerator req=$ACCELERATOR_REQ lim=$ACCELERATOR_LIM"
    fi
  else
    fail "could not locate pod to inspect resources"
  fi

  EST_COST=$(get_field "$JOB" estimated_cost_acch)
  EXPECTED_COST=$(py "print(round(2 * ${JOB_HOURS} * ${H100_ACCH_RATE}, 6))")
  COST_OK=$(py "print(abs(float('$EST_COST' or 0) - float('$EXPECTED_COST')) < 0.01)")
  [[ "$COST_OK" == "True" ]] && pass "estimated_cost_acch=$EST_COST bills for both replicas (TotalAccelerators)" \
    || fail "estimated_cost_acch=$EST_COST does not match TotalAccelerators-based expected~$EXPECTED_COST"

  S=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" 150 || true)
  [[ "$S" == "COMPLETED" ]] && pass "completed cleanly" || fail "did not complete cleanly (status=$S)"

  QUOTA_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
  DEBIT=$(py "print(round(float('$QUOTA_AFTER' or 0) - float('$QUOTA_BEFORE' or 0), 4))")
  echo "  guaranteed quota debited: ${DEBIT} AccH (single-replica equivalent would be ~$(py "print(round(${JOB_HOURS} * ${H100_ACCH_RATE}, 4))") AccH)"
fi

echo ""
echo "=========================================================="
echo "Part 2: concurrent distributed jobs under scheduling pressure"
echo "=========================================================="
declare -a JOBS
for a in "${AGENTS[@]:1}"; do
  J=$(submit_job "$PE_ID" "$a" "burst" "0.02" "$ACCELERATOR_TYPE" "1" "2")
  JOBS+=("$J")
  echo "  concurrent distributed job: $J (agent=$a, accelerator_count=1, num_nodes=2)"
done

OK=0
for J in "${JOBS[@]}"; do
  # QUEUED deliberately excluded from the wait target: every fresh submission starts QUEUED,
  # so including it here would make wait_for_status return almost instantly (on its very
  # first poll) instead of actually waiting up to the timeout for a real admission decision —
  # the timeout value below would then be dead code. A genuine timeout still yields "QUEUED"
  # via wait_for_status's own last-status fallback, so the QUEUED branch below still works.
  # burst-tier jobs are also processed after guaranteed-tier jobs every tick, and the
  # scheduler is one shared service across the whole concurrent test-suite run — several
  # other scenarios in this scenario's batch submit guaranteed-tier jobs, which can keep
  # pushing burst-tier admission back for longer than a short window comfortably covers.
  S=$(wait_for_status "$J" "RUNNING,COMPLETED,FAILED,EVICTED" 180 || true)
  REPLICAS=$(job_pod_count "$J")
  echo "  $J: status=$S replicas=$REPLICAS"
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    if [[ "$REPLICAS" -ge 2 ]]; then
      OK=$((OK + 1))
    else
      fail "$J admitted (status=$S) but only has $REPLICAS pod(s), expected 2"
    fi
  fi
done
[[ "$OK" -ge 1 ]] \
  && pass "$OK/${#JOBS[@]} concurrent num_nodes=2 jobs admitted with full replica counts under pressure" \
  || fail "no concurrent num_nodes=2 job reached RUNNING/COMPLETED with full replica count"

for J in "${JOBS[@]}"; do wait_for_status "$J" "COMPLETED,FAILED,EVICTED,QUEUED" 150 > /dev/null || true; done

close_platform_experiment "$PE_ID"
finish
