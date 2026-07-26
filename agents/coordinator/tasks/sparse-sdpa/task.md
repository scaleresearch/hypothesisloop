OBJECTIVE
Minimize the device kernel duration of ttnn.transformer.sparse_sdpa / SparseSDPAMsaOperation on Tenstorrent Blackhole.

Source is baked into your container: $TT_METAL_SRC (TT-NN at $TTNN_SRC), op under ttnn/cpp/ttnn/operations/transformer/sdpa/.

METRICS TO REPORT
  kernel_duration_ns  (minimize, RANKING METRIC) — DEVICE KERNEL DURATION [ns]
      for SparseSDPAOperation / SparseSDPAMsaOperation. Report as a running
      minimum so it never increases.
  pcc_achieved        — PCC vs the Torch sparse_mla() golden, full precision.
  program_cache_entries — program-cache entry count for the run.

Report all three at a steady cadence while the job runs, not once at the end.
Never fabricate or inflate a metric; nothing checks this server-side.

CONSTRAINTS
  - PCC >= 0.99 vs the sparse_mla() golden in
    tests/ttnn/unit_tests/operations/sdpa/sparse_sdpa_test_utils.py.
  - Falling below your own baseline PCC is a regression even if it clears 0.99.
  - program_cache_entries must match baseline — this blocks "speedups" that come
    from recompilation or shape over-specialization.
  - The existing tests define correctness. Do not weaken, redefine, skip, or
    bypass them. A submission that changes the gate is void, not competitive.

FACTS ABOUT THE PROBLEM
  - Kernel timing is noisy; the ranking metric varies run to run.
  - The perf number comes from:
      python -m tracy -p -r -v -m pytest \
        tests/ttnn/nightly/unit_tests/operations/sdpa/test_sparse_sdpa_msa_perf.py
  - agents/coordinator/tasks/dummy-tt-matmul/seed/ shows how a job opens its
    DRA-allocated device and reports metrics — a starting point, not a template.
  - One Tenstorrent ASIC per pod; only that device node is visible in the
    container.
