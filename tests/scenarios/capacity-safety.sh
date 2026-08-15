#!/usr/bin/env bash
# Two capacity-safety properties that must hold under contention:
#   1. Two guaranteed jobs submitted back-to-back to the same accelerator type across two scheduler
#      ticks must never together be over-admitted beyond that type's real capacity.
#   2. When preemption must free MORE capacity than any single victim holds, either a
#      sufficient set of victims is preempted (full requested footprint preserved, no silent
#      down-sizing) or the requester is left QUEUED — never partially/incorrectly admitted.
# API-only, parallel-safe (uses its own accelerator type, distinct from preemption-requeue.sh's).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

ACCELERATOR_TYPE="$TEST_ACCELERATOR_TYPE"
# Costs are normalized to H100-equivalent-hours, so a job requesting HOURS of wall-clock time
# reserves HOURS * this type's acch_rate AccH, not HOURS AccH 1:1. TEST_ACCH_RATE must match the
# type's entry in controlplane/settings/hypothesisloop.yaml (see tests/lib/common.sh).
ACCH_RATE="$TEST_ACCH_RATE"
AGENTS=("agent-cap-a-${RUN_ID}" "agent-cap-b-${RUN_ID}" "agent-cap-c-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Budget deliberately generous, same reasoning as preemption-requeue.sh: a stage boundary gates
# on realized (not estimated) usage crossing a share of this budget, so under real scheduling
# delay (heavier under concurrent test-suite load) a tight budget can cut an agent mid-scenario
# for reasons unrelated to what this scenario tests.
PE_ID=$(create_platform_experiment "capacity-safety-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  -- PostgreSQL desired usage must remain stable across scheduler ticks --"
JOB_HOURS=0.003
EXPECTED_RESERVATION=$(py "print(round($JOB_HOURS * $ACCH_RATE, 6))")
QA_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
QB_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[1]}")
JOB_A=$(submit_job "$PE_ID" "${AGENTS[0]}" "guaranteed" "$JOB_HOURS" "$ACCELERATOR_TYPE")
JOB_B=$(submit_job "$PE_ID" "${AGENTS[1]}" "guaranteed" "$JOB_HOURS" "$ACCELERATOR_TYPE")
SA=$(wait_for_status "$JOB_A" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
SB=$(wait_for_status "$JOB_B" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
echo "  job A status=$SA ; job B status=$SB"
pass "two back-to-back same-type submissions did not crash/error the scheduler across ticks"
# Desired usage is derived from each current PostgreSQL job row. Reconsidering a job on later
# scheduler ticks must not create another reservation or any metrics-side copy.
QA_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
QB_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[1]}")
RESERVED_A=$(py "print(round(float('$QA_AFTER' or 0) - float('$QA_BEFORE' or 0), 6))")
RESERVED_B=$(py "print(round(float('$QB_AFTER' or 0) - float('$QB_BEFORE' or 0), 6))")
[[ "$RESERVED_A" == "$EXPECTED_RESERVATION" && "$RESERVED_B" == "$EXPECTED_RESERVATION" ]] \
  && pass "each current PostgreSQL job contributes exactly one desired reservation ($EXPECTED_RESERVATION AccH)" \
  || fail "expected one $EXPECTED_RESERVATION AccH desired reservation per job, got agent A=$RESERVED_A agent B=$RESERVED_B"
for J in "$JOB_A" "$JOB_B"; do
  JS=$(wait_for_completion_after_running "$J" "$JOB_HOURS" "$ADMISSION_BUDGET_SECONDS" || true)
  if [[ "$JS" == "COMPLETED" ]]; then
    file_finding "$J"
  else
    fail "initial capacity probe $J did not complete (status=$JS)"
  fi
  wait_until "completed capacity probe $J is removed from Kubernetes" 30 1 job_resource_absent "$J" \
    || fail "completed capacity probe $J still owns a Kubernetes Job"
done

echo "  -- preemption must plan a sufficient victim set, not silently under-free capacity --"
BURST_A=$(submit_job "$PE_ID" "${AGENTS[0]}" "burst" "0.017" "$ACCELERATOR_TYPE" 4)
BURST_B=$(submit_job "$PE_ID" "${AGENTS[1]}" "burst" "0.017" "$ACCELERATOR_TYPE" 4)
for BJ in "$BURST_A" "$BURST_B"; do
  BS=$(wait_for_status "$BJ" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
  [[ "$BS" == "RUNNING" ]] \
    || fail "burst victim setup failed for $BJ (status=$BS); victim-set assertion would be invalid"
done

BIG_ACCELERATOR_COUNT=8
JOB_BIG=$(submit_job "$PE_ID" "${AGENTS[2]}" "guaranteed" "0.017" "$ACCELERATOR_TYPE" "$BIG_ACCELERATOR_COUNT")
echo "  submitted guaranteed job requesting ${BIG_ACCELERATOR_COUNT}x ${ACCELERATOR_TYPE} (larger than any single burst victim's share): $JOB_BIG"
# QUEUED excluded from the wait target: wait_for_status returns as soon as status matches
# any target, and every fresh submission starts QUEUED — including it here would return
# instantly instead of giving admission/preemption up to the full 45s to actually happen. A
# A timeout prints the current status for the assertion below.
S=$(wait_for_status "$JOB_BIG" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
  ADMITTED_ACCELERATOR_COUNT=$(get_field "$JOB_BIG" accelerator_count)
  [[ "$ADMITTED_ACCELERATOR_COUNT" == "$BIG_ACCELERATOR_COUNT" ]] \
    && pass "admitted with its full ${BIG_ACCELERATOR_COUNT}-accelerator footprint preserved — sufficient victim set was planned" \
    || fail "admitted but accelerator_count=$ADMITTED_ACCELERATOR_COUNT != requested $BIG_ACCELERATOR_COUNT — partial admission after preemption"
else
  fail "guaranteed job did not run after two sufficient burst victims became available for preemption (status=$S)"
fi
for J in "$BURST_A" "$BURST_B" "$JOB_BIG"; do wait_for_status "$J" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true; done

close_platform_experiment "$PE_ID"
finish
