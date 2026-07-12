#!/usr/bin/env bash
# e2e-flow.sh — smoke test the full platform experiment flow.
#
# Usage:
#   bash tests/e2e-flow.sh
#   AGENTS="agent-a agent-b" bash tests/e2e-flow.sh

set -euo pipefail

QUOTA_URL="${QUOTA_URL:-http://localhost:8081}"
SCHED_URL="${SCHED_URL:-http://localhost:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://localhost:8083}"
PROM_URL="${PROM_URL:-http://localhost:4000/v1/prometheus}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workload/job.yaml}"

RUN_TS="$(date +%s)"

if [[ -n "${AGENTS:-}" ]]; then
  read -ra AGENTS <<< "$AGENTS"
else
  AGENTS=("agent-alpha" "agent-beta" "agent-gamma")
fi

AGENT_COUNT="${#AGENTS[@]}"
JOB_HOURS="0.0084"
# Budget: enough guaranteed quota (40% of budget) for all agents, with 10% headroom.
# job.yaml lists acceptable_gpu_types: [T4, L40, A100] — the run tolerates landing on any of
# them, and the rate charged follows whichever it actually lands on, so the budget must be
# sized for the most expensive one (A100, 3x T4's rate) or a run that lands on a pricier type
# could exhaust its guaranteed quota. See localdev/add-fake-nodes.sh for the local cluster
# setup that actually gives these submissions more than one type to land on.
MAX_RATE="3.0"
BUDGET=$(python3 -c "print(round(${AGENT_COUNT} * ${JOB_HOURS} * ${MAX_RATE} / 0.40 * 1.1, 4))")

py() { python3 -c "$@"; }

# 1. Register agents
echo "==> Registering ${AGENT_COUNT} agents..."
for AGENT in "${AGENTS[@]}"; do
  curl -sf -X POST "$QUOTA_URL/agents" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$AGENT\",\"name\":\"$AGENT\"}" > /dev/null 2>&1 || true
  echo "  $AGENT: ok"
done

# 2. Create platform experiment
echo ""
echo "==> Creating platform experiment (budget=${BUDGET} T4h)..."
PE_RESP=$(curl -sf -X POST "$QUOTA_URL/platform-experiments" \
  -H 'Content-Type: application/json' \
  -d "{
    \"name\": \"e2e-${RUN_TS}\",
    \"budget_t4_hours\": ${BUDGET},
    \"max_agents\": ${AGENT_COUNT},
    \"metrics\": [
      {\"key\": \"val_accuracy\", \"direction\": \"maximize\"},
      {\"key\": \"val_loss\", \"direction\": \"minimize\"}
    ],
    \"phase2_boundary\": 0.40
  }")
PE_ID=$(echo "$PE_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")
echo "  id: $PE_ID"

# 3. Sign up agents
echo ""
echo "==> Signing up agents..."
for AGENT in "${AGENTS[@]}"; do
  STATUS=$(curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/signup" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"$AGENT\"}" \
    | py "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "failed")
  echo "  $AGENT: $STATUS"
done

# 4. Start experiment
echo ""
echo "==> Starting experiment..."
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/start" \
  | py "import sys,json; d=json.load(sys.stdin); print('  status:', d.get('status',''))"

curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas" \
  | py "
import sys, json
for q in json.load(sys.stdin):
  print(f\"    {q['agent_id']}: {q['guaranteed_t4_hours']:.4f} T4h\")
" 2>/dev/null || true

# 5. Submit jobs
echo ""
echo "==> Submitting jobs..."
declare -a JOB_IDS

TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

cat > "$TMPDIR_T/mk_hyp_body.py" <<'PYEOF'
import json, sys
print(json.dumps({
    "agent_id": sys.argv[1],
    "platform_experiment_id": sys.argv[2],
    "text": f"tune hyperparameters for {sys.argv[1]}",
}))
PYEOF

for AGENT in "${AGENTS[@]}"; do
  JOB_ID="job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_TS}"

  # Every experiment must reference a hypothesis registered under the same platform
  # experiment it's submitted into (hypothesis_id), not restate free text ad hoc — register
  # (or retrieve, if an equivalent one already exists in this platform experiment's idea
  # pool) it first.
  HYP_RESP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" \
    -H 'Content-Type: application/json' \
    -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "$AGENT" "$PE_ID")")
  HYPOTHESIS_ID=$(echo "$HYP_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")

  # The job definition itself (image, gpu_type/count — how the workload runs) is read
  # verbatim from the standalone job.yaml file next to the test workload; only the
  # per-run metadata (who's submitting, what they're testing, which platform experiment)
  # is built here, since that's what's actually different across agents/runs.
  SUBMIT_BODY=$(JOB_ID="$JOB_ID" AGENT="$AGENT" PE_ID="$PE_ID" JOB_HOURS="$JOB_HOURS" JOB_FILE="$JOB_FILE" HYPOTHESIS_ID="$HYPOTHESIS_ID" py "
import json, os, yaml

with open(os.environ['JOB_FILE']) as f:
    job = yaml.safe_load(f)

print(json.dumps({
    'id': os.environ['JOB_ID'],
    'metadata': {
        'agent_id': os.environ['AGENT'],
        'platform_experiment_id': os.environ['PE_ID'],
        'project_id': 'e2e',
        'hypothesis_id': os.environ['HYPOTHESIS_ID'],
        'theory': 'adjusting lr and hidden_dim will improve val_accuracy',
        'objective': 'maximize val_accuracy',
        'estimated_duration_hours': float(os.environ['JOB_HOURS']),
        'code_ref': 'git://openresearch@main',
    },
    'job': job,
}))
")

  SUBMIT_RESP=$(curl -sf -X POST "$SCHED_URL/experiments" \
    -H 'Content-Type: application/json' \
    -d "$SUBMIT_BODY")

  STATUS=$(echo "$SUBMIT_RESP" | py "import sys,json; print(json.load(sys.stdin).get('status','ERROR'))" 2>/dev/null || echo "ERROR")
  SCORE=$(echo "$SUBMIT_RESP"  | py "import sys,json; print(round(json.load(sys.stdin).get('priority_score',0),4))" 2>/dev/null || echo "n/a")
  echo "  $AGENT → $JOB_ID  status=$STATUS  priority=$SCORE"
  JOB_IDS+=("$JOB_ID")
done

# 6. Poll until all jobs reach a terminal state
echo ""
echo "==> Polling jobs (up to 10 min)..."
for i in $(seq 1 120); do
  all_done=true
  statuses=()
  for idx in "${!JOB_IDS[@]}"; do
    S=$(curl -sf "$SCHED_URL/experiments/${JOB_IDS[$idx]}" \
      | py "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null || echo "UNKNOWN")
    statuses+=("${AGENTS[$idx]}=${S}")
    [[ "$S" != "COMPLETED" && "$S" != "FAILED" && "$S" != "EVICTED" && "$S" != "REJECTED" ]] && all_done=false
  done
  echo "  [${i}] $(IFS=', '; echo "${statuses[*]}")"
  $all_done && { echo ""; echo "  All jobs terminal."; break; }
  sleep 5
done

# 7. Wait for phase-2 transition (controller reconciles every 30 s)
echo ""
echo "==> Waiting for phase-2 transition..."
for i in $(seq 1 8); do
  P2=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/phase2-status" \
    | py "import sys,json; print(json.load(sys.stdin).get('phase',1))" 2>/dev/null || echo "1")
  [[ "$P2" == "2" ]] && { echo "  Phase 2 triggered."; break; }
  echo "  [${i}] still phase 1 — waiting 15 s..."
  sleep 15
done

# 8. Phase-2 status
echo ""
echo "==> Phase-2 status:"
curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/phase2-status" \
  | py "
import sys, json
d = json.load(sys.stdin)
active = d.get('active_agents') or []
held   = d.get('held_agents')   or []
print(f\"  phase: {d.get('phase',1)}  triggered_at: {d.get('phase2_triggered_at','—')}\")
print(f\"  active ({len(active)}): {', '.join(active) or '—'}\")
print(f\"  held   ({len(held)}):   {', '.join(held)   or '—'}\")
" 2>/dev/null || echo "  (unavailable)"

# 9. Results
echo ""
echo "==> Results:"
PROM_RESULTS=$(curl -sf "${PROM_URL}/api/v1/query?query=experiment_metric_value%7Bplatform_experiment_id%3D%22${PE_ID}%22%7D" 2>/dev/null || echo '{}')

for idx in "${!JOB_IDS[@]}"; do
  JOB_ID="${JOB_IDS[$idx]}"
  AGENT="${AGENTS[$idx]}"
  curl -sf "$SCHED_URL/experiments/${JOB_ID}" | py "
import sys, json
d = json.load(sys.stdin)
ev = d.get('eviction_reason','')
prom = json.loads('${PROM_RESULTS}'.replace(\"'\", '\"'))
vals = [r['value'][1] for r in prom.get('data',{}).get('result',[]) if r.get('metric',{}).get('job_id') == '${JOB_ID}']
print(f'  ${AGENT}: {d.get(\"status\",\"?\")}' + (f' ({ev})' if ev else '') + f'  metric={vals[-1] if vals else \"n/a\"}')
" 2>/dev/null || echo "  $AGENT: (fetch failed)"
done

echo ""
curl -sf "${PROM_URL}/api/v1/query?query=experiment_metric_value%7Bplatform_experiment_id%3D%22${PE_ID}%22%7D" 2>/dev/null \
  | py "
import sys, json
results = json.load(sys.stdin).get('data',{}).get('result',[])
print(f'==> Prometheus: {len(results)} samples')
for r in results[:9]:
  m = r.get('metric',{})
  print(f\"  agent={m.get('agent_id','?')}  job={m.get('job_id','?')[:16]}  {m.get('metric_name','?')}={r.get('value',['?','?'])[1]}\")
" 2>/dev/null || true

echo ""
echo "==> Closing experiment $PE_ID..."
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/close" \
  | py "import sys,json; print('  status:', json.load(sys.stdin).get('status',''))" \
  2>/dev/null || echo "  (close failed)"

echo ""
echo "==> Done."
