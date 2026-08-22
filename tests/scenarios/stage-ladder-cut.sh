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
# Each job declares a short estimate and then genuinely runs longer than it, which is what lets a
# wave of jobs burn past the first stage's share of the budget at all.
#
# Observed consumption is now measured from the observations themselves, so a job that runs
# exactly its estimate consumes very slightly *less* than it reserved (the head and tail either
# side of its first and last observation). An agent's stage-1 allocation is the stage's share
# divided by the roster, so one on-estimate job per agent can never quite reach the boundary —
# it is bounded by the very allocation it is measured against. Overrunning is the honest way to
# push a wave past the line, and it is ordinary behaviour the platform bills for (see
# quota-exhaustion.sh, which relies on the same thing).
JOB_HOURS=0.0028   # ~10s reserved
RUN_SECONDS=40     # ~4x that actually consumed
RUN_ENV="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}"
# Sized off those two numbers. Six jobs observe roughly 6 x 0.25 x 37s = 0.0154 AccH, so a 20%
# first stage of a 0.03 AccH budget puts the boundary at 0.006 — about 40% of the way through the
# wave, drawn from real streamed metrics rather than from whoever started first. It also leaves
# each agent 0.03*0.2/6 = 0.001 guaranteed AccH against a 0.0007 reservation, so nothing queues
# behind quota instead of running.
BUDGET=$(scale_budget 0.03)
PE_ID=$(create_platform_experiment "stage-ladder-${RUN_ID}" "$BUDGET" "${#AGENTS[@]}" 10 0 "" "$STAGES")
signup_and_start "$PE_ID" "${AGENTS[@]}"

stages_json() { curl -sf "$API_URL/platform-experiments/${PE_ID}/stages"; }
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
  curl -sf "$API_URL/platform-experiments/${PE_ID}/quotas/$1" \
    | py "import sys,json; print(json.load(sys.stdin)['guaranteed_accelerator_hours'])"
}
FIRST_ALLOC=$(quota_guaranteed "${AGENTS[0]}")
echo "  stage-1 guaranteed allocation: ${FIRST_ALLOC} AccH (of ${BUDGET} total budget)"
py "import sys; sys.exit(0 if $FIRST_ALLOC < $BUDGET * 0.2 else 1)" \
  && pass "stage-1 allocation is capped to the first stage's share of the budget" \
  || fail "stage-1 allocation ${FIRST_ALLOC} exceeds the first stage's 20% share of ${BUDGET}"

declare -a JOBS
for a in "${AGENTS[@]}"; do
  JOBS+=("$(submit_job_ext "$PE_ID" "$a" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV")")
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
  if submit_job_ext "$PE_ID" "$VICTIM" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV" >/dev/null 2>&1; then
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

  # A cut job did nothing wrong: the platform decided. It must be reported as `policy`, never as
  # one of the agent's failures (domain.EvictionStageCut is FaultPolicy).
  #
  # Which cut agent still had a running job at the boundary is not fixed — an agent whose job had
  # already completed is cut without anything to stop — so find one that really was evicted rather
  # than assuming the first cut agent was. Cutting and evicting its jobs are separate writes, so
  # give the eviction a few passes to land before concluding there was none.
  # Sets CUT_JOB_LINE ("agent job") rather than printing it: wait_until runs its check in this
  # shell, so a global survives the poll — and going through wait_until is what keeps this wait
  # clamped to the scenario's remaining ceiling instead of a raw sleep loop that can outlive it.
  CUT_JOB_LINE=""
  cut_evicted_job() {
    local a i j
    for a in $CUT_AGENTS; do
      for i in "${!AGENTS[@]}"; do
        [[ "${AGENTS[$i]}" == "$a" ]] || continue
        j="${JOBS[$i]}"
        if [[ "$(get_status "$j")" == "EVICTED" && "$(get_field "$j" eviction_reason)" == stage_cut* ]]; then
          CUT_JOB_LINE="$a $j"
          return 0
        fi
      done
    done
    return 1
  }
  wait_until "a cut agent's job to reach its stage_cut eviction" 15 1 cut_evicted_job || true
  if [[ -z "$CUT_JOB_LINE" ]]; then
    echo "  every cut agent's job had already finished before the boundary — nothing was stopped by the cut, so its attribution is not observable here"
  else
    read -r CUT_JOB_AGENT CUT_JOB <<< "$CUT_JOB_LINE"
    echo "  stage-cut job: ${CUT_JOB} (${CUT_JOB_AGENT})"
    POLICY_N=$(eviction_class_count "$PE_ID" "$CUT_JOB_AGENT" policy)
    WORKLOAD_N=$(eviction_class_count "$PE_ID" "$CUT_JOB_AGENT" workload)
    INFRA_N=$(eviction_class_count "$PE_ID" "$CUT_JOB_AGENT" infrastructure)
    echo "  ${CUT_JOB_AGENT} evictions by class: policy=${POLICY_N} workload=${WORKLOAD_N} infrastructure=${INFRA_N}"
    [[ "$POLICY_N" != "-" && "$POLICY_N" -ge 1 ]] \
      && pass "the stage-cut job is reported under the policy class" \
      || fail "evictions_by_class.policy is '$POLICY_N' — a stage cut is the platform's own decision and must be reported as one"
    # The point of the class: a cut agent's record must not read as if it failed. Both of these
    # flip if stage_cut is classified as anything but policy.
    [[ "$WORKLOAD_N" == "0" ]] \
      && pass "the stage-cut job is not counted among the agent's own failures" \
      || fail "evictions_by_class.workload is '$WORKLOAD_N' for a cut agent, expected 0 — a cut reads as a failure"
    [[ "$INFRA_N" == "0" ]] \
      && pass "the stage-cut job is not counted as an infrastructure fault either" \
      || fail "evictions_by_class.infrastructure is '$INFRA_N' for a cut agent, expected 0"
    # A stage cut is terminal by policy, so it must not have bought the job a free infrastructure
    # requeue: infra_requeue_count only increments, so 0 proves it never took one.
    CUT_INFRA=$(get_field "$CUT_JOB" infra_requeue_count)
    [[ "$CUT_INFRA" == "0" ]] \
      && pass "the cut job was not requeued for free (infra_requeue_count=0)" \
      || fail "cut job has infra_requeue_count='$CUT_INFRA', expected 0 — a policy decision was treated as an infrastructure fault"

    read -r CLASS_TOTAL REASON_TOTAL UNCLASSIFIED <<< "$(eviction_class_coverage "$PE_ID" "$CUT_JOB_AGENT")" || true
    [[ "$CLASS_TOTAL" == "$REASON_TOTAL" && "$UNCLASSIFIED" == "0" ]] \
      && pass "every evicted job is accounted for by exactly one class ($CLASS_TOTAL of $REASON_TOTAL, none unclassified)" \
      || fail "class breakdown does not account for the evictions: by_class=$CLASS_TOTAL by_reason=$REASON_TOTAL unclassified=$UNCLASSIFIED"
  fi
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
