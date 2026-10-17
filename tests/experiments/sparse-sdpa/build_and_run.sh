#!/usr/bin/env bash
# The job command for a change to tt-metal source: clones tt-metal at TT_METAL_REF, applies
# tt-metal.patch from the repository root when there is one, builds it, and runs the harness against
# the freshly built ttnn in place of the job image's prebuilt one. A full build takes many minutes:
# a change to the harness's env vars alone runs `python3 harness.py` instead.
#
#   bash build_and_run.sh harness.py
set -euo pipefail

TT_METAL_REF="${TT_METAL_REF:?TT_METAL_REF must be the pinned tt-metal commit}"
HARNESS="${1:?usage: build_and_run.sh <harness.py>}"
WORKDIR="$(pwd)"
BUILD_SRC=/tmp/tt-metal-build

echo "build_and_run.sh: cloning tt-metal @ $TT_METAL_REF into $BUILD_SRC"
git clone --filter=blob:none https://github.com/tenstorrent/tt-metal.git "$BUILD_SRC"
git -C "$BUILD_SRC" checkout "$TT_METAL_REF"
git -C "$BUILD_SRC" submodule update --init --recursive

if [[ -f "$WORKDIR/tt-metal.patch" ]]; then
  echo "build_and_run.sh: applying tt-metal.patch"
  git -C "$BUILD_SRC" apply --stat --check "$WORKDIR/tt-metal.patch"
  git -C "$BUILD_SRC" apply "$WORKDIR/tt-metal.patch"
else
  echo "build_and_run.sh: no tt-metal.patch present, building pinned source unmodified"
fi

echo "build_and_run.sh: building"
cd "$BUILD_SRC"
./build_metal.sh --build-type Release

# The freshly built ttnn's python bindings take priority over the image's prebuilt wheel.
export PYTHONPATH="$BUILD_SRC:$BUILD_SRC/ttnn${PYTHONPATH:+:$PYTHONPATH}"
echo "build_and_run.sh: build OK, running $HARNESS against freshly built ttnn ($BUILD_SRC)"
cd "$WORKDIR"
exec python3 "$HARNESS"
