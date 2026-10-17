#!/usr/bin/env python3
"""
HypothesisLoop dummy-tt-matmul workload: real bf16 matrix multiplications on the Tenstorrent device
allocated to this job, opened via ttnn, each one timed.

Environment variables the platform injects:
  HYPOTHESISLOOP_EXPERIMENT_ID, HYPOTHESISLOOP_AGENT_ID, HYPOTHESISLOOP_PROJECT_ID
  HYPOTHESISLOOP_API_URL, HYPOTHESISLOOP_TOKEN

Metrics, each under its own name and in its own units:
  tflops_measured  best bf16 matmul TFLOPS so far (maximize), the ranking metric. Reported as a
                   running max, so a small matrix size visited late does not move the ranking.
  tflops_step      the measurement for this matrix size, for the raw curve.
  latency_ms       mean wall-clock per matmul at this size (minimize).

No normalized [0,1] "progress" metric: a ranking metric with a ceiling saturates, pinning every
agent to the same value on its first unmodified run, and the stage ladder can never separate
them again.
"""

import os
import sys
import json
import time
import urllib.request

EXP_ID     = os.environ["HYPOTHESISLOOP_EXPERIMENT_ID"]
AGENT_ID   = os.environ["HYPOTHESISLOOP_AGENT_ID"]
PROJECT_ID = os.environ["HYPOTHESISLOOP_PROJECT_ID"]
API_URL    = os.environ["HYPOTHESISLOOP_API_URL"]

# The job holds exactly one Tenstorrent device (accelerator_count: 1 in the job spec) and only
# that device's /dev/tenstorrent/N node is visible inside the container. ttnn enumerates what is
# visible from local index 0, so device_id is 0 whichever physical card was allocated.
DEVICE_ID = 0

MATMUL_SIZES = [256, 512, 1024, 2048, 4096]
WARMUP_ITERS = 2
TIMED_ITERS = 10


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", "Authorization": f"Bearer {os.environ['HYPOTHESISLOOP_TOKEN']}"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        # The response body is the control plane's reason. A refused token stays refused, so
        # that is said once, not on every sample.
        reason = e.read().decode(errors="replace")[:300] if hasattr(e, "read") else ""
        if getattr(e, "code", None) in (401, 403):
            if not getattr(post_metric, "refused", False):
                post_metric.refused = True
                print(f"  [error] metric POST refused, this run's metrics are not being recorded: {e} {reason}", file=sys.stderr, flush=True)
            return
        print(f"  [warn] metric POST failed: {e} {reason}", file=sys.stderr, flush=True)


def main() -> None:
    import ttnn  # imported here (not at module scope) so --help/import errors are legible

    print("HypothesisLoop Tenstorrent workload starting (real hardware)")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}  project: {PROJECT_ID}")
    print(f"  matmul sweep: {MATMUL_SIZES}  device_id: {DEVICE_ID}")

    device = ttnn.open_device(device_id=DEVICE_ID)
    running_max_tflops = 0.0
    try:
        total = len(MATMUL_SIZES)
        for step, n in enumerate(MATMUL_SIZES):
            a = ttnn.rand([n, n], device=device, dtype=ttnn.bfloat16, layout=ttnn.TILE_LAYOUT)
            b = ttnn.rand([n, n], device=device, dtype=ttnn.bfloat16, layout=ttnn.TILE_LAYOUT)

            for _ in range(WARMUP_ITERS):
                ttnn.matmul(a, b)
            ttnn.synchronize_device(device)

            t0 = time.perf_counter()
            for _ in range(TIMED_ITERS):
                ttnn.matmul(a, b)
            ttnn.synchronize_device(device)
            dt = (time.perf_counter() - t0) / TIMED_ITERS

            flops = 2 * (n ** 3)
            tflops = flops / dt / 1e12
            latency_ms = dt * 1000.0
            running_max_tflops = max(running_max_tflops, tflops)

            fraction = (step + 1) / total
            print(f"  [{step}] n={n:5d}  latency={latency_ms:8.3f} ms  tflops={tflops:7.3f}  "
                  f"(running_max={running_max_tflops:7.3f})")

            post_metric(fraction, running_max_tflops, "tflops_measured")
            post_metric(fraction, tflops, "tflops_step")
            post_metric(fraction, latency_ms, "latency_ms")
    finally:
        ttnn.close_device(device)

    print(f"\nDone. best measured: {running_max_tflops:.3f} TFLOPS (bf16 matmul, device {DEVICE_ID})")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import traceback
        print(traceback.format_exc())
        raise
