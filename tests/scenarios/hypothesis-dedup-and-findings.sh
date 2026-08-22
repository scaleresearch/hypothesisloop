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
#   3. the pool is shared with humans: an idea posted from the UI under a typed name lands in the
#      same listing agents read, dedups against agent rows under the same unique index, and owns
#      nothing — no job, no quota, no standings, no status an agent can set.
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
FIRST=$(curl -sf -X POST "$API_URL/hypotheses" -H 'Content-Type: application/json' \
  -d "{\"agent_id\": \"$AGENT_A\", \"platform_experiment_id\": \"$PE_ID\", \"text\": \"$TEXT\"}")
FIRST_ID=$(echo "$FIRST" | py "import sys,json; print(json.load(sys.stdin)['id'])")
FIRST_ALREADY=$(echo "$FIRST" | py "import sys,json; print(json.load(sys.stdin).get('already_existed'))")
[[ "$FIRST_ALREADY" == "False" ]] \
  && pass "first registration of new hypothesis text: already_existed=False" \
  || fail "first registration should be already_existed=False, got $FIRST_ALREADY"

SECOND=$(curl -sf -X POST "$API_URL/hypotheses" -H 'Content-Type: application/json' \
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
  HYP_VIEW=$(curl -sf "$API_URL/hypotheses/${FIRST_ID}")
  FOUND=$(echo "$HYP_VIEW" | py "
import sys, json
d = json.load(sys.stdin)
findings = d.get('findings') or []
print(any('$SUMMARY_TEXT' in (f.get('summary') or '') for f in findings))
")
  [[ "$FOUND" == "True" ]] \
    && pass "finding filed by $AGENT_A is visible via GET /hypotheses/{id} (readable by any agent, e.g. $AGENT_B)" \
    || fail "finding filed by $AGENT_A was not present in the hypothesis's shared findings list"
else
  fail "job never reached COMPLETED (status=$S) — cannot exercise cross-agent findings visibility"
fi

echo "  -- a human-submitted idea joins the same pool, under the same dedup, owning nothing --"
# Costs no hardware: every assertion below is API-only, and the one job submission here is one the
# platform must REFUSE, so it debits nothing. The platform experiment's budget is unchanged.
HUMAN_NAME="dana-ops-${RUN_ID}"
HUMAN_TEXT="Warmup for the first 500 steps stabilizes task_success_rate at batch size 512"
HUMAN_RESP=$(post_hypothesis "" "$HUMAN_NAME" "$PE_ID" "$HUMAN_TEXT")
HUMAN_ID=$(echo "$HUMAN_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")

# The pool an agent reads is GET /hypotheses for the platform experiment — the human row must be
# in that same listing, not in a parallel one, and must be self-describing there (source+author).
POOL_ROW=$(curl -sf "$API_URL/hypotheses?platform_experiment_id=${PE_ID}&limit=200" | py "
import sys, json
for h in json.load(sys.stdin):
    if h['id'] == '$HUMAN_ID':
        print(h.get('source'), h.get('author'), h.get('agent_id') or '-')
")
[[ "$POOL_ROW" == "human $HUMAN_NAME -" ]] \
  && pass "human row is in the same GET /hypotheses pool an agent reads, as source=human author=$HUMAN_NAME with no owning agent" \
  || fail "human row wrong or absent in the shared pool listing: expected 'human $HUMAN_NAME -', got '$POOL_ROW'"

# Dedup is one rule for the whole pool, not one per source: an AGENT registering the human's text
# must get the human's row back, exactly as agent-to-agent collides above.
AGENT_ON_HUMAN=$(post_hypothesis "$AGENT_B" "" "$PE_ID" "$HUMAN_TEXT")
AGENT_ON_HUMAN_ID=$(echo "$AGENT_ON_HUMAN" | py "import sys,json; print(json.load(sys.stdin)['id'])")
AGENT_ON_HUMAN_ALREADY=$(echo "$AGENT_ON_HUMAN" | py "import sys,json; print(json.load(sys.stdin).get('already_existed'))")
[[ "$AGENT_ON_HUMAN_ID" == "$HUMAN_ID" && "$AGENT_ON_HUMAN_ALREADY" == "True" ]] \
  && pass "an agent registering the human's text got the human's existing row back (same id, already_existed=True)" \
  || fail "agent registration of the human's text did not dedup onto it (id=$AGENT_ON_HUMAN_ID vs $HUMAN_ID, already_existed=$AGENT_ON_HUMAN_ALREADY)"

# Exactly one of agent_id/author, validated once at registration — fail fast, never defaulted into
# a row a later read has to interpret.
BOTH_CODE=$(post_hypothesis_code "$AGENT_A" "$HUMAN_NAME" "$PE_ID" "both-set probe ${RUN_ID}")
[[ "$BOTH_CODE" == "400" ]] \
  && pass "registering with BOTH agent_id and author is refused (HTTP 400)" \
  || fail "registering with both agent_id and author returned HTTP $BOTH_CODE, expected 400"
NEITHER_CODE=$(post_hypothesis_code "" "" "$PE_ID" "neither-set probe ${RUN_ID}")
[[ "$NEITHER_CODE" == "400" ]] \
  && pass "registering with NEITHER agent_id nor author is refused (HTTP 400)" \
  || fail "registering with neither agent_id nor author returned HTTP $NEITHER_CODE, expected 400"

# A human row is in the pool to be read, not to be run: it has no owner, so no quota to spend.
read -r HUMAN_JOB_CODE HUMAN_JOB_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT_B" "guaranteed" "0.01" "" "$JOB_FILE" "$HUMAN_ID")"
[[ "$HUMAN_JOB_CODE" == "400" ]] \
  && pass "a job submitted against a human-owned hypothesis is refused at admission (HTTP 400)" \
  || fail "job against a human-owned hypothesis returned HTTP $HUMAN_JOB_CODE, expected 400"
HUMAN_JOB_LOOKUP=$(experiment_http_code "$HUMAN_JOB_ID")
[[ "$HUMAN_JOB_LOOKUP" == "404" ]] \
  && pass "the refused job was never created (GET /experiments/$HUMAN_JOB_ID is 404)" \
  || fail "a job refused for naming a human hypothesis still exists (GET /experiments/$HUMAN_JOB_ID is $HUMAN_JOB_LOOKUP)"

# No quota, no standings: the typed name is attribution in the pool and nothing else. The holder
# count is the two signed-up agents, unchanged by the human row existing.
QUOTA_HOLDERS=$(curl -sf "$API_URL/platform-experiments/${PE_ID}/quotas" | py "
import sys, json
qs = json.load(sys.stdin)
print(len(qs), '$HUMAN_NAME' in [q.get('agent_id') for q in qs])
")
[[ "$QUOTA_HOLDERS" == "2 False" ]] \
  && pass "quota holders are still exactly the 2 signed-up agents, and $HUMAN_NAME holds none" \
  || fail "quota holders changed or include the human author: expected '2 False', got '$QUOTA_HOLDERS'"
STANDINGS_HIT=$(curl -sf "$API_URL/platform-experiments/${PE_ID}/results" | py "
import sys, json
print('$HUMAN_NAME' in json.dumps(json.load(sys.stdin)))
")
[[ "$STANDINGS_HIT" == "False" ]] \
  && pass "$HUMAN_NAME appears nowhere in the platform experiment's results/standings" \
  || fail "the human author's name leaked into the results/standings"

# A human may also comment on an agent's hypothesis — same origin rule, attributed the same way.
HUMAN_COMMENT="Worth re-running this at the shorter warmup before concluding — $HUMAN_NAME"
COMMENT_CODE=$(post_hypothesis_comment_code "$FIRST_ID" "" "$HUMAN_NAME" "$HUMAN_COMMENT")
[[ "$COMMENT_CODE" == "201" ]] \
  && pass "a human comment on an agent's hypothesis is accepted (HTTP 201)" \
  || fail "human comment on an agent's hypothesis returned HTTP $COMMENT_CODE, expected 201"
COMMENT_ROW=$(curl -sf "$API_URL/hypotheses/${FIRST_ID}" | py "
import sys, json
for c in json.load(sys.stdin).get('comments') or []:
    if c.get('text') == '$HUMAN_COMMENT':
        print(c.get('source'), c.get('author'))
")
[[ "$COMMENT_ROW" == "human $HUMAN_NAME" ]] \
  && pass "the human comment is readable on the hypothesis carrying source=human author=$HUMAN_NAME" \
  || fail "human comment missing or mis-attributed on GET /hypotheses/{id}: expected 'human $HUMAN_NAME', got '$COMMENT_ROW'"

# Status is the owner's verdict, and a human row names no owner — so no agent can set it.
STATUS_CODE=$(set_hypothesis_status_code "$HUMAN_ID" "$AGENT_A" "confirmed")
[[ "$STATUS_CODE" == "403" ]] \
  && pass "an agent cannot set the status of a human-owned hypothesis (HTTP 403)" \
  || fail "setting the status of a human-owned hypothesis as $AGENT_A returned HTTP $STATUS_CODE, expected 403"
HUMAN_STATUS=$(curl -sf "$API_URL/hypotheses/${HUMAN_ID}" | py "import sys,json; print(json.load(sys.stdin)['status'])")
[[ "$HUMAN_STATUS" == "open" ]] \
  && pass "the human row's status is still open after the refused write" \
  || fail "the human row's status changed to $HUMAN_STATUS despite the write being refused"

close_platform_experiment "$PE_ID"
finish
