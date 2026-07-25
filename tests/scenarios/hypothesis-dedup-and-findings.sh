#!/usr/bin/env bash
# Two core "shared evidence pool" properties from the architecture (README: "agents register
# (or retrieve, if equivalent text already exists in the same platform experiment) a
# hypothesis"; "Agents can read every hypothesis registered in a platform experiment, the jobs
# that tested each one, and the accumulated findings") with no prior coverage:
#   1. registering equivalent hypothesis text twice returns the SAME row (already_existed=true,
#      identical id), not a duplicate — the shared idea pool must actually be deduplicated, not
#      just conceptually described.
#   2. a finding filed by one agent against a hypothesis is visible to a DIFFERENT agent that
#      reads the same hypothesis afterward — the cross-agent evidence trail is a real read
#      path, not just a same-agent bookkeeping detail.
# API-only, parallel-safe (its own platform experiment).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT_A="agent-dedup-a-${RUN_ID}"
AGENT_B="agent-dedup-b-${RUN_ID}"
register_agent "$AGENT_A"
register_agent "$AGENT_B"
PE_ID=$(create_platform_experiment "hypothesis-dedup-${RUN_ID}" 1.0 2)
signup_and_start "$PE_ID" "$AGENT_A" "$AGENT_B"

echo "  -- registering equivalent hypothesis text twice returns the same row, not a duplicate --"
TEXT="Higher learning rate converges faster to a better task_success_rate"
FIRST=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "{\"agent_id\": \"$AGENT_A\", \"platform_experiment_id\": \"$PE_ID\", \"text\": \"$TEXT\"}")
FIRST_ID=$(echo "$FIRST" | py "import sys,json; print(json.load(sys.stdin)['id'])")
FIRST_ALREADY=$(echo "$FIRST" | py "import sys,json; print(json.load(sys.stdin).get('already_existed'))")
[[ "$FIRST_ALREADY" == "False" ]] \
  && pass "first registration of new hypothesis text: already_existed=False" \
  || fail "first registration should be already_existed=False, got $FIRST_ALREADY"

SECOND=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "{\"agent_id\": \"$AGENT_B\", \"platform_experiment_id\": \"$PE_ID\", \"text\": \"$TEXT\"}")
SECOND_ID=$(echo "$SECOND" | py "import sys,json; print(json.load(sys.stdin)['id'])")
SECOND_ALREADY=$(echo "$SECOND" | py "import sys,json; print(json.load(sys.stdin).get('already_existed'))")
[[ "$SECOND_ID" == "$FIRST_ID" ]] \
  && pass "re-registering identical text (from a different agent) returned the same hypothesis id" \
  || fail "re-registering identical text returned a different id ($FIRST_ID != $SECOND_ID) — idea pool not deduplicated"
[[ "$SECOND_ALREADY" == "True" ]] \
  && pass "second registration correctly reports already_existed=True" \
  || fail "second registration should be already_existed=True, got $SECOND_ALREADY"

echo "  -- a finding filed by one agent is visible to a different agent reading the same hypothesis --"
# hyp_text=$TEXT (13th positional arg) so this job tests the SAME deduplicated hypothesis
# (FIRST_ID) rather than submit_job_ext's own default-generated text creating a new one.
JOB=$(submit_job_ext "$PE_ID" "$AGENT_A" "guaranteed" "0.02" "$JOB_FILE" "" "" "" "" "" "" "" "$TEXT")
S=$(wait_for_completion_after_running "$JOB" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" == "COMPLETED" ]]; then
  SUMMARY_TEXT="Achieved 0.81 val_accuracy — e2e cross-agent findings-visibility coverage"
  file_finding "$JOB" "$SUMMARY_TEXT"
  HYP_VIEW=$(curl -sf "$REGISTRY_URL/registry/hypotheses/${FIRST_ID}")
  FOUND=$(echo "$HYP_VIEW" | py "
import sys, json
d = json.load(sys.stdin)
findings = d.get('findings') or []
print(any('$SUMMARY_TEXT' in (f.get('summary') or '') for f in findings))
")
  [[ "$FOUND" == "True" ]] \
    && pass "finding filed by $AGENT_A is visible via GET /registry/hypotheses/{id} (readable by any agent, e.g. $AGENT_B)" \
    || fail "finding filed by $AGENT_A was not present in the hypothesis's shared findings list"
else
  fail "job never reached COMPLETED (status=$S) — cannot exercise cross-agent findings visibility"
fi

close_platform_experiment "$PE_ID"
finish
