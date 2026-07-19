#!/usr/bin/env bash
# HTTP-facing helpers, reused by every scenario. Requires common.sh already sourced.

register_agent() {
  local agent="$1"
  curl -sf -X POST "$QUOTA_URL/agents" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$agent\",\"name\":\"$agent\"}" > /dev/null 2>&1 || true
}

# create_platform_experiment NAME BUDGET MAX_AGENTS [PHASE2_BOUNDARY] [REPORT_INTERVAL_SECONDS]
#                            [BUDGET_CPU_CORE_HOURS] -> prints PE_ID
create_platform_experiment() {
  local name="$1" budget="$2" max_agents="$3" phase2="${4:-0.90}" report_interval="${5:-10}" budget_cpu="${6:-0}"
  curl -sf -X POST "$QUOTA_URL/platform-experiments" -H 'Content-Type: application/json' -d "{
    \"name\": \"$name\",
    \"budget_accelerator_hours\": $budget,
    \"budget_cpu_core_hours\": $budget_cpu,
    \"max_agents\": $max_agents,
    \"metrics\": [{\"key\": \"val_accuracy\", \"direction\": \"maximize\"}],
    \"phase2_boundary\": $phase2,
    \"report_interval_seconds\": $report_interval
  }" | py "import sys,json; print(json.load(sys.stdin)['id'])"
}

# signup_and_start PE_ID AGENT...
signup_and_start() {
  local pe_id="$1"; shift
  local agent
  for agent in "$@"; do
    curl -sf -X POST "$QUOTA_URL/platform-experiments/${pe_id}/signup" -H 'Content-Type: application/json' \
      -d "{\"agent_id\":\"$agent\"}" > /dev/null
  done
  curl -sf -X POST "$QUOTA_URL/platform-experiments/${pe_id}/start" > /dev/null
}

close_platform_experiment() {
  curl -sf -X POST "$QUOTA_URL/platform-experiments/$1/close" > /dev/null 2>&1 || true
}

# register_hypothesis AGENT PE_ID [TEXT] -> prints hypothesis id
register_hypothesis() {
  local agent="$1" pe_id="$2" text="${3:-}"
  curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
    -d "$(python3 "$LIB_DIR/mk_body.py" hyp "$agent" "$pe_id" "$text")" \
    | py "import sys,json; print(json.load(sys.stdin)['id'])"
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
#                [PROJECT_ID] [THEORY] [OBJECTIVE] [HYP_TEXT] [JOB_OVERRIDE_JSON]
#                -> prints job id on stdout
# Full-control variant used by scenarios that need to inject env vars (e.g. the robotics
# workload's OPENRESEARCH_LEARNING_RATE), a custom hypothesis/theory, or override raw job
# fields (cpu/storage/accelerator_count/... — see mk_body.py's JOB_OVERRIDE_JSON doc) without hand-
# rolling their own JSON body.
submit_job_ext() {
  local pe_id="$1" agent="$2" tier="$3" hours="$4" job_file="$5" env_json="${6:-}" \
        accelerator_type="${7:-}" accelerator_count="${8:-}" num_nodes="${9:-}" \
        project_id="${10:-}" theory="${11:-}" objective="${12:-}" hyp_text="${13:-}" \
        job_override_json="${14:-}"
  local job_id body
  job_id=$(_mk_job_id)
  body=$(_mk_submit_body "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$tier" \
    "$accelerator_type" "$accelerator_count" "$num_nodes" "$env_json" "$project_id" "$theory" "$objective" "$hyp_text" "$job_override_json")
  # Check explicitly: bash only honors -e for a command substitution's own last command, so a
  # failed curl here would not trip `set -e` on the caller's `X=$(submit_job ...)` — it would
  # silently hand back a job ID that was never created. Retry only transport-level failures
  # (connection refused/reset/timeout under bursty concurrent-scenario load) — curl exit code
  # 0 with a 4xx/5xx is a real, meaningful admission rejection and must fail immediately, not
  # get masked by retrying into the same rejection a few seconds later.
  local i curl_rc http_code resp
  resp="/tmp/submit_job_ext.$$.json"
  for i in 1 2 3; do
    http_code=$(curl -s -o "$resp" -w '%{http_code}' -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$body")
    curl_rc=$?
    if [[ "$curl_rc" == 0 && "$http_code" -lt 400 ]]; then rm -f "$resp"; echo "$job_id"; return 0; fi
    # Distinguish a real HTTP-level rejection (4xx/5xx, curl itself succeeded) from a transport
    # failure (curl_rc!=0: connection refused/reset/timeout under bursty concurrent-scenario
    # load) — only the latter is worth retrying; retrying an admission rejection just wastes
    # time reproducing the same rejection.
    [[ "$curl_rc" == 0 ]] && break
    sleep "$i"
  done
  echo "submit_job: POST /experiments failed for $job_id (agent=$agent tier=$tier, curl_rc=$curl_rc http_code=$http_code body=$(cat "$resp" 2>/dev/null))" >&2
  rm -f "$resp"
  return 1
}

_mk_job_id() { echo "job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_ID}"; }

_mk_submit_body() {
  local job_id="$1" agent="$2" pe_id="$3" hours="$4" job_file="$5" tier="$6" \
        accelerator_type="$7" accelerator_count="$8" num_nodes="$9" env_json="${10}" \
        project_id="${11}" theory="${12}" objective="${13}" hyp_text="${14}" job_override_json="${15}"
  local hyp_id
  hyp_id=$(register_hypothesis "$agent" "$pe_id" "$hyp_text")
  python3 "$LIB_DIR/mk_body.py" submit "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$hyp_id" "$tier" \
    "$accelerator_type" "$accelerator_count" "$num_nodes" "$env_json" "$project_id" "$theory" "$objective" "$job_override_json"
}

# submit_job_expect_code PE_ID AGENT TIER HOURS JOB_OVERRIDE_JSON [JOB_FILE]
#   -> prints "HTTP_CODE JOB_ID" on stdout (two space-separated fields)
# For scenarios asserting on the exact admission-time HTTP status (e.g. a malformed/impossible
# request rejected at submission vs. accepted-but-never-scheduled) rather than just "did it
# succeed" — callers do: read -r code job_id <<< "$(submit_job_expect_code ...)"
submit_job_expect_code() {
  local pe_id="$1" agent="$2" tier="$3" hours="$4" job_override_json="${5:-}" job_file="${6:-$JOB_FILE}"
  local job_id body code resp
  job_id=$(_mk_job_id)
  body=$(_mk_submit_body "$job_id" "$agent" "$pe_id" "$hours" "$job_file" "$tier" "" "" "" "" "" "" "" "" "$job_override_json")
  resp="/tmp/submit_job_expect_code.$$.json"
  code=$(curl -s -o "$resp" -w '%{http_code}' -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$body")
  rm -f "$resp"
  echo "$code $job_id"
}

get_status() { curl -sf "$SCHED_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin).get('status','UNKNOWN'))" 2>/dev/null || echo UNKNOWN; }
get_field()  { curl -sf "$SCHED_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin).get('$2',''))" 2>/dev/null || echo ""; }

# wait_for_status ID WANT_COMMA_LIST [TRIES] -> prints final status, returns 1 on timeout
wait_for_status() {
  local id="$1" want="$2" tries="${3:-30}"
  local s
  for ((i = 1; i <= tries; i++)); do
    s=$(get_status "$id")
    if [[ ",$want," == *",$s,"* ]]; then echo "$s"; return 0; fi
    sleep 1
  done
  echo "$(get_status "$id")"
  return 1
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
  curl -sf "$QUOTA_URL/platform-experiments/$1/quotas" \
    | py "
import sys, json
for q in json.load(sys.stdin):
    print(f\"    {q['agent_id']}: guaranteed={q.get('guaranteed_accelerator_hours',0):.4f} burst={q.get('burst_accelerator_hours',0):.4f}\")
" 2>/dev/null || true
}

quota_used_guaranteed() { _quota_field "$1" "$2" used_guaranteed_acch; }
quota_used_guaranteed_cpu() { _quota_field "$1" "$2" used_guaranteed_cpu_core_h; }
quota_guaranteed_cpu_hours() { _quota_field "$1" "$2" guaranteed_cpu_core_hours; }

_quota_field() {
  curl -sf "$QUOTA_URL/platform-experiments/$1/quotas" | py "
import sys, json
for q in json.load(sys.stdin):
    if q['agent_id'] == '$2':
        print(q.get('$3', 0))
"
}

# file_finding JOB_ID [SUMMARY] -> writes the required post-run summary for a COMPLETED job.
# Only COMPLETED jobs gate future submissions from the same agent+PE (FAILED/EVICTED/REJECTED
# are exempt) — scenarios that submit >1 job for the same agent+PE must call this after any
# job they wait to COMPLETED, or the next submit_job for that agent+PE gets 403 summary_required.
file_finding() {
  local job_id="$1" summary="${2:-e2e scenario finding for $1}" i
  for i in 1 2 3; do
    curl -sf -X POST "$SCHED_URL/experiments/${job_id}/summary" -H 'Content-Type: application/json' \
      -d "{\"summary\": \"$summary\"}" > /dev/null 2>&1 && return 0
    sleep 1
  done
  echo "file_finding: POST .../summary failed for $job_id after 3 attempts — next submission from its agent will 403" >&2
  return 1
}

dashboard_metrics() {
  # Exercises the same endpoint controlplane/ui's fetchExperimentMetrics calls.
  curl -sf "${REGISTRY_URL}/registry/experiments/$1/metrics" 2>/dev/null || echo "[]"
}
