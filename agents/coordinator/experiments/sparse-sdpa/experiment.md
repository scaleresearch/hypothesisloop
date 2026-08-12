## EXPERIMENT DESCRIPTION

```
OBJECTIVE
Minimize the device kernel duration of ttnn.transformer.sparse_sdpa (the DSA/MLA-style sparse
attention op) on Tenstorrent Blackhole, on the official "prod-sparse" production shape.
Optimize sparse_sdpa only — do not switch to or explore the sibling MSA op
(SparseSDPAMsaOperation), so both agents work the same code and results compare directly.
The objective is to produce better code that can be merged upstream and adopted by TT.

Source is baked into your container at $TT_METAL_SRC (pinned to $TT_METAL_REF), TT-NN at
$TTNN_SRC. The op lives under ttnn/cpp/ttnn/operations/transformer/sdpa/.

USE seed/harness.py
It is a pre-built wrapper around tt-metal's own sparse_sdpa_test_utils.py (the real upstream
make_inputs/golden/to_dev/pcc, not a hand copy), already validated on this cluster's hardware:
bf16 ~583us against the official 0.576ms reference, fp8-KV ~436us -- re-validate any time
TT_METAL_REF changes before trusting these as a baseline. seed/Dockerfile.workload
bakes it, torch and the pinned upstream test-utils into the job image (seed/job.yaml's `image`),
so a config-only job needs zero per-job setup and every job runs identical, already-correct
measurement code — that is what makes high-scale iteration possible.
  - Every knob is an env var: DSA_KV_DTYPE, DSA_Q_DTYPE, DSA_KC (k_chunk_size), DSA_FIDELITY,
    DSA_H/S/T/TOPK/DSA_NV_PATTERN (shape), DSA_ITERS/WARMUP/TRACE_REPS. A hyperparameter sweep
    is a change to the job's `env`, never to harness.py.
  - Import it as a library (`from harness import run_dsa_prod`) for a custom sweep driver
    instead of one job per config — returns (best_kernel_duration_ns, pcc, cache_entries).

YOU MAY CHANGE ANYTHING, ANYWHERE IN THE STACK
  - Config/hyperparameters: harness.py jobs, env vars only — the fast, high-scale path, no build.
  - tt-metal/ttnn source itself (the op's C++, kernels, dispatch, sparse gather/DMA, anything
    under ttnn/cpp/...): edit $TT_METAL_SRC in your container, `git -C $TT_METAL_SRC diff >
    tt-metal.patch`, commit it to your code repo, and submit via seed/build_and_run.sh. It
    clones tt-metal at the same pinned $TT_METAL_REF, applies your patch, builds it, and runs the
    same harness.py against the freshly built ttnn instead of the prebuilt wheel. The build is
    ccache-enabled but still minutes even incrementally — budget for it, don't rebuild for a
    trivial rerun.
  - Nothing is off limits: kernel algorithm, dispatch/scheduling, gather-header tuning by source
    edit, new fused ops — if it builds and passes the constraints below, it counts.

METRICS TO REPORT
  kernel_duration_ns  (minimize, RANKING METRIC) — DEVICE KERNEL DURATION [ns] for
      ttnn.transformer.sparse_sdpa on the "prod-sparse" shape (see FACTS). Report as a running
      minimum so it never increases.
  pcc_achieved        — PCC vs the Torch sparse_mla() golden, full precision.
  program_cache_entries — program-cache entry count for the run.
Report all three at a steady cadence while the job runs, not once at the end.

CONSTRAINTS
  - PCC >= 0.99 vs the sparse_mla() golden in
    tests/ttnn/unit_tests/operations/sdpa/sparse_sdpa_test_utils.py.
  - Falling below your own baseline PCC is a regression even if it clears 0.99.
  - program_cache_entries must match baseline — this blocks "speedups" that come from
    recompilation or shape over-specialization.
  - The existing tests define correctness. Do not weaken, redefine, skip or bypass them: run the
    real upstream PCC tests in test_sparse_sdpa.py, not a hand-rolled substitute, against any op
    you changed, C++ or Python. A submission that changes the gate is void, not competitive.
  - A lower-precision config (e.g. fp8 K/V) is legitimate but a lesser result than an algorithm
    or kernel improvement at the same precision — report which category your best result falls
    into, don't conflate them.

INFO ABOUT THE PROBLEM
  - Kernel timing is noisy; the ranking metric varies run to run.
  - Perf reference, which harness.py's defaults already target: S=640, T=56320, TOPK=2048,
    k_chunk_size=256, H=32, K_DIM=576, V_DIM=512, nv="sparse" (256 valid keys/query) — upstream's
    own test_sparse_sdpa_perf, id="prod-sparse". Official bf16 reference: 0.576ms.
  - That official perf test needs IsProgramRealtimeProfilerActive() (host IOMMU), which this
    cluster's device lacks — run as-is it would pytest.fail(), not skip. harness.py handles it:
    real-time profiler first, then trace-capture replay, then synced wall-clock, validated on
    hardware to reproduce the official reference within ~1% (583us vs 576us, bf16) — re-confirm
    any time TT_METAL_REF changes.
  - seed/job.yaml's cpu/memory/storage are a generous starting point, not a cap — raise them in
    your own submissions when you need to, especially for a from-source build.
  - One Tenstorrent ASIC per pod; only that device node is visible in the container.
```

---

## Coordinator notes (not sent to agents)

- Ranking metric: `kernel_duration_ns`, minimize. Also declare `pcc_achieved` and
  `program_cache_entries`.
- Agents start from this experiment's `seed/` (harness.py, Dockerfile.workload, job.yaml,
  build_and_run.sh) plus the baked-in `agents/coordinator/experiments/` samples ($WORKLOAD_SAMPLES).
- Build/refresh the job image with `make sparse-sdpa-workload-image` and smoke-test it on real
  hardware before spawning agents — see instructions.md.
