#!/usr/bin/env bash
# Full multi-agent smoke flow: every submitted job reaches a terminal state, the platform
# experiment advances past its first stage boundary once enough budget burns, every terminal job gets a
# durably settled quota (until quota_settled_at is written, PostgreSQL deliberately keeps the
# terminal job's estimate in desired usage so a crash cannot undercount it before the observed
# metric is durable), and the dashboard-facing Prometheus/registry endpoints
# return data for it. API-only, parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENTS=("agent-alpha-${RUN_ID}" "agent-beta-${RUN_ID}" "agent-gamma-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Jobs declare a short estimate and genuinely run longer, which is what carries the wave past the
# first stage boundary: observed consumption is measured from the observations themselves, so a
# job that runs exactly its estimate consumes slightly less than the allocation it is measured
# against and a wave of on-estimate jobs can never quite reach the line. See stage-ladder-cut.sh.
JOB_HOURS=0.0028
RUN_SECONDS=40
RUN_ENV="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}"
# Three jobs observe roughly 3 x 0.25 x 37s = 0.0077 AccH; the default ladder's 40% first stage of
# a 0.012 AccH budget puts the boundary at 0.0048, about 60% through the wave.
PE_ID=$(create_platform_experiment "stages-settlement-${RUN_ID}" "$(scale_budget 0.012)" "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

declare -a JOBS
for a in "${AGENTS[@]}"; do
  JOBS+=("$(submit_job_ext "$PE_ID" "$a" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV")")
done
echo "  submitted: ${JOBS[*]}"

# These jobs have to be admitted before they can run, and the suite runs this scenario alongside
# a dozen others competing for the same accelerators — so the wait is admission plus the run, not
# a flat number that happened to be enough on an idle cluster.
TERMINAL_BUDGET=$(( ADMISSION_BUDGET_SECONDS + RUN_SECONDS + 30 ))
ALL_TERMINAL=0
for i in $(seq 1 "$TERMINAL_BUDGET"); do
  n=0
  for J in "${JOBS[@]}"; do
    case "$(get_status "$J")" in COMPLETED|FAILED|EVICTED|REJECTED) n=$((n + 1));; esac
  done
  [[ "$n" -ge "${#JOBS[@]}" ]] && { ALL_TERMINAL=1; break; }
  [[ "$i" -lt "$TERMINAL_BUDGET" ]] && sleep 1
done
[[ "$ALL_TERMINAL" == "1" ]] \
  && pass "all ${#JOBS[@]} jobs reached a terminal state" \
  || fail "not all jobs reached a terminal state within ${TERMINAL_BUDGET}s"

stages_json() { curl -sf "$API_URL/platform-experiments/${PE_ID}/stages"; }
get_current_stage() { stages_json | py "import sys,json; print(json.load(sys.stdin)['current_stage'])"; }
advanced() { [[ "$(get_current_stage)" -ge 2 ]]; }
# The boundary trips on observed consumption, so its arrival is bounded by how quickly the jobs
# are admitted and run — which under a loaded suite is a good deal later than on an idle cluster.
# Budgeted off the admission budget rather than a flat minute, so a slow cluster reads as slow
# rather than as a broken ladder.
if wait_until "first stage boundary" $((ADMISSION_BUDGET_SECONDS + 90)) 1 advanced; then
  pass "platform experiment advanced past its first stage boundary"
  ST=$(stages_json)
	ACTIVE_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['active_agents']))")
	CUT_N=$(echo "$ST" | py "import sys,json; print(len(json.load(sys.stdin)['cut_agents']))")
  echo "  stage $(get_current_stage): active=${ACTIVE_N} cut=${CUT_N} (of ${#AGENTS[@]} agents)"
  TOTAL_N=$((ACTIVE_N + CUT_N))
  [[ "$TOTAL_N" -eq "${#AGENTS[@]}" ]] \
    && pass "active+cut agent counts are consistent with the ${#AGENTS[@]} signed-up agents" \
    || fail "active($ACTIVE_N)+cut($CUT_N) agents don't add up against ${#AGENTS[@]} signed-up agents"
  # 3 agents is below the guardrail floor, so the boundary must advance without cutting anyone.
  [[ "$CUT_N" -eq 0 ]] \
    && pass "no agent cut with only ${#AGENTS[@]} survivors (guardrail floor)" \
    || fail "cut $CUT_N agent(s) despite only ${#AGENTS[@]} survivors"
else
  fail "stage boundary not observed within timeout"
fi

all_jobs_settled() {
  local job
  for job in "${JOBS[@]}"; do
    [[ -n "$(get_field "$job" quota_settled_at)" ]] || return 1
  done
}
wait_until "all terminal jobs are durably settled" 30 1 all_jobs_settled || true

for J in "${JOBS[@]}"; do
  [[ -n "$(get_field "$J" quota_settled_at)" ]] \
    && pass "$J: quota durably settled" \
    || fail "$J: quota_settled_at is not set after grace period — settlement did not complete"

  M=$(dashboard_metrics "$J")
	COUNT=$(echo "$M" | py "import sys,json; d=json.load(sys.stdin); assert isinstance(d,list); print(len(d))")
  JSTATUS=$(get_status "$J")
  if [[ "$JSTATUS" == "COMPLETED" ]]; then
    [[ "$COUNT" -ge 1 ]] \
      && pass "$J: $COUNT metric point(s) via registry-service" \
      || fail "$J: COMPLETED but reported 0 metric points via registry-service"
  else
    echo "  $J ($JSTATUS): $COUNT metric point(s) via registry-service"
  fi
done

close_platform_experiment "$PE_ID"
finish
