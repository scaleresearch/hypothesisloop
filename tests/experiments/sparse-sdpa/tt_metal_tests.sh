#!/usr/bin/env bash
# Fetches tt-metal's own sparse_sdpa test utilities, which harness.py imports, at TT_METAL_REF into
# TT_METAL_TEST_UTILS_PATH: one commit, without history, and the blobs of these two directories alone.
set -euo pipefail

dest="${TT_METAL_TEST_UTILS_PATH:?TT_METAL_TEST_UTILS_PATH must name where the test utilities go}"
ref="${TT_METAL_REF:?TT_METAL_REF must be the pinned tt-metal commit}"

git init -q "$dest"
git -C "$dest" remote add origin https://github.com/tenstorrent/tt-metal.git
git -C "$dest" fetch -q --depth 1 --filter=blob:none origin "$ref"
git -C "$dest" checkout -q FETCH_HEAD -- tests/ttnn/unit_tests/operations/sdpa tests/ttnn/profiling
