#!/usr/bin/env bash
# Signup roles, end to end. A signup is competitor (ranked, cut-eligible), baseline (the declared
# control) or reviewer (re-checks other agents' claims); only the first is ranked or cut. This
# covers the paths that differ from the default flow, and nothing that the default flow already
# owns:
#   0. an unrecognized role is refused at signup with reason=unknown_role, never defaulted.
#   1. one baseline + one reviewer + five competitors on one platform experiment, with
#      max_agents=5 — non-competitors must not consume the field being measured.
#   2. the baseline posts the best ranking-metric value and is still absent from results; rank 1
#      is the best *competitor*, and the baseline's metrics are readable in full.
#   3. at a real stage boundary, with the baseline deliberately the worst value in the run, the
#      baseline is not cut and every cut agent is one of the five competitors.
#   4. the baseline's job is billed and settled exactly like a competitor's — role changes
#      ranking, never accounting.
#   5. the summary gate applies to the baseline: its second submission is refused until it files
#      the finding for its first.
#   6. a reviewer comments on another agent's hypothesis, runs a job that reports a top-of-field
#      value, and still never appears in standings.
#
# The ranking arithmetic itself is pinned by controller/stages_rank_test.go and
# quota/platform_experiments_roles_test.go; what only a live run can show is that the role
# recorded at signup actually reaches the ranker, the cut, the quota ledger and the summary gate.
# API-only, parallel-safe (own platform experiments, own agents).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

# H100, not the suite default (L40). Every scenario that does not name an accelerator type runs on
# L40's 8 units, and this one needs 13 concurrent single-accelerator jobs to be admitted promptly
# or its stage boundary never arrives inside the per-scenario ceiling. A100's 8 units are claimed
# by burst-fair-round-robin and preemption-requeue, which saturate the type deliberately. The two
# H100 nodes carry 16 units and the only other H100 users -- distributed-jobs and mixed-admission
# (SLOW group), node-and-daemonset-faults and concurrent-admission-race (CLUSTER_EXCLUSIVE) --
# never run in the fast group this scenario belongs to, so the pool is uncontended here.
ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
# H100 is the AccH baseline itself (acch_rate 1.0 in controlplane/settings/hypothesisloop.yaml), so
# every budget below is wall-clock hours x accelerator count, with no rate to convert. Deliberately
# NOT scale_budget: that converts from the *configured* TEST_ACCH_RATE, which describes the type
# this scenario does not use.
ACCH_RATE=1.0

COMPETITORS=()
for i in 1 2 3 4 5; do COMPETITORS+=("agent-role-comp-${i}-${RUN_ID}"); done
BASELINE_AGENT="agent-role-baseline-${RUN_ID}"
REVIEWER_AGENT="agent-role-reviewer-${RUN_ID}"
CUT_COMPETITORS=()
for i in 1 2 3 4 5; do CUT_COMPETITORS+=("agent-role-cut-comp-${i}-${RUN_ID}"); done
CUT_BASELINE="agent-role-cut-baseline-${RUN_ID}"
STRANGER="agent-role-stranger-${RUN_ID}"
for a in "${COMPETITORS[@]}" "$BASELINE_AGENT" "$REVIEWER_AGENT" \
         "${CUT_COMPETITORS[@]}" "$CUT_BASELINE" "$STRANGER"; do
  register_agent "$a"
done

# The workload's reported value is anchored on HYPOTHESISLOOP_BASELINE: its curve runs from that
# floor up to at most floor+0.25 (tests/workloads/generic/train.py). Two well-separated anchors
# therefore decide the ordering of the whole field deterministically, without depending on which
# job happened to stream more points before a boundary — 0.90 always outranks 0.10, whatever the
# cluster does. That is the mechanism results-standings.sh and stage-ladder-cut.sh leave to
# chance, and it is the one thing this scenario cannot leave to chance: the assertions are about
# *who* is ranked and *who* is cut, not merely that ranking happened.
HIGH_ANCHOR="0.90"
LOW_ANCHOR="0.10"

echo "=========================================================="
echo "Part 0: an unrecognized role is refused, not defaulted"
echo "=========================================================="
# Cheap, and the one fail-fast the API promises: a typo'd role silently defaulting to competitor
# would rank an agent nobody meant to rank, and the mistake is invisible until the cut.
PROBE_PE=$(create_platform_experiment "agent-roles-probe-${RUN_ID}" 1.0 5)
read -r CODE REASON <<< "$(signup_agent_code "$PROBE_PE" "$STRANGER" "referee")"
[[ "$CODE" == "400" && "$REASON" == "unknown_role" ]] \
  && pass "signup with role=referee refused 400 unknown_role" \
  || fail "signup with an unknown role answered $CODE reason=$REASON — a typo'd role must be refused, not defaulted to competitor"
read -r CODE _ <<< "$(signup_role "$PROBE_PE" "$STRANGER")"
[[ "$CODE" == "404" ]] \
  && pass "the refused agent has no signup at all (404) — the bad request wrote nothing" \
  || fail "GET signups/${STRANGER} answered $CODE after a refused signup — a rejected role left a signup behind"
close_platform_experiment "$PROBE_PE"

echo "=========================================================="
echo "Part 1: roster — 5 competitors + 1 baseline + 1 reviewer against max_agents=5"
echo "=========================================================="
# max_agents sizes the field being ranked, so it counts competitors only. Set to exactly the five
# competitors: if a baseline or reviewer counted against it, the two signups below would be
# refused with max_agents_reached and the scenario would stop here.
RANK_PE=$(create_platform_experiment "agent-roles-rank-${RUN_ID}" 10.0 5 5)
ROSTER=()
for a in "${COMPETITORS[@]}"; do ROSTER+=("${a}:competitor"); done
ROSTER+=("${BASELINE_AGENT}:baseline" "${REVIEWER_AGENT}:reviewer")
signup_and_start_with_roles "$RANK_PE" "${ROSTER[@]}"
pass "baseline and reviewer signed up alongside a full field of ${#COMPETITORS[@]} competitors at max_agents=5"

for spec in "${BASELINE_AGENT}:baseline" "${REVIEWER_AGENT}:reviewer" "${COMPETITORS[0]}:competitor"; do
  want="${spec#*:}"; who="${spec%%:*}"
  read -r CODE GOT_ROLE <<< "$(signup_role "$RANK_PE" "$who")"
  [[ "$CODE" == "200" && "$GOT_ROLE" == "$want" ]] \
    && pass "$who reads back as role=$want" \
    || fail "$who reads back as $CODE/$GOT_ROLE, want 200/$want — the role an agent is briefed from is wrong"
done

echo "=========================================================="
echo "Part 2 setup: the stage-boundary platform experiment (started first, it is the long pole)"
echo "=========================================================="
# Same ladder shape as stage-ladder-cut.sh: a 20% first stage, evicting 50%. Five competitors is
# exactly minSurvivorsForCut, so the boundary cuts floor(50% x 5) = 2 and leaves 3, clearing
# minSurvivorsAfterCut (2). If the baseline were counted as a survivor the field would be 6, the
# cut would be 3 — and the baseline, holding the worst value in the run, would certainly be one of
# them. That is what makes "the baseline is not cut" an assertion and not a tautology.
CUT_STAGES='[{"length_pct":20,"evict_pct":50},{"length_pct":30,"evict_pct":25},{"length_pct":50,"evict_pct":0}]'
# Each job declares a short estimate and genuinely runs longer than it: observed consumption is
# measured from the observations themselves, so a wave of on-estimate jobs can never quite reach
# the boundary it is measured against (see stage-ladder-cut.sh for the full reasoning).
CUT_JOB_HOURS=0.0028   # ~10s reserved, = 0.0028 AccH at H100's rate of 1.0
CUT_RUN_SECONDS=30     # ~3x that actually consumed
CUT_ENV_LOW="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${CUT_RUN_SECONDS}\", \"HYPOTHESISLOOP_BASELINE\": \"${LOW_ANCHOR}\"}"
# One anchor per competitor, never a shared one. train.py floors its target at baseline+0.15 and
# clamps the reported value at 1.0, so a high shared anchor drives every competitor to the same
# saturated number -- and a cut whose whole field ties is legitimately allowed to take fewer than
# its percentage, or nobody. The assertion below is about WHICH agents the cut draws from, so a
# tie makes it prove nothing while still passing. These five are spaced 0.10 apart and low enough
# that none saturates, which makes the ranking total and the cut size exact.
CUT_ANCHORS=(0.30 0.40 0.50 0.60 0.70)
cut_env_for() { echo "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${CUT_RUN_SECONDS}\", \"HYPOTHESISLOOP_BASELINE\": \"$1\"}"; }
# Budget arithmetic. Six jobs (5 competitors + 1 baseline), each 1 accelerator for ~27 observed
# seconds at acch_rate 1.0, consume roughly 6 x 27/3600 = 0.045 AccH. The ladder advances on
# budget consumed, so the first boundary sits at 20% of the budget: 0.10 puts it at 0.020 AccH,
# about 44% of the way through the wave — late enough that every job has streamed real metrics to
# rank on, early enough to arrive well inside the scenario's ceiling. It also leaves each of the
# six signups 0.10 x 0.2 / 6 = 0.0033 guaranteed AccH against a 0.0028 reservation, so no job
# queues behind its own quota instead of running.
CUT_BUDGET=0.10
CUT_PE=$(create_platform_experiment "agent-roles-cut-${RUN_ID}" "$CUT_BUDGET" 5 10 0 "" "$CUT_STAGES")
CUT_ROSTER=()
for a in "${CUT_COMPETITORS[@]}"; do CUT_ROSTER+=("${a}:competitor"); done
CUT_ROSTER+=("${CUT_BASELINE}:baseline")
signup_and_start_with_roles "$CUT_PE" "${CUT_ROSTER[@]}"

declare -a CUT_JOBS
# The baseline runs on the LOW anchor and every competitor on the HIGH one, so the baseline holds
# the single worst value in the run — the agent the cut would take first if role were ignored.
CUT_JOBS+=("$(submit_job_ext "$CUT_PE" "$CUT_BASELINE" "guaranteed" "$CUT_JOB_HOURS" "$JOB_FILE" "$CUT_ENV_LOW" "$ACCELERATOR_TYPE" "1")")
for i in "${!CUT_COMPETITORS[@]}"; do
  CUT_JOBS+=("$(submit_job_ext "$CUT_PE" "${CUT_COMPETITORS[$i]}" "guaranteed" "$CUT_JOB_HOURS" "$JOB_FILE" \
    "$(cut_env_for "${CUT_ANCHORS[$i]}")" "$ACCELERATOR_TYPE" "1")")
done
echo "  cut-experiment jobs submitted: ${CUT_JOBS[*]}"

echo "=========================================================="
echo "Part 2: the baseline holds the best value and is still not ranked"
echo "=========================================================="
RANK_JOB_HOURS="0.01"
# The declared estimate is what billing is about; the real runtime is set separately and kept
# short. 20s at report_interval_seconds=5 is four reported points per job, enough of a stream to
# rank on without spending the scenario's ceiling on it.
RANK_RUN_SECONDS=20
RANK_ENV_HIGH="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RANK_RUN_SECONDS}\", \"HYPOTHESISLOOP_BASELINE\": \"${HIGH_ANCHOR}\"}"
RANK_ENV_LOW="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RANK_RUN_SECONDS}\", \"HYPOTHESISLOOP_BASELINE\": \"${LOW_ANCHOR}\"}"

# Here the anchors are the other way round: the baseline and the reviewer run HIGH and every
# competitor runs LOW, so both non-competitors outscore the entire field. Rank 1 can then only be
# a competitor if role is genuinely filtering the standings — with roles reverted, rank 1 would be
# the baseline or the reviewer, every time.
BASELINE_JOB=$(submit_job_ext "$RANK_PE" "$BASELINE_AGENT" "guaranteed" "$RANK_JOB_HOURS" "$JOB_FILE" "$RANK_ENV_HIGH" "$ACCELERATOR_TYPE" "1")
REVIEWER_JOB=$(submit_job_ext "$RANK_PE" "$REVIEWER_AGENT" "guaranteed" "$RANK_JOB_HOURS" "$JOB_FILE" "$RANK_ENV_HIGH" "$ACCELERATOR_TYPE" "1")
declare -a COMP_JOBS
for a in "${COMPETITORS[@]}"; do
  COMP_JOBS+=("$(submit_job_ext "$RANK_PE" "$a" "guaranteed" "$RANK_JOB_HOURS" "$JOB_FILE" "$RANK_ENV_LOW" "$ACCELERATOR_TYPE" "1")")
done
echo "  ranking-experiment jobs submitted: $BASELINE_JOB $REVIEWER_JOB ${COMP_JOBS[*]}"

echo "=========================================================="
echo "Part 6: a reviewer comments on another agent's hypothesis"
echo "=========================================================="
# Done while both waves run: it costs nothing and needs no job to have finished. A reviewer's
# output is commentary on someone else's claim, so the hypothesis it comments on belongs to a
# competitor, not to itself.
REVIEWED_HYP=$(register_hypothesis "${COMPETITORS[0]}" "$RANK_PE" "reviewed claim from ${COMPETITORS[0]}")
COMMENT_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API_URL/hypotheses/${REVIEWED_HYP}/comments" \
  -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"${REVIEWER_AGENT}\",\"text\":\"reviewer re-checked the reported metric against the job's code_ref\"}")
[[ "$COMMENT_CODE" == "201" ]] \
  && pass "reviewer commented on ${COMPETITORS[0]}'s hypothesis (201)" \
  || fail "reviewer's comment on another agent's hypothesis answered $COMMENT_CODE — a reviewer that cannot record agreement or dispute has no way to do its job"
COMMENT_AUTHORS=$(curl -sf "$API_URL/hypotheses/${REVIEWED_HYP}" | py "
import sys, json
d = json.load(sys.stdin)
print(','.join(c.get('agent_id') or c.get('author') or '?' for c in (d.get('comments') or [])))
")
[[ "$COMMENT_AUTHORS" == *"$REVIEWER_AGENT"* ]] \
  && pass "the comment is readable on the hypothesis, attributed to the reviewer" \
  || fail "hypothesis ${REVIEWED_HYP} lists comments from '${COMMENT_AUTHORS:-<none>}' — the reviewer's comment is not there"

echo "=========================================================="
echo "Part 2 (cont.): standings once the ranking wave has run"
echo "=========================================================="
# Every job must complete: the standings assertions are about which agents appear, so one that
# quietly failed would silently weaken all of them. The waits run back to back but the jobs run
# concurrently, so the first absorbs the shared queueing delay and the rest return quickly.
RANK_JOBS=("$BASELINE_JOB" "$REVIEWER_JOB" "${COMP_JOBS[@]}")
first_wait=1
for J in "${RANK_JOBS[@]}"; do
  budget="$ADMISSION_BUDGET_SECONDS"
  [[ "$first_wait" == "1" ]] || budget=$(( RANK_RUN_SECONDS * 3 + 20 ))
  first_wait=0
  S=$(wait_for_completion_after_running "$J" "$RANK_JOB_HOURS" "$budget" || true)
  [[ "$S" == "COMPLETED" ]] \
    && pass "$J completed" \
    || fail "$J ended as $S — every role must report for the standings assertions to mean anything"
done

# The baseline's numbers are the point of a baseline: not ranked is not the same as not recorded.
BASELINE_BEST=$(metric_max "$BASELINE_JOB" val_accuracy)
REVIEWER_BEST=$(metric_max "$REVIEWER_JOB" val_accuracy)
[[ -n "$BASELINE_BEST" ]] \
  && pass "the baseline's val_accuracy is recorded and readable (best=${BASELINE_BEST})" \
  || fail "the baseline job reported no val_accuracy at all — a control whose numbers are invisible is not a control"

# The best competitor, computed from the metrics store rather than assumed from the anchors, so
# the comparison below is against what actually got reported.
BEST_COMP=""; BEST_COMP_VALUE=""
for i in "${!COMPETITORS[@]}"; do
  v=$(metric_max "${COMP_JOBS[$i]}" val_accuracy)
  [[ -z "$v" ]] && continue
  if [[ -z "$BEST_COMP_VALUE" ]] || py "import sys; sys.exit(0 if $v > $BEST_COMP_VALUE else 1)"; then
    BEST_COMP="${COMPETITORS[$i]}"; BEST_COMP_VALUE="$v"
  fi
done
[[ -n "$BEST_COMP" ]] \
  && echo "  best competitor: ${BEST_COMP} at ${BEST_COMP_VALUE} (baseline ${BASELINE_BEST}, reviewer ${REVIEWER_BEST})" \
  || fail "no competitor reported val_accuracy — the ranking assertions below have nothing to rank"

# The whole part turns on the non-competitors genuinely outscoring the field. If the anchors did
# not separate them, "rank 1 is a competitor" would pass for the wrong reason.
py "import sys; sys.exit(0 if $BASELINE_BEST > $BEST_COMP_VALUE else 1)" \
  && pass "the baseline holds the best value in the run (${BASELINE_BEST} > ${BEST_COMP_VALUE})" \
  || fail "the baseline (${BASELINE_BEST}) did not outscore the best competitor (${BEST_COMP_VALUE}) — the ranking assertions below would pass for the wrong reason"

RESULTS=$(curl -sf "$API_URL/platform-experiments/${RANK_PE}/results")
RANKED_AGENTS=$(echo "$RESULTS" | py "
import sys, json
m = [x for x in json.load(sys.stdin)['metrics'] if x['metric'] == 'val_accuracy']
print(','.join(s['agent_id'] for s in (m[0]['standings'] if m else [])))
")
echo "  standings order: ${RANKED_AGENTS:-<none>}"

[[ ",$RANKED_AGENTS," != *",$BASELINE_AGENT,"* ]] \
  && pass "the baseline is absent from standings despite holding the best value" \
  || fail "the baseline ${BASELINE_AGENT} is ranked in standings — a control competing against the treatments it exists to measure"
[[ ",$RANKED_AGENTS," != *",$REVIEWER_AGENT,"* ]] \
  && pass "the reviewer is absent from standings despite reporting a top-of-field value" \
  || fail "the reviewer ${REVIEWER_AGENT} is ranked in standings — a reviewer is not one of the things being compared"

EXPECTED_RANKED=$(py "print(','.join(sorted('''${COMPETITORS[*]}'''.split())))")
ACTUAL_RANKED=$(py "print(','.join(sorted('''${RANKED_AGENTS}'''.split(','))) if '''${RANKED_AGENTS}''' else '')")
[[ "$ACTUAL_RANKED" == "$EXPECTED_RANKED" ]] \
  && pass "standings contain exactly the ${#COMPETITORS[@]} competitors, no more and no fewer" \
  || fail "standings list '${ACTUAL_RANKED}', expected exactly the competitors '${EXPECTED_RANKED}'"

RANK_ONE=$(echo "$RANKED_AGENTS" | cut -d, -f1)
[[ "$RANK_ONE" == "$BEST_COMP" ]] \
  && pass "rank 1 is the best competitor (${BEST_COMP}), not the better-scoring baseline" \
  || fail "rank 1 is '${RANK_ONE}', want the best competitor '${BEST_COMP}'"

echo "=========================================================="
echo "Part 4: the baseline is billed and settled exactly like a competitor"
echo "=========================================================="
# Same estimate, same accelerator type, same count, same runtime — so if role touched accounting
# anywhere, these two numbers would differ. They are compared with a tolerance because observed
# consumption is measured from each job's own metric stream, not from its estimate: two jobs that
# ran side by side legitimately differ by a fraction of a report interval.
all_settled() {
  local j
  for j in "$BASELINE_JOB" "${COMP_JOBS[0]}"; do
    [[ -n "$(get_field "$j" quota_settled_at)" ]] || return 1
  done
}
wait_until "the baseline's and a competitor's quota to settle" 30 1 all_settled || true
for J in "$BASELINE_JOB" "${COMP_JOBS[0]}"; do
  [[ -n "$(get_field "$J" quota_settled_at)" ]] \
    && pass "$J: quota durably settled" \
    || fail "$J: quota_settled_at is unset after the grace period — settlement did not complete"
done

BASELINE_USED=$(quota_used_guaranteed "$RANK_PE" "$BASELINE_AGENT")
COMP_USED=$(quota_used_guaranteed "$RANK_PE" "${COMPETITORS[0]}")
echo "  used guaranteed AccH: baseline=${BASELINE_USED} competitor=${COMP_USED}"
py "import sys; sys.exit(0 if float('${BASELINE_USED:-0}') > 0 else 1)" \
  && pass "the baseline's job was billed against its guaranteed quota (${BASELINE_USED} AccH)" \
  || fail "the baseline consumed ${BASELINE_USED:-0} guaranteed AccH — a role that runs for free is a hole in the budget"
# A quarter of a report interval's worth of AccH: wide enough for the streams to differ by a
# point, far too narrow to hide a role-dependent billing rule.
BILLING_TOLERANCE=$(py "print(round(5 / 3600 * $ACCH_RATE, 8))")
py "import sys; sys.exit(0 if abs(float('${BASELINE_USED:-0}') - float('${COMP_USED:-0}')) <= $BILLING_TOLERANCE else 1)" \
  && pass "the baseline and a competitor with identical jobs were billed the same within ${BILLING_TOLERANCE} AccH" \
  || fail "baseline billed ${BASELINE_USED} AccH vs competitor ${COMP_USED} for identical jobs — role changed accounting, which it must never do"

echo "=========================================================="
echo "Part 5: the summary gate applies to the baseline too"
echo "=========================================================="
# The gate is on COMPLETED jobs from the same agent+PE, and the baseline has exactly one. Pinned
# to the same accelerator type as everything else here so a refused submission cannot be refused
# for some unrelated capacity reason.
GATE_OVERRIDE="{\"accelerator_type\": \"${ACCELERATOR_TYPE}\", \"accelerator_count\": 1, \"acceptable_accelerator_types\": []}"
read -r CODE GATED_JOB <<< "$(submit_job_expect_code "$RANK_PE" "$BASELINE_AGENT" "guaranteed" "$RANK_JOB_HOURS" "$GATE_OVERRIDE")"
[[ "$CODE" == "403" ]] \
  && pass "the baseline's next submission is refused 403 until it files its finding" \
  || fail "the baseline's second submission answered $CODE (job ${GATED_JOB}), want 403 — the reference run needs a finding as much as a treatment does"

file_finding "$BASELINE_JOB" "baseline control run for agent-roles-${RUN_ID}"
read -r CODE UNGATED_JOB <<< "$(submit_job_expect_code "$RANK_PE" "$BASELINE_AGENT" "guaranteed" "$RANK_JOB_HOURS" "$GATE_OVERRIDE")"
[[ "$CODE" -lt 400 ]] \
  && pass "once the finding is filed the baseline can submit again ($CODE)" \
  || fail "the baseline's submission still answered $CODE after filing its finding — the gate never opens"
# Nothing here waits on that job; cancel it rather than leave an accelerator held for a full run
# while the stage-boundary wave below is still competing for the same pool.
[[ "$CODE" -lt 400 ]] && cancel_job "$UNGATED_JOB"

echo "=========================================================="
echo "Part 3: a real stage boundary cuts from the competitors only"
echo "=========================================================="
cut_stages_json() { curl -sf "$API_URL/platform-experiments/${CUT_PE}/stages"; }
cut_advanced() { [[ "$(cut_stages_json | py "import sys,json; print(json.load(sys.stdin)['current_stage'])")" -ge 2 ]]; }
if ! wait_until "the first stage boundary" 150 2 cut_advanced; then
  fail "the ladder never advanced past stage 1 — the cut assertions could not be made"
  close_platform_experiment "$CUT_PE"
  close_platform_experiment "$RANK_PE"
  finish
fi
pass "the ladder advanced past its first boundary"

ST=$(cut_stages_json)
CUT_LIST=$(echo "$ST" | py "import sys,json; print(' '.join(c['agent_id'] for c in json.load(sys.stdin)['cut_agents']))")
ACTIVE_LIST=$(echo "$ST" | py "import sys,json; print(' '.join(json.load(sys.stdin)['active_agents']))")
CUT_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['cut_agents']))")
echo "  after the boundary: cut=[${CUT_LIST}] active=[${ACTIVE_LIST}]"

BASELINE_WORST=$(metric_max "${CUT_JOBS[0]}" val_accuracy)
echo "  cut-experiment baseline best value: ${BASELINE_WORST:-<none>} (competitors anchored at ${CUT_ANCHORS[*]})"

[[ " $CUT_LIST " != *" $CUT_BASELINE "* ]] \
  && pass "the baseline was not cut despite holding the worst value in the run" \
  || fail "the baseline ${CUT_BASELINE} was cut — a control eliminated for losing a competition it is not in"
[[ " $ACTIVE_LIST " == *" $CUT_BASELINE "* ]] \
  && pass "the baseline is still an active agent after the boundary" \
  || fail "the baseline ${CUT_BASELINE} is neither cut nor active — it fell out of the roster entirely"

if [[ "$CUT_N" -gt 0 ]]; then
  STRAYS=0
  for victim in $CUT_LIST; do
    found=0
    for c in "${CUT_COMPETITORS[@]}"; do [[ "$victim" == "$c" ]] && found=1; done
    [[ "$found" == "1" ]] || STRAYS=$((STRAYS + 1))
  done
  [[ "$STRAYS" == "0" ]] \
    && pass "all $CUT_N cut agent(s) come from the ${#CUT_COMPETITORS[@]} competitors" \
    || fail "$STRAYS cut agent(s) are not competitors — the cut was drawn from the wrong field"
  # floor(50% x 5 competitors) = 2. Counting the baseline as a survivor would make the field 6 and
  # the cut 3, so a cut larger than 2 is exactly the symptom of the roster including non-competitors.
  # Exactly 2, not at-most-2. Every competitor has its own anchor so no tie group can straddle
  # the line, which makes floor(50% x 5) an exact expectation: 3 would mean the baseline was
  # counted into the field, and 1 would mean the cut was drawn from fewer agents than the five.
  [[ "$CUT_N" -eq 2 ]] \
    && pass "cut exactly $CUT_N agent(s) = floor(50% x ${#CUT_COMPETITORS[@]} competitors)" \
    || fail "cut $CUT_N agents, want exactly 2 — a larger cut means non-competitors were counted into the field, a smaller one means the field was not the five competitors"
else
  # Legitimate only if a tie group straddled the line. Reported, never silently passed over: with
  # no cut at all the "baseline was not cut" assertion above proved nothing.
  fail "the boundary cut nobody, so nothing distinguishes a baseline from a competitor here — the ${#CUT_COMPETITORS[@]}-competitor field should have cut 2"
fi

close_platform_experiment "$CUT_PE"
close_platform_experiment "$RANK_PE"
finish
