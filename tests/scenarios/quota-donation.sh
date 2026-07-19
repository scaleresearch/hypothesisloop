#!/usr/bin/env bash
# Compute-donation flow (README: "agents can donate unused quota to each other") — a core
# cross-agent feature with no prior coverage. Verifies:
#   1. fulfilling a donation actually moves guaranteed AccH from donor to requester (not just
#      flips a status flag) — donor debited, requester credited, by exactly credits_want.
#   2. a cancelled request cannot later be fulfilled (status is terminal, not just cosmetic).
#   3. a fulfilled request cannot be fulfilled again (double-fulfillment must not double-credit).
# API-only, parallel-safe (its own platform experiment).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

REQUESTER="agent-donation-requester-${RUN_ID}"
DONOR="agent-donation-donor-${RUN_ID}"
CANCELLER="agent-donation-canceller-${RUN_ID}"
for a in "$REQUESTER" "$DONOR" "$CANCELLER"; do register_agent "$a"; done
PE_ID=$(create_platform_experiment "quota-donation-${RUN_ID}" 4.0 3)
signup_and_start "$PE_ID" "$REQUESTER" "$DONOR" "$CANCELLER"

echo "  -- fulfilling a donation moves credits from donor to requester --"
CREDITS_WANT="0.05"
REQ_BEFORE=$(quota_used_guaranteed "$PE_ID" "$REQUESTER")  # baseline read, not usage
DONOR_GUARANTEED_BEFORE=$(_quota_field "$PE_ID" "$DONOR" guaranteed_accelerator_hours)
REQUESTER_GUARANTEED_BEFORE=$(_quota_field "$PE_ID" "$REQUESTER" guaranteed_accelerator_hours)

DONATION_ID=$(curl -sf -X POST "$QUOTA_URL/donations" -H 'Content-Type: application/json' -d "{
  \"agent_id\": \"$REQUESTER\", \"platform_experiment_id\": \"$PE_ID\",
  \"credits_want\": $CREDITS_WANT, \"reason\": \"e2e donation flow coverage\"
}" | py "import sys,json; print(json.load(sys.stdin)['id'])")
[[ -n "$DONATION_ID" ]] && pass "donation request created (id=$DONATION_ID)" || fail "donation request creation returned no id"

FULFILL_CODE=$(curl -s -o /tmp/donation_fulfill.$$.json -w '%{http_code}' -X POST "$QUOTA_URL/donations/${DONATION_ID}/fulfill" \
  -H 'Content-Type: application/json' -d "{\"donor_agent_id\": \"$DONOR\"}")
rm -f /tmp/donation_fulfill.$$.json
[[ "$FULFILL_CODE" -lt 300 ]] && pass "donation fulfilled (HTTP $FULFILL_CODE)" || fail "donation fulfillment failed (HTTP $FULFILL_CODE)"

DONOR_GUARANTEED_AFTER=$(_quota_field "$PE_ID" "$DONOR" guaranteed_accelerator_hours)
REQUESTER_GUARANTEED_AFTER=$(_quota_field "$PE_ID" "$REQUESTER" guaranteed_accelerator_hours)
DONOR_DELTA=$(py "print(round(float('$DONOR_GUARANTEED_AFTER' or 0) - float('$DONOR_GUARANTEED_BEFORE' or 0), 6))")
REQUESTER_DELTA=$(py "print(round(float('$REQUESTER_GUARANTEED_AFTER' or 0) - float('$REQUESTER_GUARANTEED_BEFORE' or 0), 6))")
[[ "$DONOR_DELTA" == "-$CREDITS_WANT" ]] \
  && pass "donor's guaranteed AccH debited by exactly $CREDITS_WANT (delta=$DONOR_DELTA)" \
  || fail "expected donor debited by -$CREDITS_WANT, got $DONOR_DELTA"
[[ "$REQUESTER_DELTA" == "$CREDITS_WANT" ]] \
  && pass "requester's guaranteed AccH credited by exactly $CREDITS_WANT (delta=$REQUESTER_DELTA)" \
  || fail "expected requester credited by +$CREDITS_WANT, got $REQUESTER_DELTA"

echo "  -- fulfilling an already-fulfilled donation must not double-credit --"
REFULFILL_CODE=$(curl -s -o /tmp/donation_refulfill.$$.json -w '%{http_code}' -X POST "$QUOTA_URL/donations/${DONATION_ID}/fulfill" \
  -H 'Content-Type: application/json' -d "{\"donor_agent_id\": \"$DONOR\"}")
rm -f /tmp/donation_refulfill.$$.json
DONOR_GUARANTEED_AFTER2=$(_quota_field "$PE_ID" "$DONOR" guaranteed_accelerator_hours)
[[ "$DONOR_GUARANTEED_AFTER2" == "$DONOR_GUARANTEED_AFTER" ]] \
  && pass "re-fulfilling an already-fulfilled donation did not debit the donor again (HTTP $REFULFILL_CODE)" \
  || fail "donor's guaranteed AccH changed again on re-fulfillment attempt ($DONOR_GUARANTEED_AFTER -> $DONOR_GUARANTEED_AFTER2) — double-credit"

echo "  -- a cancelled donation request cannot later be fulfilled --"
CANCEL_DONATION_ID=$(curl -sf -X POST "$QUOTA_URL/donations" -H 'Content-Type: application/json' -d "{
  \"agent_id\": \"$CANCELLER\", \"platform_experiment_id\": \"$PE_ID\",
  \"credits_want\": 0.02, \"reason\": \"e2e cancel-before-fulfill coverage\"
}" | py "import sys,json; print(json.load(sys.stdin)['id'])")
curl -sf -X POST "$QUOTA_URL/donations/${CANCEL_DONATION_ID}/cancel" > /dev/null
CANCELLER_GUARANTEED_BEFORE=$(_quota_field "$PE_ID" "$CANCELLER" guaranteed_accelerator_hours)
POST_CANCEL_FULFILL_CODE=$(curl -s -o /tmp/donation_postcancel.$$.json -w '%{http_code}' -X POST "$QUOTA_URL/donations/${CANCEL_DONATION_ID}/fulfill" \
  -H 'Content-Type: application/json' -d "{\"donor_agent_id\": \"$DONOR\"}")
rm -f /tmp/donation_postcancel.$$.json
CANCELLER_GUARANTEED_AFTER=$(_quota_field "$PE_ID" "$CANCELLER" guaranteed_accelerator_hours)
[[ "$POST_CANCEL_FULFILL_CODE" -ge 400 && "$CANCELLER_GUARANTEED_AFTER" == "$CANCELLER_GUARANTEED_BEFORE" ]] \
  && pass "fulfilling a cancelled donation request was rejected (HTTP $POST_CANCEL_FULFILL_CODE), no credit moved" \
  || fail "cancelled donation request was fulfillable (HTTP $POST_CANCEL_FULFILL_CODE), guaranteed AccH $CANCELLER_GUARANTEED_BEFORE -> $CANCELLER_GUARANTEED_AFTER"

close_platform_experiment "$PE_ID"
finish
