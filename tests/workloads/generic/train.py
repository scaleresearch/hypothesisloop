#!/usr/bin/env python3
"""
OpenResearch ML workload — minimal stub, no real computation.

Emits realistic metrics on a real timer, sleeping OPENRESEARCH_REPORT_INTERVAL_SECONDS
between each step so the registry receives a live stream rather than a batch dump.

Environment variables injected by the scheduler:
  OPENRESEARCH_EXPERIMENT_ID           - unique experiment UUID
  OPENRESEARCH_AGENT_ID                - agent/researcher identifier
  OPENRESEARCH_PROJECT_ID              - project identifier
  OPENRESEARCH_PRIMARY_METRIC          - metric name to optimize (e.g. val_accuracy)
  OPENRESEARCH_METRIC_DIRECTION        - maximize | minimize
  OPENRESEARCH_REPORT_INTERVAL_SECONDS - seconds between metric pushes (default: 5)
  OPENRESEARCH_REGISTRY_URL            - OpenResearch registry HTTP base URL
  OPENRESEARCH_DURATION_SECONDS        - total run time in seconds (default: 60)
  OPENRESEARCH_BASELINE                - declared baseline value to beat
  OPENRESEARCH_ACCELERATOR_TYPE                - accelerator type label (T4 | L40 | A100)
  OPENRESEARCH_ACCELERATOR_COUNT               - number of accelerators
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
METRIC     = os.environ.get("OPENRESEARCH_PRIMARY_METRIC", "val_accuracy")
DIRECTION  = os.environ.get("OPENRESEARCH_METRIC_DIRECTION", "maximize")
INTERVAL   = int(os.environ.get("OPENRESEARCH_REPORT_INTERVAL_SECONDS", "5"))
DURATION   = int(os.environ.get("OPENRESEARCH_DURATION_SECONDS", "60"))
REG_URL    = os.environ.get("OPENRESEARCH_REGISTRY_URL", "http://localhost:8083")
BASELINE   = float(os.environ.get("OPENRESEARCH_BASELINE", "0.5"))
ACCELERATOR_TYPE   = os.environ.get("OPENRESEARCH_ACCELERATOR_TYPE", "T4")
ACCELERATOR_COUNT  = int(os.environ.get("OPENRESEARCH_ACCELERATOR_COUNT", "1"))


# Per-agent hyperparameter sampling (stable from same agent seed)

agent_rng = random.Random(hash(AGENT_ID) & 0xFFFFFFFF)

LEARNING_RATE = agent_rng.choice([1e-4, 3e-4, 1e-3, 3e-3, 1e-2])
BATCH_SIZE    = agent_rng.choice([16, 32, 64, 128])
HIDDEN_DIM    = agent_rng.choice([64, 128, 256, 512])
DROPOUT       = round(agent_rng.uniform(0.1, 0.5), 2)
OPTIMIZER     = agent_rng.choice(["adam", "sgd", "adamw", "rmsprop"])
WEIGHT_DECAY  = agent_rng.choice([0.0, 1e-5, 1e-4, 1e-3])

# Per-experiment noise seed
try:
    exp_seed = int(EXP_ID.replace("-", "")[:8], 16)
except ValueError:
    exp_seed = hash(EXP_ID) & 0xFFFFFFFF
exp_rng = random.Random(exp_seed)

lr_quality  = 1.0 - abs(math.log10(LEARNING_RATE) + 3) / 3.0
dim_quality = math.log2(HIDDEN_DIM) / 9.0
hp_quality  = 0.5 * lr_quality + 0.3 * dim_quality + 0.2 * exp_rng.random()
hp_quality  = max(0.0, min(1.0, hp_quality))

TARGET = BASELINE + hp_quality * 0.25 + exp_rng.uniform(-0.02, 0.02)
# Floored comfortably above BASELINE (not just above it) — a target that ends up only
# marginally better than baseline produces a curve whose true per-step improvement is tiny
# enough for ordinary noise to obscure, especially late in the run (see _sigmoid's doc comment
# below). A firm minimum margin keeps every run's signal cleanly resolvable against its own
# noise without narrowing the variety of outcomes runs can land on above that floor.
TARGET = max(BASELINE + 0.15, min(0.98, TARGET))

# Noise scales with the run's own signal (TARGET - BASELINE) rather than a fixed absolute
# magnitude: a fixed-magnitude noise floor can swamp the true deltas late in the sigmoid's
# asymptotic tail (where consecutive steps improve by very little even on a genuinely-improving
# run), producing spurious non-improving streaks long enough to trip metric-decline eviction on
# a run that isn't actually declining. Scaling noise to a small fraction of the total swing keeps
# runs visibly noisy (so the metric stream still looks like real noisy training) while
# guaranteeing the underlying trend is always resolvable against it.
NOISE = exp_rng.uniform(0.01, 0.025) * abs(TARGET - BASELINE)


# Learning curve helpers (pure math, no computation)


# s=4.0 (softer than a typical textbook logistic) deliberately keeps meaningful slope all the
# way to f=1.0 instead of fully saturating a few steps before the run ends — a steep curve looks
# more like a real learning curve, but its near-zero-slope tail makes the last several reported
# points' true deltas smaller than realistic per-step noise, which is what was producing
# statistically real (not just simulation-config-driven) metric-decline false positives even on
# curves that were genuinely still improving. This still uses a distinct s/c per metric group
# below (accuracy vs. accuracy-with-offset) so the two don't move in perfect lockstep.
def _sigmoid(f: float, s: float = 4.0, c: float = 0.4) -> float:
    return 1.0 / (1.0 + math.exp(-s * (f - c)))

def val_accuracy_at(f: float) -> float:
    raw = BASELINE + (TARGET - BASELINE) * _sigmoid(f)
    return max(0.0, min(1.0, raw + exp_rng.gauss(0, NOISE)))

def train_accuracy_at(f: float) -> float:
    raw = BASELINE + (TARGET - BASELINE + 0.04) * _sigmoid(f, 5.0, 0.35)
    return max(0.0, min(1.0, raw + exp_rng.gauss(0, NOISE * 0.5)))

def val_loss_at(f: float) -> float:
    raw = 2.5 - 2.0 * _sigmoid(f)
    overfit = max(0.0, f - 0.8) * exp_rng.uniform(0.0, 0.3)
    return max(0.01, raw + overfit + exp_rng.gauss(0, 0.01))

def train_loss_at(f: float) -> float:
    return max(0.01, 2.5 - 2.2 * _sigmoid(f, 5.0, 0.35) + exp_rng.gauss(0, 0.008))

def lr_schedule(f: float) -> float:
    return LEARNING_RATE * (0.5 + 0.5 * math.cos(math.pi * f))


# Registry helpers


def post_metric(fraction: float, value: float, metric_name: str = METRIC) -> None:
    url = f"{REG_URL}/registry/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr)


def patch_status(status: str, final_metric=None) -> None:
    # Execution status (RUNNING/COMPLETED/FAILED) is owned by the control plane, derived from the
    # cluster agent's pod-phase reports — a workload cannot self-declare its lifecycle state. This
    # is intentionally a no-op; kept so the call sites read as documentation of the real transition.
    return



# Main: emit metrics on a real timer, sleeping between each step


def main() -> None:
    steps = max(4, DURATION // max(1, INTERVAL))

    print(f"OpenResearch workload starting")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}  project: {PROJECT_ID}")
    print(f"  metric: {METRIC} ({DIRECTION})  steps: {steps}  interval: {INTERVAL}s  total: ~{steps * INTERVAL}s")
    print(f"  pretend accelerator: {ACCELERATOR_COUNT}x {ACCELERATOR_TYPE}")
    print(f"  target: {TARGET:.4f}  baseline: {BASELINE:.4f}")
    print(f"  lr={LEARNING_RATE}  batch={BATCH_SIZE}  hidden={HIDDEN_DIM}  "
          f"dropout={DROPOUT}  optim={OPTIMIZER}  wd={WEIGHT_DECAY}")

    patch_status("RUNNING")

    best = None
    final_value = None

    for step in range(steps):
        fraction = (step + 1) / steps

        v_acc  = val_accuracy_at(fraction)
        t_acc  = train_accuracy_at(fraction)
        v_loss = val_loss_at(fraction)
        t_loss = train_loss_at(fraction)
        lr_now = lr_schedule(fraction)
        epoch  = int(fraction * 50)

        if best is None or v_acc > best:
            best = v_acc
        final_value = v_acc

        primary = v_acc if METRIC == "val_accuracy" else (v_loss if METRIC == "val_loss" else v_acc)
        post_metric(fraction, primary, metric_name=METRIC)
        # Secondary metrics: always emitted alongside the primary so the dashboard has more
        # than one series to plot per job, same as a real training run's eval sweep would.
        for name, value in (("val_loss", v_loss), ("train_accuracy", t_acc), ("train_loss", t_loss)):
            if name != METRIC:
                post_metric(fraction, value, metric_name=name)

        print(
            f"  [{step:3d}] epoch={epoch:2d}  val_acc={v_acc:.4f}  val_loss={v_loss:.4f}  "
            f"train_acc={t_acc:.4f}  train_loss={t_loss:.4f}  lr={lr_now:.2e}"
        )

        if step < steps - 1:
            time.sleep(INTERVAL)

    if final_value is None:
        final_value = val_accuracy_at(1.0)

    patch_status("COMPLETED", final_value)
    print(f"\nDone. final={final_value:.4f}  best={best:.4f}")


if __name__ == "__main__":
    main()
