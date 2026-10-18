"""Training harness for the shampoo-family-optimizers experiment.

Model: MLP autoencoder on Fashion-MNIST, 784-256-64-256-784, GELU, no BatchNorm (BN masks
preconditioning gains). This is the config-only path -- every hyperparameter is an env var /
CLI flag, never a code change.

Device: forward/backward run on --device (cpu/cuda/tt). The Shampoo-family optimizer *step*
(matrix inverse-roots, eigendecompositions) always runs on CPU in fp32 regardless of --device --
tt-metal/ttnn has no production eigh/SVD/matrix-inverse-root op, so those factors are moved to
host, computed there, and moved back (see optimizers.py module docstring).
--device tt runs each Linear layer's matmuls on a TT device through ttnn (ttnn_linear.py), with
gradients and optimizer state on CPU; MUON_STEP_ON_TT=1 also runs Muon's Newton-Schulz there.

Reports (metrics.py), at a steady cadence:
  test_loss                  test MSE at this step
  step                       attribute -- current step
  wall_clock_seconds         attribute
  optimizer_step_flops       analytic FLOP count of one optimizer step
  diverged                   the gate, a constraint -- 1.0 if loss is NaN/Inf, else 0.0. A run
                             that diverged ran to the end, so it exits 0.
and once, at the step where test_loss first reaches the target:
  optimizer_flops_to_target  RANKING METRIC (minimize) -- the sum of every step's analytic FLOPs
                             (forward + backward + that step's optimizer cost). The platform ranks a
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
from optimizers import build_optimizer, DEFAULT_LR_BY_OPT  # noqa: E402
from metrics import post_metric  # noqa: E402


class AutoencoderMLP(nn.Module):
    """ttnn_device=None: standard nn.Linear layers (cpu/cuda). ttnn_device=<open ttnn device>:
    layers run via ttnn_linear.TTLinear so their matmuls genuinely execute on TT hardware
    (see ttnn_linear.py module docstring for scope/caveats -- integration proof, not a speedup)."""

    def __init__(self, hidden: int = 256, latent: int = 64, ttnn_device=None):
        super().__init__()
        if ttnn_device is None:
            self.enc1 = nn.Linear(784, hidden)
            self.enc2 = nn.Linear(hidden, latent)
            self.dec1 = nn.Linear(latent, hidden)
            self.dec2 = nn.Linear(hidden, 784)
        else:
            from ttnn_linear import TTLinear
            self.enc1 = TTLinear(784, hidden, ttnn_device)
            self.enc2 = TTLinear(hidden, latent, ttnn_device)
            self.dec1 = TTLinear(latent, hidden, ttnn_device)
            self.dec2 = TTLinear(hidden, 784, ttnn_device)

    def forward(self, x):
        x = x.view(x.shape[0], -1)
        h = F.gelu(self.enc1(x))
        z = F.gelu(self.enc2(h))
        h = F.gelu(self.dec1(z))
        return self.dec2(h)


class ResidualMLPBig(nn.Module):
    """~36.85M-param residual MLP, TTLinear-only path -- exists SOLELY to exercise real TT
    hardware at a scale that actually uses the chip (width-2048,
    4 residual blocks, 10 TTLinear calls/forward, dims tile-aligned to 32). This is NOT the
    experiment's benchmark model -- AutoencoderMLP above is fixed by the experiment and must
    never change; this class is only ever selected via --big-model / BIG_MODEL=1, opt-in, and
    only meaningful with ttnn_device set (a cpu/cuda run of this class is possible for
    correctness-checking against the ttnn path, but is not part of the optimizer leaderboard)."""

    IN_DIM = 784
    PAD_DIM = 800  # 784 padded up to a multiple of 32 for ttnn tile alignment
    WIDTH = 2048
    N_BLOCKS = 4

    def __init__(self, ttnn_device=None):
        super().__init__()
        Layer = nn.Linear
        if ttnn_device is not None:
            from ttnn_linear import TTLinear
            Layer = lambda i, o: TTLinear(i, o, ttnn_device)  # noqa: E731
        self.in_proj = Layer(self.PAD_DIM, self.WIDTH)
        self.blocks = nn.ModuleList([
            nn.ModuleList([Layer(self.WIDTH, self.WIDTH), Layer(self.WIDTH, self.WIDTH)])
            for _ in range(self.N_BLOCKS)
        ])
        self.out_proj = Layer(self.WIDTH, self.PAD_DIM)

    def forward(self, x):
        x = x.view(x.shape[0], -1)
        x = F.pad(x, (0, self.PAD_DIM - self.IN_DIM))
        x = F.gelu(self.in_proj(x))
        for lin1, lin2 in self.blocks:
            skip = x
            x = F.gelu(lin1(x))
            x = lin2(x)
            x = skip + 0.5 * x
        x = F.gelu(x)
        x = self.out_proj(x)
        return x[:, :self.IN_DIM]


def _divmod_even_block_sizes(dim: int, block_size: int) -> list:
    """Matches CUSTOM_BlockShampoo._block_bounds / CUSTOM_BlockKLShampoo / CUSTOM_BlockSOAP's block sizing (used by
    block_shampoo, block_soap, block_kl_shampoo) -- ceil(dim/block_size) blocks, sizes as even as
    divmod allows (one extra element on the first dim % n_blocks blocks)."""
    if dim <= block_size:
        return [dim]
    n_blocks = -(-dim // block_size)
    base = dim // n_blocks
    return [base + (1 if i < dim % n_blocks else 0) for i in range(n_blocks)]


def _ceil_walk_min_block_sizes(dim: int, block_size: int) -> list:
    """Matches CUSTOM_BlockKLSOAP._block_bounds's sizing (used by kl_soap/block_kl_soap) -- a distinct
    real algorithm from the divmod-even one above: nb blocks of size ceil(dim/nb), walked off the
    end (last block may be smaller). Two genuinely different block-sizing algorithms exist in
    optimizers.py; using the wrong one here silently mis-costs the affected optimizers."""
    if dim <= block_size:
        return [dim]
    nb = max(1, -(-dim // block_size))
    bsz = -(-dim // nb)
    sizes, s = [], 0
    while s < dim:
        e = min(dim, s + bsz)
        sizes.append(e - s)
        s = e
    return sizes


# Analytic per-step optimizer FLOP counts (dominant term only: the Kronecker-factor matmuls and
# matrix-power/eigendecomp calls, O(d^3) each, amortized over precondition_freq / eigenbasis_freq
# where applicable). Used to report `optimizer_step_flops` -- the basis for the FLOPs-to-target
# ranking metric (chosen over steps-to-target because steps-to-target hides Shampoo-family's much
# higher per-step cost). Each dense chain coefficient (N in N*2*d0*d1*(d0+d1)) is the number of
# two-sided rotation or accumulation matmuls the class's step() really does: soap 4, kl_shampoo 3,
# online_kl_shampoo 3 plus its Newton-Schulz cube term, kl_soap 5 plus a per-step diagonal rebuild.
def estimate_optimizer_flops(name: str, param_shapes: list, freq: int = 10, ns_steps: int = 5,
                             eigenbasis_freq: int = None, block_size: int = None,
                             step: int = None) -> float:
    """With `step` (1-indexed), the cost of that exact step: the full basis-refresh cube cost on a
    refresh step and none on the others, following each optimizer's own refresh condition. Without
    it, the refresh cost averaged over freq."""
    def cube(n):
        return n ** 3

    def dense_chains(d0, d1, n_chains):
        return n_chains * 2 * d0 * d1 * (d0 + d1)

    def shampoo_refresh_now(s, f):
        # Matches ScalableShampoo/CUSTOM_BlockShampoo's own gate: `state["step"] % freq == 0 or
        # state["step"] == 1` -- refreshes at step 1, freq, 2*freq, ...
        f = max(f, 1)
        return s == 1 or s % f == 0

    def soap_refresh_now(s, f):
        # Matches SOAP/KLSOAP/CUSTOM_BlockSOAP/CUSTOM_BlockKLSOAP's own gate: `t > 1 and (t - 1) % freq == 0`
        # -- basis stays at its step-1 init until step freq+1, then refreshes every freq steps.
        f = max(f, 1)
        return s is not None and s > 1 and (s - 1) % f == 0

    def amortized_or_exact(cube_cost, f, refresh_predicate):
        # step=None: legacy flat-average approximation (kept for non-training-loop callers).
        # step given: exact -- full cube_cost only on an actual refresh step, else 0.
        if step is None:
            return cube_cost / max(f, 1)
        return cube_cost if refresh_predicate(step, f) else 0.0

    total = 0.0
    for shape in param_shapes:
        d0 = shape[0]
        d1 = shape[1] if len(shape) > 1 else 1
        if name in ("sgd", "adamw"):
            total += 10 * d0 * d1  # elementwise-dominated
        elif name in ("muon", "custom_block_muon", "custom_block_muon_adaptive"):
            if len(shape) == 1:
                total += 10 * d0 * d1  # Muon's auxiliary parameters use AdamW.
                continue
            # Newton-Schulz orthogonalization is matrix-matrix, not elementwise: each of ns_steps
            # iterations does x@x.T (m,n)x(n,m), a_mat@a_mat (m,m)x(m,m), b_mat@x (m,m)x(m,n) for
            # m=min(d0,d1), n=max(d0,d1) -- three matmuls, each costing the standard 2*rows*inner*cols
            # FLOPs, i.e. 2*(m*n*m) + 2*(m*m*m) + 2*(m*m*n) = 4*m*m*n + 2*m^3 per iteration.
            if name == "custom_block_muon":
                bsz = block_size or 32
                sizes = _divmod_even_block_sizes(min(d0, d1), bsz)
                n_full = max(d0, d1)
                for m in sizes:
                    total += ns_steps * (4 * m * m * n_full + 2 * cube(m))
            elif name == "custom_block_muon_adaptive":
                bsz = block_size or 32
                sizes = _divmod_even_block_sizes(min(d0, d1), bsz)
                n_full = max(d0, d1)
                for m in sizes:
                    total += ns_steps * (4 * m * m * n_full + 2 * cube(m))
                total += 10 * d0 * d1  # elementwise v EMA + rescale on top of orthogonalization
            else:
                m, n = sorted((d0, d1))
                total += ns_steps * (4 * m * m * n + 2 * cube(m))
        elif name == "full_adagrad":
            n = d0 * d1
            total += cube(n)  # full-matrix root every step -- deliberately the expensive oracle
        elif name in ("shampoo_2018",):
            total += 2 * (cube(d0) + cube(d1)) + 4 * d0 * d1 * (d0 + d1)  # root every step
        elif name == "scalable_shampoo":
            total += amortized_or_exact(2 * (cube(d0) + cube(d1)), freq, shampoo_refresh_now) \
                + 4 * d0 * d1 * (d0 + d1)
        elif name == "soap":
            basis_freq = freq if eigenbasis_freq is None else eigenbasis_freq
            total += amortized_or_exact(2 * (cube(d0) + cube(d1)), basis_freq, soap_refresh_now) \
                + dense_chains(d0, d1, 4)
        elif name == "kl_shampoo":
            # Recomputes an eigendecomposition, matrix exponential, and inverse root on every
            # optimizer step; there is no frequency gate to amortize.
            total += 2 * (cube(d0) + cube(d1)) + dense_chains(d0, d1, 3)
        elif name == "online_kl_shampoo":
            # Official-algorithm port (2026-09-14): no eigh/matrix_exp at all -- S/P update every
            # step (coupled/zero-staleness, no freq gate), whitened via the official 10-step
            # coupled Newton-Schulz (CANS), which is pure matmul. Matmul count per side,
            # hand-counted against _scaled_cans_coupled_ns (optimizers.py): step 0 does 1
            # n-by-n matmul (Y@Y); steps 1-8 do 3 each (P=Z@Y, Y@P, P@Z); step 9 does 2
            # (P=Z@Y, P@Z) -- 1 + 8*3 + 2 = 27 matmuls per side, each 2*n^3 FLOPs -> 54*cube(n).
            # dense_chains(d0, d1, 3): GPb/PaG (one-sided), S_a/S_b squaring (one-sided pair),
            # and the P_a@N_t@P_b whitening (two-sided) -- each pair/chain costs the same
            # 2*d0*d1*(d0+d1) as a true two-sided rotation.
            total += 54 * (cube(d0) + cube(d1)) + dense_chains(d0, d1, 3)
        elif name == "kl_soap":
            basis_freq = freq if eigenbasis_freq is None else eigenbasis_freq
            total += amortized_or_exact(2 * (cube(d0) + cube(d1)), basis_freq, soap_refresh_now) \
                + dense_chains(d0, d1, 5)
            total += 2 * (cube(d0) + cube(d1))  # unconditional per-step diag reconstruction cost
        elif name == "custom_block_shampoo":
            # The eigh/root cube term carries the 2x-per-call convention of the dense optimizers,
            # and the chain term is block-local (bs0+bs1), ScalableShampoo's 2 chains per block.
            bsz = block_size or 196

            def block_cost(dim):
                nb = max(1, -(-dim // bsz))
                b = -(-dim // nb)
                return nb * cube(b), b
            l_cube, bs0 = block_cost(d0)
            r_cube, bs1 = block_cost(d1)
            total += amortized_or_exact(2 * (l_cube + r_cube), freq, shampoo_refresh_now) \
                + 2 * d0 * d1 * (bs0 + bs1)
        elif name in ("custom_block_kl_shampoo", "custom_block_soap", "custom_block_kl_soap"):
            # Exact per-block sums (R2/R3, C2/C3) of kl_shampoo/soap/kl_soap's chains. Block sizing
            # follows each class: CUSTOM_BlockKLSOAP walks ceil-sized blocks, the others divmod.
            bsz = block_size or 196
            if name == "custom_block_kl_soap":
                row_sizes = _ceil_walk_min_block_sizes(d0, bsz)
                col_sizes = _ceil_walk_min_block_sizes(d1, bsz)
            else:
                row_sizes = _divmod_even_block_sizes(d0, bsz)
                col_sizes = _divmod_even_block_sizes(d1, bsz)
            R2 = sum(s * s for s in row_sizes)
            R3 = sum(cube(s) for s in row_sizes)
            C2 = sum(s * s for s in col_sizes)
            C3 = sum(cube(s) for s in col_sizes)
            if name == "custom_block_kl_shampoo":
                # No frequency amortization -- the log-Euclidean update runs fresh every step.
                total += 4 * (d1 * R2 + d0 * C2) + 6 * (R3 + C3)
            elif name == "custom_block_soap":
                basis_freq = freq if eigenbasis_freq is None else eigenbasis_freq
                total += amortized_or_exact(2 * (R3 + C3), basis_freq, soap_refresh_now) \
                    + 8 * (d1 * R2 + d0 * C2)
            else:  # block_kl_soap
                basis_freq = freq if eigenbasis_freq is None else eigenbasis_freq
                total += (2 * (R3 + C3)
                          + amortized_or_exact(2 * (R3 + C3), basis_freq, soap_refresh_now)
                          + 10 * (d1 * R2 + d0 * C2))
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
    p.add_argument("--lr", type=float, default=(
        float(os.environ["OPT_LR"]) if "OPT_LR" in os.environ else None
    ))
    p.add_argument("--precondition-freq", type=int, default=int(os.environ.get("OPT_PRECOND_FREQ", "10")))
    p.add_argument("--eigenbasis-freq", type=int, default=(
        int(os.environ["OPT_EIGENBASIS_FREQ"]) if "OPT_EIGENBASIS_FREQ" in os.environ else None
    ))
    p.add_argument("--block-size", type=int, default=(
        int(os.environ["OPT_BLOCK_SIZE"]) if "OPT_BLOCK_SIZE" in os.environ else None
    ))
    p.add_argument("--shampoo-beta", type=float, default=(
        float(os.environ["OPT_SHAMPOO_BETA"]) if "OPT_SHAMPOO_BETA" in os.environ else None
    ))
    p.add_argument("--ns-steps", type=int, default=int(
        os.environ.get("OPT_NS_STEPS", os.environ.get("NS_STEPS", "5"))))
    p.add_argument("--momentum", type=float, default=(
        float(os.environ["OPT_MOMENTUM"]) if "OPT_MOMENTUM" in os.environ else None
    ))
    p.add_argument("--beta1", type=float, default=(
        float(os.environ["OPT_BETA1"]) if "OPT_BETA1" in os.environ else None
    ))
    p.add_argument("--beta2", type=float, default=(
        float(os.environ["OPT_BETA2"]) if "OPT_BETA2" in os.environ else None
    ))
    p.add_argument("--nesterov", type=int, default=int(os.environ.get("OPT_NESTEROV", "0")))
    p.add_argument("--weight-decay", type=float, default=(
        float(os.environ["OPT_WEIGHT_DECAY"]) if "OPT_WEIGHT_DECAY" in os.environ else None
    ))
    p.add_argument("--batch-size", type=int, default=int(os.environ.get("BATCH_SIZE", "1024")))
    p.add_argument("--steps", type=int, default=int(os.environ.get("STEPS", "3000")))
    p.add_argument("--target-loss", type=float, default=float(os.environ.get("TARGET_LOSS", "0.012")))
    p.add_argument("--seed", type=int, default=int(os.environ.get("SEED", "0")))
    p.add_argument("--device", default=os.environ.get("TRAIN_DEVICE", "cpu"))
    p.add_argument("--data-dir", default=os.environ.get("DATA_DIR", "/tmp/fashion-mnist"))
    p.add_argument("--report-every", type=int, default=int(os.environ.get("REPORT_EVERY_STEPS", "25")))
    p.add_argument("--hidden", type=int, default=int(os.environ.get("HIDDEN_DIM", "256")))
    p.add_argument("--latent", type=int, default=int(os.environ.get("LATENT_DIM", "64")))
    p.add_argument("--big-model", action="store_true",
                    default=os.environ.get("BIG_MODEL", "0") == "1",
                    help="Use ResidualMLPBig (~36.85M params) instead of the fixed benchmark "
                         "AutoencoderMLP -- a ttnn-hardware-scale integration proof only, never "
                         "part of the optimizer leaderboard.")
    args = p.parse_args()

    torch.manual_seed(args.seed)

    fwd_device = args.device
    ttnn_device = None
    ttnn_module = None
    if fwd_device == "tt":
        try:
            import ttnn as ttnn_module
        except ImportError:
            print("FATAL: --device tt requires ttnn; this image has no ttnn install. Pass --device cpu "
                  "or run in a ttnn-enabled image.", file=sys.stderr)
            sys.exit(1)
        print("NOTE: --device tt runs each Linear layer's matmul via ttnn.matmul on a TT device "
              "(see ttnn_linear.py); gradients and optimizer state stay on cpu.", file=sys.stderr)
        ttnn_device = ttnn_module.open_device(device_id=int(os.environ.get("TT_DEVICE_ID", "0")))
        fwd_device = "cpu"

    if args.big_model:
        model = ResidualMLPBig(ttnn_device=ttnn_device).to(fwd_device)
        n_params = sum(p.numel() for p in model.parameters())
        print(f"NOTE: --big-model active -- ResidualMLPBig, {n_params:,} params. This run is a "
              f"ttnn-hardware-scale integration proof, not part of the optimizer leaderboard.",
              file=sys.stderr)
    else:
        model = AutoencoderMLP(hidden=args.hidden, latent=args.latent, ttnn_device=ttnn_device).to(fwd_device)
    if args.lr is None:
        args.lr = DEFAULT_LR_BY_OPT.get(args.optimizer.lower(), 1e-3)
        print(f"NOTE: OPT_LR not set -- using per-optimizer default lr={args.lr} for "
              f"'{args.optimizer}' (see optimizers.py DEFAULT_LR_BY_OPT). Set OPT_LR explicitly "
              f"to override.", file=sys.stderr)
    opt_kwargs = {"precondition_freq": args.precondition_freq}
    if args.optimizer.lower() in ("soap", "kl_soap", "custom_block_soap", "custom_block_kl_soap"):
        opt_kwargs["eigenbasis_freq"] = (args.precondition_freq if args.eigenbasis_freq is None
                                          else args.eigenbasis_freq)
    if args.optimizer.lower().startswith("custom_block_") and args.block_size is not None:
        opt_kwargs["block_size"] = args.block_size
    if args.shampoo_beta is not None:
        opt_kwargs["shampoo_beta"] = args.shampoo_beta
    if args.optimizer.lower() in ("muon", "custom_block_muon", "custom_block_muon_adaptive"):
        opt_kwargs["ns_steps"] = args.ns_steps
    if args.momentum is not None:
        opt_kwargs["momentum"] = args.momentum
    if args.beta1 is not None or args.beta2 is not None:
        opt_kwargs["betas"] = (args.beta1 if args.beta1 is not None else 0.9,
                                args.beta2 if args.beta2 is not None else 0.999)
    if args.beta1 is not None:
        # Also expose as a standalone scalar for optimizers with separate beta1/beta2 ctor args
        # (not a betas tuple) -- e.g. OnlineKLShampoo's Nesterov-momentum EMA coefficient.
        opt_kwargs["beta1"] = args.beta1
    if args.beta2 is not None:
        # Also expose as a standalone scalar for optimizers with a single beta2 (not a betas
        # tuple) in their signature, e.g. block_muon_adaptive's Adam-style second moment.
        opt_kwargs["beta2"] = args.beta2
    if args.weight_decay is not None:
        opt_kwargs["weight_decay"] = args.weight_decay
    if args.nesterov:
        opt_kwargs["nesterov"] = True
    if ttnn_device is not None and args.optimizer.lower() in ("muon", "custom_block_muon") and \
            os.environ.get("MUON_STEP_ON_TT", "0") == "1":
        # Opt-in: Muon's Newton-Schulz matmul chain also runs on-device (ttnn_muon.py), not just
        # forward/backward. Gated behind an explicit env var so every other --device tt job keeps
        # its prior (forward/backward-only) behavior unchanged.
        opt_kwargs["ttnn_device"] = ttnn_device
        opt_kwargs["ttnn_module"] = ttnn_module
    optimizer = build_optimizer(args.optimizer, model, args.lr, **opt_kwargs)

    train_x, test_x = load_fashion_mnist(args.data_dir, fwd_device)
    n = train_x.shape[0]

    param_shapes = [tuple(p.shape) for p in model.parameters()]

    start = time.time()
    diverged = False
    target_reached = False
    step = 0
    cumulative_flops = 0.0
    fwd_bwd_flops_per_step = estimate_forward_backward_flops(param_shapes, args.batch_size)
    for step in range(1, args.steps + 1):
        idx = torch.randint(0, n, (args.batch_size,), device=fwd_device)
        batch = train_x[idx]
        optimizer.zero_grad(set_to_none=True)
        out = model(batch)
        loss = F.mse_loss(out, batch.view(batch.shape[0], -1))
        loss.backward()
        optimizer.step()

        opt_flops = estimate_optimizer_flops(args.optimizer, param_shapes, args.precondition_freq,
                                             ns_steps=args.ns_steps,
                                             eigenbasis_freq=args.eigenbasis_freq,
                                             block_size=args.block_size, step=step)
        cumulative_flops += fwd_bwd_flops_per_step + opt_flops

        if not torch.isfinite(loss):
            diverged = True
            print(f"step {step}: loss diverged ({loss.item()})", file=sys.stderr)
            break

        # The target is checked every step so optimizer_flops_to_target is the step test_loss
        # first crossed it, not the next report. Eval FLOPs stay out of cumulative_flops.
        with torch.no_grad():
            test_out = model(test_x)
            test_loss = F.mse_loss(test_out, test_x.view(test_x.shape[0], -1)).item()
        elapsed = time.time() - start
        frac = step / args.steps
        report_now = step % args.report_every == 0 or step == args.steps

        if report_now:
            print(f"step={step} frac={frac:.3f} train_loss={loss.item():.6f} "
                  f"test_loss={test_loss:.6f} elapsed={elapsed:.1f}s cumulative_flops={cumulative_flops:.3e}")
            post_metric(frac, test_loss, "test_loss")
            post_metric(frac, float(step), "step")
            post_metric(frac, elapsed, "wall_clock_seconds")
            post_metric(frac, opt_flops, "optimizer_step_flops")
            post_metric(frac, 0.0, "diverged")

        if test_loss <= args.target_loss:
            target_reached = True
            print(f"TARGET REACHED at step {step}, test_loss={test_loss:.6f}, "
                  f"optimizer_flops_to_target={cumulative_flops:.4g}")
            if not report_now:
                post_metric(frac, test_loss, "test_loss")
                post_metric(frac, float(step), "step")
                post_metric(frac, elapsed, "wall_clock_seconds")
                post_metric(frac, opt_flops, "optimizer_step_flops")
                post_metric(frac, 0.0, "diverged")
            post_metric(frac, cumulative_flops, "optimizer_flops_to_target", retries=3)
            break

    if diverged:
        post_metric(1.0, 1.0, "diverged")
    elif not target_reached:
        print(f"TARGET NOT REACHED within {args.steps} steps (final test_loss={test_loss:.6f})",
              file=sys.stderr)

    if ttnn_device is not None:
        ttnn_module.close_device(ttnn_device)


if __name__ == "__main__":
    main()
