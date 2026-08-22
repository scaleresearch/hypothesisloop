#!/usr/bin/env bash
# A chain of jobs: one stage writes durable data, the next one reads it back.
#
# No other scenario submits a chain — job-lifecycle.sh follows a single job end to end, and
# nothing covers parent_id plus data handed from one stage to the next. What is asserted here:
#
#   1. stage A, a GROUPED job, writes a checkpoint under $HYPOTHESISLOOP_DATA_URI and completes.
#      Grouped deliberately: groups and cross-stage data are two features that have to COMPOSE,
#      and proving each alone would not say that the pair works.
#   2. stage B, single-node, parent_id A, finds that checkpoint under $HYPOTHESISLOOP_DATA_SHARED
#      and reports its contents as a metric — data crossing a job boundary, end to end.
#   3. GET /experiments/{id}/lineage returns the chain.
#   4. a write by B into A's prefix is REFUSED by the store, while the very same client, holding
#      the very same credentials, reads A's object and writes its own prefix successfully.
#   5. GET /experiments/{id}/data lists what A wrote, and returns an empty array — not an error —
#      for a job that wrote nothing.
#
# Nothing here asserts where either stage ran, on purpose. Data is addressed, not attached: if a
# later stage had to land where an earlier one did, adding a cluster would make the platform
# worse rather than bigger. A scenario that pinned placement would encode the opposite design.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

# A100. The suite runs its slow group concurrently and this scenario is in it (see run.sh): of the
# slow group, distributed-jobs holds H100 and everything else runs on the TEST_ACCELERATOR_TYPE
# default (L40), so A100 is the one flavor nothing else in the group competes for. The two
# scenarios that do use A100 — preemption-requeue and burst-fair-round-robin — are
# CLUSTER_EXCLUSIVE and run in their own phase, after this one has finished.
ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
A100_ACCH_RATE=0.375

# The estimate the cost/quota arithmetic is priced against; each stage's REAL runtime is set
# separately and kept short, because a chain is serial — every second stage A runs is a second
# stage B has not started yet, and nothing in this scenario is proven better by a longer run.
JOB_HOURS="0.02"
RUN_SECONDS=10

AGENT_A="agent-chain-write-${RUN_ID}"
AGENT_B="agent-chain-read-${RUN_ID}"
AGENT_NODATA="agent-chain-nodata-${RUN_ID}"
AGENTS=("$AGENT_A" "$AGENT_B" "$AGENT_NODATA")
for a in "${AGENTS[@]}"; do register_agent "$a"; done

# Every metric this scenario reads has to be declared, and each is a fact only the job itself can
# report: what it found in the parent's checkpoint, and what the store answered when it tried to
# write where it may not. val_accuracy stays the ranking metric, as in every other scenario.
CHAIN_METRICS='[{"key": "val_accuracy", "direction": "maximize"},
                {"key": "stage_rank", "direction": "maximize"},
                {"key": "checkpoint_write_status", "direction": "maximize"},
                {"key": "checkpoint_value", "direction": "maximize"},
                {"key": "session_token_present", "direction": "maximize"},
                {"key": "shared_read_status", "direction": "maximize"},
                {"key": "own_prefix_write_status", "direction": "maximize"},
                {"key": "foreign_write_status", "direction": "maximize"}]'
# Budget arithmetic: three jobs, one accelerator each that is actually billed — stage A's
# `writer` group holds 1 A100 and its two `helper` replicas hold none, stage B holds 1, the
# no-data job holds 1. Reserved: 3 x 0.02h x 0.375 = 0.0225 AccH; settled is smaller still, since
# each job really runs ~10s rather than the 72s it estimated. 1.0 AccH is ~44x that, which also
# keeps every submission far below the first stage boundary at 40% of budget, so the elimination
# ladder never advances mid-scenario and cannot refuse the next stage of the chain.
PE_ID=$(create_platform_experiment "multi-stage-chain-${RUN_ID}" 1.0 "${#AGENTS[@]}" 10 0 "$CHAIN_METRICS")
signup_and_start "$PE_ID" "${AGENTS[@]}"

# The value stage A writes and stage B must report back. Arbitrary, and that is the point: it
# exists nowhere in the platform, so the only way B can report it is by reading A's bytes.
CHECKPOINT_VALUE=4242

# Stage A's shape. Grouped, so the top-level per-node fields job.yaml carries must be DELETED
# (null pops the key in mk_body.py) — domain.JobSpec.ValidateGroups rejects a spec that states a
# thing twice. spread_across_hosts is off because this is a 3-node job and nothing here is about
# placement; leaving the default on would make the chain's first stage depend on how many hosts
# advertise A100, which is exactly the coupling this scenario exists to say does not matter.
stage_a_job() {
  cat <<JSON
{"command": ["python", "data_stage.py"],
 "max_retries": 0,
 "cpu": null, "memory": null, "storage": null, "accelerator_count": null,
 "topology": {"spread_across_hosts": false},
 "groups": [
   {"name": "writer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi", "accelerator_count": 1},
   {"name": "helper", "replicas": 2, "cpu": "100m", "memory": "64Mi",  "storage": "256Mi", "accelerator_count": 0}
 ]}
JSON
}

echo "=========================================================="
echo "Stage A: a grouped job writes a checkpoint to its own prefix"
echo "=========================================================="
# Submitted alongside the no-data job, which shares none of A's state and only has to reach
# COMPLETED — running the two concurrently keeps the scenario's serial length to the chain itself.
JOB_NODATA=$(submit_job_ext "$PE_ID" "$AGENT_NODATA" "guaranteed" "$JOB_HOURS" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "1" "")
echo "  submitted a job that writes nothing at all: $JOB_NODATA"

JOB_A=$(submit_job_ext "$PE_ID" "$AGENT_A" "guaranteed" "$JOB_HOURS" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\", \"STAGE_MODE\": \"write\", \"CHECKPOINT_VALUE\": \"${CHECKPOINT_VALUE}\"}" \
  "$ACCELERATOR_TYPE" "" "" "" "" "" "" "$(stage_a_job)")
echo "  submitted grouped stage A (writer x1 + helper x2): $JOB_A"

S=$(wait_for_completion_after_running "$JOB_A" "$JOB_HOURS" "$ADMISSION_BUDGET_SECONDS" "$(( RUN_SECONDS + 60 ))" || true)
if [[ "$S" != "COMPLETED" ]]; then
  fail "stage A did not complete (status=$S, eviction_reason=$(get_field "$JOB_A" eviction_reason), not_admitted_reason=$(get_field "$JOB_A" not_admitted_reason))"
  echo "  stage A never produced a checkpoint; the rest of the chain cannot say anything."
  close_platform_experiment "$PE_ID"
  finish
fi
pass "grouped stage A reached COMPLETED — every replica of both groups exited 0"
file_finding "$JOB_A" "multi-stage chain: grouped stage A wrote its checkpoint."

# Three nodes, three DISTINCT global ranks. A grouped pod is never handed RANK — a job-global rank
# is the group's offset plus the pod's index within its group, and Kubernetes cannot add — so this
# number is the workload adding the two terms the platform published. Were the group-local index
# used instead, the helper group would report ranks 0 and 1 again and this count would be 2.
RANKS=$(metric_distinct_count "$JOB_A" stage_rank)
[[ "$RANKS" -eq 3 ]] \
  && pass "stage A's 3 replicas reported 3 distinct job-global ranks across 2 groups" \
  || fail "stage A reported $RANKS distinct rank(s), want 3 — two pods in different groups share a rank"
TOP_RANK=$(metric_max "$JOB_A" stage_rank)
[[ "$(py "print(abs(float('${TOP_RANK:-0}') - 2.0) < 0.001)")" == "True" ]] \
  && pass "the highest rank is 2, so the ranks are 0..2 rather than three copies of one group's" \
  || fail "highest reported rank is ${TOP_RANK:-<never reported>}, want 2"

echo ""
echo "=========================================================="
echo "GET /experiments/{id}/data — what a job left behind"
echo "=========================================================="
read -r DATA_CODE DATA_COUNT <<< "$(experiment_data_http "$JOB_A")"
[[ "$DATA_CODE" == "200" && "$DATA_COUNT" == "3" ]] \
  && pass "stage A's prefix lists $DATA_COUNT objects, one per replica (HTTP $DATA_CODE)" \
  || fail "GET /experiments/$JOB_A/data returned HTTP $DATA_CODE with $DATA_COUNT object(s), want 200 and 3"
# Listed live from the store, so the keys are the real ones the job wrote, not a copy the control
# plane kept — a checkpoint nobody wrote cannot appear here.
CKPT0=$(experiment_data_keys "$JOB_A" | grep -c 'checkpoint-rank0\.txt$' || true)
[[ "$CKPT0" -eq 1 ]] \
  && pass "the listing names the rank-0 checkpoint stage B is about to read" \
  || fail "stage A's listing has $CKPT0 rank-0 checkpoint(s): $(experiment_data_keys "$JOB_A" | tr '\n' ' ')"

S=$(wait_for_status "$JOB_NODATA" "COMPLETED,FAILED,EVICTED" "$(( ADMISSION_BUDGET_SECONDS + RUN_SECONDS + 60 ))" || true)
if [[ "$S" == "COMPLETED" ]]; then
  file_finding "$JOB_NODATA" "multi-stage chain: a completed job that kept nothing."
  # Empty and error are different answers, and only one of them is right: keeping nothing is the
  # ordinary case for most jobs, so a 404 or a 500 here would make every such job look broken.
  read -r NODATA_CODE NODATA_COUNT <<< "$(experiment_data_http "$JOB_NODATA")"
  [[ "$NODATA_CODE" == "200" && "$NODATA_COUNT" == "0" ]] \
    && pass "a completed job that wrote nothing lists as an empty array (HTTP 200), not an error" \
    || fail "GET /experiments/$JOB_NODATA/data returned HTTP $NODATA_CODE with $NODATA_COUNT object(s), want 200 and 0"
else
  fail "the no-data job ended $S — nothing was learned about the empty listing"
fi

echo ""
echo "=========================================================="
echo "Stage B: parent_id A, reads A's checkpoint back"
echo "=========================================================="
# parent_id is metadata about how a result was derived, never part of how the job executes, so it
# rides in the request's metadata rather than its job spec.
JOB_B=$(submit_job_ext "$PE_ID" "$AGENT_B" "guaranteed" "$JOB_HOURS" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\", \"STAGE_MODE\": \"read\", \"STAGE_PARENT_ID\": \"${JOB_A}\"}" \
  "$ACCELERATOR_TYPE" "1" "" "" "" "" "" \
  '{"command": ["python", "data_stage.py"], "max_retries": 0}' \
  "{\"parent_id\": \"${JOB_A}\"}")
echo "  submitted single-node stage B with parent_id=$JOB_A: $JOB_B"

S=$(wait_for_completion_after_running "$JOB_B" "$JOB_HOURS" "$ADMISSION_BUDGET_SECONDS" "$(( RUN_SECONDS + 60 ))" || true)
if [[ "$S" == "COMPLETED" ]]; then
  pass "stage B completed"
  file_finding "$JOB_B" "multi-stage chain: stage B read its parent's checkpoint."

  # The whole of G9 in one number: 4242 exists nowhere in the platform's own records, so B can
  # only report it by having read the bytes A wrote — in another agent's prefix, from wherever it
  # was placed, over an address rather than a volume.
  VALUE=$(metric_max "$JOB_B" checkpoint_value)
  if [[ -z "$VALUE" ]]; then
    fail "stage B never reported checkpoint_value — it completed without reading its parent's data"
  elif [[ "$(py "print(abs(float('$VALUE') - ${CHECKPOINT_VALUE}.0) < 0.001)")" == "True" ]]; then
    pass "stage B read $VALUE out of stage A's checkpoint (G9: data crossed the job boundary)"
  else
    fail "stage B reported checkpoint_value=$VALUE, want ${CHECKPOINT_VALUE} — it read something else"
  fi

  echo ""
  echo "  --- the write into A's prefix, and why its refusal means what it says ---"
  # Three facts have to hold before the refusal is evidence of anything. Each is reported by the
  # same process, using one client and one set of credentials:
  #   1. the job holds a session token at all. Without one it is not running under a scoped
  #      session, and the store would refuse a cross-prefix write for an entirely different
  #      reason — the assertion would pass while proving nothing.
  #   2. that client CAN read A's object across prefixes: the endpoint, the signature and the
  #      credentials all work, so a later refusal is not a broken client.
  #   3. that client CAN write — in its own prefix. So the refusal is about WHERE, not about
  #      whether this job may write at all.
  # Only then does 403 on A's prefix mean the session's policy is what stopped it.
  TOKEN=$(metric_max "$JOB_B" session_token_present)
  [[ "$(py "print(abs(float('${TOKEN:-0}') - 1.0) < 0.001)")" == "True" ]] \
    && pass "stage B ran under a real STS session (AWS_SESSION_TOKEN present and signed into every request)" \
    || fail "stage B did not report a session token — every credential assertion below would be about the wrong thing"
  READ_STATUS=$(metric_max "$JOB_B" shared_read_status)
  [[ "$(py "print(abs(float('${READ_STATUS:-0}') - 200.0) < 0.001)")" == "True" ]] \
    && pass "the same client read A's object across prefixes (HTTP 200) — the client itself works" \
    || fail "cross-prefix read returned ${READ_STATUS:-<never reported>}, want 200"
  OWN_WRITE=$(metric_max "$JOB_B" own_prefix_write_status)
  [[ "$(py "print(abs(float('${OWN_WRITE:-0}') - 200.0) < 0.001)")" == "True" ]] \
    && pass "the same client wrote its OWN prefix (HTTP 200) — this job may write, somewhere" \
    || fail "own-prefix write returned ${OWN_WRITE:-<never reported>}, want 200"
  FOREIGN=$(metric_max "$JOB_B" foreign_write_status)
  if [[ -z "$FOREIGN" ]]; then
    fail "stage B never reported foreign_write_status — the refusal was never attempted"
  elif [[ "$(py "print(abs(float('$FOREIGN') - 403.0) < 0.001)")" == "True" ]]; then
    pass "the same client, one key away, was refused a write into A's prefix with 403 — the store enforces the scoping, not convention"
  else
    fail "a write into another job's prefix answered $FOREIGN, want 403 — a 200 means an agent can overwrite another's evidence"
  fi
else
  fail "stage B did not complete (status=$S, eviction_reason=$(get_field "$JOB_B" eviction_reason))"
fi

echo ""
echo "=========================================================="
echo "Lineage: what this result was derived from"
echo "=========================================================="
CHAIN=$(lineage_ids "$JOB_B" | tr '\n' ' ')
CHAIN="${CHAIN% }"
[[ "$CHAIN" == "$JOB_A $JOB_B" ]] \
  && pass "GET /experiments/$JOB_B/lineage returns the chain oldest-first: $CHAIN" \
  || fail "lineage of stage B is [$CHAIN], want [$JOB_A $JOB_B]"
# A job with no parent is a chain of one — the chain endpoint always starts at the row itself, so
# an empty answer here would mean the row is missing rather than that it has no ancestry.
ROOT_CHAIN=$(lineage_ids "$JOB_A" | tr '\n' ' ')
[[ "${ROOT_CHAIN% }" == "$JOB_A" ]] \
  && pass "stage A, which has no parent, is its own single-entry lineage" \
  || fail "lineage of stage A is [${ROOT_CHAIN% }], want [$JOB_A]"

close_platform_experiment "$PE_ID"
finish
