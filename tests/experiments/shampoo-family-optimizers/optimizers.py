"""Shampoo-family optimizer implementations for the shampoo-family-optimizers experiment.

Every optimizer below is a plain torch.optim.Optimizer so train.py can swap between them with a
single --optimizer flag. Implementations favor faithfulness to the cited paper's core update rule
over raw speed -- this experiment measures optimization quality (steps/wall-clock to a target
loss), not kernel throughput, and the model is intentionally tiny (see train.py) so O(dim^3)
matrix ops on per-layer factors are cheap.

FIDELITY NOTES (read before trusting a "confirmed"/"not confirmed" verdict against these):
  - shampoo_2018, scalable_shampoo, soap, muon are direct, unabridged implementations of their
    papers' stated update rules (Kronecker-factored preconditioners with matrix inverse p-th
    roots; Newton-Schulz orthogonalization for Muon) -- no official reference code was available
    to diff against in this sandbox (no internet egress here), so cross-check against the
    upstream repos (google-research/google-research/scalable_shampoo, nikhilvyas/soap,
    KellerJordan/Muon) the first time an agent has internet access, before trusting a "not
    confirmed" verdict on any of these.
  - kl_shampoo and online_kl_shampoo are BEST-EFFORT reconstructions from the papers' abstracts/
    algorithm sketches (KL-Shampoo arXiv:2509.03378, Online KL Shampoo
    github.com/tilde-research/online-kl-shampoo-release) -- written without the official code
    (not fetchable from this sandbox). They implement the natural-gradient/mirror-descent-in-KL
    read of Shampoo's preconditioner update (replacing the Frobenius-norm nearest-preconditioner
    update with a KL-divergence-minimizing one between consecutive Gaussian preconditioners, which
    yields a matrix-geometric-mean-style update rather than Shampoo's arithmetic accumulation).
    FIRST TASK for any agent assigned this optimizer: clone the official
    tilde-research/online-kl-shampoo-release repo and diff this implementation's update rule
    against it line-by-line before running any comparison job; if they diverge, note the specific
    divergence in the hypothesis and prefer the official rule.
"""

from __future__ import annotations

import math
from typing import Iterable

import torch
from torch.optim import Optimizer


def _matrix_power(mat: torch.Tensor, power: float, eps: float) -> torch.Tensor:
    """mat^power via eigendecomposition, for symmetric PSD mat. eps regularizes for stability."""
    dim = mat.shape[0]
    reg = eps * torch.eye(dim, device=mat.device, dtype=mat.dtype)
    eigvals, eigvecs = torch.linalg.eigh(mat + reg)
    eigvals = eigvals.clamp(min=eps)
    return eigvecs @ torch.diag(eigvals.pow(power)) @ eigvecs.T


class FullMatrixAdaGrad(Optimizer):
    """Full-matrix AdaGrad (Duchi et al. 2011), the "ideal reference" upper bound the Shampoo
    family approximates. G_t = sum_{s<=t} g_s g_s^T (flattened per-parameter tensor), update =
    G_t^{-1/2} g_t. O(dim^3) compute / O(dim^2) memory per step where dim = that tensor's flattened
    element count -- e.g. a 784x256 Linear weight is dim=200704, making G a 200704x200704 matrix
    (~160GB in fp32). This is intractable on the main 784-256-64-256-784 model by construction, not
    a bug to work around: per Shampoo (Gupta/Koren/Singer 2018), GGT (Agarwal et al. 2018) and SOAP
    (Vyas et al. 2024), full-matrix AdaGrad is a theoretical reference object each paper's method
    approximates, and every one of them evaluates it empirically only on a scaled-down model where
    G actually fits, never on the real target network. Run this optimizer only via HIDDEN_DIM/
    LATENT_DIM overrides small enough to keep every flattened tensor dim in the low thousands (see
    experiment.md's FULL-MATRIX ADAGRAD REFERENCE note) -- max_dim below fails fast with a clear
    error instead of silently OOMing or corrupting eigh (MKL SSYEVD errors on a too-large/ill-formed
    matrix look like a numerical bug but are actually this).
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 1.0, eps: float = 1e-6,
                 max_dim: int = 4096):
        super().__init__(params, dict(lr=lr, eps=eps, max_dim=max_dim))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad.flatten()
                if g.numel() > group["max_dim"]:
                    raise ValueError(
                        f"FullMatrixAdaGrad: parameter tensor has {g.numel()} elements, exceeding "
                        f"max_dim={group['max_dim']}. Full-matrix AdaGrad is only run on a scaled-"
                        f"down model (HIDDEN_DIM/LATENT_DIM) where this fits -- see experiment.md "
                        f"FULL-MATRIX ADAGRAD REFERENCE. Not intended for the main comparison model."
                    )
                state = self.state[p]
                if "G" not in state:
                    state["G"] = torch.zeros(g.numel(), g.numel(), device=g.device, dtype=g.dtype)
                state["G"].add_(torch.outer(g, g))
                G_inv_half = _matrix_power(state["G"], -0.5, group["eps"])
                update = (G_inv_half @ g).view_as(p)
                p.add_(update, alpha=-group["lr"])
        return loss


class Shampoo2018(Optimizer):
    """Shampoo (Gupta, Koren, Singer 2018, arXiv:1802.09568). Per-tensor Kronecker-factored
    preconditioner: one statistics matrix L_i per dimension i of the (<=2D, flattened-if-higher)
    parameter, accumulated as L_i += g_(i) g_(i)^T (g reshaped so dim i is rows), preconditioner
    applied as g <- L_1^(-1/2k) x_1 g x_2 L_2^(-1/2k) for a matrix param (k = number of dims).
    Matrix roots recomputed every step (the original paper's formulation; see ScalableShampoo for
    the periodic-recompute relaxation).
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4):
        super().__init__(params, dict(lr=lr, eps=eps))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if g.dim() == 1:
                    # k=1 tensor: a single statistics matrix, exponent -1/(2*1) = -1/2. Treating a
                    # 1D param as a (d,1) "matrix" and applying two -1/4 factors (one of them on a
                    # 1x1 R) is NOT equivalent to a single -1/2 factor and introduces a spurious
                    # extra scaling -- found via Codex review 2026-09-11, see FINAL_RESULT.md.
                    g1 = g.unsqueeze(1)
                    d0 = g1.shape[0]
                    if "G" not in state:
                        state["G"] = group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype)
                    state["G"].add_(g1 @ g1.T)
                    G_inv = _matrix_power(state["G"], -0.5, group["eps"])
                    update = (G_inv @ g1).view_as(g)
                    p.add_(update, alpha=-group["lr"])
                    continue
                g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L" not in state:
                    state["L"] = group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype)
                    state["R"] = group["eps"] * torch.eye(d1, device=g.device, dtype=g.dtype)
                state["L"].add_(g2 @ g2.T)
                state["R"].add_(g2.T @ g2)
                L_inv = _matrix_power(state["L"], -0.25, group["eps"])
                R_inv = _matrix_power(state["R"], -0.25, group["eps"])
                update = (L_inv @ g2 @ R_inv).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


class ScalableShampoo(Optimizer):
    """Scalable Shampoo (Anil et al. 2020, arXiv:2002.09018): Shampoo + (a) periodic root
    recomputation every `precondition_freq` steps (the paper's main scalability lever -- matrix
    roots are the dominant per-step cost), (b) grafting: the preconditioned direction's *norm* is
    replaced by an SGD-with-momentum step's norm, so Shampoo only supplies *direction*, not step
    size (fixes Shampoo-2018's sensitivity to its own preconditioner's scale).
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4,
                 momentum: float = 0.9, precondition_freq: int = 10):
        super().__init__(params, dict(lr=lr, eps=eps, momentum=momentum,
                                       precondition_freq=precondition_freq))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                is_1d = g.dim() == 1
                if is_1d:
                    # k=1 tensor: single statistics matrix, exponent -1/2 (see Shampoo2018's same
                    # fix above for why the old (d,1)-with-two-(-1/4)-factors form was wrong).
                    g2 = g.unsqueeze(1)
                    d0 = g2.shape[0]
                    if "L" not in state:
                        state["L"] = group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype)
                        state["L_inv"] = torch.eye(d0, device=g.device, dtype=g.dtype)
                        state["graft_momentum"] = torch.zeros_like(g)
                        state["step"] = 0
                    state["step"] += 1
                    state["L"].add_(g2 @ g2.T)
                    if state["step"] % group["precondition_freq"] == 0 or state["step"] == 1:
                        state["L_inv"] = _matrix_power(state["L"], -0.5, group["eps"])
                    shampoo_dir = (state["L_inv"] @ g2).view_as(g)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                    d0, d1 = g2.shape
                    if "L" not in state:
                        state["L"] = group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype)
                        state["R"] = group["eps"] * torch.eye(d1, device=g.device, dtype=g.dtype)
                        state["L_inv"] = torch.eye(d0, device=g.device, dtype=g.dtype)
                        state["R_inv"] = torch.eye(d1, device=g.device, dtype=g.dtype)
                        state["graft_momentum"] = torch.zeros_like(g)
                        state["step"] = 0
                    state["step"] += 1
                    state["L"].add_(g2 @ g2.T)
                    state["R"].add_(g2.T @ g2)
                    if state["step"] % group["precondition_freq"] == 0 or state["step"] == 1:
                        state["L_inv"] = _matrix_power(state["L"], -0.25, group["eps"])
                        state["R_inv"] = _matrix_power(state["R"], -0.25, group["eps"])
                    shampoo_dir = (state["L_inv"] @ g2 @ state["R_inv"]).view_as(g)

                # Grafting (Anil et al. 2020 Algorithm 1): rescale the Shampoo *direction* to the
                # current gradient's norm first (Shampoo supplies direction only), THEN accumulate
                # momentum on that rescaled direction. The prior version inverted this -- it scaled
                # the direction by ||momentum(g)||'s norm and applied no momentum to the Shampoo
                # direction itself, a different (and, per Codex review 2026-09-11, plausibly worse-
                # converging) algorithm. See FINAL_RESULT.md for the reproduction this fixes.
                scaled_dir = shampoo_dir * (g.norm() / (shampoo_dir.norm() + group["eps"]))
                state["graft_momentum"].mul_(group["momentum"]).add_(scaled_dir)
                update = state["graft_momentum"]
                p.add_(update, alpha=-group["lr"])
        return loss


class SOAP(Optimizer):
    """SOAP (Vyas et al. 2024, arXiv:2409.11321): run Adam in the (slowly-updated) eigenbasis of
    Shampoo's Kronecker factors, instead of Shampoo's own matrix-power preconditioning. L, R
    accumulate as in Shampoo; their eigenbases Q_L, Q_R are refreshed every
    `eigenbasis_freq` steps; Adam's second moment is updated in that rotated basis, while its
    first moment is kept in parameter coordinates and rotated for the update. This is SOAP's
    benefit comes from the eigenbasis, and running Adam there is cheaper (no matrix root every
    step) and at least as good.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 3e-3, eps: float = 1e-8,
                 betas=(0.9, 0.999), shampoo_beta: float = 0.95, eigenbasis_freq: int = 10):
        super().__init__(params, dict(lr=lr, eps=eps, betas=betas, shampoo_beta=shampoo_beta,
                                       eigenbasis_freq=eigenbasis_freq))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            b1, b2 = group["betas"]
            sb = group["shampoo_beta"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if g.dim() == 1:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L" not in state:
                    state["L"] = torch.zeros(d0, d0, device=g.device, dtype=g.dtype)
                    state["R"] = torch.zeros(d1, d1, device=g.device, dtype=g.dtype)
                    state["QL"] = torch.eye(d0, device=g.device, dtype=g.dtype)
                    state["QR"] = torch.eye(d1, device=g.device, dtype=g.dtype)
                    state["m"] = torch.zeros_like(g2)
                    state["v"] = torch.zeros_like(g2)
                    state["step"] = 0
                state["step"] += 1
                t = state["step"]
                # Refresh QL/QR from L/R accumulated up to (not including) this step's gradient,
                # THEN accumulate this step's gradient into L/R for the next refresh -- official
                # SOAP (Algorithm 2/6) rotates the current gradient through the *previous* basis,
                # never a basis computed from that same gradient (zero-staleness would leak the
                # current update into its own rotation). t==1 keeps the identity init from above
                # (no history yet to refresh from).
                if t > 1 and (t - 1) % group["eigenbasis_freq"] == 0:
                    _, QL_new = torch.linalg.eigh(state["L"] + group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype))
                    _, QR_new = torch.linalg.eigh(state["R"] + group["eps"] * torch.eye(d1, device=g.device, dtype=g.dtype))
                    state["QL"], state["QR"] = QL_new, QR_new
                state["L"].mul_(sb).add_(g2 @ g2.T, alpha=1 - sb)
                state["R"].mul_(sb).add_(g2.T @ g2, alpha=1 - sb)
                QL, QR = state["QL"], state["QR"]
                g_rot = QL.T @ g2 @ QR
                # SOAP Algorithm 6 keeps M in parameter coordinates. V is a diagonal estimator
                # in the current (slowly changing) eigenbasis; it is not a dense covariance that
                # can be transported by squared change-of-basis matrices.
                state["m"].mul_(b1).add_(g2, alpha=1 - b1)
                state["v"].mul_(b2).addcmul_(g_rot, g_rot, value=1 - b2)
                m_hat = (QL.T @ state["m"] @ QR) / (1 - b1 ** t)
                v_hat = state["v"] / (1 - b2 ** t)
                update_rot = m_hat / (v_hat.sqrt() + group["eps"])
                update = (QL @ update_rot @ QR.T).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


def _newton_schulz(g: torch.Tensor, steps: int = 5) -> torch.Tensor:
    """Newton-Schulz iteration approximating the orthogonalization g -> U V^T of g = U S V^T
    (Muon's core step), avoiding an explicit SVD. Coefficients from Jordan et al.'s reference
    Muon implementation (quintic iteration, tuned for fast convergence near the orthogonal
    manifold)."""
    a, b, c = 3.4445, -4.7750, 2.0315
    x = g.bfloat16() if g.is_cuda else g.float()
    x = x / (x.norm() + 1e-7)
    if x.shape[0] > x.shape[1]:
        x = x.T
        transposed = True
    else:
        transposed = False
    for _ in range(steps):
        a_mat = x @ x.T
        b_mat = b * a_mat + c * a_mat @ a_mat
        x = a * x + b_mat @ x
    if transposed:
        x = x.T
    return x.to(g.dtype)


class Muon(Optimizer):
    """Muon (Jordan et al. 2024). Nesterov momentum whose update is orthogonalized via
    Newton-Schulz before the parameter step. Defined for >=2D
    (matrix-shaped) parameters; 1D parameters (biases, norms) fall back to plain AdamW-style
    updates in this experiment's train.py (see build_optimizer), matching the reference recipe of
    "Muon for hidden matrices, AdamW for everything else."
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.02, momentum: float = 0.95,
                 ns_steps: int = 5, ttnn_device=None, ttnn_module=None):
        super().__init__(params, dict(lr=lr, momentum=momentum, ns_steps=ns_steps))
        # When set, Newton-Schulz's matmul chain runs on real TT hardware via ttnn.matmul
        # (ttnn_muon.newton_schulz_ttnn) instead of CPU torch -- see ttnn_muon.py module
        # docstring. Everything else in this step (momentum buffer, final scale-by-lr) stays CPU.
        self.ttnn_device = ttnn_device
        self.ttnn_module = ttnn_module

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if "momentum_buf" not in state:
                    state["momentum_buf"] = torch.zeros_like(g)
                buf = state["momentum_buf"]
                momentum = group["momentum"]
                buf.lerp_(g, 1 - momentum)
                nesterov = g.lerp(buf, momentum)
                g2 = nesterov if g.dim() >= 2 else nesterov.unsqueeze(1)
                if self.ttnn_device is not None:
                    from ttnn_muon import newton_schulz_ttnn
                    orth = newton_schulz_ttnn(g2.reshape(g2.shape[0], -1), self.ttnn_device,
                                               self.ttnn_module, steps=group["ns_steps"])
                else:
                    orth = _newton_schulz(g2.reshape(g2.shape[0], -1), steps=group["ns_steps"])
                update = orth.view_as(g) if g.dim() >= 2 else orth.view_as(buf.unsqueeze(1)).squeeze(1)
                scale = max(1.0, g2.shape[0] / g2.shape[1]) ** 0.5
                p.add_(update * scale, alpha=-group["lr"])
        return loss


class KLShampoo(Optimizer):
    """KL-Shampoo (arXiv:2509.03378) -- BEST-EFFORT reconstruction, see module docstring.

    Reframes Shampoo's preconditioner update as minimizing KL divergence between consecutive
    matrix-Gaussian preconditioners instead of Shampoo's Frobenius-nearest (arithmetic-mean)
    update. In closed form for zero-mean matrix Gaussians this yields a *geometric*-mean-style
    update of the precision matrices: P_t = geodesic_interp(P_{t-1}, g g^T, alpha) via the
    Karcher/log-Euclidean mean approximation log(P_t) = (1-alpha) log(P_{t-1}) + alpha log(g g^T + eps I),
    P_t = exp(...), which is what's implemented below (log-Euclidean mean is the standard tractable
    surrogate for the true affine-invariant/KL geometric mean when an exact Riemannian step is too
    expensive per iteration). Direction still Kronecker-factored the same way as Shampoo2018.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4,
                 alpha: float = 0.05):
        super().__init__(params, dict(lr=lr, eps=eps, alpha=alpha))

    @staticmethod
    def _log_euclidean_update(P_log: torch.Tensor, stat: torch.Tensor, alpha: float, eps: float) -> torch.Tensor:
        dim = stat.shape[0]
        reg = eps * torch.eye(dim, device=stat.device, dtype=stat.dtype)
        eigvals, eigvecs = torch.linalg.eigh(stat + reg)
        eigvals = eigvals.clamp(min=eps)
        stat_log = eigvecs @ torch.diag(eigvals.log()) @ eigvecs.T
        return (1 - alpha) * P_log + alpha * stat_log

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                is_1d = g.dim() == 1
                # k=1 tensor: single statistics matrix, exponent -1/2 -- same fix as Shampoo2018/
                # ScalableShampoo/CUSTOM_BlockShampoo (the (d,1)-with-two-(-1/4)-factors form double-
                # applies the exponent on a rank-1 tensor; Codex-reviewed 2026-09-14, found while
                # auditing the newly-merged block variants but present here identically on main).
                if is_1d:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L_log" not in state:
                    state["L_log"] = torch.zeros(d0, d0, device=g.device, dtype=g.dtype)
                    state["R_log"] = None if is_1d else torch.zeros(d1, d1, device=g.device, dtype=g.dtype)
                state["L_log"] = self._log_euclidean_update(state["L_log"], g2 @ g2.T, group["alpha"], group["eps"])
                L = torch.linalg.matrix_exp(state["L_log"])
                if is_1d:
                    L_inv = _matrix_power(L, -0.5, group["eps"])
                    update = (L_inv @ g2).view_as(g)
                else:
                    state["R_log"] = self._log_euclidean_update(state["R_log"], g2.T @ g2, group["alpha"], group["eps"])
                    R = torch.linalg.matrix_exp(state["R_log"])
                    L_inv = _matrix_power(L, -0.25, group["eps"])
                    R_inv = _matrix_power(R, -0.25, group["eps"])
                    update = (L_inv @ g2 @ R_inv).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


# ---------------------------------------------------------------------------------------------
# OnlineKLShampoo: official-algorithm port (2026-09-14).
#
# Ported directly from github.com/tilde-research/online-kl-shampoo-release (okls/okls_optim.py,
# okls/okls_step.py, okls/scaled_cans_coupled_ns.py, fetched and read in full -- the "no internet
# egress when built" caveat in this module's original docstring/experiment.yaml is stale, the repo
# is publicly reachable). This REPLACES the prior best-effort log-Euclidean reconstruction, which
# diffing against the official source confirmed was a fundamentally different (and much more
# expensive, full-eigh-based) algorithm -- see fix-later.md's "online_kl_shampoo diffed against the
# official release" note for the full divergence writeup.
#
# What the official algorithm actually is (structurally closer to SOAP than to log-Euclidean
# Shampoo): a zero-staleness *coupled* covariance EMA -- S_a accumulates from the *already
# preconditioned* gradient G@P_b (and S_b from P_a@G), so this step's freshly-recomputed P feeds
# next step's S in the same step, not a lagged/staled basis -- combined with a pure-matmul,
# eigh-free inverse-square-root (10-step "Scaled CANS coupled Newton-Schulz", arXiv:2506.10935),
# Nesterov momentum with an exact variance-correction constant, muP shape scaling, and AdamC
# decoupled weight decay. Because the whitening step never calls `torch.linalg.eigh` or
# `matrix_exp` -- only matmuls, a diagonal-max-based spectral bound, and elementwise ops -- this is
# also a concrete instance of the eigh-free inverse-square-root path fix-later.md's ttnn item was
# looking for (see that item's update note); a future ttnn port of this optimizer would not need
# real eigenvectors the way SOAP/KL-Shampoo's eigenbasis-rotation mechanism does.
#
# Simplifications made porting to this codebase (documented, not silent):
#   - The official code batches N independent (m, n) matrices per call and stores S/P as packed
#     upper triangles for memory; this codebase calls one optimizer instance per-parameter with
#     N=1 (2D params only, matching every weight in AutoencoderMLP), so both are unpacked into
#     plain dense (m, m)/(n, n) tensors -- same math, no packing/batching complexity needed here.
#   - The official ScaledCANS routine casts to fp16 mid-iteration purely for GPU throughput (the
#     _S_Y/_S_Z per-step magnitude-scaling arrays exist to keep values in fp16 range); the
#     algebra already has those scale factors fully absorbed into the CANS_COEFFS-derived runtime
#     coefficients, so dropping the fp16 casts and running the identical coefficient arithmetic in
#     fp32 throughout is mathematically the same iteration, just without the GPU-only speed trick
#     -- correct on CPU/any-device, not a fidelity shortcut.
#   - The official optimizer raises on non-2D/3D params (biases must use a separate optimizer);
#     this codebase's build_optimizer gives every optimizer a single instance across all
#     parameters (see build_optimizer's shared-instance convention for kl_shampoo etc.), so 1D
#     bias params here fall back to a plain decoupled-AdamW-style update (`weight_decay=0.0`,
#     matching this codebase's existing convention for non-matrix aux params, e.g. Muon's bias
#     fallback) instead of being routed through the matrix algorithm they were never defined for.
# ---------------------------------------------------------------------------------------------

# CANS polynomial coefficients (arXiv:2506.10935), verbatim from the official release.
_CANS_COEFFS = [
    (5.182503604966906, -5.178098480082684),
    (2.586120737395915, -0.6479542005271643),
    (2.567364126726186, -0.6454968804392178),
    (2.520560084348265, -0.6393528082067044),
    (2.410759275435182, -0.6248683598710716),
    (2.1883348130094173, -0.5952022073798908),
    (1.8595760874873613, -0.5504490972723968),
    (1.589020160467417, -0.5126569802066718),
    (1.5051653981684994, -0.5007377068751799),
    (1.5, -0.5),
]


def _okls_estimate_max_eigenvalue(A: torch.Tensor, eps: float) -> torch.Tensor:
    """Strict upper bound on the largest eigenvalue of a batch of SPD matrices, via
    min(Wolkowicz-Styan, Minc-Sainte-Marie) -- O(n^2), no eigh. Ported from the official
    scaled_cans_coupled_ns.py, dropping only its fp16-range rescaling (irrelevant off-GPU).

    Keeps the official's SEPARATE fp32-overflow-guard rescaling (unrelated to the fp16 machinery
    this port otherwise drops): both bounds below square-and-sum O(n^2) intermediates, which
    overflow fp32 for a large-magnitude S (e.g. S ~ 1e20) without first rescaling by each row's
    own magnitude -- Codex review (2026-09-14) caught this as a real bug in an earlier version of
    this function that omitted it, producing L=inf (and a silently-zeroed S^-1/2) on a
    large-magnitude S where the official estimator stays finite.
    """
    n = A.shape[-1]
    diag = A.diagonal(dim1=-2, dim2=-1)
    c = (diag.abs().max(dim=-1).values * (n / math.sqrt(torch.finfo(torch.float32).max))) \
        .clamp(min=1e-30)
    c_inv = 1.0 / c
    abs_A_s = (A * c_inv[..., None, None]).abs()

    m_s = diag.sum(dim=-1) * (c_inv / n)
    f_s_n = torch.linalg.matrix_norm(abs_A_s) / math.sqrt(n)
    s_s = torch.sqrt(torch.clamp((f_s_n + m_s) * (f_s_n - m_s), min=0.0))
    ws_bound = c * (m_s + s_s * math.sqrt(max(n - 1, 0)))

    d_s = torch.sum(abs_A_s, dim=-1)
    d_s_clamped = torch.clamp(d_s, min=1e-12)
    y_s = torch.einsum("...ij,...j->...i", abs_A_s, d_s_clamped)
    minc_bound = c * torch.max(y_s / d_s_clamped, dim=-1).values

    return torch.minimum(ws_bound, minc_bound)


def _scaled_cans_coupled_ns(S: torch.Tensor, eps: float) -> torch.Tensor:
    """S^{-1/2} via the official 10-step coupled Newton-Schulz (CANS polynomials), eigh-free.
    Pure fp32 (see module note above for why dropping the official's fp16 casts is safe)."""
    L = (_okls_estimate_max_eigenvalue(S, eps) * 1.01).clamp(min=eps)
    Lv = L.view(*L.shape, 1, 1)
    Y = S / Lv  # Y0's eigenvalues are in (0, 1]

    a0, b0 = _CANS_COEFFS[0]
    Z = b0 * Y
    Z.diagonal(dim1=-2, dim2=-1).add_(a0)
    Y = a0 * Y + b0 * (Y @ Y)

    for k in range(1, 9):
        a, b = _CANS_COEFFS[k]
        P = Z @ Y
        Y, Z = a * Y + b * (Y @ P), a * Z + b * (P @ Z)

    a9, b9 = _CANS_COEFFS[9]
    P = Z @ Y
    W = a9 * Z + b9 * (P @ Z)
    W = W * torch.rsqrt(L).view(*L.shape, 1, 1)
    return 0.5 * (W + W.mT)


class OnlineKLShampoo(Optimizer):
    """Online KL-Shampoo -- official-algorithm port, see the module-level note above this class
    for the full derivation/provenance and the (documented) simplifications made porting it here.
    Defaults match the official release (arXiv:2509.03378); DEFAULT_LR_BY_OPT below still governs
    this codebase's actual runs unless OPT_LR is set explicitly.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.09434,
                 beta1: float = 0.9684, beta2: float = 0.9482, eps: float = 1e-9,
                 weight_decay: float = 0.0303):
        defaults = dict(lr=lr, lr_peak=lr, beta1=beta1, beta2=beta2, eps=eps,
                         weight_decay=weight_decay)
        super().__init__(params, defaults)

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            lr, lr_peak = group["lr"], group["lr_peak"]
            beta1, beta2, eps, wd = group["beta1"], group["beta2"], group["eps"], group["weight_decay"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]

                if g.dim() == 1:
                    # Official algorithm is undefined for 1D params -- fall back to a plain
                    # decoupled-AdamW-style update (wd=0.0, matching this codebase's convention
                    # for non-matrix aux params, see e.g. Muon's bias fallback).
                    if "adam_m" not in state:
                        state["adam_m"] = torch.zeros_like(g)
                        state["adam_v"] = torch.zeros_like(g)
                        state["adam_step"] = 0
                    state["adam_step"] += 1
                    t = state["adam_step"]
                    m, v = state["adam_m"], state["adam_v"]
                    m.lerp_(g, 1 - 0.9)
                    v.lerp_(g * g, 1 - 0.999)
                    m_hat = m / (1 - 0.9 ** t)
                    v_hat = v / (1 - 0.999 ** t)
                    p.add_(m_hat / (v_hat.sqrt() + 1e-8), alpha=-lr)
                    continue

                m_dim, n_dim = g.shape
                if "momentum" not in state:
                    state["momentum"] = torch.zeros_like(g)
                    state["S_a"] = torch.zeros(m_dim, m_dim, device=g.device, dtype=torch.float32)
                    state["S_b"] = torch.zeros(n_dim, n_dim, device=g.device, dtype=torch.float32)
                    state["P_a"] = torch.zeros(m_dim, m_dim, device=g.device, dtype=torch.float32)
                    state["P_b"] = torch.zeros(n_dim, n_dim, device=g.device, dtype=torch.float32)
                    state["step"] = 0

                grad = g.float()
                S_a, S_b, P_a, P_b = state["S_a"], state["S_b"], state["P_a"], state["P_b"]

                if state["step"] == 0:
                    # Warm-start S/P from the first gradient instead of zero-init (official
                    # init_preconditioners) -- avoids an all-zero/degenerate first whitening step.
                    # Official: sqrt(m / (n * ||G||^2 + eps)) -- eps is added to the denominator
                    # sum, not used as a floor/clamp on ||G||^2 (those are not equivalent,
                    # especially for a small-magnitude gradient; caught by Codex review).
                    frob_sq = grad.square().sum()
                    GGt = grad @ grad.T
                    S_a.copy_(math.sqrt(m_dim / (n_dim * frob_sq + eps)) * GGt)
                    S_a.copy_(0.5 * (S_a + S_a.T))
                    S_a.diagonal().add_(S_a.norm() / math.sqrt(m_dim) + eps)
                    GtG = grad.T @ grad
                    S_b.copy_(math.sqrt(n_dim / (m_dim * frob_sq + eps)) * GtG)
                    S_b.copy_(0.5 * (S_b + S_b.T))
                    S_b.diagonal().add_(S_b.norm() / math.sqrt(n_dim) + eps)
                    P_a.copy_(_scaled_cans_coupled_ns(S_a, eps))
                    P_b.copy_(_scaled_cans_coupled_ns(S_b, eps))

                # 1. Nesterov momentum.
                mom = state["momentum"]
                mom.lerp_(grad, 1 - beta1)
                N_t = torch.lerp(grad, mom, beta1)

                # 2. Coupled (zero-staleness) preconditioner EMA: this step's P whitens the
                # gradient fed into *this same step's* S update, not a lagged/stale P.
                GPb = grad @ P_b
                PaG = P_a @ grad
                S_a.mul_(beta2).add_(GPb @ GPb.T, alpha=(1 - beta2) / n_dim)
                S_a.copy_(0.5 * (S_a + S_a.T))
                S_a.diagonal().add_(eps)
                S_b.mul_(beta2).add_(PaG.T @ PaG, alpha=(1 - beta2) / m_dim)
                S_b.copy_(0.5 * (S_b + S_b.T))
                S_b.diagonal().add_(eps)

                # 3. Fast matrix roots -- eigh-free.
                P_a.copy_(_scaled_cans_coupled_ns(S_a, eps))
                P_b.copy_(_scaled_cans_coupled_ns(S_b, eps))

                # 4. Gradient whitening.
                U = P_a @ N_t @ P_b

                # 5. Decoupled weight decay (AdamC) + Nesterov variance correction + muP shape
                # scaling, then the update.
                v_nesterov = ((1 - beta1) / (1 + beta1)) * (1 + 2 * beta1 - 2 * beta1 ** 3)
                c_momentum = v_nesterov ** -0.5
                s_shape = (m_dim / n_dim) ** 0.5 / (m_dim ** 0.5 + n_dim ** 0.5)
                wd_coeff = wd * lr * (lr / lr_peak)

                p.mul_(1 - wd_coeff)
                p.add_((c_momentum * s_shape * U).to(p.dtype), alpha=-lr)

                state["step"] += 1
        return loss



class CUSTOM_BlockShampoo(Optimizer):
    """Block-diagonal Kronecker-factored Shampoo (agent-shampoo-4 architecture variant, building on
    ScalableShampoo). MECHANISM: for this model every Shampoo-family method's per-step cost is
    dominated by the cube() of whichever factor dimension equals 784 (enc1's R, dec2's L) --
    accumulating that whole 784x784 covariance/eigendecomp every `precondition_freq` steps.
    Structural change: block-diagonalize any factor dimension > `block_size` into
    ceil(dim/block_size) equal chunks and keep one small covariance/inverse-root per block (zero
    cross-block terms), instead of one dense dim x dim factor. This is strictly less capacity than
    full Shampoo (drops cross-block curvature) but cube(block) x num_blocks << cube(dim) -- e.g.
    784 -> 4 blocks of 196 cuts that term ~16x. The FLOP saved buys a *smaller* precondition_freq
    (more frequent root updates) at equal or lower total FLOPs, which should track curvature better
    than a stale full factor recomputed rarely. Grafting (direction from block-Shampoo, step size
    from SGD-momentum) reused from ScalableShampoo so this isolates the block-diagonal structure,
    not the grafting mechanism.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4,
                 momentum: float = 0.9, precondition_freq: int = 10, block_size: int = 196):
        super().__init__(params, dict(lr=lr, eps=eps, momentum=momentum,
                                       precondition_freq=precondition_freq, block_size=block_size))

    @staticmethod
    def _block_bounds(dim: int, block_size: int) -> list:
        if dim <= block_size:
            return [(0, dim)]
        n_blocks = math.ceil(dim / block_size)
        base = dim // n_blocks
        bounds = []
        s = 0
        for i in range(n_blocks):
            e = s + base + (1 if i < dim % n_blocks else 0)
            bounds.append((s, e))
            s = e
        return bounds

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            bsz = group["block_size"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                is_1d = g.dim() == 1
                # k=1 tensor: single statistics matrix, exponent -1/2 -- same fix as Shampoo2018/
                # ScalableShampoo (the (d,1)-shaped-with-two-(-1/4)-factors form double-applies the
                # exponent on a rank-1 tensor and is wrong; Codex-reviewed 2026-09-14).
                l_power = -0.5 if is_1d else -0.25
                if g.dim() == 1:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "row_bounds" not in state:
                    state["row_bounds"] = self._block_bounds(d0, bsz)
                    state["col_bounds"] = [] if is_1d else self._block_bounds(d1, bsz)
                    state["L_blocks"] = [group["eps"] * torch.eye(e - s, device=g.device, dtype=g.dtype)
                                         for s, e in state["row_bounds"]]
                    state["R_blocks"] = [group["eps"] * torch.eye(e - s, device=g.device, dtype=g.dtype)
                                         for s, e in state["col_bounds"]]
                    state["L_inv_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype)
                                              for s, e in state["row_bounds"]]
                    state["R_inv_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype)
                                              for s, e in state["col_bounds"]]
                    state["graft_momentum"] = torch.zeros_like(g)
                    state["step"] = 0
                state["step"] += 1
                row_bounds, col_bounds = state["row_bounds"], state["col_bounds"]
                for bi, (rs, re) in enumerate(row_bounds):
                    state["L_blocks"][bi].add_(g2[rs:re, :] @ g2[rs:re, :].T)
                for bj, (cs, ce) in enumerate(col_bounds):
                    state["R_blocks"][bj].add_(g2[:, cs:ce].T @ g2[:, cs:ce])
                if state["step"] % group["precondition_freq"] == 0 or state["step"] == 1:
                    state["L_inv_blocks"] = [_matrix_power(Lb, l_power, group["eps"]) for Lb in state["L_blocks"]]
                    state["R_inv_blocks"] = [_matrix_power(Rb, -0.25, group["eps"]) for Rb in state["R_blocks"]]
                shampoo_dir = torch.zeros_like(g2)
                if is_1d:
                    for bi, (rs, re) in enumerate(row_bounds):
                        shampoo_dir[rs:re, :] = state["L_inv_blocks"][bi] @ g2[rs:re, :]
                else:
                    for bi, (rs, re) in enumerate(row_bounds):
                        for bj, (cs, ce) in enumerate(col_bounds):
                            shampoo_dir[rs:re, cs:ce] = state["L_inv_blocks"][bi] @ g2[rs:re, cs:ce] @ state["R_inv_blocks"][bj]
                shampoo_dir = shampoo_dir.view_as(g)

                # Grafting (Anil et al. 2020 Algorithm 1), corrected to match the same fix applied
                # to ScalableShampoo (b4e5db0, Codex-reviewed): rescale the Shampoo *direction* to
                # the current gradient's norm FIRST, THEN accumulate momentum on that rescaled
                # direction -- not (as this class originally did, mirroring ScalableShampoo's old
                # bug) normalize to unit norm and scale by a momentum-of-raw-gradient's norm with
                # no momentum ever applied to the Shampoo direction itself.
                scaled_dir = shampoo_dir * (g.norm() / (shampoo_dir.norm() + group["eps"]))
                state["graft_momentum"].mul_(group["momentum"]).add_(scaled_dir)
                update = state["graft_momentum"]
                p.add_(update, alpha=-group["lr"])
        return loss


class CUSTOM_BlockSOAP(SOAP):
    """Block-diagonal SOAP (agent-shampoo-4 architecture variant #2, building on this repo's
    SOAP and on agent-shampoo-3's finding that SOAP at its own paper defaults beats AdamW 2.58x
    here -- far better than this agent's own block_shampoo variant of the matrix-power Shampoo
    family, which never reached TARGET_LOSS at any block_size/precondition_freq tried). MECHANISM:
    same block-diagonal idea as block_shampoo (block-diagonalize any factor dim > block_size,
    cube-cost cheaper eigh per block instead of one dense dim x dim eigh), but grafted onto SOAP's
    stronger base -- Adam's own per-coordinate adaptivity in the rotated basis is far more robust
    to a coarser (block-diagonal, cross-block-blind) eigenbasis approximation than Shampoo's
    direction-only preconditioning is, since Adam still individually rescales each rotated
    coordinate rather than trusting the preconditioner's direction outright. Testing whether that
    robustness holds up under the same capacity-for-compute trade that failed for block_shampoo.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 3e-3, eps: float = 1e-8,
                 betas=(0.9, 0.999), shampoo_beta: float = 0.95, eigenbasis_freq: int = 10,
                 block_size: int = 196):
        Optimizer.__init__(self, params, dict(lr=lr, eps=eps, betas=betas, shampoo_beta=shampoo_beta,
                                               eigenbasis_freq=eigenbasis_freq, block_size=block_size))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            b1, b2 = group["betas"]
            sb = group["shampoo_beta"]
            bsz = group["block_size"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if g.dim() == 1:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "row_bounds" not in state:
                    state["row_bounds"] = CUSTOM_BlockShampoo._block_bounds(d0, bsz)
                    state["col_bounds"] = CUSTOM_BlockShampoo._block_bounds(d1, bsz)
                    state["L_blocks"] = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype)
                                          for s, e in state["row_bounds"]]
                    state["R_blocks"] = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype)
                                          for s, e in state["col_bounds"]]
                    state["QL_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype)
                                           for s, e in state["row_bounds"]]
                    state["QR_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype)
                                           for s, e in state["col_bounds"]]
                    state["m"] = torch.zeros_like(g2)
                    state["v"] = torch.zeros_like(g2)
                    state["step"] = 0
                state["step"] += 1
                t = state["step"]
                row_bounds, col_bounds = state["row_bounds"], state["col_bounds"]
                # Refresh QL/QR blocks from L/R accumulated up to (not including) this step, THEN
                # accumulate this step's gradient into L/R -- same staleness requirement as SOAP
                # (a basis must never be computed from the same gradient it is then applied to).
                # Same correctness requirement as SOAP (2b7fc80): M stays in PARAMETER coordinates
                # (never transported across a basis refresh); V is a diagonal estimator in the
                # CURRENT per-block eigenbasis, also never transported (a squared change-of-basis
                # is not a valid transport of a diagonal covariance -- see SOAP's own fix).
                if t > 1 and (t - 1) % group["eigenbasis_freq"] == 0:
                    for bi, (rs, re) in enumerate(row_bounds):
                        bs0 = re - rs
                        _, state["QL_blocks"][bi] = torch.linalg.eigh(
                            state["L_blocks"][bi] + group["eps"] * torch.eye(bs0, device=g.device, dtype=g.dtype))
                    for bj, (cs, ce) in enumerate(col_bounds):
                        bs1 = ce - cs
                        _, state["QR_blocks"][bj] = torch.linalg.eigh(
                            state["R_blocks"][bj] + group["eps"] * torch.eye(bs1, device=g.device, dtype=g.dtype))
                for bi, (rs, re) in enumerate(row_bounds):
                    state["L_blocks"][bi].mul_(sb).add_(g2[rs:re, :] @ g2[rs:re, :].T, alpha=1 - sb)
                for bj, (cs, ce) in enumerate(col_bounds):
                    state["R_blocks"][bj].mul_(sb).add_(g2[:, cs:ce].T @ g2[:, cs:ce], alpha=1 - sb)
                g_rot = torch.zeros_like(g2)
                for bi, (rs, re) in enumerate(row_bounds):
                    for bj, (cs, ce) in enumerate(col_bounds):
                        g_rot[rs:re, cs:ce] = state["QL_blocks"][bi].T @ g2[rs:re, cs:ce] @ state["QR_blocks"][bj]
                state["m"].mul_(b1).add_(g2, alpha=1 - b1)
                state["v"].mul_(b2).addcmul_(g_rot, g_rot, value=1 - b2)
                m_rot = torch.zeros_like(g2)
                for bi, (rs, re) in enumerate(row_bounds):
                    for bj, (cs, ce) in enumerate(col_bounds):
                        m_rot[rs:re, cs:ce] = state["QL_blocks"][bi].T @ state["m"][rs:re, cs:ce] @ state["QR_blocks"][bj]
                m_hat = m_rot / (1 - b1 ** t)
                v_hat = state["v"] / (1 - b2 ** t)
                update_rot = m_hat / (v_hat.sqrt() + group["eps"])
                update = torch.zeros_like(g2)
                for bi, (rs, re) in enumerate(row_bounds):
                    for bj, (cs, ce) in enumerate(col_bounds):
                        update[rs:re, cs:ce] = state["QL_blocks"][bi] @ update_rot[rs:re, cs:ce] @ state["QR_blocks"][bj].T
                p.add_(update.view_as(g), alpha=-group["lr"])
        return loss


class KLSOAP(Optimizer):
    """KL-SOAP (Lin et al., arXiv:2509.03378, Section 5 / Figure 3 -- verified via
    arxiv.org/html/2509.03378v9). NEW VARIANT for this experiment's success criterion 3, not a
    fidelity fix to an existing optimizer: the paper proposes KL-SOAP as SOAP with its factor
    accumulation replaced by KL-Shampoo's coupled update (each factor whitened by the OTHER
    factor's current inverse, reconstructed from its cached eigenbasis instead of a fresh matrix
    inverse -- cheaper, and reuses eigenbasis SOAP already maintains):
        S_a <- (1-b2) S_a + (b2/d_b) G Q_b diag(lambda_b^-1) Q_b^T G^T    (coupled, was G G^T)
        S_b <- (1-b2) S_b + (b2/d_a) G^T Q_a diag(lambda_a^-1) Q_a^T G
    then SOAP's own mechanism unchanged: periodic eigenbasis refresh, Adam/RMSProp run in that
    rotated basis. HONEST PRIOR: the paper's own results (its Introduction, Figure 1) report
    KL-Shampoo beats KL-SOAP AND plain SOAP on language-model training -- so this is not expected
    to be the family's overall winner. Testing it here anyway because (a) that result is on a
    different domain (LM, not this tiny AE) and CONSTRAINTS treats each variant as needing its own
    fair sweep before a verdict, (b) a well-grounded negative result confirming the paper's own
    ordering on a second domain is still a real, useful confirmed/not-confirmed data point.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 3e-3, eps: float = 1e-8,
                 betas=(0.9, 0.999), shampoo_beta: float = 0.95, eigenbasis_freq: int = 10):
        super().__init__(params, dict(lr=lr, eps=eps, betas=betas, shampoo_beta=shampoo_beta,
                                       eigenbasis_freq=eigenbasis_freq))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            b1, b2 = group["betas"]
            sb = group["shampoo_beta"]
            eps = group["eps"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                g2 = g.unsqueeze(1) if g.dim() == 1 else g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L" not in state:
                    state["L"] = eps * torch.eye(d0, device=g.device, dtype=g.dtype)
                    state["R"] = eps * torch.eye(d1, device=g.device, dtype=g.dtype)
                    state["QL"] = torch.eye(d0, device=g.device, dtype=g.dtype)
                    state["QR"] = torch.eye(d1, device=g.device, dtype=g.dtype)
                    state["lamL_inv"] = torch.ones(d0, device=g.device, dtype=g.dtype) / eps
                    state["lamR_inv"] = torch.ones(d1, device=g.device, dtype=g.dtype) / eps
                    state["m"] = torch.zeros_like(g2)
                    state["v"] = torch.zeros_like(g2)
                    state["step"] = 0
                state["step"] += 1
                t = state["step"]
                # Refresh QL/QR from L/R accumulated up to (not including) this step, THEN
                # accumulate this step's coupled stat -- same staleness requirement as SOAP: the
                # basis applied to g_rot below must never be derived from that same gradient.
                if t > 1 and (t - 1) % group["eigenbasis_freq"] == 0:
                    lamL, QL_new = torch.linalg.eigh(state["L"] + eps * torch.eye(d0, device=g.device, dtype=g.dtype))
                    lamR, QR_new = torch.linalg.eigh(state["R"] + eps * torch.eye(d1, device=g.device, dtype=g.dtype))
                    state["QL"], state["QR"] = QL_new, QR_new
                    state["lamL_inv"] = 1.0 / lamL.clamp(min=eps)
                    state["lamR_inv"] = 1.0 / lamR.clamp(min=eps)
                QL, QR = state["QL"], state["QR"]
                # Coupled accumulation: each side whitened by the OTHER side's cached inverse
                # (reconstructed from its eigenbasis: Q diag(lambda^-1) Q^T == that side's S^-1).
                Rb_inv_half = QR @ torch.diag(state["lamR_inv"].sqrt())
                La_inv_half = QL @ torch.diag(state["lamL_inv"].sqrt())
                whitened_a = g2 @ Rb_inv_half
                whitened_b = La_inv_half.T @ g2
                state["L"].mul_(sb).add_(whitened_a @ whitened_a.T, alpha=(1 - sb) / d1)
                state["R"].mul_(sb).add_(whitened_b.T @ whitened_b, alpha=(1 - sb) / d0)
                # SOAP's own mechanism, unchanged: Adam/RMSProp run in the (now coupled-updated)
                # eigenbasis.
                g_rot = QL.T @ g2 @ QR
                state["m"].mul_(b1).add_(g2, alpha=1 - b1)
                state["v"].mul_(b2).addcmul_(g_rot, g_rot, value=1 - b2)
                m_hat = (QL.T @ state["m"] @ QR) / (1 - b1 ** t)
                v_hat = state["v"] / (1 - b2 ** t)
                update_rot = m_hat / (v_hat.sqrt() + eps)
                update = (QL @ update_rot @ QR.T).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


class CUSTOM_BlockKLSOAP(Optimizer):
    """Block-diagonal KL-SOAP: agent-shampoo-1's own new variant (success criterion 3), combining
    three independently-verified pieces rather than guessing from scratch: (1) block-diagonal
    Kronecker factors (agent-shampoo-4's block_soap -- cheaper per-block eigh instead of one dense
    eigh, ~1.28x win over plain SOAP), (2) KL-Shampoo's coupled factor accumulation (each side
    whitened by the OTHER side's current inverse, arxiv 2509.03378 Eq. 5 -- see KLSOAP docstring),
    (3) SOAP's periodic Adam-in-eigenbasis mechanism: the first moment stays in parameter
    coordinates and is rotated only to form an update, while the diagonal second moment is an EMA
    of squared gradients in the current eigenbasis.

    Coupling is computed block-locally: each (row-block, col-block) sub-block of the gradient is
    whitened by the OTHER side's cached block inverse (reconstructed from that block's eigenbasis
    + inverse eigenvalues) before accumulating into this block's own L/R statistic -- the natural
    block-diagonalization of KL-Shampoo's coupling (which is over the FULL other dimension in the
    unblocked version), applied independently per block on each side, at the same O(num_blocks^2 *
    block_size^3) cost CUSTOM_BlockSOAP's own rotation step already pays.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 3e-3, eps: float = 1e-8,
                 betas=(0.9, 0.999), shampoo_beta: float = 0.95, eigenbasis_freq: int = 10,
                 block_size: int = 196, nesterov: bool = False):
        # nesterov=True switches the final update from plain Adam's bias-corrected m_hat to the
        # NAdam-style look-ahead correction (Dozat 2016: m_hat <- b1*m_hat + (1-b1)*g/(1-b1^t)),
        # motivated by Online-KL-Shampoo's own paper-verified use of "Nesterov momentum with
        # variance correction" (see OnlineKLShampoo docstring) -- same lineage, untested on this
        # variant. Opt-in and default-off so the existing verified plain-Adam path is unchanged.
        super().__init__(params, dict(lr=lr, eps=eps, betas=betas, shampoo_beta=shampoo_beta,
                                       eigenbasis_freq=eigenbasis_freq, block_size=block_size,
                                       nesterov=nesterov))

    @staticmethod
    def _block_bounds(dim: int, block_size: int):
        nb = max(1, -(-dim // block_size))
        bsz = -(-dim // nb)
        bounds, s = [], 0
        while s < dim:
            e = min(dim, s + bsz)
            bounds.append((s, e))
            s = e
        return bounds

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            b1, b2 = group["betas"]
            sb = group["shampoo_beta"]
            eps = group["eps"]
            bsz = group["block_size"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                g2 = g.unsqueeze(1) if g.dim() == 1 else g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "row_bounds" not in state:
                    state["row_bounds"] = self._block_bounds(d0, bsz)
                    state["col_bounds"] = self._block_bounds(d1, bsz)
                    rb, cb = state["row_bounds"], state["col_bounds"]
                    state["L_blocks"] = [eps * torch.eye(e - s, device=g.device, dtype=g.dtype) for s, e in rb]
                    state["R_blocks"] = [eps * torch.eye(e - s, device=g.device, dtype=g.dtype) for s, e in cb]
                    state["QL_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype) for s, e in rb]
                    state["QR_blocks"] = [torch.eye(e - s, device=g.device, dtype=g.dtype) for s, e in cb]
                    state["lamL_inv_blocks"] = [torch.ones(e - s, device=g.device, dtype=g.dtype) / eps for s, e in rb]
                    state["lamR_inv_blocks"] = [torch.ones(e - s, device=g.device, dtype=g.dtype) / eps for s, e in cb]
                    state["m"] = torch.zeros_like(g2)
                    state["v"] = torch.zeros_like(g2)
                    state["step"] = 0
                state["step"] += 1
                t = state["step"]
                rb, cb = state["row_bounds"], state["col_bounds"]

                # Refresh QL/QR blocks from L/R accumulated up to (not including) this step, THEN
                # accumulate this step's coupled stat -- same staleness requirement as SOAP/KLSOAP:
                # the basis applied to this step's gradient below must never be derived from that
                # same gradient.
                if t > 1 and (t - 1) % group["eigenbasis_freq"] == 0:
                    for bi, (rs, re) in enumerate(rb):
                        bs0 = re - rs
                        lamL, state["QL_blocks"][bi] = torch.linalg.eigh(
                            state["L_blocks"][bi] + eps * torch.eye(bs0, device=g.device, dtype=g.dtype))
                        state["lamL_inv_blocks"][bi] = 1.0 / lamL.clamp(min=eps)
                    for bj, (cs, ce) in enumerate(cb):
                        bs1 = ce - cs
                        lamR, state["QR_blocks"][bj] = torch.linalg.eigh(
                            state["R_blocks"][bj] + eps * torch.eye(bs1, device=g.device, dtype=g.dtype))
                        state["lamR_inv_blocks"][bj] = 1.0 / lamR.clamp(min=eps)

                # Coupled block-local accumulation. KLSOAP's own unblocked reference (see its
                # docstring/step()) normalizes each side by the OTHER side's dimension -- L by
                # 1/d1, R by 1/d0 -- and this class was silently missing the block-local analogue
                # of that (fix-later.md broad-review item #5, confirmed 2026-09-14: CUSTOM_BlockKLSOAP
                # accumulated raw wa@wa.T/wb.T@wb with no dimensional normalization at all). The
                # block-local analogue of KLSOAP's "other side's full dimension" is that
                # contribution's own block size, since wa/wb are only ever as wide as the block
                # they were whitened against.
                Lsum = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype) for s, e in rb]
                Rsum = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype) for s, e in cb]
                for bi, (rs, re) in enumerate(rb):
                    for bj, (cs, ce) in enumerate(cb):
                        block = g2[rs:re, cs:ce]
                        Rb_inv_half = state["QR_blocks"][bj] @ torch.diag(state["lamR_inv_blocks"][bj].sqrt())
                        La_inv_half = state["QL_blocks"][bi] @ torch.diag(state["lamL_inv_blocks"][bi].sqrt())
                        wa = block @ Rb_inv_half
                        wb = La_inv_half.T @ block
                        Lsum[bi] += (wa @ wa.T) / (ce - cs)
                        Rsum[bj] += (wb.T @ wb) / (re - rs)
                for bi in range(len(rb)):
                    state["L_blocks"][bi].mul_(sb).add_(Lsum[bi], alpha=1 - sb)
                for bj in range(len(cb)):
                    state["R_blocks"][bj].mul_(sb).add_(Rsum[bj], alpha=1 - sb)

                g_rot = torch.zeros_like(g2)
                for bi, (rs, re) in enumerate(rb):
                    for bj, (cs, ce) in enumerate(cb):
                        g_rot[rs:re, cs:ce] = state["QL_blocks"][bi].T @ g2[rs:re, cs:ce] @ state["QR_blocks"][bj]
                state["m"].mul_(b1).add_(g2, alpha=1 - b1)
                state["v"].mul_(b2).addcmul_(g_rot, g_rot, value=1 - b2)
                v_hat = state["v"] / (1 - b2 ** t)
                # 2026-09-12 fix: group["nesterov"] was accepted in __init__ but never read here --
                # a silent no-op for every job that set OPT_NESTEROV=1 since aa464f5 moved m_hat
                # computation inside this per-block loop (the prior single-shot NAdam blend was
                # dropped and never re-added, per the removed NOTE above this diff). Re-added
                # properly using the per-block g_rot slice already computed above: NAdam-style
                # look-ahead (Dozat 2016) m_hat <- b1*m_hat_plain + (1-b1)*g_rot/(1-b1^t), composed
                # per-block the same way the plain-Adam path already is.
                nesterov = group["nesterov"]
                update = torch.zeros_like(g2)
                for bi, (rs, re) in enumerate(rb):
                    for bj, (cs, ce) in enumerate(cb):
                        m_hat = (state["QL_blocks"][bi].T @ state["m"][rs:re, cs:ce]
                                 @ state["QR_blocks"][bj]) / (1 - b1 ** t)
                        if nesterov:
                            m_hat = b1 * m_hat + (1 - b1) * g_rot[rs:re, cs:ce] / (1 - b1 ** t)
                        update_rot = m_hat / (v_hat[rs:re, cs:ce].sqrt() + eps)
                        update[rs:re, cs:ce] = state["QL_blocks"][bi] @ update_rot @ state["QR_blocks"][bj].T
                p.add_(update.view_as(g), alpha=-group["lr"])
        return loss


class CUSTOM_BlockMuon(Optimizer):
    """Block-diagonal Muon (agent-shampoo-4 architecture variant #3, distinct family from
    block_shampoo/block_soap). MECHANISM, and why this one needed its own honest FLOP model (see
    this agent's fidelity flag on the Muon hypothesis): Newton-Schulz orthogonalization is NOT
    elementwise -- per iteration it costs ~m^2*n (two matmuls) + m^3 (a_mat@a_mat), where
    m=min(d0,d1), n=max(d0,d1); the m^3 term is a square m x m matmul independent of n. Blocking
    the *smaller* dimension m into ceil(m/block_size) blocks and orthogonalizing each block
    independently (a block-diagonal orthogonal matrix, not one full m x n orthogonalization) cuts
    the m^2*n term ~linearly in block count and the m^3 term ~quadratically (m^3/k^2 summed over k
    blocks) -- unlike block_shampoo blocking the *larger* dimension of a Kronecker factor, here
    it's specifically the smaller side that pays the cubic cost, so that's the one to split.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.02, momentum: float = 0.95,
                 ns_steps: int = 5, block_size: int = 32, nesterov: bool = False,
                 ttnn_device=None, ttnn_module=None):
        super().__init__(params, dict(lr=lr, momentum=momentum, ns_steps=ns_steps, block_size=block_size,
                                       nesterov=nesterov))
        # Same on-device Newton-Schulz option as plain Muon (see ttnn_muon.py): when set, each
        # block's NS iteration runs via ttnn.matmul on real TT hardware instead of CPU torch.
        # Blocking the smaller NS dimension means each on-device matmul is also smaller than
        # plain Muon's -- more, cheaper on-device calls rather than one big one.
        self.ttnn_device = ttnn_device
        self.ttnn_module = ttnn_module

    def _block_orthogonalize(self, x: torch.Tensor, ns_steps: int, block_size: int) -> torch.Tensor:
        """x is (m, n) with m <= n. Orthogonalizes each row-block of x independently."""
        m = x.shape[0]
        if self.ttnn_device is not None:
            from ttnn_muon import newton_schulz_ttnn
            def ns(block):
                return newton_schulz_ttnn(block, self.ttnn_device, self.ttnn_module, steps=ns_steps)
        else:
            def ns(block):
                return _newton_schulz(block, steps=ns_steps)
        if m <= block_size:
            return ns(x)
        out = torch.zeros_like(x)
        for s, e in CUSTOM_BlockShampoo._block_bounds(m, block_size):
            out[s:e, :] = ns(x[s:e, :])
        return out

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            bsz = group["block_size"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if "momentum_buf" not in state:
                    state["momentum_buf"] = torch.zeros_like(g)
                buf = state["momentum_buf"]
                buf.mul_(group["momentum"]).add_(g)
                # Nesterov lookahead, matching the official Muon reference implementation
                # (Keller Jordan et al., github.com/KellerJordan/Muon): orthogonalize
                # g + momentum*buf instead of buf itself. This repo's Muon/CUSTOM_BlockMuon both
                # previously orthogonalized plain heavy-ball momentum only (nesterov=False
                # default preserves every existing confirmed number bit-for-bit).
                look = g.add(buf, alpha=group["momentum"]) if group["nesterov"] else buf
                g2 = look if g.dim() >= 2 else look.unsqueeze(1)
                mat = g2.reshape(g2.shape[0], -1)
                transposed = mat.shape[0] > mat.shape[1]
                x = mat.T if transposed else mat
                orth = self._block_orthogonalize(x, group["ns_steps"], bsz)
                orth = orth.T if transposed else orth
                update = orth.view_as(g) if g.dim() >= 2 else orth.view_as(buf.unsqueeze(1)).squeeze(1)
                scale = max(1.0, mat.shape[0] / mat.shape[1]) ** 0.5
                p.add_(update * scale, alpha=-group["lr"])
        return loss


class CUSTOM_BlockMuonAdaptive(Optimizer):
    """5th distinct architecture variant (agent-shampoo-4): does STACKING more per-coordinate
    adaptivity on top of the already-winning block_muon help further, or is it saturated?

    MOTIVATION: this agent's own mechanistic theory (validated 4/4 times across
    block_shampoo/block_soap/block_muon/block_kl_shampoo) is that block-diagonal approximation
    wins specifically when there is a secondary per-coordinate adaptive stage downstream that can
    absorb the coarser structure. block_muon already has one such stage implicitly, via
    Newton-Schulz's orthogonalization + the final row/col norm rescale -- but it has no genuine
    *learned*, per-coordinate second-moment adaptivity the way SOAP's Adam-in-eigenbasis does.
    Grounded in real optimizer literature combining orthogonalized (Muon-style) updates with an
    Adam-style elementwise RMS rescale (discussed as a practical extension in Muon's own
    follow-up writeups, e.g. "RMS-matching"/"Adam-Muon" hybrids): after block-orthogonalizing the
    momentum, maintain a plain elementwise EMA of its squared magnitude (v, Adam's second moment,
    computed on the ORIGINAL un-rotated coordinates -- no eigenbasis change, unlike SOAP) and
    rescale the orthogonalized update by 1/sqrt(v+eps) before the final row/col norm scale and lr
    step. If more per-coordinate adaptivity keeps helping monotonically, this should beat plain
    block_muon; if it doesn't (e.g. because Newton-Schulz's own normalization already saturates
    the benefit, or the extra rescale fights the orthogonality property), that is itself a
    genuine, informative negative result about where this mechanism's benefit comes from.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.02, momentum: float = 0.95,
                 ns_steps: int = 5, block_size: int = 32, beta2: float = 0.999, eps: float = 1e-8,
                 nesterov: bool = False, ttnn_device=None, ttnn_module=None):
        super().__init__(params, dict(lr=lr, momentum=momentum, ns_steps=ns_steps, block_size=block_size,
                                       beta2=beta2, eps=eps, nesterov=nesterov))
        # Reuses CUSTOM_BlockMuon._block_orthogonalize (called as an unbound method below), which reads
        # self.ttnn_device/self.ttnn_module -- must be set here even though this class's own
        # __init__ doesn't otherwise touch on-device execution, or that lookup raises.
        self.ttnn_device = ttnn_device
        self.ttnn_module = ttnn_module

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            bsz = group["block_size"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                if "momentum_buf" not in state:
                    state["momentum_buf"] = torch.zeros_like(g)
                    state["v"] = torch.zeros_like(g)
                    state["step"] = 0
                state["step"] += 1
                buf = state["momentum_buf"]
                buf.mul_(group["momentum"]).add_(g)
                look = g.add(buf, alpha=group["momentum"]) if group["nesterov"] else buf
                g2 = look if g.dim() >= 2 else look.unsqueeze(1)
                mat = g2.reshape(g2.shape[0], -1)
                transposed = mat.shape[0] > mat.shape[1]
                x = mat.T if transposed else mat
                orth = CUSTOM_BlockMuon._block_orthogonalize(self, x, group["ns_steps"], bsz)
                orth = orth.T if transposed else orth
                update = orth.view_as(g) if g.dim() >= 2 else orth.view_as(buf.unsqueeze(1)).squeeze(1)
                # Elementwise Adam-style second-moment EMA + bias correction, on the ORIGINAL
                # (un-rotated) coordinates -- this is what makes it distinct from SOAP (which
                # rotates into the eigenbasis first): plain per-coordinate RMS matching on top of
                # an already block-orthogonalized direction.
                v = state["v"]
                v.mul_(group["beta2"]).addcmul_(update, update, value=1 - group["beta2"])
                v_hat = v / (1 - group["beta2"] ** state["step"])
                adapted = update / (v_hat.sqrt() + group["eps"])
                scale = max(1.0, mat.shape[0] / mat.shape[1]) ** 0.5
                p.add_(adapted * scale, alpha=-group["lr"])
        return loss


class CUSTOM_BlockKLShampoo(Optimizer):
    """Block-diagonal KL-Shampoo (agent-shampoo-4 architecture variant #4 -- a PREDICTION TEST,
    not a screening-for-a-win run). MECHANISM/PREDICTION: this agent's prior 3 variants established
    a clean pattern -- block-diagonalizing the preconditioner hurts direction-only methods (Shampoo
    2018, ScalableShampoo: block_shampoo never reached target competitively) but is tolerated by
    methods with a secondary per-coordinate adaptive stage (SOAP/Adam: block_soap won; Muon's
    orthogonalization: block_muon won even bigger). KL-Shampoo is a direction-only method like
    Shampoo -- no Adam moments, no per-coordinate rescaling after the Kronecker-factored update, just
    L_inv @ g @ R_inv @ lr. PREDICTING this variant will fail the same way block_shampoo did,
    regardless of block_size/lr. Implemented (log-Euclidean block-diagonal statistic, same
    within-block-only cross terms dropped) specifically to test that prediction, not because it's
    expected to win -- a confirmed prediction is itself evidence for the underlying mechanism.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4,
                 alpha: float = 0.05, block_size: int = 196):
        super().__init__(params, dict(lr=lr, eps=eps, alpha=alpha, block_size=block_size))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            bsz = group["block_size"]
            alpha = group["alpha"]
            eps = group["eps"]
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad
                state = self.state[p]
                is_1d = g.dim() == 1  # see KLShampoo.step()'s same fix -- one -1/2 factor, not two -1/4.
                if is_1d:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "row_bounds" not in state:
                    state["row_bounds"] = CUSTOM_BlockShampoo._block_bounds(d0, bsz)
                    state["col_bounds"] = [] if is_1d else CUSTOM_BlockShampoo._block_bounds(d1, bsz)
                    state["L_log_blocks"] = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype)
                                              for s, e in state["row_bounds"]]
                    state["R_log_blocks"] = [torch.zeros(e - s, e - s, device=g.device, dtype=g.dtype)
                                              for s, e in state["col_bounds"]]
                row_bounds, col_bounds = state["row_bounds"], state["col_bounds"]
                for bi, (rs, re) in enumerate(row_bounds):
                    stat = g2[rs:re, :] @ g2[rs:re, :].T
                    state["L_log_blocks"][bi] = KLShampoo._log_euclidean_update(
                        state["L_log_blocks"][bi], stat, alpha, eps)
                for bj, (cs, ce) in enumerate(col_bounds):
                    stat = g2[:, cs:ce].T @ g2[:, cs:ce]
                    state["R_log_blocks"][bj] = KLShampoo._log_euclidean_update(
                        state["R_log_blocks"][bj], stat, alpha, eps)
                update = torch.zeros_like(g2)
                if is_1d:
                    for bi, (rs, re) in enumerate(row_bounds):
                        L_inv = _matrix_power(torch.linalg.matrix_exp(state["L_log_blocks"][bi]), -0.5, eps)
                        update[rs:re, :] = L_inv @ g2[rs:re, :]
                else:
                    # Hoist each R_inv out of the row loop -- it does not depend on bi, so
                    # recomputing it once per row block (as an earlier draft did) redundantly
                    # repeated a full matrix_exp+matrix_power per row, undercounting this in
                    # estimate_optimizer_flops too (Codex-reviewed 2026-09-14).
                    L_invs = [_matrix_power(torch.linalg.matrix_exp(Lb), -0.25, eps) for Lb in state["L_log_blocks"]]
                    R_invs = [_matrix_power(torch.linalg.matrix_exp(Rb), -0.25, eps) for Rb in state["R_log_blocks"]]
                    for bi, (rs, re) in enumerate(row_bounds):
                        for bj, (cs, ce) in enumerate(col_bounds):
                            update[rs:re, cs:ce] = L_invs[bi] @ g2[rs:re, cs:ce] @ R_invs[bj]
                p.add_(update.view_as(g), alpha=-group["lr"])
        return loss

# Sensible per-optimizer defaults, observed converging in local verification runs.
# AdamW's 1e-3 is wrong for every
# whitened-preconditioner method here -- each needs its own scale, not a shared default. train.py
# uses this only when OPT_LR is not explicitly set; an explicit OPT_LR always overrides it.
DEFAULT_LR_BY_OPT = {
    "sgd": 1e-3,
    "adamw": 1e-3,
    "full_adagrad": 1e-3,
    "shampoo_2018": 1.0,
    "scalable_shampoo": 1.0,
    "muon": 0.3,
    "kl_shampoo": 0.3,
    # 2026-09-14: OnlineKLShampoo replaced with the official-algorithm port (see optimizers.py's
    # class docstring) -- default now matches the official release's own tuned lr (0.09434), not
    # the old best-effort reconstruction's 0.05. A real LR sweep against this algorithm has not
    # been run yet; treat this as a starting point, not a confirmed-optimal value.
    "online_kl_shampoo": 0.09434,
    "soap": 0.05,  # still needs the real N=32 LR sweep CONSTRAINTS calls for; not fully tuned
    # Block-diagonal variants: inherit their base method's tuned default unless a local sweep
    # found otherwise (see each entry below). Merged into main from agent branches 2026-09-14.
    "custom_block_shampoo": 1.0,
    "custom_block_soap": 3e-3,
    "kl_soap": 3e-3,
    "custom_block_kl_soap": 3e-3,
    # NOT muon's 0.3 default -- block_muon/block_muon_adaptive diverge/plateau badly at 0.3
    # (never reach target); 0.02 (Muon's own constructor default) is what actually works.
    "custom_block_muon": 0.02,
    "custom_block_muon_adaptive": 0.02,
    "custom_block_kl_shampoo": 0.1,  # was missing entirely -- silently fell back to 1e-3 (100x too low)
}


def build_optimizer(name: str, model: torch.nn.Module, lr: float, **kwargs) -> Optimizer:
    """Single entry point train.py uses -- keeps the optimizer choice a config-only (env var)
    change, never a code change.
    """
    params = list(model.parameters())
    name = name.lower()
    if name == "sgd":
        return torch.optim.SGD(params, lr=lr, momentum=kwargs.get("momentum", 0.9))
    if name == "adamw":
        return torch.optim.AdamW(params, lr=lr, betas=kwargs.get("betas", (0.9, 0.999)),
                                  weight_decay=kwargs.get("weight_decay", 0.0))
    if name == "full_adagrad":
        return FullMatrixAdaGrad(params, lr=lr, **_pick(kwargs, "eps"))
    if name == "shampoo_2018":
        return Shampoo2018(params, lr=lr, **_pick(kwargs, "eps"))
    if name == "scalable_shampoo":
        return ScalableShampoo(params, lr=lr, **_pick(kwargs, "eps", "momentum", "precondition_freq"))
    if name == "soap":
        return SOAP(params, lr=lr, **_pick(kwargs, "eps", "betas", "shampoo_beta", "eigenbasis_freq"))
    if name == "muon":
        # Reference recipe: Muon only for >=2D "hidden" matrices, AdamW for 1D params (biases/norms).
        matrix_params = [p for p in params if p.dim() >= 2]
        other_params = [p for p in params if p.dim() < 2]
        muon = Muon(matrix_params, lr=lr,
                    **_pick(kwargs, "momentum", "ns_steps", "ttnn_device", "ttnn_module"))
        if other_params:
            adamw = torch.optim.AdamW(other_params, lr=kwargs.get("aux_lr", lr * 0.1),
                                      weight_decay=0.0)
            return _MultiOptimizer([muon, adamw])
        return muon
    if name == "kl_shampoo":
        return KLShampoo(params, lr=lr, **_pick(kwargs, "eps", "alpha"))
    if name == "online_kl_shampoo":
        return OnlineKLShampoo(params, lr=lr,
                                **_pick(kwargs, "eps", "beta1", "beta2", "weight_decay"))
    if name == "custom_block_shampoo":
        return CUSTOM_BlockShampoo(params, lr=lr, momentum=kwargs.get("momentum", 0.9),
                             precondition_freq=kwargs.get("precondition_freq", 10),
                             block_size=kwargs.get("block_size", 196))
    if name == "custom_block_soap":
        return CUSTOM_BlockSOAP(params, lr=lr, betas=kwargs.get("betas", (0.9, 0.999)),
                          shampoo_beta=kwargs.get("shampoo_beta", 0.95),
                          eigenbasis_freq=kwargs.get("eigenbasis_freq", kwargs.get("precondition_freq", 10)),
                          block_size=kwargs.get("block_size", 196))
    if name == "kl_soap":
        return KLSOAP(params, lr=lr, **_pick(kwargs, "eps", "betas", "shampoo_beta", "eigenbasis_freq"))
    if name == "custom_block_kl_soap":
        return CUSTOM_BlockKLSOAP(params, lr=lr,
                            **_pick(kwargs, "eps", "betas", "shampoo_beta", "eigenbasis_freq",
                                    "block_size", "nesterov"))
    if name == "custom_block_muon":
        matrix_params = [p for p in params if p.dim() >= 2]
        other_params = [p for p in params if p.dim() < 2]
        bmuon = CUSTOM_BlockMuon(matrix_params, lr=lr, block_size=kwargs.get("block_size", 32),
                           **_pick(kwargs, "momentum", "ns_steps", "nesterov", "ttnn_device", "ttnn_module"))
        if other_params:
            adamw = torch.optim.AdamW(other_params, lr=kwargs.get("aux_lr", lr * 0.1), weight_decay=0.0)
            return _MultiOptimizer([bmuon, adamw])
        return bmuon
    if name == "custom_block_muon_adaptive":
        matrix_params = [p for p in params if p.dim() >= 2]
        other_params = [p for p in params if p.dim() < 2]
        bmuona = CUSTOM_BlockMuonAdaptive(matrix_params, lr=lr, block_size=kwargs.get("block_size", 32),
                                    **_pick(kwargs, "momentum", "ns_steps", "nesterov", "beta2", "eps",
                                            "ttnn_device", "ttnn_module"))
        if other_params:
            adamw = torch.optim.AdamW(other_params, lr=kwargs.get("aux_lr", lr * 0.1), weight_decay=0.0)
            return _MultiOptimizer([bmuona, adamw])
        return bmuona
    if name == "custom_block_kl_shampoo":
        return CUSTOM_BlockKLShampoo(params, lr=lr, **_pick(kwargs, "eps", "alpha"),
                               block_size=kwargs.get("block_size", 196))
    raise ValueError(f"unknown optimizer {name!r}")


def _pick(kwargs: dict, *keys: str) -> dict:
    """Forward only the kwargs a given optimizer's __init__ actually accepts, so an unrelated
    hyperparameter (e.g. muon's ns_steps passed while running adamw) is silently ignored rather
    than raising, but a *relevant* one (precondition_freq for scalable_shampoo/soap) always reaches
    the optimizer instead of being dropped on the floor -- this is the bug agent-shampoo-1 flagged:
    every non-torch optimizer previously ignored all of its own tunable hyperparameters.
    """
    return {k: kwargs[k] for k in keys if k in kwargs}


class _MultiOptimizer:
    """Thin wrapper so build_optimizer can return one object for Muon's split matrix/1D params."""

    def __init__(self, optimizers: list):
        self._optimizers = optimizers

    def step(self, closure=None):
        loss = None
        for i, opt in enumerate(self._optimizers):
            loss = opt.step(closure if i == 0 else None)
        return loss

    def zero_grad(self, set_to_none: bool = True):
        for opt in self._optimizers:
            opt.zero_grad(set_to_none=set_to_none)
