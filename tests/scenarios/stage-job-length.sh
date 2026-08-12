#!/usr/bin/env bash
# A stage's max_job_hours, end to end: an over-long submission is rejected before it costs
# anything, and a job that understates its duration to get in is evicted job_too_long once its
# observed runtime passes the cap.
#
# The second half is the one that matters: estimated_duration_hours is a claim, and the cap is
# only real if the control plane measures what actually ran.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-joblen-${RUN_ID}"
register_agent "$AGENT"

# One stage, no cuts — this scenario is about the length cap alone, so nothing else can stop a
# job. 30s cap; the budget is far larger than anything below can burn, so quota exhaustion never
# races the cap for the eviction.
CAP_HOURS=0.0084
STAGES="[{\"length_pct\":100,\"evict_pct\":0,\"max_job_hours\":${CAP_HOURS}}]"
PE_ID=$(create_platform_experiment "stage-joblen-${RUN_ID}" 5 2 5 0 "" "$STAGES")
signup_and_start "$PE_ID" "$AGENT"

# The cap must round-trip and be published to agents — they can only plan around a limit they
# can read.
PUBLISHED=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/stages" \
  | py "import sys,json; print(float(json.load(sys.stdin)['stages'][0]['max_job_hours']))")
py "import sys; sys.exit(0 if $PUBLISHED == $CAP_HOURS else 1)" \
  && pass "max_job_hours=${CAP_HOURS} is published on the stages endpoint" \
  || fail "stages endpoint reports max_job_hours=${PUBLISHED}, want ${CAP_HOURS}"

# 1. Submit gate: an honest over-long estimate is rejected outright, before any quota is debited.
read -r CODE _ <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "1.0")"
[[ "$CODE" -ge 400 ]] \
  && pass "a job estimating 1.0h is rejected (HTTP $CODE) under a ${CAP_HOURS}h cap" \
  || fail "a 1.0h job was accepted (HTTP $CODE) despite the ${CAP_HOURS}h cap"

# 2. Runtime enforcement: estimate under the cap, then run well past it. This is the job the
# submit gate cannot catch.
OVERRUN_SECONDS=150
JOB=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" 0.005 "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\":\"${OVERRUN_SECONDS}\"}")
echo "  submitted $JOB: estimates 0.005h, actually runs ${OVERRUN_SECONDS}s (cap ${CAP_HOURS}h = $(py "print(int($CAP_HOURS*3600))")s)"

STATUS=$(wait_for_status "$JOB" "RUNNING" "$ADMISSION_BUDGET_SECONDS")
[[ "$STATUS" == "RUNNING" ]] \
  && pass "under-estimating job was admitted and started" \
  || fail "job never reached RUNNING (status=$STATUS)"

# The cap is measured against observed elapsed hours, so eviction lands a reconcile tick or two
# after the cap itself — never before it, which is the half that would be a bug.
# Budget: the cap itself plus a generous margin for reconcile ticks and the metrics-store lag —
# far short of OVERRUN_SECONDS, so a job that is never evicted fails here instead of completing.
STATUS=$(wait_for_status "$JOB" "EVICTED,COMPLETED,FAILED" 100)
REASON=$(get_field "$JOB" eviction_reason)
echo "  final: status=$STATUS eviction_reason=${REASON:-n/a}"
[[ "$STATUS" == "EVICTED" && "$REASON" == "job_too_long" ]] \
  && pass "job outrunning the stage cap was evicted job_too_long" \
  || fail "job ended $STATUS/${REASON:-n/a}, want EVICTED/job_too_long"

# Eviction settles like any other terminal path — the agent is billed for what genuinely ran,
# not for the estimate it submitted.
USED=$(quota_used_guaranteed "$PE_ID" "$AGENT")
py "import sys; sys.exit(0 if $USED > 0 else 1)" \
  && pass "the evicted job settled against observed usage (${USED} AccH)" \
  || fail "evicted job settled to ${USED} AccH — nothing was billed for a job that ran"

close_platform_experiment "$PE_ID"
finish
