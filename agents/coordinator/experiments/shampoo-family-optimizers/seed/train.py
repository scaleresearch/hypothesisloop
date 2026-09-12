"""Training harness for the shampoo-family-optimizers experiment.

Model: MLP autoencoder on Fashion-MNIST, 784-256-64-256-784, GELU, no BatchNorm (BN masks
preconditioning gains). This is the config-only path -- every hyperparameter is an env var /
CLI flag, never a code change (experiment-checklist.md item 1).

Device: forward/backward run on --device (cpu/cuda/tt). The Shampoo-family optimizer *step*
(matrix inverse-roots, eigendecompositions) always runs on CPU in fp32 regardless of --device --
tt-metal/ttnn has no production eigh/SVD/matrix-inverse-root op, so those factors are moved to
host, computed there, and moved back (see optimizers.py module docstring and the coordinator's
findings file for why this is the only tractable architecture right now, per Fable review).
--device tt requires a ttnn-enabled image; falls back to a clear error (not a silent no-op) if
ttnn is unavailable, so a missing device mount reads as a real failure, not a quietly-wrong CPU run.

Reports (metrics.py, steady cadence, never once at the end):
  test_loss_to_target        RANKING METRIC (see experiment.md) -- FLOPs-to-target-loss (minimize)
  step                       attribute -- current step
  wall_clock_seconds         attribute
  optimizer_step_flops       attribute -- analytic FLOP count for this optimizer's step, this call
  diverged                   attribute -- 1.0 if loss is NaN/Inf, else 0.0
"""

import argparse
import os
import sys
import time

import torch
import torch.nn as nn
import torch.nn.functional as F

sys.path.insert(0, os.path.dirname(__file__))
from optimizers import build_optimizer  # noqa: E402
from metrics import post_metric  # noqa: E402
from loglines import LogTail  # noqa: E402


class AutoencoderMLP(nn.Module):
    def __init__(self, hidden: int = 256, latent: int = 64):
        super().__init__()
        self.enc1 = nn.Linear(784, hidden)
        self.enc2 = nn.Linear(hidden, latent)
        self.dec1 = nn.Linear(latent, hidden)
        self.dec2 = nn.Linear(hidden, 784)

    def forward(self, x):
        x = x.view(x.shape[0], -1)
        h = F.gelu(self.enc1(x))
        z = F.gelu(self.enc2(h))
        h = F.gelu(self.dec1(z))
        return self.dec2(h)


# Analytic per-step optimizer FLOP counts (dominant term only: the Kronecker-factor matmuls and
# matrix-power/eigendecomp calls, O(d^3) each, amortized over precondition_freq / eigenbasis_freq
# where applicable). Used to report `optimizer_step_flops` -- the basis for the FLOPs-to-target
# ranking metric (see experiment.md METRICS -- chosen over steps-to-target specifically because
# steps-to-target hides Shampoo-family's much higher per-step cost, per Fable design review).
def estimate_optimizer_flops(name: str, param_shapes: list, freq: int = 10) -> float:
    def cube(n):
        return n ** 3

    total = 0.0
    for shape in param_shapes:
        d0 = shape[0]
        d1 = shape[1] if len(shape) > 1 else 1
        if name in ("sgd", "adamw", "muon"):
            total += 10 * d0 * d1  # elementwise-dominated
        elif name == "full_adagrad":
            n = d0 * d1
            total += cube(n)  # full-matrix root every step -- deliberately the expensive oracle
        elif name in ("shampoo_2018",):
            total += 2 * (cube(d0) + cube(d1)) + 4 * d0 * d1 * max(d0, d1)  # root every step
        elif name in ("scalable_shampoo", "soap", "kl_shampoo", "online_kl_shampoo"):
            total += (2 * (cube(d0) + cube(d1))) / max(freq, 1) + 4 * d0 * d1 * max(d0, d1)
    return total


def load_fashion_mnist(data_dir: str, device: str):
    import torchvision

    train_ds = torchvision.datasets.FashionMNIST(data_dir, train=True, download=True)
    test_ds = torchvision.datasets.FashionMNIST(data_dir, train=False, download=True)
    train_x = (train_ds.data.float() / 255.0).to(device)
    test_x = (test_ds.data.float() / 255.0).to(device)
    return train_x, test_x


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--optimizer", default=os.environ.get("OPT_NAME", "adamw"))
    p.add_argument("--lr", type=float, default=float(os.environ.get("OPT_LR", "1e-3")))
    p.add_argument("--precondition-freq", type=int, default=int(os.environ.get("OPT_PRECOND_FREQ", "10")))
    p.add_argument("--batch-size", type=int, default=int(os.environ.get("BATCH_SIZE", "1024")))
    p.add_argument("--steps", type=int, default=int(os.environ.get("STEPS", "3000")))
    p.add_argument("--target-loss", type=float, default=float(os.environ.get("TARGET_LOSS", "0.012")))
    p.add_argument("--seed", type=int, default=int(os.environ.get("SEED", "0")))
    p.add_argument("--device", default=os.environ.get("TRAIN_DEVICE", "cpu"))
    p.add_argument("--data-dir", default=os.environ.get("DATA_DIR", "/tmp/fashion-mnist"))
    p.add_argument("--report-every", type=int, default=int(os.environ.get("REPORT_EVERY_STEPS", "25")))
    args = p.parse_args()

    torch.manual_seed(args.seed)
    log_tail = LogTail()

    fwd_device = args.device
    if fwd_device == "tt":
        try:
            import ttnn  # noqa: F401
        except ImportError:
            print("FATAL: --device tt requires ttnn; this image/container has no ttnn install. "
                  "Refusing to silently fall back to cpu -- fix the image or pass --device cpu.",
                  file=sys.stderr)
            log_tail.push()
            sys.exit(1)
        print("NOTE: --device tt requested. ttnn forward/backward wiring for this model is not "
              "yet implemented in this seed (see experiment.md TENSTORRENT INTEGRATION STATUS) -- "
              "running forward/backward on cpu instead so training itself is still correct; only "
              "the on-device-execution comparison is unavailable until that wiring lands.",
              file=sys.stderr)
        fwd_device = "cpu"

    model = AutoencoderMLP().to(fwd_device)
    optimizer = build_optimizer(args.optimizer, model, args.lr, precondition_freq=args.precondition_freq)

    train_x, test_x = load_fashion_mnist(args.data_dir, fwd_device)
    n = train_x.shape[0]

    param_shapes = [tuple(p.shape) for p in model.parameters()]

    start = time.time()
    diverged = False
    step = 0
    for step in range(1, args.steps + 1):
        idx = torch.randint(0, n, (args.batch_size,), device=fwd_device)
        batch = train_x[idx]
        optimizer.zero_grad(set_to_none=True)
        out = model(batch)
        loss = F.mse_loss(out, batch.view(batch.shape[0], -1))
        loss.backward()
        optimizer.step()

        if not torch.isfinite(loss):
            diverged = True
            print(f"step {step}: loss diverged ({loss.item()})", file=sys.stderr)
            break

        if step % args.report_every == 0 or step == args.steps:
            with torch.no_grad():
                test_out = model(test_x)
                test_loss = F.mse_loss(test_out, test_x.view(test_x.shape[0], -1)).item()
            elapsed = time.time() - start
            opt_flops = estimate_optimizer_flops(args.optimizer, param_shapes, args.precondition_freq)
            frac = step / args.steps
            print(f"step={step} frac={frac:.3f} train_loss={loss.item():.6f} "
                  f"test_loss={test_loss:.6f} elapsed={elapsed:.1f}s")
            post_metric(frac, test_loss, "test_loss")
            post_metric(frac, float(step), "step")
            post_metric(frac, elapsed, "wall_clock_seconds")
            post_metric(frac, opt_flops, "optimizer_step_flops")
            post_metric(frac, 0.0, "diverged")
            log_tail.push()
            if test_loss <= args.target_loss:
                print(f"TARGET REACHED at step {step}, test_loss={test_loss:.6f}")
                break

    if diverged:
        post_metric(1.0, float("inf"), "test_loss")
        post_metric(1.0, 1.0, "diverged")
        log_tail.push()
        sys.exit(1)


if __name__ == "__main__":
    main()
