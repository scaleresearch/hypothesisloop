#!/usr/bin/env python3
"""Unified sparse_sdpa (DSA op) measure-and-report harness for pe-d91322a7-style runs.

The point of this file: agents should NOT hand-write their own copy of make_inputs/golden/pcc,
their own upload-once/trace-capture timing loop, or their own metric-posting boilerplate --
every prior run had both agents independently reinvent (slightly differently) exactly this, which
cost real iteration time and made results harder to compare apples-to-apples. This is that single,
pre-built, pre-tested harness. It is a THIN wrapper around tt-metal's own real test code
(tests/ttnn/unit_tests/operations/sdpa/sparse_sdpa_test_utils.py) plus the extra pieces upstream
doesn't provide (upload-once-outside-the-timed-loop, trace-capture replay, an IOMMU-unavailable
fallback), proven working against real hardware on this QuietBox (see coordinator's smoke test
output alongside this file).

Not a template to copy-paste and diverge from: import and call it, or invoke it as a script.
Tunable purely via env vars, so pure config/hyperparameter sweeps need zero code changes -- that's
what makes rapid, high-scale iteration possible (every job is the same script, different env).

Usage as a job command: `python3 harness.py` (reads config from env vars below).
Usage as a library: `from harness import run_dsa_prod, DsaConfig` for a custom sweep driver.
"""

import os
import sys
import json
import time
import traceback
import urllib.request

sys.path.insert(0, os.environ.get("TT_METAL_TEST_UTILS_PATH", "/opt/tt-metal-tests"))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

EXP_ID = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
REG_URL = os.environ.get("HYPOTHESISLOOP_REGISTRY_URL", "http://localhost:8083")
DEVICE_ID = int(os.environ.get("DSA_DEVICE_ID", 0))

# Official "prod-sparse" shape from test_sparse_sdpa.py::test_sparse_sdpa_perf -- the canonical
# reference point (official bf16 duration on this shape: 0.576ms). Every knob below is
# individually overridable so a pure-config sweep needs zero code changes, but these are the
# defaults agents should optimize unless the hypothesis specifically calls for a different shape.
K_DIM = 576
V_DIM = 512
_NV = {
    "dense": lambda s, T, K: K,
    "half": lambda s, T, K: K // 2,
    "causal": lambda s, T, K: min(s + 1, K),
    "sparse": lambda s, T, K: 256,
    "mixed": lambda s, T, K: 1 + (s * 7) % K,
}


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    url = f"{REG_URL}/registry/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr)


def _run_op_compat(ttnn, tt_q, tt_kv, tt_idx, *, v_dim, scale, k_chunk_size, kv_dtype, compute_kernel_config=None):
    """Same call upstream's run_op makes, with one addition: the job image's prebuilt wheel can
    be older than $TT_METAL_SRC (confirmed on this cluster: no ttnn.transformer.SparseKVFormat in
    the deployed release wheel) -- fall back to calling without kv_format when it's absent rather
    than hard-failing. Only kicks in on the fast/prebuilt-wheel path; a build_and_run.sh build
    from current source never needs this fallback."""
    kwargs = {"scale": scale, "k_chunk_size": k_chunk_size}
    if compute_kernel_config is not None:
        kwargs["compute_kernel_config"] = compute_kernel_config
    if hasattr(ttnn.transformer, "SparseKVFormat"):
        kwargs["kv_format"] = (
            ttnn.transformer.SparseKVFormat.BF16
            if kv_dtype == ttnn.bfloat16
            else ttnn.transformer.SparseKVFormat.FP8_E4M3
        )
    return ttnn.transformer.sparse_sdpa(tt_q, tt_kv, tt_idx, v_dim, **kwargs)


def _out_to_torch(ttnn, tt_out):
    if tt_out.dtype == ttnn.fp8_e4m3:
        tt_out = ttnn.typecast(tt_out, ttnn.bfloat16)
    return ttnn.to_torch(tt_out)


def run_dsa_prod(*, kv_dtype_name="bfloat16", q_dtype_name="bfloat16", k_chunk_size=256, fidelity_name=None,
                  H=32, S=640, T=56320, TOPK=2048, nv_pattern="sparse", n_iters=100, n_warmup=5,
                  trace_reps=32, seed=7, device_id=DEVICE_ID, report=True):
    """Runs the official prod-sparse-shaped sparse_sdpa benchmark once, returns
    (best_kernel_duration_ns, final_pcc, program_cache_entries). Reuses tt-metal's own
    make_inputs/golden/to_dev/pcc directly; only the upload-once + timing-loop + kv_format
    compat shim is new here."""
    import torch
    import ttnn
    from tests.ttnn.unit_tests.operations.sdpa.sparse_sdpa_test_utils import make_inputs, golden, to_dev, pcc
    from tests.ttnn.profiling.realtime_profiler_utils import profile_realtime_program

    kv_dtype = getattr(ttnn, kv_dtype_name)
    q_dtype = getattr(ttnn, q_dtype_name)
    nv_fn = _NV[nv_pattern]
    q, kv, indices = make_inputs(H, S, T, TOPK, K_DIM, lambda s: nv_fn(s, T, TOPK), seed=seed)
    scale = K_DIM**-0.5
    golden_out = golden(q, kv, indices, scale, V_DIM)

    device = None
    try:
        device = ttnn.open_device(
            device_id=device_id, dispatch_core_config=ttnn.device.DispatchCoreConfig(ttnn.device.DispatchCoreType.WORKER)
        )
    except Exception:
        device = ttnn.open_device(device_id=device_id)

    try:
        try:
            device.enable_program_cache()
        except Exception:
            pass

        q_host = q.to(torch.bfloat16) if q_dtype == ttnn.bfloat16 else q.to(torch.float32)
        tt_q = to_dev(q_host, device, q_dtype)
        kv_host = kv.to(torch.bfloat16) if kv_dtype == ttnn.bfloat16 else kv.to(torch.float32)
        tt_kv = to_dev(kv_host, device, kv_dtype)
        tt_idx = to_dev(indices.to(torch.int32), device, ttnn.uint32)

        compute_kernel_config = None
        if fidelity_name:
            cfg_cls = getattr(ttnn, "WormholeComputeKernelConfig", None) or getattr(ttnn, "DeviceComputeKernelConfig", None)
            compute_kernel_config = cfg_cls(math_fidelity=getattr(ttnn.MathFidelity, fidelity_name))

        def run_once():
            return _run_op_compat(
                ttnn, tt_q, tt_kv, tt_idx, v_dim=V_DIM, scale=scale, k_chunk_size=k_chunk_size,
                kv_dtype=kv_dtype, compute_kernel_config=compute_kernel_config,
            )

        def check_pcc():
            return pcc(_out_to_torch(ttnn, run_once()), golden_out)

        for _ in range(n_warmup):
            run_once()
        ttnn.synchronize_device(device)

        # Layered timing strategy, most-accurate-first, proven across every run so far on this
        # cluster (no IOMMU -> RT profiler never active here, but this stays correct if a future
        # cluster does have it):
        use_rt_profiler = True
        try:
            profile_realtime_program(device, run_once)
        except Exception as e:
            use_rt_profiler = False
            print(f"  [info] RT profiler unavailable ({e}); using trace-capture replay")

        use_trace = False
        trace_id = None
        if not use_rt_profiler:
            try:
                trace_id = ttnn.begin_trace_capture(device, cq_id=0)
                out_traced = None
                for _ in range(trace_reps):
                    out_traced = run_once()
                ttnn.end_trace_capture(device, trace_id, cq_id=0)
                ttnn.execute_trace(device, trace_id, cq_id=0, blocking=True)
                use_trace = True

                def check_pcc():
                    ttnn.execute_trace(device, trace_id, cq_id=0, blocking=True)
                    return pcc(_out_to_torch(ttnn, out_traced), golden_out)
            except Exception as e:
                print(f"  [info] trace capture unavailable ({e}); falling back to wall-clock")

        def timed_call():
            if use_rt_profiler:
                _, rec = profile_realtime_program(device, run_once)
                return float(rec["duration_ns"])
            if use_trace:
                t0 = time.perf_counter()
                ttnn.execute_trace(device, trace_id, cq_id=0, blocking=True)
                return (time.perf_counter() - t0) * 1e9 / trace_reps
            ttnn.synchronize_device(device)
            t0 = time.perf_counter()
            run_once()
            ttnn.synchronize_device(device)
            return (time.perf_counter() - t0) * 1e9

        running_min_ns = None
        pcc_val = check_pcc()
        for i in range(n_iters):
            dur_ns = timed_call()
            running_min_ns = dur_ns if running_min_ns is None else min(running_min_ns, dur_ns)
            if (i + 1) % 20 == 0 or i == n_iters - 1:
                pcc_val = check_pcc()
            n_cache = device.num_program_cache_entries()
            if report:
                post_metric((i + 1) / n_iters, running_min_ns, "kernel_duration_ns")
                post_metric((i + 1) / n_iters, pcc_val, "pcc_achieved")
                post_metric((i + 1) / n_iters, n_cache, "program_cache_entries")
            print(f"  [{i}] dur={dur_ns:9.0f}ns running_min={running_min_ns:9.0f}ns pcc={pcc_val:.5f} cache={n_cache}")

        return running_min_ns, pcc_val, device.num_program_cache_entries()
    finally:
        ttnn.close_device(device)


def main() -> None:
    kv_dtype_name = os.environ.get("DSA_KV_DTYPE", "bfloat16")
    q_dtype_name = os.environ.get("DSA_Q_DTYPE", "bfloat16")
    kc = int(os.environ.get("DSA_KC", 256))
    fidelity_name = os.environ.get("DSA_FIDELITY")
    H = int(os.environ.get("DSA_H", 32))
    S = int(os.environ.get("DSA_S", 640))
    T = int(os.environ.get("DSA_T", 56320))
    TOPK = int(os.environ.get("DSA_TOPK", 2048))
    nv_pattern = os.environ.get("DSA_NV_PATTERN", "sparse")
    n_iters = int(os.environ.get("DSA_ITERS", 100))
    n_warmup = int(os.environ.get("DSA_WARMUP", 5))
    trace_reps = max(1, int(os.environ.get("DSA_TRACE_REPS", 32)))
    seed = int(os.environ.get("DSA_SEED", 7))

    print("HypothesisLoop unified sparse_sdpa harness starting")
    print(f"  H={H} S={S} T={T} TOPK={TOPK} kc={kc} nv={nv_pattern} kv_dtype={kv_dtype_name} q_dtype={q_dtype_name} fidelity={fidelity_name}")

    best_ns, pcc_val, cache_entries = run_dsa_prod(
        kv_dtype_name=kv_dtype_name, q_dtype_name=q_dtype_name, k_chunk_size=kc, fidelity_name=fidelity_name,
        H=H, S=S, T=T, TOPK=TOPK, nv_pattern=nv_pattern, n_iters=n_iters, n_warmup=n_warmup,
        trace_reps=trace_reps, seed=seed,
    )
    print(f"\nDone. best kernel_duration_ns={best_ns:.0f} final pcc={pcc_val:.5f} program_cache_entries={cache_entries}")


if __name__ == "__main__":
    print("harness.py: starting")
    try:
        main()
    except Exception:
        print(traceback.format_exc())
        raise
    else:
        print("harness.py: completed normally")
