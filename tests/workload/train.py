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
  OPENRESEARCH_GPU_TYPE                - GPU type label (T4 | L40 | A100)
  OPENRESEARCH_GPU_COUNT               - number of GPUs
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
GPU_TYPE   = os.environ.get("OPENRESEARCH_GPU_TYPE", "T4")
GPU_COUNT  = int(os.environ.get("OPENRESEARCH_GPU_COUNT", "1"))


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

NOISE = exp_rng.uniform(0.005, 0.02)

lr_quality  = 1.0 - abs(math.log10(LEARNING_RATE) + 3) / 3.0
dim_quality = math.log2(HIDDEN_DIM) / 9.0
hp_quality  = 0.5 * lr_quality + 0.3 * dim_quality + 0.2 * exp_rng.random()
hp_quality  = max(0.0, min(1.0, hp_quality))

TARGET = BASELINE + hp_quality * 0.25 + exp_rng.uniform(-0.02, 0.02)
TARGET = max(BASELINE - 0.05, min(0.98, TARGET))

if exp_rng.random() < 0.12:
    TARGET = BASELINE + exp_rng.uniform(-0.03, 0.02)


# Learning curve helpers (pure math, no computation)


def _sigmoid(f: float, s: float = 8.0, c: float = 0.4) -> float:
    return 1.0 / (1.0 + math.exp(-s * (f - c)))

def val_accuracy_at(f: float) -> float:
    raw = BASELINE + (TARGET - BASELINE) * _sigmoid(f)
    return max(0.0, min(1.0, raw + exp_rng.gauss(0, NOISE)))

def train_accuracy_at(f: float) -> float:
    raw = BASELINE + (TARGET - BASELINE + 0.04) * _sigmoid(f, 10.0, 0.35)
    return max(0.0, min(1.0, raw + exp_rng.gauss(0, NOISE * 0.5)))

def val_loss_at(f: float) -> float:
    raw = 2.5 - 2.0 * _sigmoid(f)
    overfit = max(0.0, f - 0.8) * exp_rng.uniform(0.0, 0.3)
    return max(0.01, raw + overfit + exp_rng.gauss(0, 0.05))

def train_loss_at(f: float) -> float:
    return max(0.01, 2.5 - 2.2 * _sigmoid(f, 10.0, 0.35) + exp_rng.gauss(0, 0.03))

def lr_schedule(f: float) -> float:
    return LEARNING_RATE * (0.5 + 0.5 * math.cos(math.pi * f))


# Registry helpers


def post_metric(fraction: float, value: float) -> None:
    url = f"{REG_URL}/registry/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": METRIC, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr)


def patch_status(status: str, final_metric=None) -> None:
    url = f"{REG_URL}/registry/experiments/{EXP_ID}/status"
    body: dict = {"status": status}
    if final_metric is not None:
        body["final_metric"] = final_metric
    payload = json.dumps(body).encode()
    req = urllib.request.Request(
        url, data=payload,
        headers={"Content-Type": "application/json"},
        method="PATCH",
    )
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] status PATCH failed: {e}", file=sys.stderr)



# Main: emit metrics on a real timer, sleeping between each step


def main() -> None:
    steps = max(4, DURATION // max(1, INTERVAL))

    print(f"OpenResearch workload starting")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}  project: {PROJECT_ID}")
    print(f"  metric: {METRIC} ({DIRECTION})  steps: {steps}  interval: {INTERVAL}s  total: ~{steps * INTERVAL}s")
    print(f"  pretend GPU: {GPU_COUNT}x {GPU_TYPE}")
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
        post_metric(fraction, primary)

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
