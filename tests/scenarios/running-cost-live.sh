#!/usr/bin/env bash
# Live running-cost correction (controlplane/services/quota/running_cost.go): a RUNNING job's
# contribution to used_guaranteed_acch must track its actual observed-elapsed cost, not stay
# pinned at the static admission-time estimate for the job's whole lifetime. Concretely: right
# after admission the estimate is debited in full (AddDesiredQuotaUsage, so budget is never
# under-reserved before a job has reported anything); once the job starts reporting metrics,
# correctRunningCosts should replace that full estimate with the (much smaller) actual
# observed-so-far cost, then grow it back up as the job keeps running — never staying flat at the
# original estimate throughout. API-only, parallel-safe (its own fresh PE/agent).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-livecost-${RUN_ID}"
register_agent "$AGENT"
# report_interval_seconds=5: frequent enough samples to see the correction move within the
# scenario's own short runtime.
PE_ID=$(create_platform_experiment "running-cost-live-${RUN_ID}" 50.0 1 5)
signup_and_start "$PE_ID" "$AGENT"

# scale_budget scales HOURS (and so wall-clock duration = hours*3600) by TEST_ACCH_RATE, same as
# every other scenario's cost-sized "hours" argument — derive the sample delays from the actual
# resulting duration rather than a fixed wall-clock guess, so a non-default TEST_ACCH_RATE can
# never shrink the job's real runtime out from under fixed sleep values.
HOURS="$(scale_budget 0.05)"
DURATION_SECONDS=$(py "print(round(float('$HOURS') * 3600))")
EARLY_DELAY=$(py "print(max(1, round(float('$DURATION_SECONDS') * 0.15)))")
LATE_DELAY=$(py "print(max(1, round(float('$DURATION_SECONDS') * 0.65)))")
JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$HOURS")
echo "  ==> $JOB submitted (estimated_duration_hours=$HOURS, ~${DURATION_SECONDS}s), waiting for RUNNING..."
wait_for_status "$JOB" "RUNNING" "$ADMISSION_BUDGET_SECONDS" > /dev/null \
  || { fail "$JOB never reached RUNNING; live-cost setup failed"; close_platform_experiment "$PE_ID"; finish; }

ESTIMATED=$(get_field "$JOB" estimated_cost_acch)
echo "  ==> $JOB RUNNING, estimated_cost_acch=$ESTIMATED"

# Sample early (job has barely run) and late (job has run for a while, but still well inside its
# own duration). correctRunningCosts only ever replaces a RUNNING job's estimate once it has
# observed at least one sample from that job — the 15%-in early sample gives it a couple of
# report intervals first, rather than reading the pre-correction static estimate on a technicality.
# Both samples assert the job is still RUNNING — if it already finished, the sample says nothing
# about live correction of a RUNNING job and the scenario should fail loudly, not silently pass.
sleep "$EARLY_DELAY"
[[ "$(get_status "$JOB")" == "RUNNING" ]] \
  || { fail "$JOB is no longer RUNNING at the early sample point (t+${EARLY_DELAY}s of ~${DURATION_SECONDS}s) — duration too short for this scenario's sample delays"; close_platform_experiment "$PE_ID"; finish; }
EARLY=$(quota_used_guaranteed "$PE_ID" "$AGENT")
echo "  ==> early sample (t+${EARLY_DELAY}s): used_guaranteed_acch=$EARLY"

sleep "$(( LATE_DELAY - EARLY_DELAY ))"
[[ "$(get_status "$JOB")" == "RUNNING" ]] \
  || { fail "$JOB is no longer RUNNING at the late sample point (t+${LATE_DELAY}s of ~${DURATION_SECONDS}s) — duration too short for this scenario's sample delays"; close_platform_experiment "$PE_ID"; finish; }
LATE=$(quota_used_guaranteed "$PE_ID" "$AGENT")
echo "  ==> late sample (t+${LATE_DELAY}s): used_guaranteed_acch=$LATE"

GREW=$(py "print(float('$LATE' or 0) > float('$EARLY' or 0))")
[[ "$GREW" == "True" ]] \
  && pass "used_guaranteed_acch grew between samples ($EARLY -> $LATE) — tracking live observed cost, not frozen at a fixed value" \
  || fail "used_guaranteed_acch did not grow while $JOB kept running ($EARLY -> $LATE) — live running-cost correction may not be applying"

# The early sample, taken well before the job's estimated duration has elapsed, must read below
# the full estimate — otherwise correctRunningCosts isn't replacing the static estimate with the
# (smaller) actual-so-far figure at all, and this is silently just re-reading the estimate.
BELOW_ESTIMATE=$(py "print(float('$EARLY' or 0) < float('$ESTIMATED' or 0))")
[[ "$BELOW_ESTIMATE" == "True" ]] \
  && pass "early used_guaranteed_acch ($EARLY) is below the full estimate ($ESTIMATED) — live correction is active, not just the admission-time debit" \
  || fail "early used_guaranteed_acch ($EARLY) is not below the full estimate ($ESTIMATED) — job may still be debited at its static estimate instead of observed cost"

cancel_job "$JOB"
wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED,REJECTED" 30 > /dev/null || true
close_platform_experiment "$PE_ID"
finish
