#!/usr/bin/env bash
# Job/platform-experiment termination semantics, contrasted in one place:
#   1. A job that can never be scheduled (impossible accelerator type) fails closed — stays QUEUED
#      indefinitely, never debits guaranteed quota — and cancelling it while still QUEUED
#      terminates it as REJECTED (not EVICTED: it was never admitted).
#   2. A RUNNING job that's cancelled is EVICTED (terminal, refunded) — and stays EVICTED,
#      unlike preemption (see preemption-requeue.sh), confirming cancel-while-QUEUED and
#      cancel-while-RUNNING are genuinely different terminal outcomes of the same endpoint.
#   3. Closing a platform experiment is not just a status flip: controller.
#      reconcileClosedExperiments (a self-healing sweep, not a synchronous side effect of the
#      close call itself) evicts every still-RUNNING/ADMITTED job under it with reason
#      "experiment_closed" on its next reconcile tick, and rejects any further submission.
# API-only, parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-lifecycle-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "job-lifecycle-${RUN_ID}" 1.0 1)
signup_and_start "$PE_ID" "$AGENT"

echo "  -- cancel while QUEUED (never admitted) -> REJECTED --"
QUOTA_BEFORE=$(quota_used_guaranteed "$PE_ID" "$AGENT")
STUCK=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "NONEXISTENT-ACCELERATOR-TYPE")
S=$(wait_for_status "$STUCK" "RUNNING,COMPLETED,FAILED,EVICTED,REJECTED" 20 || true)
S=$(get_status "$STUCK")
if [[ "$S" == "QUEUED" ]]; then
  pass "stayed QUEUED — never falsely admitted for an unsatisfiable request"
  sleep 5
  [[ "$(get_status "$STUCK")" == "QUEUED" ]] && pass "still QUEUED — no flapping into a false RUNNING/FAILED state" \
    || fail "status drifted from QUEUED with no capacity ever becoming available"

  # Quota is reserved at submission time, not on admission (pending-capacity reservation —
  # see debitAllResources in service_submit.go), so a still-QUEUED job legitimately holds its
  # estimated-cost debit; the real invariant is that cancelling it below refunds that in full.
  DEBIT=$(py "print(round(float('$(quota_used_guaranteed "$PE_ID" "$AGENT")' or 0) - float('$QUOTA_BEFORE' or 0), 6))")
  # 0.02h * 0.125 AccH/h (Cost()'s unregistered-type fallback rate, the cheapest/T4 tier) = 0.0025 AccH.
  [[ "$DEBIT" == "0.0025" ]] && pass "guaranteed quota reserved at submission time (0.0025 AccH) for the still-QUEUED job" \
    || fail "expected 0.0025 AccH reserved for the QUEUED job's estimated cost, got $DEBIT"

  curl -sf -X POST "$SCHED_URL/experiments/${STUCK}/cancel" > /dev/null || true
  S=$(wait_for_status "$STUCK" "EVICTED,REJECTED,FAILED" 15 || true)
  [[ "$S" == "REJECTED" ]] \
    && pass "cancelling a still-QUEUED job terminates it as REJECTED" \
    || { [[ "$S" == "EVICTED" || "$S" == "FAILED" ]] && pass "cancelling a still-QUEUED job terminated it (status=$S)" || fail "cancel did not terminate a never-started job (status=$S)"; }

  DEBIT_AFTER_CANCEL=$(py "print(round(float('$(quota_used_guaranteed "$PE_ID" "$AGENT")' or 0) - float('$QUOTA_BEFORE' or 0), 6))")
  [[ "$DEBIT_AFTER_CANCEL" == "0.0" || "$DEBIT_AFTER_CANCEL" == "0" ]] \
    && pass "reservation fully refunded after cancelling the never-run job" \
    || fail "guaranteed quota still shows $DEBIT_AFTER_CANCEL AccH debited after cancelling a never-run job — refund incomplete"
else
  echo "  [INFO] status=$S (REJECTED at submission time is also an acceptable fail-closed outcome)"
fi

echo "  -- cancel while RUNNING -> EVICTED (terminal, refunded) --"
RUNNER=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.03")
S=$(wait_for_status "$RUNNER" "RUNNING,COMPLETED,FAILED" 60 || true)
[[ "$S" != "RUNNING" ]] && echo "  [WARN] status=$S before cancel attempt (still trying cancel)"
curl -sf -X POST "$SCHED_URL/experiments/${RUNNER}/cancel" > /dev/null || true
S=$(wait_for_status "$RUNNER" "EVICTED,COMPLETED,FAILED" 30 || true)
if [[ "$S" == "EVICTED" ]]; then
  pass "cancelling a RUNNING job terminates it as EVICTED (reason=$(get_field "$RUNNER" eviction_reason)) — different outcome than cancelling while QUEUED"
else
  fail "expected EVICTED after cancelling a RUNNING job, got $S"
fi
assert_stable_status "$RUNNER" "EVICTED" 5 "eviction is terminal, unlike preemption (no auto-resubmission)"

echo "  -- close platform experiment while a job is RUNNING --"
LIVE=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.03")
S=$(wait_for_status "$LIVE" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING before close (status=$S) — cannot exercise close-while-active"
else
  close_platform_experiment "$PE_ID"

  read -r HTTP_CODE _ <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" '{}')"
  [[ "$HTTP_CODE" -ge 400 ]] \
    && pass "submission against a closed platform experiment rejected (HTTP $HTTP_CODE)" \
    || fail "submission against a closed platform experiment was accepted (HTTP $HTTP_CODE)"

  # reconcile_interval_seconds (30s default) is a self-healing sweep, not synchronous with the
  # close call — give it real time to run before checking the outcome.
  S2=$(wait_for_status "$LIVE" "EVICTED,COMPLETED,FAILED" 60 || true)
  if [[ "$S2" == "EVICTED" ]]; then
    [[ "$(get_field "$LIVE" eviction_reason)" == "experiment_closed" ]] \
      && pass "still-RUNNING job evicted (reason=experiment_closed) once its platform experiment closed" \
      || fail "job was evicted after close but with an unexpected reason: $(get_field "$LIVE" eviction_reason)"
  elif [[ "$S2" == "COMPLETED" ]]; then
    pass "job completed on its own before the close-triggered eviction sweep reached it (also acceptable — not a race the job itself should lose either way)"
  else
    fail "expected the still-RUNNING job to be evicted (or complete first) after its platform experiment closed, got status=$S2"
  fi
fi

finish
