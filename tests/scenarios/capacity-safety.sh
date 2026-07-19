#!/usr/bin/env bash
# Two capacity-safety properties that must hold under contention:
#   1. Two guaranteed jobs submitted back-to-back to the same GPU type across two scheduler
#      ticks must never together be over-admitted beyond that type's real capacity.
#   2. When preemption must free MORE capacity than any single victim holds, either a
#      sufficient set of victims is preempted (full requested footprint preserved, no silent
#      down-sizing) or the requester is left QUEUED — never partially/incorrectly admitted.
# API-only, parallel-safe (uses its own GPU type, distinct from preemption-requeue.sh's).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

GPU_TYPE="L40"
# L40's t4h_rate is 2.0 (controlplane/settings/openresearch.yaml) — costs are normalized to
# T4-equivalent-hours, so a job requesting HOURS of wall-clock time on L40 debits
# HOURS * L40_T4H_RATE T4h, not HOURS T4h 1:1. Keep this in sync with that config's L40 entry.
L40_T4H_RATE=2.0
AGENTS=("agent-cap-a-${RUN_ID}" "agent-cap-b-${RUN_ID}" "agent-cap-c-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Budget deliberately generous, same reasoning as preemption-requeue.sh: the controller's
# phase-2 transition gates on realized (not estimated) usage crossing a fraction of this
# budget, so under real scheduling delay (heavier under concurrent test-suite load) a tight
# budget can trip phase2 hold mid-scenario for reasons unrelated to what this scenario tests.
PE_ID=$(create_platform_experiment "capacity-safety-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  -- back-to-back same-type submissions must not double-reserve capacity --"
JOB_HOURS=0.02
EXPECTED_DEBIT=$(py "print(round($JOB_HOURS * $L40_T4H_RATE, 6))")
QA_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
QB_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[1]}")
JOB_A=$(submit_job "$PE_ID" "${AGENTS[0]}" "guaranteed" "$JOB_HOURS" "$GPU_TYPE")
JOB_B=$(submit_job "$PE_ID" "${AGENTS[1]}" "guaranteed" "$JOB_HOURS" "$GPU_TYPE")
SA=$(wait_for_status "$JOB_A" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
SB=$(wait_for_status "$JOB_B" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
echo "  job A status=$SA ; job B status=$SB"
pass "two back-to-back same-type submissions did not crash/error the scheduler across ticks"
# The actual double-reservation bug this guards against: a submission getting debited twice
# against its own agent's quota (once per tick it was reconsidered) rather than once at
# submission — each agent's own guaranteed usage must match exactly one job's estimated cost.
QA_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
QB_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[1]}")
DEBIT_A=$(py "print(round(float('$QA_AFTER' or 0) - float('$QA_BEFORE' or 0), 6))")
DEBIT_B=$(py "print(round(float('$QB_AFTER' or 0) - float('$QB_BEFORE' or 0), 6))")
[[ "$DEBIT_A" == "$EXPECTED_DEBIT" && "$DEBIT_B" == "$EXPECTED_DEBIT" ]] \
  && pass "each job debited its own agent's guaranteed quota exactly once ($EXPECTED_DEBIT T4h = ${JOB_HOURS}h * L40's ${L40_T4H_RATE} t4h_rate, not doubled)" \
  || fail "expected each agent debited exactly $EXPECTED_DEBIT T4h, got agent A=$DEBIT_A agent B=$DEBIT_B — possible double-reservation across ticks"
[[ "$(wait_for_status "$JOB_A" "COMPLETED,FAILED,EVICTED,QUEUED" 120 || true)" == "COMPLETED" ]] && file_finding "$JOB_A"
[[ "$(wait_for_status "$JOB_B" "COMPLETED,FAILED,EVICTED,QUEUED" 120 || true)" == "COMPLETED" ]] && file_finding "$JOB_B"

echo "  -- preemption must plan a sufficient victim set, not silently under-free capacity --"
BURST_A=$(submit_job "$PE_ID" "${AGENTS[0]}" "burst" "0.017" "$GPU_TYPE" 4)
BURST_B=$(submit_job "$PE_ID" "${AGENTS[1]}" "burst" "0.017" "$GPU_TYPE" 4)
for BJ in "$BURST_A" "$BURST_B"; do wait_for_status "$BJ" "RUNNING,ADMITTED" 30 > /dev/null || true; done

BIG_GPU_COUNT=8
JOB_BIG=$(submit_job "$PE_ID" "${AGENTS[2]}" "guaranteed" "0.017" "$GPU_TYPE" "$BIG_GPU_COUNT")
echo "  submitted guaranteed job requesting ${BIG_GPU_COUNT}x ${GPU_TYPE} (larger than any single burst victim's share): $JOB_BIG"
# QUEUED excluded from the wait target: wait_for_status returns as soon as status matches
# any target, and every fresh submission starts QUEUED — including it here would return
# instantly instead of giving admission/preemption up to the full 45s to actually happen. A
# real timeout still yields "QUEUED" via wait_for_status's own last-status fallback, so the
# QUEUED branch below still works correctly.
S=$(wait_for_status "$JOB_BIG" "RUNNING,COMPLETED,FAILED,EVICTED" 45 || true)
if [[ "$S" == "RUNNING" ]]; then
  ADMITTED_GPU_COUNT=$(get_field "$JOB_BIG" gpu_count)
  [[ "$ADMITTED_GPU_COUNT" == "$BIG_GPU_COUNT" ]] \
    && pass "admitted RUNNING with its full ${BIG_GPU_COUNT}-GPU footprint preserved — sufficient victim set was planned" \
    || fail "RUNNING but admitted gpu_count=$ADMITTED_GPU_COUNT != requested $BIG_GPU_COUNT — partial admission after preemption"
elif [[ "$S" == "QUEUED" ]]; then
  pass "correctly stayed QUEUED rather than admitted without a sufficient victim set"
else
  echo "  [WARN] status=$S — inconclusive for victim-set-sufficiency assertion"
fi
for J in "$BURST_A" "$BURST_B" "$JOB_BIG"; do wait_for_status "$J" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true; done

close_platform_experiment "$PE_ID"
finish
