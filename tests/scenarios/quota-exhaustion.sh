#!/usr/bin/env bash
# Quota exhaustion — the path that evicts RUNNING jobs and cancels queued ones when an agent's
# budget is genuinely spent. Two halves, and the order matters:
#
#   1. Reservations alone must NEVER evict anything. Queuing work worth the whole remaining quota
#      is a claim about the future; only observed consumption can exhaust a budget. This once was
#      not true — admission legitimately lets reservations fill a quota to 100%, and the exhaustion
#      check read the same reservation-inclusive figure, so an agent that queued enough work had
#      its already-running jobs evicted (irreversibly, unrefunded) for budget nobody had spent.
#   2. Observed overrun DOES evict. A job that outruns its own estimate keeps consuming real
#      accelerator-hours; once those pass the budget the running job is evicted with
#      `quota_exhaustion` and its queued siblings are cancelled with their reservations returned.
#
# Overrun is produced honestly rather than by waiting out a large budget: the job is submitted with
# a small estimated_duration_hours (what admission reserves against) while its workload is told to
# run far longer (HYPOTHESISLOOP_DURATION_SECONDS), so observed cost climbs past the estimate on a
# timescale a scenario can watch. API-only, parallel-safe (own PE/agent, one accelerator).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-quota-exhaustion-${RUN_ID}"
register_agent "$AGENT"

# One stage taking the whole run, so the agent's guaranteed allocation is the entire budget and no
# stage boundary can move it mid-scenario.
STAGES='[{"length_pct":100,"evict_pct":0}]'
# The estimate is a fixed wall-clock duration, deliberately NOT scaled by the accelerator rate: it
# decides how long this scenario runs, and a pricier accelerator must not stretch it past the
# suite's per-scenario ceiling. The rate belongs in the budget instead, which is where cost lives.
JOB_HOURS="0.01"
ESTIMATE_SECONDS=$(py "print(round(float('$JOB_HOURS') * 3600))")
JOB_COST=$(py "print(round(float('$JOB_HOURS') * $TEST_ACCH_RATE, 6))")
# Budget covers exactly TWO of these jobs. That is the whole design of half 1: both jobs fit at
# admission, so reservations alone reach 100% of the quota — which is precisely what used to trip
# the exhaustion check and kill the running job. Anything less and the second job is refused at
# submission, no reservation is ever recorded, and half 1 asserts nothing at all.
BUDGET=$(py "print(round(float('$JOB_COST') * 2, 6))")
PE_ID=$(create_platform_experiment "quota-exhaustion-${RUN_ID}" "$BUDGET" 1 5 0 "" "$STAGES")
signup_and_start "$PE_ID" "$AGENT"

GUARANTEED=$(_quota_field "$PE_ID" "$AGENT" guaranteed_accelerator_hours)
echo "  budget=${BUDGET} AccH (2 x ${JOB_COST}), agent guaranteed=${GUARANTEED} AccH, estimate=${ESTIMATE_SECONDS}s at rate ${TEST_ACCH_RATE}"

# The workload runs 3x its estimate. Observed consumption therefore passes the 2-estimate budget
# around the 2x mark, leaving a full estimate's worth of headroom for a reconcile tick to notice
# before the job would have ended on its own.
OVERRUN_SECONDS=$(py "print($ESTIMATE_SECONDS * 3)")
JOB=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${OVERRUN_SECONDS}\"}")
echo "  ==> $JOB submitted (reserves ${ESTIMATE_SECONDS}s of budget, workload runs ~${OVERRUN_SECONDS}s)"

S=$(wait_for_status "$JOB" "RUNNING" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" ]] \
  && pass "job admitted and RUNNING against a budget covering two of its estimate" \
  || { fail "job never reached RUNNING (status=$S) — cannot exercise quota exhaustion"; close_platform_experiment "$PE_ID"; finish; }

echo "  -- half 1: a queued reservation must not evict the running job --"
# The budget covers two jobs, so this one is admitted as a reservation rather than refused. Its
# estimate plus the running job's now account for 100% of the quota while the running job has
# barely consumed anything — the exact state that used to evict the RUNNING job on the next tick.
read -r QUEUE_CODE QUEUED_JOB <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS")"
[[ "$QUEUE_CODE" -lt 400 ]] \
  && pass "second job accepted, so reservations now fill the whole quota (${QUEUED_JOB})" \
  || fail "second job was refused at submission (HTTP $QUEUE_CODE) — no reservation exists, so this half proves nothing; the budget is mis-sized"

# Two full reconcile intervals (5s each, see scheduler.reconcile_interval_seconds). Observed
# consumption after this long is still far below the two-estimate budget, so any eviction here is
# the reservation-driven bug and nothing else.
assert_stable_status "$JOB" "RUNNING" 12 "running job survives a sibling's reservation filling the quota"

USED_EARLY=$(quota_used_guaranteed "$PE_ID" "$AGENT")
echo "  used_guaranteed_acch while both jobs exist: ${USED_EARLY} (budget ${GUARANTEED})"

echo "  -- half 2: observed overrun evicts the running job --"
# The job is now consuming real accelerator-hours past what it reserved. Generous ceiling: this
# must happen once observed cost crosses the budget, not at any particular second.
FINAL=$(wait_for_status "$JOB" "EVICTED,COMPLETED,FAILED" "$OVERRUN_SECONDS" || true)
if [[ "$FINAL" == "EVICTED" ]]; then
  pass "overrunning job was evicted once observed consumption passed its budget"
  REASON=$(get_field "$JOB" eviction_reason)
  [[ "$REASON" == "quota_exhaustion" ]] \
    && pass "eviction reason is quota_exhaustion" \
    || fail "job evicted for '$REASON', expected quota_exhaustion"

  # No refund for a running job — the budget really was spent. Settlement must still record what
  # it consumed, so the eviction is billed rather than silently free.
  # A function, not `test -n "$(...)"`: the substitution would be evaluated once at call time and
  # wait_until would then re-test the same stale value on every poll.
  job_settled() { [[ -n "$(get_field "$1" quota_settled_at)" ]]; }
  wait_until "evicted job settled" 20 1 job_settled "$JOB" || true
  [[ -n "$(get_field "$JOB" quota_settled_at)" ]] \
    && pass "evicted job's observed cost was durably settled" \
    || fail "evicted job has no quota_settled_at — its real consumption was never billed"

  # A pre-run job consumed nothing, so exhaustion cancels it rather than billing it. If the
  # cluster had a free accelerator it may have started and been evicted alongside its sibling, and
  # if it was quick it may even have finished before the budget ran out — all three are the
  # platform behaving correctly; only "still sitting there QUEUED" is not.
  QS=$(wait_for_status "$QUEUED_JOB" "REJECTED,EVICTED,COMPLETED,FAILED" 20 || true)
  QS_REASON=$(get_field "$QUEUED_JOB" eviction_reason)
  case "$QS" in
    REJECTED|EVICTED)
      # The reason matters as much as the status: an unrelated rejection would otherwise satisfy
      # "cancelled by the same exhaustion".
      [[ "$QS_REASON" == "quota_exhaustion" ]] \
        && pass "queued sibling was cancelled by the same exhaustion (status=$QS)" \
        || fail "queued sibling is $QS but for reason '$QS_REASON', not quota_exhaustion" ;;
    COMPLETED)
      pass "queued sibling had already run to completion before the budget ran out" ;;
    *)
      fail "queued sibling is $QS (reason='$QS_REASON') after its agent's budget was exhausted — it should have been cancelled" ;;
  esac

elif [[ "$FINAL" == "COMPLETED" ]]; then
  fail "job ran ~${OVERRUN_SECONDS}s on a budget covering $(py "print($ESTIMATE_SECONDS * 2)")s of runtime and completed — observed overrun never exhausted the quota"
else
  fail "job ended as '$FINAL' — expected EVICTED for quota_exhaustion"
fi

USED_FINAL=$(quota_used_guaranteed "$PE_ID" "$AGENT")
echo "  final used_guaranteed_acch: ${USED_FINAL} (budget ${GUARANTEED})"
# The bound the platform actually promises: consumption may pass the budget by the running work of
# a few reconcile ticks, never by an unbounded amount. Computed from the real quantities (reconcile
# interval x accelerator count x rate) rather than a round multiple, so it stays a real ceiling.
ALLOWANCE=$(py "print(round(float('$GUARANTEED') + (30.0 / 3600.0) * $TEST_ACCH_RATE, 6))")
OVERSHOOT_OK=$(py "print(float('$USED_FINAL' or 0) <= float('$ALLOWANCE'))")
[[ "$OVERSHOOT_OK" == "True" ]] \
  && pass "settled consumption ${USED_FINAL} stayed within the ${ALLOWANCE} AccH bound (budget + a few reconcile ticks)" \
  || fail "settled consumption ${USED_FINAL} ran past the ${ALLOWANCE} AccH bound — exhaustion is not bounding overrun"

close_platform_experiment "$PE_ID"
finish
