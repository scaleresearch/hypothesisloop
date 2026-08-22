#!/usr/bin/env bash
# A ranking metric that no surviving agent reports must not veto the boundary.
#
# An agent is cut only when it ranks below the line on *every* ranking metric that produced usable
# data. If "usable data" is judged from any agent's samples rather than from the current
# survivors', a metric nobody still standing reports looks healthy while ranking all survivors as
# one tie group — which the whole-tie-group guardrail then refuses to cut. The result is a ladder
# that advances through every boundary and never cuts anyone, silently, with nothing in the API to
# say why. That is exactly the shape a mistyped or retired metric key takes in a real platform
# experiment, so it is worth pinning end to end rather than only in the ranking unit tests.
#
# The ladder here is identical to stage-ladder-cut.sh's except for the extra decoy metric, so a
# failure here against a pass there isolates the cause to the decoy.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENTS=()
for i in 1 2 3 4 5 6; do AGENTS+=("agent-decoy-${i}-${RUN_ID}"); done
for a in "${AGENTS[@]}"; do register_agent "$a"; done

STAGES='[{"length_pct":20,"evict_pct":50},{"length_pct":30,"evict_pct":25},{"length_pct":50,"evict_pct":0}]'
# val_accuracy is what the generic workload actually emits (HYPOTHESISLOOP_PRIMARY_METRIC).
# never_reported_score is a second *ranking* metric no workload ever posts a sample for — the
# decoy. Both must be ranking-role, since constraint/attribute metrics never cut and so could not
# reproduce the veto.
METRICS='[{"key":"val_accuracy","direction":"maximize"},{"key":"never_reported_score","direction":"maximize"}]'
# Estimate, real runtime and budget all sized exactly as stage-ladder-cut.sh sizes them — see the
# comment there for why a wave has to overrun its estimate to reach a boundary at all.
JOB_HOURS=0.0028
RUN_SECONDS=40
RUN_ENV="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}"
BUDGET=$(scale_budget 0.03)
PE_ID=$(create_platform_experiment "stage-decoy-${RUN_ID}" "$BUDGET" "${#AGENTS[@]}" 10 0 "$METRICS" "$STAGES")
signup_and_start "$PE_ID" "${AGENTS[@]}"

stages_json() { curl -sf "$API_URL/platform-experiments/${PE_ID}/stages"; }
jq_stages() { stages_json | py "import sys,json; d=json.load(sys.stdin); $1"; }

declare -a JOBS
for a in "${AGENTS[@]}"; do
  JOBS+=("$(submit_job "$PE_ID" "$a" "guaranteed" "$JOB_HOURS")")
done
echo "  submitted: ${JOBS[*]}"

advanced() { [[ "$(jq_stages "print(d['current_stage'])")" -ge 2 ]]; }
if ! wait_until "first stage boundary" 180 2 advanced; then
  fail "ladder never advanced past stage 1 — cannot judge whether the decoy metric vetoed the cut"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "ladder advanced past its first boundary with an unreported ranking metric configured"

ST=$(stages_json)
CUT_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['cut_agents']))")
ACTIVE_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['active_agents']))")
echo "  after boundary: active=${ACTIVE_N} cut=${CUT_N}"

# The assertion this scenario exists for. Reaching the boundary and cutting nobody is precisely
# the silent failure: the ladder looks healthy from the outside while the whole point of the
# ladder — removing the weakest agents — never happens.
[[ "$CUT_N" -ge 1 ]] \
  && pass "cut ${CUT_N} agent(s) on the reported metric; the unreported one did not veto the boundary" \
  || fail "boundary cut nobody: a ranking metric no survivor reports vetoed the whole cut"

[[ "$CUT_N" -le 3 ]] \
  && pass "cut no more than the configured 50% of 6" \
  || fail "cut $CUT_N agents, more than floor(50% x 6) = 3"
[[ "$ACTIVE_N" -ge 2 ]] \
  && pass "at least 2 survivors remain (guardrail floor)" \
  || fail "only $ACTIVE_N survivor(s) left — the survivor floor was violated"
[[ $((ACTIVE_N + CUT_N)) -eq "${#AGENTS[@]}" ]] \
  && pass "active+cut accounts for all ${#AGENTS[@]} agents" \
  || fail "active($ACTIVE_N)+cut($CUT_N) != ${#AGENTS[@]} agents"

close_platform_experiment "$PE_ID"
finish
