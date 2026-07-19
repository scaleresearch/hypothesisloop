#!/usr/bin/env bash
# Stress + integrity coverage for the accounting-safety fixes (SYSTEM_EVALUATION_MERGED P0s):
#   - P0-e: a donation transfers exactly once even under a burst of concurrent fulfillments of
#           the SAME donation — the atomic, status-gated FulfillDonationTx must not double-debit
#           the donor no matter how many callers race.
#   - P0-f: malformed / out-of-range / unknown-target metric samples are rejected at ingestion
#           (4xx), never written to the metrics store where they'd poison rankings.
#   - P0-d: a job may not reference a hypothesis registered under a different platform experiment
#           (cross-program scope contamination).
# API-only, parallel-safe (its own agents/platform experiments).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

# ---------------------------------------------------------------------------
echo "  -- P0-e: concurrent fulfillment of one donation transfers exactly once --"
REQUESTER="agent-stress-req-${RUN_ID}"
DONOR="agent-stress-donor-${RUN_ID}"
register_agent "$REQUESTER"; register_agent "$DONOR"
PE_ID=$(create_platform_experiment "stress-integrity-${RUN_ID}" 4.0 2)
signup_and_start "$PE_ID" "$REQUESTER" "$DONOR"

CREDITS_WANT="0.05"
DONOR_BEFORE=$(_quota_field "$PE_ID" "$DONOR" guaranteed_t4_hours)
REQ_BEFORE=$(_quota_field "$PE_ID" "$REQUESTER" guaranteed_t4_hours)

DONATION_ID=$(curl -sf -X POST "$QUOTA_URL/donations" -H 'Content-Type: application/json' -d "{
  \"agent_id\": \"$REQUESTER\", \"platform_experiment_id\": \"$PE_ID\",
  \"credits_want\": $CREDITS_WANT, \"reason\": \"concurrent-fulfill stress\"
}" | py "import sys,json; print(json.load(sys.stdin)['id'])")

# Fire 12 fulfillments of the SAME donation at once; each writes its HTTP code to a file.
CODES_DIR=$(mktemp -d)
for i in $(seq 1 12); do
  ( curl -s -o /dev/null -w '%{http_code}' -X POST "$QUOTA_URL/donations/${DONATION_ID}/fulfill" \
      -H 'Content-Type: application/json' -d "{\"donor_agent_id\": \"$DONOR\"}" > "$CODES_DIR/$i" ) &
done
wait
SUCCESSES=$(grep -lE '^2[0-9][0-9]$' "$CODES_DIR"/* 2>/dev/null | wc -l | tr -d ' ')
rm -rf "$CODES_DIR"
[[ "$SUCCESSES" == "1" ]] \
  && pass "exactly one of 12 concurrent fulfillments succeeded (got $SUCCESSES)" \
  || fail "expected exactly 1 concurrent fulfillment to succeed, got $SUCCESSES — atomic gate leaked"

DONOR_AFTER=$(_quota_field "$PE_ID" "$DONOR" guaranteed_t4_hours)
REQ_AFTER=$(_quota_field "$PE_ID" "$REQUESTER" guaranteed_t4_hours)
DONOR_DELTA=$(py "print(round(float('$DONOR_AFTER' or 0) - float('$DONOR_BEFORE' or 0), 6))")
REQ_DELTA=$(py "print(round(float('$REQ_AFTER' or 0) - float('$REQ_BEFORE' or 0), 6))")
[[ "$DONOR_DELTA" == "-$CREDITS_WANT" ]] \
  && pass "donor debited exactly once under concurrency (delta=$DONOR_DELTA)" \
  || fail "donor debited $DONOR_DELTA, expected -$CREDITS_WANT — double transfer under concurrency"
[[ "$REQ_DELTA" == "$CREDITS_WANT" ]] \
  && pass "requester credited exactly once under concurrency (delta=$REQ_DELTA)" \
  || fail "requester credited $REQ_DELTA, expected +$CREDITS_WANT — conservation broken"

# ---------------------------------------------------------------------------
echo "  -- P0-f: malformed / unknown-target metric samples are rejected at ingestion --"
metric_code() { # experiment_id fraction value
  curl -s -o /dev/null -w '%{http_code}' -X POST "$REGISTRY_URL/registry/experiments/$1/metrics" \
    -H 'Content-Type: application/json' \
    -d "{\"metric_name\":\"val_accuracy\",\"fraction_complete\":$2,\"metric_value\":$3}"
}
UNKNOWN_ID="job-does-not-exist-${RUN_ID}"
C_HIGH=$(metric_code "$UNKNOWN_ID" 1.5 0.5)
[[ "$C_HIGH" -ge 400 && "$C_HIGH" -lt 500 ]] \
  && pass "fraction_complete=1.5 rejected (HTTP $C_HIGH)" \
  || fail "fraction_complete>1 should be a 4xx rejection, got HTTP $C_HIGH"
C_NEG=$(metric_code "$UNKNOWN_ID" -0.1 0.5)
[[ "$C_NEG" -ge 400 && "$C_NEG" -lt 500 ]] \
  && pass "fraction_complete=-0.1 rejected (HTTP $C_NEG)" \
  || fail "negative fraction_complete should be a 4xx rejection, got HTTP $C_NEG"
C_UNKNOWN=$(metric_code "$UNKNOWN_ID" 0.5 0.5)
[[ "$C_UNKNOWN" -ge 400 && "$C_UNKNOWN" -lt 500 ]] \
  && pass "metric for unknown experiment rejected (HTTP $C_UNKNOWN)" \
  || fail "metric for a nonexistent experiment should be a 4xx rejection, got HTTP $C_UNKNOWN"

# ---------------------------------------------------------------------------
echo "  -- P0-d: a job cannot reference a hypothesis from a different platform experiment --"
PE_OTHER=$(create_platform_experiment "stress-integrity-other-${RUN_ID}" 4.0 2)
signup_and_start "$PE_OTHER" "$DONOR"
# Register a hypothesis scoped to PE_OTHER, then try to submit a job under PE_ID referencing it.
FOREIGN_HYP=$(register_hypothesis "$DONOR" "$PE_OTHER")
# DONOR is already signed up to PE_ID (above), so a submit under PE_ID passes the signup gate and
# fails specifically on the cross-PE hypothesis scope check — which is what we want to assert.
JOB_ID="job-crossscope-${RUN_ID}"
BODY=$(python3 "$LIB_DIR/mk_body.py" submit "$JOB_ID" "$DONOR" "$PE_ID" "0.02" "$JOB_FILE" "$FOREIGN_HYP" "guaranteed" "" "" "" "" "" "" "")
XSCOPE_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$SCHED_URL/experiments" \
  -H 'Content-Type: application/json' -d "$BODY")
[[ "$XSCOPE_CODE" -ge 400 && "$XSCOPE_CODE" -lt 500 ]] \
  && pass "submit referencing a foreign-PE hypothesis rejected (HTTP $XSCOPE_CODE)" \
  || fail "cross-PE hypothesis reference should be a 4xx rejection, got HTTP $XSCOPE_CODE"

close_platform_experiment "$PE_ID"
close_platform_experiment "$PE_OTHER"
finish
