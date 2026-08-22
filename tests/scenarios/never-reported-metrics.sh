#!/usr/bin/env bash
# A job that runs but never emits a metric its platform experiment declared. It cannot be ranked,
# cut, or compared against anything — there is nothing to judge it by — while it holds an
# accelerator and bills for it, so the controller evicts it and names the actual fault: its
# reporting path, not a hung trainer.
#
# The distinction this scenario exists to protect: "no samples recently" and "never reported at
# all" are different jobs with different fixes. A healthy job that reports normally must survive
# the same window that condemns the mute one, so both run side by side here.
#
# API-only and parallel-safe: two short jobs on their own platform experiment.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-mute-${RUN_ID}"
register_agent "$AGENT"
# report_interval_seconds=5 sets the contract the job is held to, but it does NOT set the grace
# period: the silence window is max(min_silence_window_seconds, silence_multiplier x interval),
# and the 60s floor dominates any short interval. So the real grace before a job is called mute is
# two windows = ~120s, plus a reconcile tick — not the ~30s the interval alone suggests. The mute
# job below must outlive that, or the scenario proves nothing about eviction.
REPORT_INTERVAL=5
GRACE_SECONDS=120
PE_ID=$(create_platform_experiment "never-reported-${RUN_ID}" 10.0 1 "$REPORT_INTERVAL")
signup_and_start "$PE_ID" "$AGENT"

# Comfortably longer than GRACE_SECONDS so the job is still running when the verdict lands: if it
# exited first, its disappearance — not the mute check — would explain the terminal status.
MUTE_SECONDS=180
MUTE_HOURS=$(py "print(round($MUTE_SECONDS / 3600.0, 6))")
MUTE_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$MUTE_HOURS" "" "" "" "" \
  "$(py "
import json
print(json.dumps({'command': ['/bin/sh', '-c'], 'args': ['sleep $MUTE_SECONDS'], 'max_retries': 0}))
")")
echo "  ==> $MUTE_JOB submitted: runs ${MUTE_SECONDS}s, emits nothing at all"

S=$(wait_for_status "$MUTE_JOB" "RUNNING" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" ]] \
  && pass "mute job reached RUNNING (it is alive, just silent)" \
  || { fail "mute job never reached RUNNING (status=$S) — cannot exercise the check"; close_platform_experiment "$PE_ID"; finish; }

echo "  -- a live job that never reports a declared metric is evicted --"
# The verdict cannot land before the ~120s grace, so waiting only that long would time out on a
# perfectly healthy platform. Wait the grace plus a reconcile margin, bounded by whatever the
# scenario has left, and still short of the job's own 180s runtime so a pass here cannot be the
# job simply finishing.
EVICT_WAIT=$((GRACE_SECONDS + 45))
LEFT=$(scenario_seconds_left)
[[ "$EVICT_WAIT" -gt "$LEFT" ]] && EVICT_WAIT="$LEFT"
FINAL=$(wait_for_status "$MUTE_JOB" "EVICTED,FAILED,COMPLETED" "$EVICT_WAIT" || true)
if [[ "$FINAL" == "EVICTED" ]]; then
  pass "mute job was evicted while still running"
  REASON=$(get_field "$MUTE_JOB" eviction_reason)
  # The code is the first token; a reason may carry a ": detail" suffix (see EvictionReason.WithDetail).
  [[ "$REASON" == never_reported_metrics* ]] \
    && pass "eviction reason is never_reported_metrics — the fault is named as the reporting path" \
    || fail "mute job evicted for '$REASON', expected never_reported_metrics"
elif [[ "$FINAL" == "COMPLETED" ]]; then
  fail "mute job ran its full ${MUTE_SECONDS}s emitting nothing and was never evicted — it held an accelerator and produced nothing rankable"
else
  fail "mute job ended as '$FINAL', expected EVICTED for never_reported_metrics"
fi

echo "  -- a job that does report survives the same window --"
# The control: same platform experiment, same declared metric, same silence window — the only
# difference is that this one reports. If this is evicted too, the check is not measuring
# reporting, it is just killing jobs.
HEALTHY_HOURS=$(py "print(round(90 / 3600.0, 6))")
HEALTHY_JOB=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "$HEALTHY_HOURS" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"90\", \"HYPOTHESISLOOP_REPORT_INTERVAL_SECONDS\": \"${REPORT_INTERVAL}\"}")
echo "  ==> $HEALTHY_JOB submitted: same contract, actually reports"

HS=$(wait_for_status "$HEALTHY_JOB" "RUNNING" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$HS" == "RUNNING" ]]; then
  # Hold it across more than the grace that condemned the mute job. Polling the whole time rather
  # than sleeping once catches an eviction that happens and is then superseded.
  assert_stable_status "$HEALTHY_JOB" "RUNNING,COMPLETED" 45 "reporting job survives the window that evicted the mute one"
  [[ "$(get_field "$HEALTHY_JOB" eviction_reason)" == "" ]] \
    && pass "reporting job carries no eviction reason" \
    || fail "reporting job was evicted for '$(get_field "$HEALTHY_JOB" eviction_reason)' despite reporting normally"
else
  fail "control job never reached RUNNING (status=$HS) — cannot prove the check discriminates"
fi

cancel_job "$HEALTHY_JOB" || true
wait_for_status "$HEALTHY_JOB" "COMPLETED,FAILED,EVICTED,REJECTED" 30 > /dev/null || true
close_platform_experiment "$PE_ID"
finish
