#!/usr/bin/env python3
"""
HypothesisLoop Tenstorrent workload — real hardware, not a stub.

Unlike tests/workloads/generic/train.py (a synthetic-metrics simulator shared by every other
accelerator type), this one actually opens the Tenstorrent device DRA allocated to this pod via ttnn
and runs real matrix multiplications on it, timing each one for real. Runs inside Tenstorrent's
own official tt-metal release image (see Dockerfile.train) — verified against a QuietBox's Blackhole
cards with a manual `docker run --device /dev/tenstorrent` smoke test before this script was
written (KMD 2.10.0, firmware 19.11.0, sustained ~106 TFLOPS bf16 at n=2048).

Environment variables injected by the scheduler (see tests/workloads/generic/train.py's doc comment —
identical contract, this workload is a drop-in for the same HYPOTHESISLOOP_* env vars):
  HYPOTHESISLOOP_EXPERIMENT_ID, HYPOTHESISLOOP_AGENT_ID, HYPOTHESISLOOP_PROJECT_ID
  HYPOTHESISLOOP_API_URL
  HYPOTHESISLOOP_DURATION_SECONDS - keep repeating the matmul sweep for this long (default 0:
                                    one sweep), for runs that must hold a device a while

Metric contract: this workload pushes its real measurements under their own names, in their
own units — nothing is normalized or rescaled. A platform experiment's MetricDefinition.Key
is a free-form Prometheus metric name (controlplane/domain/platform_experiment.go),
so a benchmark workload ranks on benchmark numbers directly; there is no fixed metric
vocabulary to fork or repurpose.

  tflops_measured — best bf16 matmul TFLOPS so far (maximize). This is the ranking metric.
                    Reported as a running max, not the per-step value, so it is monotonically
                    non-decreasing across the sweep, so the ranking is stable
                    just because the sweep visits a small matrix size late.
  tflops_step     — the individual measurement for this matrix size, for the raw curve.
  latency_ms      — mean wall-clock per matmul at this size (minimize).

Do not reintroduce a normalized [0,1] "progress" metric here. A ranking metric with a ceiling
saturates: the measured baseline on a Blackhole QuietBox is ~182 TFLOPS, so any clamp at a
lower reference pins every agent to the same value on their first unmodified run and the stage
ladder (controller/stages_rank.go ranks agents on the observed spread) can never separate them
again — a full tie group is always kept whole, so no one is ever cut.
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

# DRA gives this pod exactly one Tenstorrent device (accelerator_count: 1 in
# tests/workloads/tenstorrent/job.yaml) and only that device's /dev/tenstorrent/N node is
# visible inside the container — ttnn/UMD enumerate whatever's visible starting at local
# index 0, so device_id is always 0 here regardless of which physical card (tt-0..tt-3) the
# platform actually allocated; see the manual smoke test that established this.
DEVICE_ID = 0

MATMUL_SIZES = [256, 512, 1024, 2048, 4096]
WARMUP_ITERS = 2
TIMED_ITERS = 10
DURATION = float(os.environ.get("HYPOTHESISLOOP_DURATION_SECONDS", "0"))


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
    started = time.monotonic()
    try:
        total = len(MATMUL_SIZES)
        step = -1
        while True:
            step += 1
            if step >= total and time.monotonic() - started >= DURATION:
                break
            n = MATMUL_SIZES[step % total]
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

            if DURATION > 0:
                fraction = min(1.0, (time.monotonic() - started) / DURATION)
            else:
                fraction = (step + 1) / total
            print(f"  [{step}] n={n:5d}  latency={latency_ms:8.3f} ms  tflops={tflops:7.3f}  "
                  f"(running_max={running_max_tflops:7.3f})")

            post_metric(fraction, running_max_tflops, "tflops_measured")
            post_metric(fraction, tflops, "tflops_step")
            post_metric(fraction, latency_ms, "latency_ms")
            if DURATION > 0:
                time.sleep(1)
    finally:
        ttnn.close_device(device)

    print(f"\nDone. best measured: {running_max_tflops:.3f} TFLOPS (bf16 matmul, device {DEVICE_ID})")


if __name__ == "__main__":
    main()
