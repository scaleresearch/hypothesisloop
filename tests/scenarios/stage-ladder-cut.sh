#!/usr/bin/env bash
# A real stage cut, end to end: a 3-stage ladder over a roster large enough to clear the
# guardrail floor crosses its first boundary, cuts the configured share of the field, stops the
# cut agents' jobs, blocks their resubmissions with 422, and moves their unspent budget to the
# survivors.
#
# Deliberately asserts the mechanism, not which agent loses. Who ranks where depends on how much
# of each agent's metric stream landed before the boundary, which depends on cluster capacity at
# run time — the ranking arithmetic itself (worst-first order, tie groups, no-data-last, the
# guardrails) is pinned exactly by controller/stages_rank_test.go instead.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

# Six agents: above minSurvivorsForCut (5), and 50% of 6 is 3 cut / 3 kept, which clears
# minSurvivorsAfterCut (2) without being clamped.
AGENTS=()
for i in 1 2 3 4 5 6; do AGENTS+=("agent-ladder-${i}-${RUN_ID}"); done
for a in "${AGENTS[@]}"; do register_agent "$a"; done

STAGES='[{"length_pct":20,"evict_pct":50},{"length_pct":30,"evict_pct":25},{"length_pct":50,"evict_pct":0}]'
JOB_HOURS=0.0084
# Budget sized so the first boundary (20%) lands late in this one wave of jobs. Cost is in
# H100-equivalent AccH, not raw hours: the generic workload's L40 carries acch_rate 0.25, so six
# 0.0084h jobs settle at roughly 0.021 AccH total, and 20% of 0.075 is 0.015 — about two thirds
# of the way through. Every agent has therefore been streaming metrics for most of its run by the
# time the boundary trips, so the cut is drawn from real data rather than from whoever started
# first. It also leaves each agent 0.075*0.2/6 = 0.0025 guaranteed AccH, comfortably above one
# job's ~0.0021, so nothing queues behind quota instead of running.
BUDGET=$(scale_budget 0.075)
PE_ID=$(create_platform_experiment "stage-ladder-${RUN_ID}" "$BUDGET" "${#AGENTS[@]}" 10 0 "" "$STAGES")
signup_and_start "$PE_ID" "${AGENTS[@]}"

stages_json() { curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/stages"; }
jq_stages() { stages_json | py "import sys,json; d=json.load(sys.stdin); $1"; }

# The ladder must come back exactly as configured — this is the write path (POST with an
# explicit stages field) round-tripping through Postgres.
# Compared as numbers, not as JSON text: 20 and 20.0 are the same ladder.
CONFIGURED=$(jq_stages "print([(float(s['length_pct']), float(s['evict_pct'])) for s in d['stages']])")
EXPECTED=$(py "print([(20.0, 50.0), (30.0, 25.0), (50.0, 0.0)])")
[[ "$CONFIGURED" == "$EXPECTED" ]] \
  && pass "configured 3-stage ladder round-tripped through the API" \
  || fail "ladder came back as $CONFIGURED, want $EXPECTED"

[[ "$(jq_stages "print(d['current_stage'])")" == "1" ]] \
  && pass "starts on stage 1" \
  || fail "did not start on stage 1"

# Initial allocation is capped to the first stage's share (20%), not the whole budget — the rest
# is released at the boundaries.
quota_guaranteed() {
  curl -sf "$QUOTA_URL/quota/$1/experiment/${PE_ID}" \
    | py "import sys,json; print(json.load(sys.stdin)['guaranteed_accelerator_hours'])"
}
FIRST_ALLOC=$(quota_guaranteed "${AGENTS[0]}")
echo "  stage-1 guaranteed allocation: ${FIRST_ALLOC} AccH (of ${BUDGET} total budget)"
py "import sys; sys.exit(0 if $FIRST_ALLOC < $BUDGET * 0.2 else 1)" \
  && pass "stage-1 allocation is capped to the first stage's share of the budget" \
  || fail "stage-1 allocation ${FIRST_ALLOC} exceeds the first stage's 20% share of ${BUDGET}"

declare -a JOBS
for a in "${AGENTS[@]}"; do
  JOBS+=("$(submit_job "$PE_ID" "$a" "guaranteed" "$JOB_HOURS")")
done
echo "  submitted: ${JOBS[*]}"

advanced() { [[ "$(jq_stages "print(d['current_stage'])")" -ge 2 ]]; }
if ! wait_until "first stage boundary" 180 2 advanced; then
  fail "ladder never advanced past stage 1"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "ladder advanced past its first boundary"

ST=$(stages_json)
CUT_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['cut_agents']))")
ACTIVE_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['active_agents']))")
CUT_AGENTS=$(echo "$ST" | py "import sys,json; print(' '.join(c['agent_id'] for c in json.load(sys.stdin)['cut_agents']))")
echo "  after boundary: active=${ACTIVE_N} cut=${CUT_N} (${CUT_AGENTS})"

[[ $((ACTIVE_N + CUT_N)) -eq "${#AGENTS[@]}" ]] \
  && pass "active+cut accounts for all ${#AGENTS[@]} agents" \
  || fail "active($ACTIVE_N)+cut($CUT_N) != ${#AGENTS[@]} agents"

# 50% of 6 survivors. Fewer is legitimate only if a tie group straddled the line; more is a bug.
[[ "$CUT_N" -le 3 ]] \
  && pass "cut $CUT_N agent(s), never more than the configured 50% of 6" \
  || fail "cut $CUT_N agents, more than floor(50% x 6) = 3"
[[ "$ACTIVE_N" -ge 2 ]] \
  && pass "at least 2 survivors remain (guardrail floor)" \
  || fail "only $ACTIVE_N survivor(s) left — the survivor floor was violated"

if [[ "$CUT_N" -gt 0 ]]; then
  # Every boundary must record which stage did the cutting.
  BAD_STAGE=$(echo "$ST" | py "import sys,json; print(sum(1 for c in json.load(sys.stdin)['cut_agents'] if c['stage_index'] != 1))")
  [[ "$BAD_STAGE" == "0" ]] \
    && pass "every cut is attributed to stage 1" \
    || fail "$BAD_STAGE cut record(s) attributed to the wrong stage"

  # A cut is terminal: further submissions are rejected platform-side, not merely discouraged.
  VICTIM=$(echo "$CUT_AGENTS" | awk '{print $1}')
  if submit_job "$PE_ID" "$VICTIM" "guaranteed" "$JOB_HOURS" >/dev/null 2>&1; then
    fail "$VICTIM was cut but its resubmission was accepted"
  else
    pass "$VICTIM was cut and its resubmission is rejected"
  fi

  # A cut agent's quota is zeroed, and its unspent share plus the incoming stage's release goes
  # to the survivors.
  VICTIM_Q=$(quota_guaranteed "$VICTIM")
  py "import sys; sys.exit(0 if $VICTIM_Q == 0 else 1)" \
    && pass "$VICTIM's guaranteed quota was zeroed" \
    || fail "$VICTIM was cut but still holds ${VICTIM_Q} guaranteed AccH"

  SURVIVOR=$(echo "$ST" | py "import sys,json; print(json.load(sys.stdin)['active_agents'][0])")
  SURVIVOR_Q=$(quota_guaranteed "$SURVIVOR")
  echo "  survivor ${SURVIVOR}: ${SURVIVOR_Q} AccH (was ${FIRST_ALLOC} at stage 1)"
  py "import sys; sys.exit(0 if $SURVIVOR_Q > $FIRST_ALLOC else 1)" \
    && pass "survivor's guaranteed quota grew at the boundary" \
    || fail "survivor's quota did not grow: ${SURVIVOR_Q} <= ${FIRST_ALLOC}"
else
  echo "  no agent was cut (a tie group straddled the line) — cut-specific assertions skipped"
fi

# Progress is monotonic and the published next boundary is the one that actually follows.
PROGRESS=$(jq_stages "print(d['progress'])")
NEXT=$(jq_stages "print(d['next_boundary_progress'])")
echo "  progress=${PROGRESS} next_boundary=${NEXT}"
py "import sys; sys.exit(0 if $NEXT == 0.5 else 1)" \
  && pass "next boundary is stage 2's end at 50%" \
  || fail "next_boundary_progress=${NEXT}, want 0.5 after advancing to stage 2"

# The endpoint must never leak standings — an agent may see that it is cut, not how close it is.
LEAKED=$(stages_json | py "import sys,json; d=json.load(sys.stdin); print(','.join(k for k in ('rank','ranks','standings','scores','metric_values') if k in d))")
[[ -z "$LEAKED" ]] \
  && pass "stages endpoint exposes no per-agent rank or standings" \
  || fail "stages endpoint leaked ranking fields: $LEAKED"

close_platform_experiment "$PE_ID"
finish
