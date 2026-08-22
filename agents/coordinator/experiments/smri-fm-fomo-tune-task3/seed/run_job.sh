#!/usr/bin/env bash
# Job entrypoint. Two phases, in order:
#
#   1. The correctness gate. One subject through both the PyTorch reference encoder and the TT
#      encoder, same checkpoint, PCC compared. This EXITS NON-ZERO below the gate, so a job whose
#      encoder is wrong never reaches the ranking with a plausible-looking score (checklist item
#      13: a gate that only reports is not a gate). PCC_GATE below is the enforcement point; a
#      submission that raises it is void.
#   2. The task itself: embed every subject once on device, then the frozen 20-fold protocol.
#
# Both phases post metrics themselves via fomo_tune_tt.metrics.post_metric; this script adds no
# reporting of its own.
set -euo pipefail

TASK="${TASK:?TASK must be task3 or task5}"
DATA_ROOT="${DATA_ROOT:-/data/fomo}"
CKPT_PATH="${CKPT_PATH:-$DATA_ROOT/checkpoint/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth}"
OUTPUT_DIR="${OUTPUT_DIR:-/tmp/fomo-tune-out/$TASK}"
PCC_GATE="${PCC_GATE:-0.999}"

export PYTHONUNBUFFERED=1  # or a healthy long run looks hung

mkdir -p "$OUTPUT_DIR"

# The gate subject is the task's first subject in the frozen ordering -- deterministic, and part
# of the real dataset rather than a synthetic volume, so the gate exercises the same transform
# and the same mask occupancy the scored run does.
GATE_T1="$(python3 -c "
from pathlib import Path
from fomo_tune_tt.data import load_task
print(load_task('$TASK', Path('$DATA_ROOT')).pop(0).t1w_path)
")"

echo "run_job.sh: correctness gate on $GATE_T1 (PCC gate $PCC_GATE)"
python3 -m fomo_tune_tt.parity_fomo \
  --ckpt-path "$CKPT_PATH" \
  --t1 "$GATE_T1" \
  --pcc-gate "$PCC_GATE" \
  --out "$OUTPUT_DIR/parity.json"

echo "run_job.sh: gate passed, running $TASK"
exec python3 -m fomo_tune_tt.run_task \
  --task "$TASK" \
  --ckpt-path "$CKPT_PATH" \
  --data-root "$DATA_ROOT" \
  --output-dir "$OUTPUT_DIR" \
  ${L_VIS:+--l-vis "$L_VIS"}
