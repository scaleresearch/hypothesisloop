## EXPERIMENT DESCRIPTION

```
OBJECTIVE
Maximize sustained bf16 matmul throughput (TFLOPS) on Tenstorrent Blackhole.
This is an end-to-end shakedown of the full agent journey on real hardware, using
a simple, self-contained workload — not a deep kernel-optimization task.

STARTING POINT
Your shared code repo already contains a working Blackhole matmul benchmark: it opens
the DRA-allocated device via ttnn, runs bf16 matmuls across a range of sizes, times
them, and reports the metrics below. Clone it, branch, and iterate — you should not
need to write much new code, just adapt parameters (matmul sizes, warmup/timed iters,
dtype, tiling) and observe.

METRICS TO REPORT
  tflops_measured (maximize, RANKING METRIC) — best sustained bf16 matmul TFLOPS,
      reported as a running max (monotonic).
  latency_ms — mean wall-clock per matmul at the current size (minimize).

CONSTRAINTS
  - Runs on one Tenstorrent Blackhole ASIC per pod (accelerator_type
    tenstorrent.com/chipArch=blackhole, accelerator_count 1).
  - Report metrics at a steady cadence while the job runs, not once at the end.
  - Never fabricate or inflate a metric; nothing checks this server-side.

FACTS ABOUT THE PROBLEM
  - The measured baseline on this QuietBox's Blackhole is ~180 TFLOPS bf16.
  - Kernel/matmul timing is noisy; a gain inside run-to-run variance is not a gain.
```

---

## Coordinator notes (not sent to agents)

- Ranking metric: `tflops_measured`, maximize. Also declare `latency_ms` (minimize).
- Purpose: full e2e shakedown on REAL Blackhole hardware — coordinator prepares the
  environment and creates the experiment; two research agents sign up, clone the
  seeded repo, submit real jobs, and iterate. Uses the simple matmul workload so the
  agents barely need to change code.
- The shared repo's `main` branch is seeded with this task's own `seed/` directory
  (agents/coordinator/tasks/dummy-tt-matmul/seed/) as the agents' starting point —
  a self-contained copy the coordinator owns, not a reference into tests/workloads/.
