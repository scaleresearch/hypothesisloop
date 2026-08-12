#!/usr/bin/env bash
# Job entrypoint for the fast/prebuilt path (baked into Dockerfile.workload as the default
# command; job.yaml's `command` points here). Runs the real, unmodified production entrypoint,
# `main_pretrain_tt.py`, which already posts train_loss/val_loss via post_metric() at a steady
# cadence (see main_pretrain_tt.py's smri_mae_tt.metrics import) -- this script does not
# duplicate that reporting.
set -euo pipefail

# job.yaml declares host_mounts for /data/dataset. The platform validates that mount exists on
# whichever node the job lands on BEFORE starting the container (admission fails otherwise --
# see runtime/{bare-metal,k8s}'s resolveHostMounts/HostPathDirectory) -- so by the time this
# script runs, /data/dataset is guaranteed populated. No runtime existence check or fetch
# fallback needed here; that would just be re-deriving a guarantee the platform already enforces
# before this process ever starts. (A job variant for a node without a pre-populated dataset
# should omit host_mounts from its job.yaml and invoke fetch_dataset.sh itself as its `command`
# instead of run_job.sh -- see fetch_dataset.sh's own header.)
DATA_DIR="/data/dataset"

# real_data.py resolves DATA_DIR as 3 parents up from its own installed file location
# (tenstorrent/src/smri_mae_tt/../../../datasets/FOMO_with_dwi) -- i.e. relative to
# /build/smri-fm (see Dockerfile.workload's comment on why that absolute path is preserved
# unchanged from the build stage), not /app. Symlink there so the mounted/fetched shards land
# exactly where it looks, without forking real_data.py's path logic for the job-image layout.
REPO_ROOT="/build/smri-fm"
mkdir -p "$REPO_ROOT/datasets"
ln -sfn "$DATA_DIR" "$REPO_ROOT/datasets/FOMO_with_dwi"

export PYTHONUNBUFFERED=1  # see first_time.local.md: unbuffered stdout, or a healthy long run looks hung
exec python3 -m smri_mae_tt.main_pretrain_tt \
  --preset "${PRESET:-full-vitl-real}" \
  --steps "${STEPS:-6000}" \
  --base-lr "${BASE_LR:-1.5e-4}" \
  --weight-decay "${WEIGHT_DECAY:-0.05}" \
  --warmup-steps "${WARMUP_STEPS:-200}" \
  --seed "${SEED:-0}" \
  --log-every "${LOG_EVERY:-10}" \
  --eval-every "${EVAL_EVERY:-500}" \
  --checkpoint-every "${CHECKPOINT_EVERY:-500}" \
  --run-name "${HYPOTHESISLOOP_JOB_ID:-smri-fm-job}" \
  --no-wandb
