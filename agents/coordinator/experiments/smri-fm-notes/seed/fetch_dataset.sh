#!/usr/bin/env bash
# Idempotent, resumable dataset fetch for a smri-fm job. Thin wrapper around the real
# `download_r2_shards.py` already baked into the job image (see Dockerfile.workload) --
# NOT a hand-rolled copy: that script already skips any shard already present on disk
# (dest.is_file() check) and downloads strictly sequentially, so re-running this on a job
# restart (checklist item 7: cheap resume) costs nothing for shards already local and never
# opens multiple concurrent connections against R2.
#
# There is currently no platform-level shared dataset volume (JobSpec has no hostPath/PV
# field -- see experiment.md's coordinator notes), so every job fetches into its own
# ephemeral storage. At 67 shards / ~42.9GB this is real but bounded -- size `seed/job.yaml`'s
# `storage` for it, don't just request a number that happens to work today.
#
# Usage: fetch_dataset.sh <dest-dir> [max-mbps]
set -euo pipefail

DEST="${1:?usage: fetch_dataset.sh <dest-dir> [max-mbps]}"
MAX_MBPS="${2:-}"

ENV_FILE="${HYPOTHESISLOOP_R2_ENV_FILE:-/app/.env}"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "fetch_dataset.sh: no R2 credentials at $ENV_FILE -- set HYPOTHESISLOOP_R2_ENV_FILE or" \
       "mount one (R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_ENDPOINT_URL/R2_BUCKET), see" \
       "first_time.local.md section 3" >&2
  exit 1
fi

# All 67 shards this project's real_data.py hardcodes (shard.000000.tar .. shard.000066.tar,
# 50 train + 17 val). Listed explicitly rather than --after/--count so a job never accidentally
# pulls more of the ~1800-shard bucket than this experiment actually uses.
KEYS=()
for i in $(seq -f "%06g" 0 66); do
  KEYS+=("shard.${i}.tar")
done

ARGS=(--keys "${KEYS[@]}" --dest "$DEST" --env-file "$ENV_FILE")
if [[ -n "$MAX_MBPS" ]]; then
  ARGS+=(--max-mbps "$MAX_MBPS")
fi

echo "fetch_dataset.sh: resolving/downloading ${#KEYS[@]} shards into $DEST (idempotent -- already-present shards are skipped)"
python3 -m smri_mae_tt.scripts.download_r2_shards "${ARGS[@]}"
