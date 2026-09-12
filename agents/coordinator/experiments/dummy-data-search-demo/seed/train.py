#!/usr/bin/env python3
"""
HypothesisLoop dummy-data-search-demo workload.

A small synthetic binary-classification task, deliberately with three independent, cheap-to-vary
axes so a fleet can be judged on which axis it actually moved:

  ARCHITECTURE  HIDDEN_LAYERS (comma-separated widths, e.g. "32,16"), ACTIVATION (relu|tanh)
  HYPERPARAMS   LR, BATCH_SIZE, EPOCHS, L2
  DATA          N_SAMPLES, NOISE, N_FEATURES, CLASS_SEP, CURRICULUM (0|1 — easy-to-hard ordering)

No numpy-external ML dependency: a two-moons-style synthetic dataset and a plain numpy MLP,
trained with manual forward/backward, so the whole job runs in seconds on CPU and needs no real
accelerator — accelerator_type in job.yaml is a fake type purely to exercise the scheduler.

Metric contract:
  val_accuracy  (maximize, RANKING METRIC) — held-out accuracy, reported as a running max.
  train_loss    (minimize) — final-epoch mean training loss.
"""
import json
import os
import sys
import time
import urllib.request

import numpy as np

EXP_ID = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
AGENT_ID = os.environ.get("HYPOTHESISLOOP_AGENT_ID", "agent-dev")
API_URL = os.environ.get("HYPOTHESISLOOP_API_URL", "http://localhost:8081")

HIDDEN_LAYERS = [int(x) for x in os.environ.get("HIDDEN_LAYERS", "16").split(",") if x]
ACTIVATION = os.environ.get("ACTIVATION", "relu")

LR = float(os.environ.get("LR", "0.05"))
BATCH_SIZE = int(os.environ.get("BATCH_SIZE", "32"))
EPOCHS = int(os.environ.get("EPOCHS", "40"))
L2 = float(os.environ.get("L2", "0.0"))

N_SAMPLES = int(os.environ.get("N_SAMPLES", "800"))
NOISE = float(os.environ.get("NOISE", "0.25"))
N_FEATURES = int(os.environ.get("N_FEATURES", "2"))
CLASS_SEP = float(os.environ.get("CLASS_SEP", "1.0"))
CURRICULUM = os.environ.get("CURRICULUM", "0") == "1"
SEED = int(os.environ.get("SEED", "0"))


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction,
                           "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr)


def make_dataset(n, noise, n_features, class_sep, seed):
    """Two interleaved-moons-style clusters, embedded in n_features dims (extra dims are noise
    columns) so N_FEATURES itself is a meaningful data-axis knob."""
    rng = np.random.default_rng(seed)
    n0 = n // 2
    n1 = n - n0
    theta0 = rng.uniform(0, np.pi, n0)
    theta1 = rng.uniform(0, np.pi, n1)
    x0 = np.stack([np.cos(theta0), np.sin(theta0)], axis=1) * class_sep
    x1 = np.stack([1 - np.cos(theta1), 1 - np.sin(theta1) - 0.5], axis=1) * class_sep
    X2 = np.concatenate([x0, x1], axis=0)
    y = np.concatenate([np.zeros(n0), np.ones(n1)])
    X2 += rng.normal(scale=noise, size=X2.shape)
    if n_features > 2:
        extra = rng.normal(scale=noise, size=(n, n_features - 2))
        X = np.concatenate([X2, extra], axis=1)
    else:
        X = X2[:, :n_features]
    perm = rng.permutation(n)
    return X[perm], y[perm]


def difficulty_order(X, y, seed):
    """Curriculum ordering: samples closest to the opposite class's centroid (hardest) go last."""
    c0 = X[y == 0].mean(axis=0)
    c1 = X[y == 1].mean(axis=0)
    centroid = np.where(y[:, None] == 0, c1, c0)
    dist_to_opposite = np.linalg.norm(X - centroid, axis=1)
    # Easy = far from the opposite class's centroid, so sort descending.
    return np.argsort(-dist_to_opposite)


def relu(z):
    return np.maximum(0, z)


def relu_grad(z):
    return (z > 0).astype(z.dtype)


def act(z, kind):
    return relu(z) if kind == "relu" else np.tanh(z)


def act_grad(z, kind):
    return relu_grad(z) if kind == "relu" else (1 - np.tanh(z) ** 2)


def init_params(sizes, seed):
    rng = np.random.default_rng(seed)
    params = []
    for i in range(len(sizes) - 1):
        w = rng.normal(scale=np.sqrt(2.0 / sizes[i]), size=(sizes[i], sizes[i + 1]))
        b = np.zeros(sizes[i + 1])
        params.append([w, b])
    return params


def forward(params, X, activation):
    a = X
    cache = [a]
    for i, (w, b) in enumerate(params):
        z = a @ w + b
        if i < len(params) - 1:
            a = act(z, activation)
        else:
            a = 1 / (1 + np.exp(-z))  # sigmoid output
        cache.append((z, a))
    return cache


def train_one_epoch(params, X, y, lr, l2, activation, batch_size):
    n = X.shape[0]
    losses = []
    for start in range(0, n, batch_size):
        xb = X[start:start + batch_size]
        yb = y[start:start + batch_size]
        m = xb.shape[0]

        cache = forward(params, xb, activation)
        y_pred = cache[-1][1]  # (m, 1)
        yb_col = yb.reshape(-1, 1)  # (m, 1) -- avoids (m,1)-(m,) broadcasting to (m,m)
        eps = 1e-9
        loss = -np.mean(yb_col * np.log(y_pred + eps) + (1 - yb_col) * np.log(1 - y_pred + eps))
        losses.append(loss)

        # Backprop.
        grads = [None] * len(params)
        delta = (y_pred - yb_col) / m
        a_prev = cache[-2][1] if len(cache) > 2 else cache[0]
        for i in reversed(range(len(params))):
            w, b = params[i]
            a_in = cache[i] if i == 0 else cache[i][1]
            dw = a_in.T @ delta + l2 * w
            db = delta.sum(axis=0)
            grads[i] = (dw, db)
            if i > 0:
                z_prev = cache[i][0]
                delta = (delta @ w.T) * act_grad(z_prev, activation)

        for i, (w, b) in enumerate(params):
            dw, db = grads[i]
            w -= lr * dw
            b -= lr * db
    return float(np.mean(losses))


def evaluate(params, X, y, activation):
    cache = forward(params, X, activation)
    y_pred = cache[-1][1].ravel()  # (m, 1) -> (m,), matching y's shape
    acc = float(np.mean((y_pred > 0.5).astype(int) == y))
    return acc


def main():
    print("HypothesisLoop dummy-data-search-demo workload starting")
    print(f"  experiment: {EXP_ID}  agent: {AGENT_ID}")
    print(f"  architecture: hidden_layers={HIDDEN_LAYERS} activation={ACTIVATION}")
    print(f"  hyperparams: lr={LR} batch_size={BATCH_SIZE} epochs={EPOCHS} l2={L2}")
    print(f"  data: n_samples={N_SAMPLES} noise={NOISE} n_features={N_FEATURES} "
          f"class_sep={CLASS_SEP} curriculum={CURRICULUM}")

    X, y = make_dataset(N_SAMPLES, NOISE, N_FEATURES, CLASS_SEP, SEED)
    n_train = int(N_SAMPLES * 0.8)
    X_train, y_train = X[:n_train], y[:n_train]
    X_val, y_val = X[n_train:], y[n_train:]

    if CURRICULUM:
        order = difficulty_order(X_train, y_train, SEED)
        X_train, y_train = X_train[order], y_train[order]

    sizes = [N_FEATURES] + HIDDEN_LAYERS + [1]
    params = init_params(sizes, SEED)

    running_max_acc = 0.0
    for epoch in range(EPOCHS):
        if CURRICULUM:
            # Widen the visible training window each epoch: easy examples first, full set by the
            # final third of training. Only meaningful because X_train/y_train were pre-sorted
            # easy-to-hard above.
            frac_visible = min(1.0, 0.3 + 0.7 * (epoch / max(1, EPOCHS * 0.66)))
            n_visible = max(BATCH_SIZE, int(n_train * frac_visible))
            xb_epoch, yb_epoch = X_train[:n_visible], y_train[:n_visible]
        else:
            xb_epoch, yb_epoch = X_train, y_train

        perm = np.random.default_rng(SEED + epoch).permutation(len(xb_epoch))
        train_loss = train_one_epoch(params, xb_epoch[perm], yb_epoch[perm], LR, L2, ACTIVATION,
                                      BATCH_SIZE)
        val_acc = evaluate(params, X_val, y_val, ACTIVATION)
        running_max_acc = max(running_max_acc, val_acc)

        fraction = (epoch + 1) / EPOCHS
        print(f"  [{epoch}] train_loss={train_loss:.4f}  val_acc={val_acc:.4f}  "
              f"(running_max={running_max_acc:.4f})")
        post_metric(fraction, running_max_acc, "val_accuracy")
        post_metric(fraction, train_loss, "train_loss")

    print(f"\nDone. best val_accuracy: {running_max_acc:.4f}")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        import traceback
        print(traceback.format_exc())
        raise
