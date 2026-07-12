#!/usr/bin/env bash
# robotics-demo.sh — spin up a "best robotics VLA policy" platform experiment where several
# agents train the *same* VLA baseline model but each bets on a different hypothesis (a
# specific hyperparameter choice), competing head-to-head. Mirrors tests/e2e-flow.sh's flow
# (register -> create PE -> signup -> start -> submit -> poll) but targets the robotics
# workload (tests/workload-robotics/train.py, image openresearch-robotics-workload) instead
# of the generic image-classification stub, and leaves the platform experiment open/running
# (does not auto-close) so the UI has live, multi-agent competing metrics to show.
#
# Usage:
#   bash tests/robotics-demo.sh
#   DURATION_SECONDS=180 bash tests/robotics-demo.sh

set -euo pipefail

QUOTA_URL="${QUOTA_URL:-http://localhost:8081}"
SCHED_URL="${SCHED_URL:-http://localhost:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://localhost:8083}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workload-robotics/job.yaml}"

RUN_TS="$(date +%s)"

# Same baseline VLA model for every agent — only the hypothesis (hyperparameter bet)
# differs. Parallel arrays (agent / lr / chunk_len / cameras / one-line hypothesis) since
# macOS ships bash 3.2, which has no associative arrays.
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

field_for() {
  # field_for <target-agent> <array-name-as-string-via-nameref-emulation>
  local target="$1"; shift
  local -a arr=("$@")
  local i
  for i in "${!AGENTS[@]}"; do
    [[ "${AGENTS[$i]}" == "$target" ]] && { echo "${arr[$i]}"; return 0; }
  done
}

# How long each job trains for, and how often it reports — kept short for a live demo.
DURATION_SECONDS="${DURATION_SECONDS:-180}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-5}"
JOB_HOURS=$(python3 -c "print(round(${DURATION_SECONDS}/3600, 6))")

# Budget: guaranteed quota (Phase 1 = 40% of budget) sized for all agents on A100
# (job.yaml's most expensive acceptable_gpu_types entry), with 20% headroom.
MAX_RATE="3.0"
BUDGET=$(python3 -c "print(round(${AGENT_COUNT} * ${JOB_HOURS} * ${MAX_RATE} / 0.40 * 1.2, 4))")

py() { python3 -c "$@"; }

echo "==> Registering ${AGENT_COUNT} agents..."
for idx in "${!AGENTS[@]}"; do
  AGENT="${AGENTS[$idx]}"
  curl -sf -X POST "$QUOTA_URL/agents" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$AGENT\",\"name\":\"$AGENT\"}" > /dev/null 2>&1 || true
  echo "  $AGENT: ok (lr=${LRS[$idx]} chunk_len=${CHUNKS[$idx]} cameras=${CAMS[$idx]})"
done

echo ""
echo "==> Creating platform experiment 'robotics-vla-${RUN_TS}' (budget=${BUDGET} T4h)..."
PE_RESP=$(curl -sf -X POST "$QUOTA_URL/platform-experiments" \
  -H 'Content-Type: application/json' \
  -d "{
    \"name\": \"Best Robotics VLA Policy — ${RUN_TS}\",
    \"description\": \"Same VLA baseline model, competing hypotheses about which hyperparameter choice maximizes task_success_rate.\",
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

echo ""
echo "==> Signing up agents..."
for AGENT in "${AGENTS[@]}"; do
  STATUS=$(curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/signup" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"$AGENT\"}" \
    | py "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "failed")
  echo "  $AGENT: $STATUS"
done

echo ""
echo "==> Starting experiment..."
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/start" \
  | py "import sys,json; d=json.load(sys.stdin); print('  status:', d.get('status',''))"

echo ""
echo "==> Submitting jobs (one per agent, same VLA baseline, different hypothesis)..."
declare -a JOB_IDS

TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

cat > "$TMPDIR_T/mk_hyp_body.py" <<'PYEOF'
import json, sys
print(json.dumps({"agent_id": sys.argv[1], "platform_experiment_id": sys.argv[2], "text": sys.argv[3]}))
PYEOF

cat > "$TMPDIR_T/mk_submit_body.py" <<'PYEOF'
import json, sys, yaml

job_id, agent, pe_id, job_hours, job_file, lr, chunk, cam, hypothesis_id = sys.argv[1:10]

with open(job_file) as f:
    job = yaml.safe_load(f)

job['env'] = {
    'OPENRESEARCH_LEARNING_RATE': lr,
    'OPENRESEARCH_CHUNK_LEN': chunk,
    'OPENRESEARCH_CAMERA_VIEWS': cam,
    'OPENRESEARCH_REPORT_INTERVAL_SECONDS': sys.argv[10],
    'OPENRESEARCH_BASELINE': '0.30',
}

print(json.dumps({
    'id': job_id,
    'metadata': {
        'agent_id': agent,
        'platform_experiment_id': pe_id,
        'project_id': 'robotics-vla-demo',
        'hypothesis_id': hypothesis_id,
        'theory': f"lr={lr} chunk_len={chunk} cameras={cam}, same VLA baseline architecture as every other agent in this experiment",
        'objective': 'maximize task_success_rate above 0.30 baseline',
        'estimated_duration_hours': float(job_hours),
        'code_ref': 'git://openresearch/robotics-vla@main',
    },
    'job': job,
}))
PYEOF

for idx in "${!AGENTS[@]}"; do
  AGENT="${AGENTS[$idx]}"
  LR="${LRS[$idx]}"
  CHUNK="${CHUNKS[$idx]}"
  CAM="${CAMS[$idx]}"
  HYPOTHESIS="${HYPOTHESES[$idx]}"
  JOB_ID="job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_TS}"

  # Every experiment must reference a hypothesis registered under the same platform
  # experiment it's submitted into (hypothesis_id), not restate free text ad hoc — register
  # (or retrieve, if an equivalent one already exists in this platform experiment's idea
  # pool) it first.
  HYP_RESP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" \
    -H 'Content-Type: application/json' \
    -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "$AGENT" "$PE_ID" "$HYPOTHESIS")")
  HYPOTHESIS_ID=$(echo "$HYP_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")

  SUBMIT_BODY=$(python3 "$TMPDIR_T/mk_submit_body.py" \
    "$JOB_ID" "$AGENT" "$PE_ID" "$JOB_HOURS" "$JOB_FILE" "$LR" "$CHUNK" "$CAM" "$HYPOTHESIS_ID" "$REPORT_INTERVAL_SECONDS")

  SUBMIT_RESP=$(curl -sf -X POST "$SCHED_URL/experiments" \
    -H 'Content-Type: application/json' \
    -d "$SUBMIT_BODY")

  STATUS=$(echo "$SUBMIT_RESP" | py "import sys,json; print(json.load(sys.stdin).get('status','ERROR'))" 2>/dev/null || echo "ERROR")
  echo "  $AGENT (lr=$LR chunk=$CHUNK cam=$CAM) → $JOB_ID  status=$STATUS"
  JOB_IDS+=("$JOB_ID")
done

echo ""
echo "==> Jobs submitted. Platform experiment: ${PE_ID}"
echo "    UI: http://localhost:3000/platform-experiments/${PE_ID}"
echo ""
echo "==> Polling jobs for up to 60s so you can see them move to RUNNING..."
for i in $(seq 1 12); do
  statuses=()
  for idx in "${!JOB_IDS[@]}"; do
    S=$(curl -sf "$SCHED_URL/experiments/${JOB_IDS[$idx]}" \
      | py "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null || echo "UNKNOWN")
    statuses+=("${AGENTS[$idx]}=${S}")
  done
  echo "  [${i}] $(IFS=', '; echo "${statuses[*]}")"
  sleep 5
done

echo ""
echo "==> Done. Jobs keep running/reporting metrics for ~${DURATION_SECONDS}s total from submission."
echo "    Open the UI to watch task_success_rate / action_mse race across the same VLA baseline"
echo "    under 4 competing hyperparameter hypotheses (high-lr / low-lr / long-chunk / multi-cam)."
