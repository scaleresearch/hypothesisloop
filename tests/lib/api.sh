#!/usr/bin/env bash
# HTTP-facing helpers, reused by every scenario. Requires common.sh already sourced.

register_agent() {
  local agent="$1"
  curl -sf -X POST "$API_URL/agents" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$agent\",\"name\":\"$agent\"}" > /dev/null
}

# create_platform_experiment NAME BUDGET MAX_AGENTS [REPORT_INTERVAL_SECONDS]
#                            [BUDGET_CPU_CORE_HOURS] [METRICS_JSON] [STAGES_JSON] -> prints PE_ID
# STAGES_JSON is the elimination ladder (docs/stages.md); omitted means the platform default from
# config stages.default. starts_at/ends_at are required by the Huma schema but the zero value is
# treated downstream as "start immediately"/"never expires" — with both zero the ladder advances
# on budget alone, which is what every scenario here wants.
#
# METRICS_JSON must name metrics the scenario's workload actually emits — a declared metric
# nobody pushes yields no admission data, and a metric the workload pushes but nobody declares
# is never ranked on. It defaults to val_accuracy, which is what tests/workloads/generic/train.py
# emits (HYPOTHESISLOOP_PRIMARY_METRIC). Hardware-benchmark workloads emit their own names and
# must pass this explicitly — see tests/scenarios/tenstorrent-hardware.sh.
create_platform_experiment() {
  local name="$1" budget="$2" max_agents="$3" report_interval="${4:-10}" budget_cpu="${5:-0}"
  # Assigned in two steps, not via ${6:-<json>}: a literal '}' inside a default-value expansion
  # terminates the expansion early, silently corrupting the JSON. The empty default here is safe
  # for exactly that reason (no '}' in it) and is required — under `set -u` a bare "$6" aborts
  # the whole scenario for every caller that omits the argument.
  local metrics="${6-}"
  [[ -z "$metrics" ]] && metrics='[{"key": "val_accuracy", "direction": "maximize"}]'
  # Same two-step reason as metrics above: a literal '}' can't live in a default-value expansion.
  local stages="${7-}"
  local stages_field=""
  [[ -n "$stages" ]] && stages_field="\"stages\": $stages,"
  curl -sf -X POST "$API_URL/platform-experiments" -H 'Content-Type: application/json' -d "{
    $stages_field
    \"name\": \"$name\",
    \"description\": \"$name\",
    \"budget_accelerator_hours\": $budget,
    \"budget_cpu_core_hours\": $budget_cpu,
    \"max_agents\": $max_agents,
    \"metrics\": $metrics,
    \"report_interval_seconds\": $report_interval,
    \"starts_at\": \"0001-01-01T00:00:00Z\",
    \"ends_at\": \"0001-01-01T00:00:00Z\"
  }" | py "import sys,json; print(json.load(sys.stdin)['id'])" | tee -a "$CREATED_PE_LOG"
}

# signup_agent PE_ID AGENT [ROLE] -> signs one agent up, failing the call on any HTTP error.
# ROLE is competitor (default, and what every pre-roles caller means), baseline or reviewer; it is
# fixed at signup and there is no path to change it. An omitted ROLE sends no `role` field at all,
# so this stays byte-identical to the request shape the API has always accepted.
signup_agent() {
  local pe_id="$1" agent="$2" role="${3:-}"
  local body="{\"agent_id\":\"$agent\"}"
  [[ -n "$role" ]] && body="{\"agent_id\":\"$agent\",\"role\":\"$role\"}"
  curl -sf -X POST "$API_URL/platform-experiments/${pe_id}/signup" -H 'Content-Type: application/json' \
    -d "$body" > /dev/null
}

# signup_agent_code PE_ID AGENT ROLE -> prints "HTTP_CODE REASON" (two space-separated fields;
# REASON is "-" when the response carries no reason field). For scenarios asserting that a bad
# signup is refused with the documented machine-readable reason rather than merely "not a 2xx" —
# an unrecognized role must fail fast, never silently default to competitor.
signup_agent_code() {
  local pe_id="$1" agent="$2" role="$3" resp code reason
  resp=$(mktemp)
  code=$(curl -s -o "$resp" -w '%{http_code}' -X POST "$API_URL/platform-experiments/${pe_id}/signup" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"$agent\",\"role\":\"$role\"}")
  reason=$(py "
import json
try:
    print(json.load(open('$resp')).get('reason') or '-')
except Exception:
    print('-')
")
  rm -f "$resp"
  echo "$code $reason"
}

# signup_role PE_ID AGENT -> prints "HTTP_CODE ROLE" as read back from
# GET /platform-experiments/{id}/signups/{agent_id}. ROLE is "-" when the agent is not signed up
# (404) — "not signed up" is not a role, and a scenario must be able to tell the two apart.
signup_role() {
  local pe_id="$1" agent="$2" resp code role
  resp=$(mktemp)
  code=$(curl -s -o "$resp" -w '%{http_code}' "$API_URL/platform-experiments/${pe_id}/signups/${agent}")
  role=$(py "
import json
try:
    print(json.load(open('$resp')).get('role') or '-')
except Exception:
    print('-')
")
  rm -f "$resp"
  echo "$code $role"
}

# signup_and_start PE_ID AGENT...
signup_and_start() {
  local pe_id="$1"; shift
  local agent
  for agent in "$@"; do signup_agent "$pe_id" "$agent"; done
  curl -sf -X POST "$API_URL/platform-experiments/${pe_id}/start" > /dev/null
}

# signup_and_start_with_roles PE_ID AGENT:ROLE...
# Same as signup_and_start, but each agent carries an explicit role: "agent-x:baseline". A bare
# "agent-x" with no colon is signed up with no role field, i.e. the competitor default — so the
# two helpers agree on what an unqualified agent means.
signup_and_start_with_roles() {
  local pe_id="$1"; shift
  local spec agent role
  for spec in "$@"; do
    agent="${spec%%:*}"
    role=""
    [[ "$spec" == *:* ]] && role="${spec#*:}"
    signup_agent "$pe_id" "$agent" "$role"
  done
  curl -sf -X POST "$API_URL/platform-experiments/${pe_id}/start" > /dev/null
}

# close_platform_experiment PE_ID -> closes with no final results to report (top_results: [])
close_platform_experiment() {
  curl -sf -X POST "$API_URL/platform-experiments/$1/close" -H 'Content-Type: application/json' \
    -d '{"top_results":[]}' > /dev/null
}

cancel_job() {
  curl -sf -X POST "$API_URL/experiments/$1/cancel" > /dev/null
}

# post_hypothesis AGENT AUTHOR PE_ID [TEXT] -> prints the full POST /hypotheses response body.
# Exactly one of AGENT/AUTHOR is meant to be set (the API rejects both/neither with 400) — pass
# "" for the one that does not apply. Use this where the scenario asserts on more than the id
# (already_existed, source, author); register_hypothesis / register_human_hypothesis wrap it for
# the common "just give me the id" case.
post_hypothesis() {
  local agent="$1" author="$2" pe_id="$3" text="${4:-}"
  curl -sf -X POST "$API_URL/hypotheses" -H 'Content-Type: application/json' \
    -d "$(python3 "$LIB_DIR/mk_body.py" hyp "$agent" "$pe_id" "$text" "$author")"
}

# post_hypothesis_code AGENT AUTHOR PE_ID [TEXT] -> prints the HTTP status code only.
# For asserting that a bad origin pair (both agent_id and author, or neither) is refused at
# registration rather than defaulted into a row some later read has to interpret.
post_hypothesis_code() {
  local agent="$1" author="$2" pe_id="$3" text="${4:-}" code
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API_URL/hypotheses" \
    -H 'Content-Type: application/json' \
    -d "$(python3 "$LIB_DIR/mk_body.py" hyp "$agent" "$pe_id" "$text" "$author")")
  echo "$code"
}

# register_hypothesis AGENT PE_ID [TEXT] -> prints hypothesis id
register_hypothesis() {
  local agent="$1" pe_id="$2" text="${3:-}"
  post_hypothesis "$agent" "" "$pe_id" "$text" | py "import sys,json; print(json.load(sys.stdin)['id'])"
}

# register_human_hypothesis AUTHOR PE_ID TEXT -> prints hypothesis id
# The human-submitted sibling of register_hypothesis: no agent owns the row, the typed name is
# carried in `author`. Same pool, same dedup — see domain.HypothesisSource.
register_human_hypothesis() {
  local author="$1" pe_id="$2" text="${3:-}"
  post_hypothesis "" "$author" "$pe_id" "$text" | py "import sys,json; print(json.load(sys.stdin)['id'])"
}

# post_hypothesis_comment_code HYP_ID AGENT AUTHOR TEXT -> prints the HTTP status code.
# Same exactly-one-of AGENT/AUTHOR rule as registration; pass "" for the one that does not apply.
post_hypothesis_comment_code() {
  local hyp_id="$1" agent="$2" author="$3" text="$4"
  curl -s -o /dev/null -w '%{http_code}' -X POST "$API_URL/hypotheses/${hyp_id}/comments" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\": \"$agent\", \"author\": \"$author\", \"text\": \"$text\"}"
}

# set_hypothesis_status_code HYP_ID AGENT STATUS -> prints the HTTP status code.
# Status is owner-only (403 for a non-owner, and a human row has no owner at all), so a scenario
# asserting the guard needs the code, not a curl that dies on it.
set_hypothesis_status_code() {
  local hyp_id="$1" agent="$2" status="$3"
  curl -s -o /dev/null -w '%{http_code}' -X POST "$API_URL/hypotheses/${hyp_id}/status" \
    -H 'Content-Type: application/json' -d "{\"agent_id\": \"$agent\", \"status\": \"$status\"}"
}

# experiment_http_code JOB_ID -> prints the HTTP status code of GET /experiments/{id}, so a
# scenario can tell "refused at submission, nothing persisted" (404) from "accepted" (200).
experiment_http_code() {
  curl -s -o /dev/null -w '%{http_code}' "$API_URL/experiments/$1"
}

# submit_job PE_ID AGENT TIER [HOURS] [ACCELERATOR_TYPE] [ACCELERATOR_COUNT] [NUM_NODES] [JOB_FILE]
#            [JOB_OVERRIDE_JSON] -> prints job id on stdout
submit_job() {
  local pe_id="$1" agent="$2" tier="$3" hours="${4:-0.02}" accelerator_type="${5:-}" accelerator_count="${6:-}" \
        num_nodes="${7:-}" job_file="${8:-$JOB_FILE}" job_override_json="${9:-}"
  submit_job_ext "$pe_id" "$agent" "$tier" "$hours" "$job_file" "" "$accelerator_type" "$accelerator_count" "$num_nodes" \
    "" "" "" "" "$job_override_json"
}

# submit_job_ext PE_ID AGENT TIER HOURS JOB_FILE ENV_JSON [ACCELERATOR_TYPE] [ACCELERATOR_COUNT] [NUM_NODES]
#                [PROJECT_ID] [THEORY] [OBJECTIVE] [HYP_TEXT] [JOB_OVERRIDE_JSON] [METADATA_OVERRIDE_JSON]
#                -> prints job id on stdout
# Full-control variant used by scenarios that need to inject env vars, a custom
# hypothesis/theory, or override raw job fields (cpu/storage/accelerator_count/... — see
# mk_body.py's JOB_OVERRIDE_JSON doc) without hand-rolling their own JSON body.
submit_job_ext() {
  local pe_id="$1" agent="$2" tier="$3" hours="$4" job_file="$5" env_json="${6:-}" \
        accelerator_type="${7:-}" accelerator_count="${8:-}" num_nodes="${9:-}" \
        project_id="${10:-}" theory="${11:-}" objective="${12:-}" hyp_text="${13:-}" \
        job_override_json="${14:-}" metadata_override_json="${15:-}"
  local job_id body
  job_id=$(_mk_job_id)
  if ! body=$(_mk_submit_body "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$tier" \
    "$accelerator_type" "$accelerator_count" "$num_nodes" "$env_json" "$project_id" "$theory" "$objective" "$hyp_text" "$job_override_json" \
    "" "$metadata_override_json"); then
    echo "submit_job: failed to build request body for $job_id (agent=$agent tier=$tier)" >&2
    return 1
  fi
  # One request, one result. A transport or HTTP failure is the test failure; retrying would
  # hide an unreliable control plane and make concurrent tests nondeterministic.
  local http_code resp
  resp=$(mktemp)
  if ! http_code=$(curl -sS -o "$resp" -w '%{http_code}' -X POST "$API_URL/experiments" -H 'Content-Type: application/json' -d "$body"); then
    echo "submit_job: POST /experiments transport failure for $job_id (agent=$agent tier=$tier)" >&2
    rm -f "$resp"
    return 1
  fi
  if [[ "$http_code" -ge 400 ]]; then
    echo "submit_job: POST /experiments failed for $job_id (agent=$agent tier=$tier, http_code=$http_code body=$(<"$resp"))" >&2
    rm -f "$resp"
    return 1
  fi
  rm -f "$resp"
  echo "$job_id"
}

_mk_job_id() { echo "job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_ID}"; }

# mk_submit_body_for_id ID PE_ID AGENT TIER [HOURS] -> prints the request body for POST
# /experiments. For scenarios that must control the experiment id, or fire one prepared body
# several times over -- e.g. to submit the same id concurrently and assert the platform keeps
# exactly one of them.
mk_submit_body_for_id() {
  local job_id="$1" pe_id="$2" agent="$3" tier="$4" hours="${5:-0.02}"
  _mk_submit_body "$job_id" "$agent" "$pe_id" "$hours" "$JOB_FILE" "$tier" "" "" "" "" "" "" "" "" ""
}

# post_experiment_body BODY -> prints the HTTP status code.
post_experiment_body() {
  local body="$1" resp code
  resp=$(mktemp)
  code=$(curl -s -o "$resp" -w '%{http_code}' -X POST "$API_URL/experiments" \
    -H 'Content-Type: application/json' -d "$body")
  rm -f "$resp"
  echo "$code"
}

_mk_submit_body() {
  local job_id="$1" agent="$2" pe_id="$3" hours="$4" job_file="$5" tier="$6" \
        accelerator_type="$7" accelerator_count="$8" num_nodes="$9" env_json="${10}" \
        project_id="${11}" theory="${12}" objective="${13}" hyp_text="${14}" job_override_json="${15}" \
        hyp_id="${16-}" metadata_override_json="${17-}"
  # An explicit hypothesis id points the job at a row this helper must NOT create: a human-owned
  # one, or another platform experiment's. Empty (every caller that doesn't care) keeps the
  # original behaviour of registering the agent's own row from hyp_text.
  [[ -z "$hyp_id" ]] && hyp_id=$(register_hypothesis "$agent" "$pe_id" "$hyp_text")
  python3 "$LIB_DIR/mk_body.py" submit "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$hyp_id" "$tier" \
    "$accelerator_type" "$accelerator_count" "$num_nodes" "$env_json" "$project_id" "$theory" "$objective" "$job_override_json" \
    "$metadata_override_json"
}

# experiment_data_http JOB_ID -> prints "HTTP_CODE OBJECT_COUNT" for GET /experiments/{id}/data
# (two space-separated fields; OBJECT_COUNT is "-" when the body is not a JSON array).
# A job that wrote nothing lists as an empty array, which is a different answer from an error —
# a scenario that only checked "not 2xx" could not tell the two apart.
experiment_data_http() {
  local resp code count
  resp=$(mktemp)
  code=$(curl -s -o "$resp" -w '%{http_code}' "$API_URL/experiments/$1/data")
  count=$(py "
import json
try:
    body = json.load(open('$resp'))
    print(len(body) if isinstance(body, list) else '-')
except Exception:
    print('-')
")
  rm -f "$resp"
  echo "$code $count"
}

# experiment_data_keys JOB_ID -> every object key the job left under its own prefix, one per line.
experiment_data_keys() {
  curl -sf "$API_URL/experiments/$1/data" | py "
import sys, json
for obj in json.load(sys.stdin) or []:
    print(obj.get('key'))
"
}

# lineage_ids JOB_ID -> the experiment ids of the lineage chain, oldest first, one per line.
lineage_ids() {
  curl -sf "$API_URL/experiments/$1/lineage" | py "
import sys, json
for exp in json.load(sys.stdin) or []:
    print(exp.get('id'))
"
}

# submit_job_expect_code PE_ID AGENT TIER HOURS JOB_OVERRIDE_JSON [JOB_FILE] [HYP_ID]
#   -> prints "HTTP_CODE JOB_ID" on stdout (two space-separated fields)
# For scenarios asserting on the exact admission-time HTTP status (e.g. a malformed/impossible
# request rejected at submission vs. accepted-but-never-scheduled) rather than just "did it
# succeed" — callers do: read -r code job_id <<< "$(submit_job_expect_code ...)"
# HYP_ID submits against an already-registered hypothesis instead of registering one for the
# agent — how a scenario points a job at a row the agent does not own (e.g. a human-submitted
# idea, which must be refused).
submit_job_expect_code() {
  local pe_id="$1" agent="$2" tier="$3" hours="$4" job_override_json="${5:-}" job_file="${6:-$JOB_FILE}" \
        hyp_id="${7:-}"
  local job_id body code resp
  job_id=$(_mk_job_id)
  if ! body=$(_mk_submit_body "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$tier" "" "" "" "" "" "" "" "" "$job_override_json" "$hyp_id"); then
    echo "submit_job_expect_code: failed to build request body for $job_id (agent=$agent tier=$tier)" >&2
    return 1
  fi
  resp="/tmp/submit_job_expect_code.$$.json"
  code=$(curl -s -o "$resp" -w '%{http_code}' -X POST "$API_URL/experiments" -H 'Content-Type: application/json' -d "$body")
  rm -f "$resp"
  echo "$code $job_id"
}

get_status() { curl -sf "$API_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin)['status'])"; }
get_field()  { curl -sf "$API_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin).get('$2',''))"; }

# wait_for_status ID WANT_COMMA_LIST [TRIES] -> prints final status, returns 1 on timeout
wait_for_status() {
  local id="$1" want="$2" tries="${3:-30}"
  local s
  # Clamped to the scenario's remaining ceiling (see scenario_seconds_left): a wait that outlives
  # the suite's own timeout gets the scenario killed mid-assertion instead of reporting anything.
  tries=$(budgeted_tries "$tries" 1)
  if [[ "$tries" -le 0 ]]; then
    budget_exhausted "$id to reach {$want}"
    get_status "$id"
    return 1
  fi
  for ((i = 1; i <= tries; i++)); do
    s=$(get_status "$id")
    if [[ ",$want," == *",$s,"* ]]; then echo "$s"; return 0; fi
    [[ "$i" -lt "$tries" ]] && sleep 1
  done
  echo "$(get_status "$id")"
  return 1
}

# completion_wait_tries JOB_HOURS [MARGIN_SECONDS] -> prints a completion-wait budget (secs)
# sized from the job's own known duration, not a hand-tuned absolute constant. Use this only for
# a wait started right when the job is already confirmed RUNNING (fresh timestamp), never from
# submission time, or the queueing delay it isn't meant to cover leaks back in.
completion_wait_tries() {
  local job_hours="$1" margin="${2:-25}"
  py "import math; print(math.ceil(float('$job_hours') * 3600) + $margin)"
}

# wait_for_completion_after_running ID JOB_HOURS ADMISSION_BUDGET [MARGIN_SECONDS]
#   -> prints final status, returns 1 on timeout (same convention as wait_for_status)
# Two-phase wait: admission (unpredictable queueing/scheduling delay, bounded by
# ADMISSION_BUDGET, generous and not per-scenario-tuned) then completion (the job's own known
# duration + a small fixed margin, timed fresh from confirmed RUNNING rather than submission).
wait_for_completion_after_running() {
  local id="$1" job_hours="$2" admission_budget="$3" margin="${4:-25}"
  local s rc=0
  s=$(wait_for_status "$id" "RUNNING,COMPLETED,FAILED,EVICTED" "$admission_budget") || rc=$?
  [[ "$s" != "RUNNING" ]] && { echo "$s"; return "$rc"; }
  wait_for_status "$id" "COMPLETED,FAILED,EVICTED" "$(completion_wait_tries "$job_hours" "$margin")"
}

# assert_stable_status ID ALLOWED_COMMA_LIST DURATION_SECS DESC -> polls every second for the
# full duration instead of a single fixed sleep+sample, so a transient flip outside ALLOWED
# (even one that self-corrects before the window ends) still gets caught. Used to verify
# invariants like "stays terminal" / "undisturbed by X" where there's no single readiness
# event to wait for — the assertion itself *is* "nothing changes for this long".
assert_stable_status() {
  local id="$1" allowed="$2" duration="$3" desc="$4" s
  for ((i = 0; i < duration; i++)); do
    s=$(get_status "$id")
    if [[ ",$allowed," != *",$s,"* ]]; then
      fail "$desc: left {$allowed} after ${i}s (status=$s)"
      return 1
    fi
    sleep 1
  done
  pass "$desc: stayed in {$allowed} for the full ${duration}s"
}

quota_snapshot() {
  curl -sf "$API_URL/platform-experiments/$1/quotas" \
    | py "
import sys, json
for q in json.load(sys.stdin):
    print(f\"    {q['agent_id']}: guaranteed={q.get('guaranteed_accelerator_hours',0):.4f} burst={q.get('burst_accelerator_hours',0):.4f}\")
"
}

quota_used_guaranteed() { _quota_field "$1" "$2" used_guaranteed_acch; }
quota_used_guaranteed_cpu() { _quota_field "$1" "$2" used_guaranteed_cpu_core_h; }
quota_guaranteed_cpu_hours() { _quota_field "$1" "$2" guaranteed_cpu_core_hours; }

_quota_field() {
  curl -sf "$API_URL/platform-experiments/$1/quotas" | py "
import sys, json
for q in json.load(sys.stdin):
    if q['agent_id'] == '$2':
        print(q.get('$3', 0))
"
}

# experiment_stats PE_ID AGENT -> the raw GET /experiments/stats body for one agent's jobs under
# one platform experiment. Counted in PostgreSQL over the whole matching set, so a scenario never
# has to page rows to total anything. Only EVICTED rows carry an eviction reason, so the
# evictions_* breakdowns below say nothing about a FAILED or COMPLETED job.
experiment_stats() {
  curl -sf "$API_URL/experiments/stats?agent=$2&platform_experiment_id=$1"
}

# eviction_class_count PE_ID AGENT CLASS -> how many of that agent's EVICTED jobs under PE_ID were
# that fault class (workload / infrastructure / policy / unclassified), or "-" when the response
# carries no evictions_by_class at all.
#
# "-" and 0 are deliberately different answers. Absent means this control plane does not attribute
# faults; zero means it does and put none of them in this class. A scenario asserting "none of my
# failures were my own fault" that accepted the first as the second would pass just as happily
# against a build with no attribution in it, which is the one thing it exists to catch.
eviction_class_count() {
  experiment_stats "$1" "$2" | py "
import sys, json
by_class = json.load(sys.stdin).get('evictions_by_class')
print('-' if by_class is None else by_class.get('$3', 0))
"
}

# eviction_class_coverage PE_ID AGENT -> prints "CLASS_TOTAL REASON_TOTAL UNCLASSIFIED" (three
# space-separated integers) for that agent's evictions under PE_ID.
#
# A class is a pure function of a reason, so the first two must always be equal and the third
# always zero: a reason this build cannot classify is counted as unclassified rather than dropped,
# precisely so "no infrastructure failures" and "failures we could not classify" cannot look the
# same to a reader. Callers assert on all three — the totals alone would still match if every
# reason fell through to unclassified.
eviction_class_coverage() {
  experiment_stats "$1" "$2" | py "
import sys, json
d = json.load(sys.stdin)
by_class = d.get('evictions_by_class') or {}
by_reason = d.get('evictions_by_reason') or {}
print(sum(by_class.values()), sum(by_reason.values()), by_class.get('unclassified', 0))
"
}

# file_finding JOB_ID [SUMMARY] -> writes the required post-run summary for a COMPLETED job.
# Only COMPLETED jobs gate future submissions from the same agent+PE (FAILED/EVICTED/REJECTED
# are exempt) — scenarios that submit >1 job for the same agent+PE must call this after any
# job they wait to COMPLETED, or the next submit_job for that agent+PE gets 403 summary_required.
file_finding() {
  local job_id="$1" summary="${2:-e2e scenario finding for $1}"
  curl -sf -X POST "$API_URL/experiments/${job_id}/summary" -H 'Content-Type: application/json' \
    -d "{\"summary\": \"$summary\"}" > /dev/null
}

dashboard_metrics() {
  # Exercises the same endpoint controlplane/ui's fetchExperimentMetrics calls.
  curl -sf "${API_URL}/experiments/$1/metrics"
}

# metric_values JOB_ID METRIC_NAME -> prints every recorded value of that metric, one per line,
# in the order the store returns them. Empty output means the metric was never reported, which is
# a different (and usually more interesting) failure than a wrong value — so callers should check
# for it explicitly rather than comparing against an empty string.
metric_values() {
  dashboard_metrics "$1" | py "
import sys, json
points = json.load(sys.stdin) or []
for p in points:
    if p.get('metric_name') == '$2':
        print(p.get('metric_value'))
"
}

# metric_max JOB_ID METRIC_NAME -> prints the largest recorded value, or nothing at all if the
# metric was never reported. Used where a job reports the same fact from several ranks, or across
# several attempts, and the scenario wants one number out of it.
metric_max() {
  local values
  values=$(metric_values "$1" "$2")
  [[ -z "$values" ]] && return 0
  echo "$values" | py "
import sys
print(max(float(line) for line in sys.stdin if line.strip()))
"
}

# metric_distinct_count JOB_ID METRIC_NAME -> how many distinct values of a metric were recorded.
# The way a scenario counts gang attempts: each attempt reports its own index under the same
# experiment id, so the number of distinct values IS the number of attempts that ran.
metric_distinct_count() {
  metric_values "$1" "$2" | py "
import sys
print(len({line.strip() for line in sys.stdin if line.strip()}))
"
}
