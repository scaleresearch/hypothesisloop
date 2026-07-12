#!/usr/bin/env bash
# robotics-compete-loop.sh — keep the robotics VLA platform experiment "alive" by having
# each agent repeatedly resubmit new rounds (new job, same hypothesis) as soon as its
# previous one finishes, for MAX_ROUNDS per agent. Every agent trains the *same* VLA
# baseline model — what's actually being compared is a fixed hyperparameter hypothesis
# per agent (learning rate / chunk length / camera views). This is what makes the
# "Competing Agents over time" chart show a real race instead of one static point per
# agent: many jobs land over time, at slightly different values each round (train.py
# reseeds its per-run noise from the job id even for the same agent/hypothesis), so agents
# visibly trade places on task_success_rate / action_mse.
#
# Meant to be started once and left running in the background:
#   nohup bash tests/robotics-compete-loop.sh > /tmp/robotics-compete-loop.log 2>&1 &
#
# Env overrides:
#   MAX_ROUNDS (default 10), ROUND_DURATION_SECONDS (default 90),
#   REPORT_INTERVAL_SECONDS (default 5)

set -uo pipefail  # no -e: one agent's submission hiccup shouldn't kill the whole loop

QUOTA_URL="${QUOTA_URL:-http://localhost:8081}"
SCHED_URL="${SCHED_URL:-http://localhost:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://localhost:8083}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workload-robotics/job.yaml}"

RUN_TS="$(date +%s)"

# Same baseline VLA model for every agent — only the hypothesis (hyperparameter bet)
# differs. Parallel arrays (macOS ships bash 3.2, which has no associative arrays).
AGENTS=(agent-high-lr       agent-low-lr        agent-long-chunk    agent-multi-cam)
LRS=(1e-3                   3e-5                 3e-4                 3e-4)
CHUNKS=(16                  16                   50                   16)
CAMS=(1                     1                    1                    3)
HYPOTHESES=(
  "A higher learning rate (1e-3) converges faster to a better task_success_rate within the same wall-clock budget"
  "A conservative learning rate (3e-5) trades speed for stability and avoids the success-rate regressions higher rates cause"
  "Longer action-chunk prediction (50 steps) captures multi-step manipulation better than short chunks, raising success rate"
  "Adding camera viewpoints (3 views) gives the policy better spatial coverage of the scene, raising success rate"
)
AGENT_COUNT="${#AGENTS[@]}"

MAX_ROUNDS="${MAX_ROUNDS:-10}"
ROUND_DURATION_SECONDS="${ROUND_DURATION_SECONDS:-90}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-5}"
JOB_HOURS=$(python3 -c "print(round(${ROUND_DURATION_SECONDS}/3600, 6))")

# Budget sized for every agent to run MAX_ROUNDS rounds on A100 (job.yaml's priciest
# acceptable_gpu_types entry), with 20% headroom.
MAX_RATE="3.0"
BUDGET=$(python3 -c "print(round(${AGENT_COUNT} * ${MAX_ROUNDS} * ${JOB_HOURS} * ${MAX_RATE} / 0.40 * 1.2, 4))")

py() { python3 -c "$@"; }

field_for() {
  local target="$1"; shift
  local -a arr=("$@")
  local i
  for i in "${!AGENTS[@]}"; do
    [[ "${AGENTS[$i]}" == "$target" ]] && { echo "${arr[$i]}"; return 0; }
  done
}

echo "==> Registering ${AGENT_COUNT} agents..."
for AGENT in "${AGENTS[@]}"; do
  curl -sf -X POST "$QUOTA_URL/agents" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$AGENT\",\"name\":\"$AGENT\"}" > /dev/null 2>&1 || true
done

echo "==> Creating marathon platform experiment (budget=${BUDGET} T4h, ${MAX_ROUNDS} rounds x ${AGENT_COUNT} agents)..."
PE_RESP=$(curl -sf -X POST "$QUOTA_URL/platform-experiments" \
  -H 'Content-Type: application/json' \
  -d "{
    \"name\": \"Best Robotics VLA Policy — Marathon ${RUN_TS}\",
    \"description\": \"Long-running competition: ${AGENT_COUNT} agents training the same VLA baseline model, each betting on a different hyperparameter hypothesis, resubmitting new rounds continuously.\",
    \"budget_t4_hours\": ${BUDGET},
    \"max_agents\": ${AGENT_COUNT},
    \"metrics\": [
      {\"key\": \"task_success_rate\", \"direction\": \"maximize\"},
      {\"key\": \"action_mse\", \"direction\": \"minimize\"}
    ],
    \"phase2_boundary\": 0.40
  }")
PE_ID=$(echo "$PE_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")
echo "  id: $PE_ID"
echo "  UI: http://localhost:3000/platform-experiments/${PE_ID}"

echo "==> Signing up agents..."
for AGENT in "${AGENTS[@]}"; do
  curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/signup" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"$AGENT\"}" > /dev/null 2>&1
done

echo "==> Starting experiment..."
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/start" > /dev/null 2>&1

# --- Per-agent round loop: submit, poll to terminal, summarize, repeat -------------------

agent_loop() {
  local AGENT="$1" LR="$2" CHUNK="$3" CAM="$4" HYPOTHESIS="$5"
  local round job_id status body summary

  for round in $(seq 1 "${MAX_ROUNDS}"); do
    job_id="job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_TS}-r${round}"

    # Every experiment must reference a registered hypothesis (hypothesis_id), not restate
    # free text ad hoc — register (or retrieve, if an equivalent one already exists) it first.
    hyp_resp=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" \
      -H 'Content-Type: application/json' \
      -d "$(AGENT="$AGENT" PE_ID="$PE_ID" HYPOTHESIS="$HYPOTHESIS" ROUND="$round" py "import json,os; print(json.dumps({'agent_id': os.environ['AGENT'], 'platform_experiment_id': os.environ['PE_ID'], 'text': f\"Round {os.environ['ROUND']}: {os.environ['HYPOTHESIS']}\"}))")")
    hypothesis_id=$(echo "$hyp_resp" | py "import sys,json; print(json.load(sys.stdin)['id'])")

    body=$(JOB_ID="$job_id" AGENT="$AGENT" PE_ID="$PE_ID" JOB_HOURS="$JOB_HOURS" \
      JOB_FILE="$JOB_FILE" LR="$LR" CHUNK="$CHUNK" CAM="$CAM" HYPOTHESIS_ID="$hypothesis_id" ROUND="$round" \
      REPORT_INTERVAL_SECONDS="$REPORT_INTERVAL_SECONDS" py "
import json, os, yaml

with open(os.environ['JOB_FILE']) as f:
    job = yaml.safe_load(f)

job['env'] = {
    'OPENRESEARCH_LEARNING_RATE': os.environ['LR'],
    'OPENRESEARCH_CHUNK_LEN': os.environ['CHUNK'],
    'OPENRESEARCH_CAMERA_VIEWS': os.environ['CAM'],
    'OPENRESEARCH_REPORT_INTERVAL_SECONDS': os.environ['REPORT_INTERVAL_SECONDS'],
    'OPENRESEARCH_BASELINE': '0.30',
}

print(json.dumps({
    'id': os.environ['JOB_ID'],
    'metadata': {
        'agent_id': os.environ['AGENT'],
        'platform_experiment_id': os.environ['PE_ID'],
        'project_id': 'robotics-vla-marathon',
        'hypothesis_id': os.environ['HYPOTHESIS_ID'],
        'theory': f\"lr={os.environ['LR']} chunk_len={os.environ['CHUNK']} cameras={os.environ['CAM']}, same VLA baseline architecture as every other agent\",
        'objective': 'maximize task_success_rate above 0.30 baseline',
        'estimated_duration_hours': float(os.environ['JOB_HOURS']),
        'code_ref': 'git://openresearch/robotics-vla@main',
    },
    'job': job,
}))
")

    curl -sf -X POST "$SCHED_URL/experiments" \
      -H 'Content-Type: application/json' \
      -d "$body" > /tmp/robotics-loop-${AGENT}-submit.json 2>/dev/null

    echo "[$(date +%T)] ${AGENT} round ${round}/${MAX_ROUNDS} → ${job_id} submitted"

    # Poll until terminal.
    while true; do
      status=$(curl -sf "$SCHED_URL/experiments/${job_id}" \
        | py "import sys,json; print(json.load(sys.stdin).get('status','UNKNOWN'))" 2>/dev/null || echo "UNKNOWN")
      case "$status" in
        COMPLETED|FAILED|EVICTED|REJECTED) break ;;
      esac
      sleep 5
    done
    echo "[$(date +%T)] ${AGENT} round ${round}/${MAX_ROUNDS} → ${job_id} ${status}"

    if [[ "$status" == "COMPLETED" ]]; then
      summary="Round ${round} (lr=${LR} chunk=${CHUNK} cam=${CAM}) completed."
      curl -sf -X POST "$SCHED_URL/experiments/${job_id}/summary" \
        -H 'Content-Type: application/json' \
        -d "{\"summary\": \"${summary}\"}" > /dev/null 2>&1
    fi

    sleep 3
  done

  echo "[$(date +%T)] ${AGENT} finished all ${MAX_ROUNDS} rounds"
}

echo "==> Launching ${AGENT_COUNT} per-agent round loops (${MAX_ROUNDS} rounds each, ~${ROUND_DURATION_SECONDS}s/round)..."
pids=()
for idx in "${!AGENTS[@]}"; do
  agent_loop "${AGENTS[$idx]}" "${LRS[$idx]}" "${CHUNKS[$idx]}" "${CAMS[$idx]}" "${HYPOTHESES[$idx]}" &
  pids+=("$!")
done

wait "${pids[@]}"
echo "==> Marathon complete. Platform experiment: ${PE_ID}"
