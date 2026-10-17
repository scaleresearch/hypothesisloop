"""Training harness for the shampoo-family-optimizers experiment.

Model: MLP autoencoder on Fashion-MNIST, 784-256-64-256-784, GELU, no BatchNorm (BN masks
preconditioning gains). This is the config-only path -- every hyperparameter is an env var /
CLI flag, never a code change.

Device: forward/backward run on --device (cpu/cuda/tt). The Shampoo-family optimizer *step*
(matrix inverse-roots, eigendecompositions) always runs on CPU in fp32 regardless of --device --
tt-metal/ttnn has no production eigh/SVD/matrix-inverse-root op, so those factors are moved to
host, computed there, and moved back (see optimizers.py module docstring).
--device tt exits with an error until its ttnn forward/backward is implemented.

Reports (metrics.py), at a steady cadence:
  test_loss                  test MSE at this step
  step                       attribute -- current step
  wall_clock_seconds         attribute
  optimizer_step_flops       analytic FLOP count of one optimizer step
  diverged                   the gate, a constraint -- 1.0 if loss is NaN/Inf, else 0.0. A run
                             that diverged ran to the end, so it exits 0.
and once, at the report where test_loss first reaches the target:
  optimizer_flops_to_target  RANKING METRIC (minimize) -- steps taken times the analytic FLOPs of
                             one step (forward + backward + optimizer step). The platform ranks a
                             job on the lowest value it ever reported, so a running total would
                             rank every job on its first report. A run that never reaches the
                             target reports none and ranks nowhere.
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
# ranking metric (chosen over steps-to-target because steps-to-target hides Shampoo-family's much
# higher per-step cost).
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


# Analytic FLOPs of one forward and backward pass (dominant term only: the matmul of each weight
# matrix, 2*B*d0*d1 forward, and twice that backward for the weight and the input gradients).
def estimate_forward_backward_flops(param_shapes: list, batch_size: int) -> float:
    return float(sum(6 * batch_size * shape[0] * shape[1] for shape in param_shapes if len(shape) > 1))


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

    fwd_device = args.device
    if fwd_device == "tt":
        print("FATAL: --device tt forward/backward is not implemented; pass --device cpu.", file=sys.stderr)
        sys.exit(1)

    model = AutoencoderMLP().to(fwd_device)
    optimizer = build_optimizer(args.optimizer, model, args.lr, precondition_freq=args.precondition_freq)

    train_x, test_x = load_fashion_mnist(args.data_dir, fwd_device)
    n = train_x.shape[0]

    param_shapes = [tuple(p.shape) for p in model.parameters()]
    opt_flops = estimate_optimizer_flops(args.optimizer, param_shapes, args.precondition_freq)
    step_flops = estimate_forward_backward_flops(param_shapes, args.batch_size) + opt_flops

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
            frac = step / args.steps
            print(f"step={step} frac={frac:.3f} train_loss={loss.item():.6f} "
                  f"test_loss={test_loss:.6f} elapsed={elapsed:.1f}s")
            post_metric(frac, test_loss, "test_loss")
            post_metric(frac, float(step), "step")
            post_metric(frac, elapsed, "wall_clock_seconds")
            post_metric(frac, opt_flops, "optimizer_step_flops")
            post_metric(frac, 0.0, "diverged")
            if test_loss <= args.target_loss:
                flops_to_target = step * step_flops
                print(f"TARGET REACHED at step {step}, test_loss={test_loss:.6f}, "
                      f"optimizer_flops_to_target={flops_to_target:.4g}")
                post_metric(frac, flops_to_target, "optimizer_flops_to_target")
                break

    if diverged:
        post_metric(1.0, 1.0, "diverged")


if __name__ == "__main__":
    main()
