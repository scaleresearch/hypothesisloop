#!/usr/bin/env python3
"""
OpenResearch robotics workload — minimal stub, no real computation.

Simulates training a single VLA (vision-language-action) baseline policy for
robotic manipulation. Every job runs the *same* model/architecture — what
differs between competing agents is the hypothesis: a specific hyperparameter
bet (learning rate, action-chunk length, camera views, batch size) about what
will push the baseline's task success rate higher. Emits metrics on a real
timer, sleeping OPENRESEARCH_REPORT_INTERVAL_SECONDS between each step so the
registry receives a live stream rather than a batch dump.

Environment variables injected by the scheduler:
  OPENRESEARCH_EXPERIMENT_ID           - unique experiment UUID
  OPENRESEARCH_AGENT_ID                - agent/researcher identifier
  OPENRESEARCH_PROJECT_ID              - project identifier
  OPENRESEARCH_REPORT_INTERVAL_SECONDS - seconds between metric pushes (default: 5)
  OPENRESEARCH_REGISTRY_URL            - OpenResearch registry HTTP base URL
  OPENRESEARCH_DURATION_SECONDS        - total run time in seconds (default: 60)
  OPENRESEARCH_BASELINE                - declared baseline value to beat
  OPENRESEARCH_ACCELERATOR_TYPE                - accelerator type label (T4 | L40 | A100)
  OPENRESEARCH_ACCELERATOR_COUNT               - number of accelerators

Hypothesis knobs (the actual hyperparameters each competing job is betting on
— set explicitly per submission, not sampled, so results are reproducible and
attributable to the stated hypothesis):
  OPENRESEARCH_LEARNING_RATE           - optimizer learning rate (default: 3e-4)
  OPENRESEARCH_CHUNK_LEN               - action-chunk length (default: 16)
  OPENRESEARCH_CAMERA_VIEWS            - number of camera viewpoints (default: 1)
  OPENRESEARCH_BATCH_SIZE              - batch size (default: 32)
"""

import os
import sys
import math
import random
import json
import time
import urllib.request


# Config from environment

EXP_ID     = os.environ.get("OPENRESEARCH_EXPERIMENT_ID", "local-test")
AGENT_ID   = os.environ.get("OPENRESEARCH_AGENT_ID", "agent-dev")
PROJECT_ID = os.environ.get("OPENRESEARCH_PROJECT_ID", "dev")
METRIC     = "task_success_rate"
DIRECTION  = "maximize"
INTERVAL   = int(os.environ.get("OPENRESEARCH_REPORT_INTERVAL_SECONDS", "5"))
DURATION   = int(os.environ.get("OPENRESEARCH_DURATION_SECONDS", "60"))
REG_URL    = os.environ.get("OPENRESEARCH_REGISTRY_URL", "http://localhost:8083")
BASELINE   = float(os.environ.get("OPENRESEARCH_BASELINE", "0.30"))
ACCELERATOR_TYPE   = os.environ.get("OPENRESEARCH_ACCELERATOR_TYPE", "A100")
ACCELERATOR_COUNT  = int(os.environ.get("OPENRESEARCH_ACCELERATOR_COUNT", "1"))

# The hypothesis under test: same baseline model, explicit hyperparameter bet.
LEARNING_RATE = float(os.environ.get("OPENRESEARCH_LEARNING_RATE", "3e-4"))
CHUNK_LEN     = int(os.environ.get("OPENRESEARCH_CHUNK_LEN", "16"))
CAMERA_VIEWS  = int(os.environ.get("OPENRESEARCH_CAMERA_VIEWS", "1"))
BATCH_SIZE    = int(os.environ.get("OPENRESEARCH_BATCH_SIZE", "32"))

# Per-experiment noise seed — every run (even repeat rounds from the same agent)
# gets its own noise draw, so results vary run to run the way a real training
# run would, without being able to game the same seed twice.
try:
    exp_seed = int(EXP_ID.replace("-", "")[:8], 16)
except ValueError:
    exp_seed = hash(EXP_ID) & 0xFFFFFFFF
exp_rng = random.Random(exp_seed)

NOISE = exp_rng.uniform(0.01, 0.03)

# Hyperparameter quality prior — this is the "which hypothesis wins" signal.
# Bounded log-scale sweet spot around 3e-4, longer action chunks and more
# camera views help (diminishing returns), same baseline architecture throughout.
lr_quality  = 1.0 - min(1.0, abs(math.log10(LEARNING_RATE) - math.log10(3e-4)) / 1.5)
chunk_bonus = math.log2(max(1, CHUNK_LEN)) / 8.0
cam_bonus   = (CAMERA_VIEWS - 1) * 0.04
hp_quality  = 0.55 * lr_quality + 0.25 * chunk_bonus + 0.10 * cam_bonus + 0.10 * exp_rng.random()
hp_quality  = max(0.0, min(1.0, hp_quality))

TARGET_SUCCESS = BASELINE + hp_quality * (0.92 - BASELINE) + exp_rng.uniform(-0.03, 0.03)
TARGET_SUCCESS = max(BASELINE - 0.05, min(0.97, TARGET_SUCCESS))

# ~12% of runs are duds (unlucky rollout / policy collapse) regardless of hypothesis.
if exp_rng.random() < 0.12:
    TARGET_SUCCESS = BASELINE + exp_rng.uniform(-0.05, 0.03)


# Learning curve helpers (pure math, no computation)


def _sigmoid(f: float, s: float = 7.0, c: float = 0.45) -> float:
    return 1.0 / (1.0 + math.exp(-s * (f - c)))

def task_success_rate_at(f: float) -> float:
    raw = BASELINE + (TARGET_SUCCESS - BASELINE) * _sigmoid(f)
    return max(0.0, min(1.0, raw + exp_rng.gauss(0, NOISE)))

def action_mse_at(f: float) -> float:
    raw = 0.42 - 0.36 * _sigmoid(f, 8.0, 0.35)
    overfit = max(0.0, f - 0.85) * exp_rng.uniform(0.0, 0.02)
    return max(0.005, raw + overfit + exp_rng.gauss(0, 0.01))

def lr_schedule(f: float) -> float:
    return LEARNING_RATE * (0.5 + 0.5 * math.cos(math.pi * f))


# Registry helpers


def post_metric(fraction: float, metric_name: str, value: float) -> None:
    url = f"{REG_URL}/registry/experiments/{EXP_ID}/metrics"
    payload = json.dumps({
        "metric_name": metric_name,
        "fraction_complete": fraction,
        "metric_value": value,
    }).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed ({metric_name}): {e}", file=sys.stderr)


def patch_status(status: str, final_metric=None) -> None:
    # Execution status (RUNNING/COMPLETED/FAILED) is owned by the control plane, derived from the
    # cluster agent's pod-phase reports — a workload cannot self-declare its lifecycle state. This
    # is intentionally a no-op; kept so the call sites read as documentation of the real transition.
    return


# Main: emit metrics on a real timer, sleeping between each step


def main() -> None:
    steps = max(4, DURATION // max(1, INTERVAL))

    print("OpenResearch robotics (VLA baseline) workload starting")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}  project: {PROJECT_ID}")
    print(f"  primary metric: {METRIC} ({DIRECTION})")
    print(f"  steps: {steps}  interval: {INTERVAL}s  total: ~{steps * INTERVAL}s")
    print(f"  pretend accelerator: {ACCELERATOR_COUNT}x {ACCELERATOR_TYPE}")
    print(f"  target success: {TARGET_SUCCESS:.4f}  baseline: {BASELINE:.4f}")
    print(f"  hypothesis: lr={LEARNING_RATE}  chunk_len={CHUNK_LEN}  cameras={CAMERA_VIEWS}  batch={BATCH_SIZE}")

    patch_status("RUNNING")

    best = None
    final_value = None

    for step in range(steps):
        fraction = (step + 1) / steps

        success = task_success_rate_at(fraction)
        act_mse = action_mse_at(fraction)
        lr_now  = lr_schedule(fraction)
        epoch   = int(fraction * 50)

        if best is None or success > best:
            best = success
        final_value = success

        post_metric(fraction, "task_success_rate", success)
        post_metric(fraction, "action_mse", act_mse)

        print(
            f"  [{step:3d}] epoch={epoch:2d}  success={success:.4f}  action_mse={act_mse:.4f}  lr={lr_now:.2e}"
        )

        if step < steps - 1:
            time.sleep(INTERVAL)

    if final_value is None:
        final_value = task_success_rate_at(1.0)

    patch_status("COMPLETED", final_value)
    print(f"\nDone. final={final_value:.4f}  best={best:.4f}")


if __name__ == "__main__":
    main()
