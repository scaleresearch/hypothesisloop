#!/usr/bin/env bash
# Build-from-source path for smri-fm jobs that edit tt-metal/tt-train C++ source (kernels,
# dispatch, the vendored SDPA backward-KV family), not just main_pretrain_tt.py's Python/CLI
# surface. The job image (Dockerfile.workload) already bakes a full built tt-metal+tt-train tree
# at $TT_METAL_REF with a warm ccache -- this script clones a FRESH working copy from that same
# pin, applies your patch, rebuilds (incremental, ccache-enabled), then runs against the freshly
# built tree instead of the image's baked one.
#
# CRITICAL: this always clones its own working copy. Never point BUILD_SRC-equivalent logic at
# this host's live `tenstorrent/tt-metal/` -- that tree has open file handles from the running
# production job (main_pretrain_tt.py --preset full-vitl-real, run fleet-baseline-d0-v2) and
# must not be read into any build or touched in place.
#
# Usage (as a job.yaml `command`): bash build_and_run.sh <steps> [main_pretrain_tt.py args...]
# Expects, in the same code repo checkout as this script:
#   - tt-metal.patch (optional): a `git diff`-style patch against $TT_METAL_REF's tt-metal
#     checkout. Absence means "run the pinned upstream + vendored-patch source unmodified" --
#     useful for isolating a Python-side (ops_tt/) change from a C++ one.
set -euo pipefail

TT_METAL_REF="${TT_METAL_REF:?TT_METAL_REF must be set to the same pinned rev this experiment Dockerfile.experimentator clones}"
BUILD_SRC=/tmp/tt-metal-build

# This script itself lives at $REPO_ROOT/seed/build_and_run.sh in the image (see
# Dockerfile.workload: the whole project tree is preserved at /build/smri-fm), so REPO_ROOT is
# exactly ONE level up, not two -- getting this wrong (as an earlier version of this script did)
# means the patch/pytest paths below silently resolve to nothing and this script degrades to
# building unpatched upstream tt-metal with no error at all.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TT_METAL_PREBUILT="${TT_METAL_PREBUILT:-$REPO_ROOT/tenstorrent/tt-metal}"

if [[ -d "$TT_METAL_PREBUILT/.git" ]]; then
  echo "build_and_run.sh: copying prebuilt tt-metal tree (warm ccache) from $TT_METAL_PREBUILT to $BUILD_SRC"
  cp -a "$TT_METAL_PREBUILT" "$BUILD_SRC"
  git -C "$BUILD_SRC" checkout -- .   # drop any prior job's patch; tree returns to $TT_METAL_REF
else
  echo "build_and_run.sh: no prebuilt tree found, cloning tt-metal @ $TT_METAL_REF (SLOW cold path)"
  git clone --filter=blob:none https://github.com/tenstorrent/tt-metal.git "$BUILD_SRC"
  git -C "$BUILD_SRC" checkout "$TT_METAL_REF"
  git -C "$BUILD_SRC" submodule update --init --recursive
fi

PATCH="$REPO_ROOT/tenstorrent/patches/vendored-tt-metal-changes.patch"
if [[ -f "$PATCH" ]]; then
  git -C "$BUILD_SRC" apply --stat --check "$PATCH"
  git -C "$BUILD_SRC" apply "$PATCH"
else
  echo "build_and_run.sh: FATAL: expected vendored patch at $PATCH, not found -- refusing to" \
       "build unpatched tt-metal silently (would miss the AttentionMaskType::None fix + SDPA" \
       "fusion this project requires)" >&2
  exit 1
fi
if [[ -f "$(pwd)/tt-metal.patch" ]]; then
  echo "build_and_run.sh: applying tt-metal.patch"
  git -C "$BUILD_SRC" apply --stat --check "$(pwd)/tt-metal.patch"
  git -C "$BUILD_SRC" apply "$(pwd)/tt-metal.patch"
else
  echo "build_and_run.sh: no tt-metal.patch present, building pinned + vendored source unmodified"
fi

echo "build_and_run.sh: building (ccache enabled; real minutes even incremental)"
cd "$BUILD_SRC"
./build_metal.sh --enable-ccache --build-type Release
./build_metal.sh --build-tests --enable-ccache --build-type Release   # tt-train / ttml_tests

export TT_METAL_HOME="$BUILD_SRC"
export PYTHONPATH="$BUILD_SRC${PYTHONPATH:+:$PYTHONPATH}"

echo "build_and_run.sh: running correctness gate before trusting any perf number"
cd "$REPO_ROOT/tenstorrent/src/smri_mae_tt"
pytest -q
"$BUILD_SRC/build_Release/test/ttml_tests" --gtest_filter=SDPABackwardTest.*

echo "build_and_run.sh: gate passed, running training"
exec bash "$REPO_ROOT/seed/run_job.sh" "$@"
