#!/usr/bin/env bash
# One experiment id, one experiment — even when the same id arrives N times at the same instant.
#
# Submission decides inside its admission transaction whether an id is new, already QUEUED, or
# taken. Two outcomes are both correct and must be told apart:
#
#   - the SAME agent re-submitting its own queued job is an idempotent retry. An agent that never
#     saw the response to its first POST must be able to send it again and be told "queued", not
#     handed an error for work it legitimately owns. Every one of those returns 2xx and exactly
#     one row exists.
#   - a DIFFERENT agent naming that id is a collision, and must be refused (409) without touching
#     the owner's job. Otherwise an agent could refresh the priority of work it does not own just
#     by guessing an id.
#
# What must never happen either way is a 5xx: that was the original defect, where concurrent
# submissions all passed a pre-transaction check and the loser hit the primary key.
#
# API-only, no accelerator required (the jobs need never be admitted), parallel-safe: own agent
# and platform experiment, own run-scoped id.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-dup-race-${RUN_ID}"
# The second agent exists from the start: signup is only open before the platform experiment
# starts, and the collision half below needs it to be a legitimate participant — so that what it
# is refused is the id, not its right to submit at all.
AGENT_B="agent-dup-other-${RUN_ID}"
register_agent "$AGENT"
register_agent "$AGENT_B"
PE_ID=$(create_platform_experiment "dup-race-${RUN_ID}" "$(scale_budget 5.0)" 2)
signup_and_start "$PE_ID" "$AGENT" "$AGENT_B"

N=4
JOB_ID="job-dup-${RUN_ID}"
OUT_DIR="$TMPDIR_T/dup"
mkdir -p "$OUT_DIR"

# Built once and reused: the race under test is N POSTs of one identical request, not N
# racing body constructions (each of which would register its own hypothesis first).
BODY=$(mk_submit_body_for_id "$JOB_ID" "$PE_ID" "$AGENT" "guaranteed") \
  || { fail "could not build the submission body"; finish; }

echo "  ==> firing ${N} submissions of the same id (${JOB_ID}) at the same instant..."
PIDS=()
for i in $(seq 1 "$N"); do
  ( post_experiment_body "$BODY" > "$OUT_DIR/code_$i" ) &
  PIDS+=("$!")
done
for p in "${PIDS[@]}"; do wait "$p" || true; done

ACCEPTED=0
OTHER=""
for i in $(seq 1 "$N"); do
  CODE=$(cat "$OUT_DIR/code_$i" 2>/dev/null || echo "")
  if [[ "$CODE" =~ ^2 ]]; then
    ACCEPTED=$((ACCEPTED + 1))
  else
    OTHER="${OTHER} ${CODE}"
  fi
done
echo "  accepted=${ACCEPTED} other=${OTHER:-none}"

# No 5xx, and no 409 either: every one of these is the owning agent retrying its own submission.
[[ "$ACCEPTED" == "$N" ]] \
  && pass "all ${N} concurrent retries by the owning agent were answered as accepted — the retry is idempotent" \
  || fail "only ${ACCEPTED} of ${N} concurrent retries succeeded, the rest returned${OTHER} — an agent re-sending a request it never saw the answer to must not be handed an error for its own queued job"

STATUS=$(get_status "$JOB_ID" || true)
[[ "$STATUS" == "QUEUED" || "$STATUS" == "SUBMITTED" || "$STATUS" == "RUNNING" ]] \
  && pass "exactly one experiment exists under that id and it is live (status=$STATUS)" \
  || fail "experiment ${JOB_ID} is '$STATUS' after ${N} concurrent submissions — expected one live row"

# The id is the primary key, so a second row is impossible by construction; what this checks is
# that the winner is a complete row and not a half-written one from a rolled-back transaction.
COST=$(get_field "$JOB_ID" estimated_cost_acch)
[[ -n "$COST" ]] \
  && pass "the surviving row is fully written (estimated_cost_acch=$COST)" \
  || fail "experiment ${JOB_ID} has no estimated_cost_acch — the surviving row is incomplete"

echo "  -- a second agent cannot claim, or disturb, a job id it does not own --"
# The id check is scoped to the submitter. Without that, an agent could name any queued job's id
# and have its submission treated as a legal re-submission of that job -- refreshing the priority
# of work it does not own. The admission transaction holds the lock for the *submitter's*
# (agent, platform experiment) and so can say nothing about another owner's row.
BODY_B=$(mk_submit_body_for_id "$JOB_ID" "$PE_ID" "$AGENT_B" "guaranteed") \
  || { fail "could not build the second agent's submission body"; finish; }
CODE_B=$(post_experiment_body "$BODY_B")
[[ "$CODE_B" == "409" ]] \
  && pass "a different agent submitting the same id was refused as a duplicate (409)" \
  || fail "a different agent submitting the same id got HTTP $CODE_B, expected 409 — an id must not be claimable by whoever guesses it"

OWNER=$(get_field "$JOB_ID" agent_id)
[[ "$OWNER" == "$AGENT" ]] \
  && pass "the job still belongs to its original agent" \
  || fail "job ${JOB_ID} is now owned by '$OWNER', expected '$AGENT'"

close_platform_experiment "$PE_ID"
finish
