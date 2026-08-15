#!/usr/bin/env bash
# ONE-TIME node setup, run by whoever brings this experiment up on a node -- NOT by a job.
#
# Jobs bind-mount the result read-only via seed/job.yaml's host_mounts and never fetch anything
# (checklist item 11: a large, shared, mostly-static input must not be re-fetched every run). The
# datasets are deliberately NOT vendored into the repo -- they are ~3.3GB of medical imaging that
# belongs on the node, not in git.
#
# Idempotent and resumable: curl -C - resumes a partial download, and an already-unpacked task is
# left alone. Safe to re-run after an interruption.
#
# Usage: fetch_data.sh [dest-root]   (default: /home/ttuser/fomo-tune-data)
set -euo pipefail

DEST="${1:-/home/ttuser/fomo-tune-data}"
mkdir -p "$DEST"

TASK3_URL="https://sid.erda.dk/share_redirect/fmeuvo1EdF/Task_3.zip"
TASK5_URL="https://huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip"
# The checkpoint experiment.md pins. hf:// resolution would work too, but a job image with no HF
# token and no network should still find it, so it lives on the node next to the data.
CKPT_URL="https://huggingface.co/medarc/walnut/resolve/main/checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth"
CKPT_REL="checkpoint/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth"

fetch_and_unpack() {
  local name="$1" url="$2" marker="$3"
  if [[ -d "$DEST/$marker" ]]; then
    echo "fetch_data.sh: $name already unpacked at $DEST/$marker, skipping"
    return
  fi
  echo "fetch_data.sh: downloading $name"
  curl -fL -C - -o "$DEST/$name.zip" "$url"
  echo "fetch_data.sh: unpacking $name"
  unzip -q -o "$DEST/$name.zip" -d "$DEST"
  rm -f "$DEST/$name.zip"   # the unpacked tree is what jobs read; keeping both doubles the footprint
}

fetch_and_unpack Task_3 "$TASK3_URL" Task_3
fetch_and_unpack Task_5 "$TASK5_URL" Task_5

if [[ -f "$DEST/$CKPT_REL" ]]; then
  echo "fetch_data.sh: checkpoint already present at $DEST/$CKPT_REL, skipping"
else
  echo "fetch_data.sh: downloading checkpoint"
  mkdir -p "$(dirname "$DEST/$CKPT_REL")"
  curl -fL -C - -o "$DEST/$CKPT_REL" "$CKPT_URL"
fi

echo "fetch_data.sh: done. Layout under $DEST:"
echo "  Task_3/preprocessed/<sub>/ses-01/t1w.nii.gz   Task_3/labels/<sub>/ses-01/labels.txt   (494 subjects, age in years)"
echo "  Task_5/preprocessed/<sub>/ses_01/t1.nii.gz    Task_5/labels/<sub>/ses_01/labels.txt   (48 subjects, 0/1)"
echo "  $CKPT_REL"
du -sh "$DEST"
