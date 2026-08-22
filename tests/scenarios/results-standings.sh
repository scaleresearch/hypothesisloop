#!/usr/bin/env bash
# GET /platform-experiments/{id}/results — what a finished run amounts to. The ranking arithmetic
# itself is pinned by Go unit tests; this covers the parts only a live run exercises:
#   1. standings come back ranked, one entry per agent that reported, on the declared metric.
#   2. every standing carries the experiment that produced it and that job's code_ref — the whole
#      point of the endpoint is getting from a winning number to the commit that produced it, and
#      those two fields were documented but never populated.
#   3. a constraint metric gates eligibility: an agent that never reports a declared constraint is
#      excluded from standings entirely rather than ranked on the metric it did report.
# API-only, parallel-safe (own PE, its own agents).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENTS=("agent-results-a-${RUN_ID}" "agent-results-b-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done

# val_accuracy is what the generic workload emits and is the ranking metric. The constraint metric
# is declared but never emitted by any job here, which is exactly the case part 3 asserts on.
METRICS='[{"key": "val_accuracy", "direction": "maximize", "role": "ranking"},
          {"key": "never_emitted_budget", "direction": "minimize", "role": "constraint", "bound": 1.0}]'
PE_ID=$(create_platform_experiment "results-standings-${RUN_ID}" 10.0 "${#AGENTS[@]}" 5 0 "$METRICS")
# The second platform experiment declares only the ranking metric, so nothing gates eligibility and
# the agents that report are actually ranked. Created up front so all four jobs can be submitted
# before anything is waited on — four sequential submit-then-wait cycles would serialise the whole
# scenario against the suite's per-scenario ceiling, while four concurrent jobs finish in about the
# time of one.
PE2=$(create_platform_experiment "results-standings-plain-${RUN_ID}" 10.0 "${#AGENTS[@]}" 5 0 \
  '[{"key": "val_accuracy", "direction": "maximize"}]')
signup_and_start "$PE_ID" "${AGENTS[@]}"
signup_and_start "$PE2" "${AGENTS[@]}"

# Deliberately NOT scale_budget: real runtime is fixed by RUN_SECONDS below, and the completion
# budget derived from JOB_HOURS must not grow with the accelerator rate — this scenario's wall
# clock has to stay the same on every kind of hardware.
JOB_HOURS="0.01"
# The workload's real runtime is HYPOTHESISLOOP_DURATION_SECONDS (default 60s), NOT the declared
# estimate — shortening estimated_duration_hours alone would leave every job running a full minute.
# Both are set: the estimate for cost, the duration for wall clock. 20s is several report intervals
# at report_interval_seconds=5, so each job still produces a real metric stream to rank on.
RUN_SECONDS=20
RUN_ENV="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}"

declare -a JOBS JOBS2
for a in "${AGENTS[@]}"; do
  JOBS+=("$(submit_job_ext "$PE_ID" "$a" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV")")
  JOBS2+=("$(submit_job_ext "$PE2" "$a" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV")")
done
echo "  submitted: ${JOBS[*]} ${JOBS2[*]}"

# Every job must complete: the standings assertions are about *which* agents appear, so one that
# quietly failed would silently weaken all of them. The waits run back to back but the jobs run
# concurrently, so the later calls return immediately once the first has finished.
first_wait=1
for J in "${JOBS[@]}" "${JOBS2[@]}"; do
  # The first wait absorbs the whole queueing delay for all four jobs, since they were submitted
  # together. The rest only need enough budget to cover being scheduled behind it on a cluster with
  # fewer free accelerators than jobs — giving each the full admission budget again would let this
  # scenario's worst case run four times longer than it can ever actually need.
  budget="$ADMISSION_BUDGET_SECONDS"
  [[ "$first_wait" == "1" ]] || budget=$(( RUN_SECONDS * 4 + 20 ))
  first_wait=0
  S=$(wait_for_completion_after_running "$J" "$JOB_HOURS" "$budget" || true)
  [[ "$S" == "COMPLETED" ]] \
    && { pass "$J completed"; file_finding "$J"; } \
    || fail "$J ended as $S — every agent must report for the standings assertions to mean anything"
done

results() { curl -sf "$API_URL/platform-experiments/${PE_ID}/results"; }

echo "  -- the endpoint answers, and answers about the declared ranking metric --"
R=$(results)
[[ -n "$R" ]] && pass "results endpoint returned a document" || fail "results endpoint returned nothing"

METRIC_NAMES=$(echo "$R" | py "import sys,json; print(','.join(m['metric'] for m in json.load(sys.stdin)['metrics']))")
echo "  ranked metrics: ${METRIC_NAMES:-<none>}"
[[ "$METRIC_NAMES" == *"val_accuracy"* ]] \
  && pass "standings are computed on the declared ranking metric" \
  || fail "results contain no val_accuracy standings (got: ${METRIC_NAMES:-<none>})"
# A constraint metric gates; it is never itself a ranked column.
[[ "$METRIC_NAMES" != *"never_emitted_budget"* ]] \
  && pass "the constraint metric is not presented as its own ranking" \
  || fail "constraint metric never_emitted_budget was ranked as if it were a ranking metric"

echo "  -- an unreported constraint makes every agent ineligible --"
# Nothing emits never_emitted_budget, so no agent satisfies it — a constraint must be reported and
# satisfied, not merely absent. Every agent is therefore correctly excluded.
STANDING_N=$(echo "$R" | py "
import sys, json
d = json.load(sys.stdin)
print(sum(len(m['standings']) for m in d['metrics'] if m['metric'] == 'val_accuracy'))
")
[[ "$STANDING_N" == "0" ]] \
  && pass "agents that never reported the declared constraint are excluded from standings" \
  || fail "$STANDING_N agent(s) ranked despite never reporting the declared constraint metric"

echo "  -- with no constraint in the way, standings carry rank, experiment id and code_ref --"
R2=$(curl -sf "$API_URL/platform-experiments/${PE2}/results")
echo "$R2" | py "
import sys, json
d = json.load(sys.stdin)
ranked = [m for m in d['metrics'] if m['metric'] == 'val_accuracy']
print('standings:', json.dumps(ranked[0]['standings'] if ranked else [], indent=None))
"
STANDINGS_OK=$(echo "$R2" | py "
import sys, json
d = json.load(sys.stdin)
ranked = [m for m in d['metrics'] if m['metric'] == 'val_accuracy']
print(1 if ranked and ranked[0]['standings'] else 0)
")
if [[ "$STANDINGS_OK" == "1" ]]; then
  pass "reporting agents are ranked once no constraint excludes them"

  RANKS_OK=$(echo "$R2" | py "
import sys, json
s = [m for m in json.load(sys.stdin)['metrics'] if m['metric'] == 'val_accuracy'][0]['standings']
# Best-first, ranks dense from 1, and a maximized metric must be in non-increasing order.
print(1 if [x['rank'] for x in s] == list(range(1, len(s) + 1))
        and all(s[i]['best'] >= s[i+1]['best'] for i in range(len(s) - 1)) else 0)
")
  [[ "$RANKS_OK" == "1" ]] \
    && pass "standings are ranked 1..N, best-first, consistent with the metric's direction" \
    || fail "standings ranks or ordering are inconsistent with a maximized metric"

  # Exactly the agents that reported, no more and no fewer. A nonempty standings array alone would
  # pass with one of the two agents silently missing.
  EXPECTED_AGENTS=$(py "print(','.join(sorted(['${AGENTS[0]}', '${AGENTS[1]}'])))")
  ACTUAL_AGENTS=$(echo "$R2" | py "
import sys, json
s = [m for m in json.load(sys.stdin)['metrics'] if m['metric'] == 'val_accuracy'][0]['standings']
print(','.join(sorted(x['agent_id'] for x in s)))
")
  [[ "$ACTUAL_AGENTS" == "$EXPECTED_AGENTS" ]] \
    && pass "standings contain exactly the agents that reported ($ACTUAL_AGENTS)" \
    || fail "standings list '$ACTUAL_AGENTS', expected exactly '$EXPECTED_AGENTS'"

  # experiment_id must be one of this run's own jobs, not merely nonempty — a stale or wrong id is
  # exactly as useless as a missing one for getting back to the code that produced a number.
  # Exported rather than prefixed onto the call: `VAR=x py ...` on a shell *function* has
  # shell-dependent scoping, and the job ids must reach python reliably.
  export JOBS2_CSV="$(IFS=,; echo "${JOBS2[*]}")"
  TRACEABLE=$(echo "$R2" | py "
import sys, json, os
mine = set(os.environ['JOBS2_CSV'].split(','))
s = [m for m in json.load(sys.stdin)['metrics'] if m['metric'] == 'val_accuracy'][0]['standings']
print(sum(1 for x in s if x.get('experiment_id') in mine and x.get('code_ref')))
")
  TOTAL2=$(echo "$R2" | py "
import sys, json
s = [m for m in json.load(sys.stdin)['metrics'] if m['metric'] == 'val_accuracy'][0]['standings']
print(len(s))
")
  [[ "$TRACEABLE" == "$TOTAL2" && "$TOTAL2" != "0" ]] \
    && pass "every standing names one of this run's own jobs and its code_ref ($TRACEABLE/$TOTAL2)" \
    || fail "only $TRACEABLE of $TOTAL2 standings name a real job from this run with a code_ref — a winning number nobody can reproduce"
else
  fail "no agent was ranked on val_accuracy even with no constraint declared"
fi

close_platform_experiment "$PE2"
close_platform_experiment "$PE_ID"
finish
