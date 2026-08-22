#!/usr/bin/env bash
# One experiment id, one experiment — even when the same id arrives N times at the same instant.
#
# Submission checks for an existing row before it inserts one, so concurrent submissions of one id
# all find nothing and all proceed; the primary key is what actually decides. The loser must come
# back as a duplicate (409), not as a server error: an agent retrying a request whose response it
# never saw has to be able to tell "you already have this" from "try again".
#
# API-only, no accelerator required (the jobs need never be admitted), parallel-safe: own agent and
# platform experiment, own run-scoped id.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-dup-race-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "dup-race-${RUN_ID}" "$(scale_budget 5.0)" 1)
signup_and_start "$PE_ID" "$AGENT"

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
CONFLICT=0
OTHER=""
for i in $(seq 1 "$N"); do
  CODE=$(cat "$OUT_DIR/code_$i" 2>/dev/null || echo "")
  if [[ "$CODE" =~ ^2 ]]; then
    ACCEPTED=$((ACCEPTED + 1))
  elif [[ "$CODE" == "409" ]]; then
    CONFLICT=$((CONFLICT + 1))
  else
    OTHER="${OTHER} ${CODE}"
  fi
done
echo "  accepted=${ACCEPTED} conflict=${CONFLICT} other=${OTHER:-none}"

[[ "$ACCEPTED" == "1" ]] \
  && pass "exactly one of ${N} concurrent submissions of the same id was accepted" \
  || fail "${ACCEPTED} of ${N} concurrent submissions of the same id were accepted, expected exactly 1"

[[ -z "$OTHER" ]] \
  && pass "every loser was refused as a duplicate (409), not as a server error" \
  || fail "losing submissions returned${OTHER} — a duplicate id must be a 409, so a retrying agent can tell it apart from a transient failure"

# The row that survived is a real, readable experiment, not a half-written one from the loser's
# rolled-back transaction.
STATUS=$(get_status "$JOB_ID" || true)
[[ -n "$STATUS" ]] \
  && pass "the surviving experiment is readable (status=$STATUS)" \
  || fail "no experiment exists under ${JOB_ID} after a submission was accepted"

close_platform_experiment "$PE_ID"
finish
