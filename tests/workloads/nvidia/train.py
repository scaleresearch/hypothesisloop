#!/usr/bin/env python3
"""
HypothesisLoop NVIDIA workload — real hardware, not a stub.

Same role as tests/workloads/tenstorrent/train.py, torch/CUDA instead of ttnn: runs real
matmuls on the GPU the container was actually given (see Dockerfile.train and podexec's
DeviceRequests) and reports genuine measurements, not synthetic ones.

Metric contract mirrors the Tenstorrent workload so both hardware-benchmark scenarios rank the
same way (controller/stages_rank.go): tflops_measured (running max, maximize), tflops_step (raw
per-size value), latency_ms (mean per-matmul wall clock, minimize). Do not clamp tflops_measured
to a fixed reference ceiling — see tenstorrent/train.py's doc comment for why that flattens the
stage ladder's ranking spread.
"""

import os
import sys
import json
import time
import urllib.request

EXP_ID     = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
AGENT_ID   = os.environ.get("HYPOTHESISLOOP_AGENT_ID", "agent-dev")
PROJECT_ID = os.environ.get("HYPOTHESISLOOP_PROJECT_ID", "dev")
API_URL    = os.environ.get("HYPOTHESISLOOP_API_URL", "http://localhost:8081")

# Repeats the sweep in a loop, sleeping REPEAT_SLEEP_SECONDS between passes, until killed —
# lets a scenario request a job that stays RUNNING long enough to observe live metrics and/or
# test eviction, without a separate workload script.
REPEAT_FOREVER = os.environ.get("HYPOTHESISLOOP_REPEAT_FOREVER", "") not in ("", "0", "false")
REPEAT_SLEEP_SECONDS = float(os.environ.get("HYPOTHESISLOOP_REPEAT_SLEEP_SECONDS", "3"))

MATMUL_SIZES = [512, 1024, 2048, 4096, 8192]
WARMUP_ITERS = 5
TIMED_ITERS = 20


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr)


def main() -> None:
    import torch  # imported here so --help/import errors are legible

    if not torch.cuda.is_available():
        print("ERROR: torch.cuda.is_available() is False — no GPU was actually passed through", file=sys.stderr)
        sys.exit(1)

    device = torch.device("cuda:0")
    name = torch.cuda.get_device_name(device)
    print("HypothesisLoop NVIDIA workload starting (real hardware)")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}  project: {PROJECT_ID}")
    print(f"  device: {name}  matmul sweep: {MATMUL_SIZES}")

    running_max_tflops = 0.0
    total = len(MATMUL_SIZES)
    pass_num = 0
    while True:
        pass_num += 1
        for step, n in enumerate(MATMUL_SIZES):
            a = torch.randn((n, n), device=device, dtype=torch.float16)
            b = torch.randn((n, n), device=device, dtype=torch.float16)

            for _ in range(WARMUP_ITERS):
                torch.matmul(a, b)
            torch.cuda.synchronize()

            t0 = time.perf_counter()
            for _ in range(TIMED_ITERS):
                torch.matmul(a, b)
            torch.cuda.synchronize()
            dt = (time.perf_counter() - t0) / TIMED_ITERS

            flops = 2 * (n ** 3)
            tflops = flops / dt / 1e12
            latency_ms = dt * 1000.0
            running_max_tflops = max(running_max_tflops, tflops)

            fraction = (step + 1) / total
            print(f"  [pass {pass_num}] [{step}] n={n:5d}  latency={latency_ms:8.3f} ms  tflops={tflops:7.3f}  "
                  f"(running_max={running_max_tflops:7.3f})")

            post_metric(fraction, running_max_tflops, "tflops_measured")
            post_metric(fraction, tflops, "tflops_step")
            post_metric(fraction, latency_ms, "latency_ms")

        if not REPEAT_FOREVER:
            break
        time.sleep(REPEAT_SLEEP_SECONDS)

    print(f"\nDone. best measured: {running_max_tflops:.3f} TFLOPS (fp16 matmul, {name})")


if __name__ == "__main__":
    main()
