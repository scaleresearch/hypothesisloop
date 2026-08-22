#!/usr/bin/env bash
# The summary gate must be answerable, not just enforceable.
#
# An agent may not submit into a platform experiment while it still has a finished job there with
# no write-up filed. That rule is enforced at admission, but a rule an agent cannot inspect is a
# dead end: told only "you are blocked", it has to cross-reference every completed job against
# every hypothesis's findings to work out which one to write up.
#
# So the same set the gate reads is listable: GET /experiments?needs_summary=true. This checks the
# two halves agree — the job that blocks submission is exactly the job the filter returns, and
# clearing it clears both.
#
# API-only, one accelerator, parallel-safe: own agent and platform experiment.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-summary-gate-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "summary-gate-${RUN_ID}" "$(scale_budget 5.0)" 1)
signup_and_start "$PE_ID" "$AGENT"

needs_summary_ids() {
  curl -sf "$API_URL/experiments?needs_summary=true&agent=${AGENT}&platform_experiment_id=${PE_ID}&limit=50" \
    | py "import sys,json; print(' '.join(e['id'] for e in json.load(sys.stdin)))"
}

echo "  -- nothing is owed before anything has finished --"
[[ -z "$(needs_summary_ids)" ]] \
  && pass "needs_summary is empty for an agent with no finished jobs" \
  || fail "needs_summary listed $(needs_summary_ids) before any job finished"

JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.01")
echo "  ==> $JOB submitted, waiting for it to finish"
FINAL=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" \
  "$(( ADMISSION_BUDGET_SECONDS + $(completion_wait_tries 0.01) ))" || true)
if [[ "$FINAL" != "COMPLETED" ]]; then
  fail "job ended as '$FINAL', expected COMPLETED — the gate only applies to completed work"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "job completed, so its write-up is now owed"

echo "  -- the blocking job is the one the filter names --"
# The gate reads its answer from the same predicate the filter does, so a completed job with no
# finding must appear here the moment it completes.
listed() { [[ " $(needs_summary_ids) " == *" $JOB "* ]]; }
wait_until "the completed job appears in needs_summary" 20 1 listed || true
listed \
  && pass "needs_summary names the job whose write-up is owed ($JOB)" \
  || fail "needs_summary returned '$(needs_summary_ids)', which does not include $JOB — an agent cannot discover what is blocking it"

read -r BLOCKED_CODE _ <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.01")"
[[ "$BLOCKED_CODE" == "403" ]] \
  && pass "a further submission is refused while the write-up is owed (HTTP 403)" \
  || fail "second submission returned HTTP $BLOCKED_CODE, expected 403 — the gate did not engage, so this scenario proves nothing"

echo "  -- filing the write-up clears both the list and the gate --"
file_finding "$JOB"
cleared() { [[ " $(needs_summary_ids) " != *" $JOB "* ]]; }
wait_until "the job leaves needs_summary" 20 1 cleared || true
cleared \
  && pass "the job left needs_summary once its write-up was filed" \
  || fail "needs_summary still lists $JOB after its summary was filed — the list and the gate disagree"

read -r AFTER_CODE _ <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.01")"
[[ "$AFTER_CODE" -lt 400 ]] \
  && pass "submission is accepted again once nothing is owed — clearing the list cleared the gate" \
  || fail "submission still refused with HTTP $AFTER_CODE after the write-up was filed"

close_platform_experiment "$PE_ID"
finish
