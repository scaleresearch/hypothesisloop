#!/usr/bin/env bash
# Build-from-source path for sparse-sdpa jobs that edit tt-metal/ttnn C++ (kernel/op source),
# not just Python harness config. The job's base image (see job.yaml's `image`) is the same
# Tenstorrent release image dummy-tt-matmul uses -- it already ships cmake/ninja/clang/git, just
# not the tt-metal source tree or a build of it, so this script does both, then runs the harness
# against the freshly built ttnn instead of the image's prebuilt /opt/venv wheel.
#
# Only use this path when you've actually changed tt-metal source. If you're only changing
# harness-side config (dtype, k_chunk_size, math_fidelity, shapes) run against the prebuilt
# wheel directly (see run_harness.sh) -- a full/incremental build costs real minutes per job that
# a config-only change doesn't need.
#
# Usage (as a job.yaml `command`): bash build_and_run.sh <harness.py> [harness env vars already
# exported]. Expects, in the same code repo checkout as this script:
#   - tt-metal.patch (optional): a `git diff` / `git format-patch`-style patch against
#     $TT_METAL_REF, applied on top of the pinned clone before building. Absence just means "run
#     the pinned upstream source unmodified" -- useful for confirming the wheel-skew fix alone.
set -euo pipefail

TT_METAL_REF="${TT_METAL_REF:?TT_METAL_REF must be set to the same pinned rev the agent container clones (see this experiment Dockerfile.experimentator)}"
HARNESS="${1:?usage: build_and_run.sh <harness.py>}"
WORKDIR="$(pwd)"
BUILD_SRC=/tmp/tt-metal-build

# Fast path: the `:src` job image (seed/Dockerfile.workload-src) bakes in the tt-metal tree at
# $TT_METAL_REF ALREADY BUILT, plus a warm ccache. Use it when present -- applying a patch and
# rebuilding then touches only what the patch changed (minutes), instead of a cold clone + full
# build (far longer than any job length cap, which is why every from-source job used to die).
# Set `image: localhost/hypothesisloop-sparse-sdpa-workload:src` in your job.yaml to get it.
if [[ -d "${TT_METAL_BUILD_SRC:-/opt/tt-metal-build}/.git" ]]; then
  BUILD_SRC="${TT_METAL_BUILD_SRC:-/opt/tt-metal-build}"
  echo "build_and_run.sh: using prebuilt tt-metal tree at $BUILD_SRC (warm ccache) -- no clone needed"
  git -C "$BUILD_SRC" checkout -- .          # drop a previous job's patch; tree returns to $TT_METAL_REF
  git -C "$BUILD_SRC" rev-parse HEAD
else
  echo "build_and_run.sh: cloning tt-metal @ $TT_METAL_REF into $BUILD_SRC (SLOW cold path --"
  echo "  prefer the :src image, see above)"
  git clone --filter=blob:none https://github.com/tenstorrent/tt-metal.git "$BUILD_SRC"
  git -C "$BUILD_SRC" checkout "$TT_METAL_REF"
fi

if [[ -f "$WORKDIR/tt-metal.patch" ]]; then
  echo "build_and_run.sh: applying tt-metal.patch"
  git -C "$BUILD_SRC" apply --stat --check "$WORKDIR/tt-metal.patch"
  git -C "$BUILD_SRC" apply "$WORKDIR/tt-metal.patch"
else
  echo "build_and_run.sh: no tt-metal.patch present, building pinned source unmodified"
fi

echo "build_and_run.sh: building (ccache enabled; this is the slow step, expect real minutes even incremental)"
cd "$BUILD_SRC"
./build_metal.sh --enable-ccache --build-type Release

# Freshly built ttnn's python bindings take priority over the image's prebuilt /opt/venv wheel.
export PYTHONPATH="$BUILD_SRC:$BUILD_SRC/ttnn${PYTHONPATH:+:$PYTHONPATH}"
echo "build_and_run.sh: build OK, running $HARNESS against freshly built ttnn ($BUILD_SRC)"
cd "$WORKDIR"
exec python3 "$HARNESS"
