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
    family approximates. G_t = sum_{s<=t} g_s g_s^T (flattened per-parameter), update = G_t^{-1/2} g_t.
    O(dim^3) per step -- only tractable for the tiny model this experiment targets; do not point
    this optimizer at anything with a flattened-parameter dimension above a few hundred.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 1.0, eps: float = 1e-6):
        super().__init__(params, dict(lr=lr, eps=eps))

    @torch.no_grad()
    def step(self, closure=None):
        loss = closure() if closure is not None else None
        for group in self.param_groups:
            for p in group["params"]:
                if p.grad is None:
                    continue
                g = p.grad.flatten()
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
                    g2 = g.unsqueeze(1)
                else:
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
                if g.dim() == 1:
                    g2 = g.unsqueeze(1)
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
                shampoo_dir = shampoo_dir / (shampoo_dir.norm() + group["eps"])

                state["graft_momentum"].mul_(group["momentum"]).add_(g)
                graft_step = state["graft_momentum"]
                update = shampoo_dir * graft_step.norm()
                p.add_(update, alpha=-group["lr"])
        return loss


class SOAP(Optimizer):
    """SOAP (Vyas et al. 2024, arXiv:2409.11321): run Adam in the (slowly-updated) eigenbasis of
    Shampoo's Kronecker factors, instead of Shampoo's own matrix-power preconditioning. L, R
    accumulate as in Shampoo; their eigenbases Q_L, Q_R are refreshed every
    `eigenbasis_freq` steps; Adam's first/second moments are kept and updated *in that rotated
    basis*, then rotated back for the parameter update. This is SOAP's core claim: Shampoo's
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
                state["L"].mul_(sb).add_(g2 @ g2.T, alpha=1 - sb)
                state["R"].mul_(sb).add_(g2.T @ g2, alpha=1 - sb)
                if t % group["eigenbasis_freq"] == 0 or t == 1:
                    _, state["QL"] = torch.linalg.eigh(state["L"] + group["eps"] * torch.eye(d0, device=g.device, dtype=g.dtype))
                    _, state["QR"] = torch.linalg.eigh(state["R"] + group["eps"] * torch.eye(d1, device=g.device, dtype=g.dtype))
                QL, QR = state["QL"], state["QR"]
                g_rot = QL.T @ g2 @ QR
                state["m"].mul_(b1).add_(g_rot, alpha=1 - b1)
                state["v"].mul_(b2).addcmul_(g_rot, g_rot, value=1 - b2)
                m_hat = state["m"] / (1 - b1 ** t)
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
    """Muon (Jordan et al. 2024). Momentum SGD whose update is orthogonalized via Newton-Schulz
    before the parameter step: m <- mu*m + g; update <- NewtonSchulz(m). Defined for >=2D
    (matrix-shaped) parameters; 1D parameters (biases, norms) fall back to plain AdamW-style
    updates in this experiment's train.py (see build_optimizer), matching the reference recipe of
    "Muon for hidden matrices, AdamW for everything else."
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.02, momentum: float = 0.95,
                 ns_steps: int = 5):
        super().__init__(params, dict(lr=lr, momentum=momentum, ns_steps=ns_steps))

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
                buf.mul_(group["momentum"]).add_(g)
                g2 = buf if g.dim() >= 2 else buf.unsqueeze(1)
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
                if g.dim() == 1:
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L_log" not in state:
                    state["L_log"] = torch.zeros(d0, d0, device=g.device, dtype=g.dtype)
                    state["R_log"] = torch.zeros(d1, d1, device=g.device, dtype=g.dtype)
                state["L_log"] = self._log_euclidean_update(state["L_log"], g2 @ g2.T, group["alpha"], group["eps"])
                state["R_log"] = self._log_euclidean_update(state["R_log"], g2.T @ g2, group["alpha"], group["eps"])
                L = torch.linalg.matrix_exp(state["L_log"])
                R = torch.linalg.matrix_exp(state["R_log"])
                L_inv = _matrix_power(L, -0.25, group["eps"])
                R_inv = _matrix_power(R, -0.25, group["eps"])
                update = (L_inv @ g2 @ R_inv).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


class OnlineKLShampoo(KLShampoo):
    """Online KL Shampoo (Tilde Research, 2026, github.com/tilde-research/online-kl-shampoo-release)
    -- BEST-EFFORT reconstruction, see module docstring.

    Adds two things on top of KLShampoo that the announcement/README describes as the online
    variant's improvement: (1) per-step exponential decay `alpha` scheduled to shrink over training
    (more weight on recent curvature early, more stability later) instead of KLShampoo's fixed
    alpha, matching "online" (streaming, non-stationary-aware) framing; (2) bias-corrected
    accumulation (divide by 1 - (1-alpha)^t) so early steps aren't under-weighted the way a raw
    fixed-alpha EMA is, the same bias-correction idea Adam applies to its raw moment EMAs.
    REPLACE with the official update rule once the release repo is cloned and read (see module
    docstring) -- this is the single highest-value verification task for whichever agent works
    this optimizer.
    """

    def __init__(self, params: Iterable[torch.nn.Parameter], lr: float = 0.1, eps: float = 1e-4,
                 alpha_start: float = 0.2, alpha_end: float = 0.02, alpha_decay_steps: int = 200):
        Optimizer.__init__(self, params, dict(lr=lr, eps=eps, alpha_start=alpha_start,
                                               alpha_end=alpha_end, alpha_decay_steps=alpha_decay_steps))

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
                    g2 = g.unsqueeze(1)
                else:
                    g2 = g.reshape(g.shape[0], -1)
                d0, d1 = g2.shape
                if "L_log" not in state:
                    state["L_log"] = torch.zeros(d0, d0, device=g.device, dtype=g.dtype)
                    state["R_log"] = torch.zeros(d1, d1, device=g.device, dtype=g.dtype)
                    state["step"] = 0
                state["step"] += 1
                t = state["step"]
                frac = min(1.0, t / group["alpha_decay_steps"])
                alpha = group["alpha_start"] + frac * (group["alpha_end"] - group["alpha_start"])
                state["L_log"] = self._log_euclidean_update(state["L_log"], g2 @ g2.T, alpha, group["eps"])
                state["R_log"] = self._log_euclidean_update(state["R_log"], g2.T @ g2, alpha, group["eps"])
                bias_corr = 1 - (1 - alpha) ** t
                L = torch.linalg.matrix_exp(state["L_log"] / bias_corr)
                R = torch.linalg.matrix_exp(state["R_log"] / bias_corr)
                L_inv = _matrix_power(L, -0.25, group["eps"])
                R_inv = _matrix_power(R, -0.25, group["eps"])
                update = (L_inv @ g2 @ R_inv).view_as(g)
                p.add_(update, alpha=-group["lr"])
        return loss


def build_optimizer(name: str, model: torch.nn.Module, lr: float, **kwargs) -> Optimizer:
    """Single entry point train.py uses -- keeps the optimizer choice a config-only (env var)
    change, never a code change, matching experiment-checklist.md item 1's config-only-job rule.
    """
    params = list(model.parameters())
    name = name.lower()
    if name == "sgd":
        return torch.optim.SGD(params, lr=lr, momentum=kwargs.get("momentum", 0.9))
    if name == "adamw":
        return torch.optim.AdamW(params, lr=lr, betas=kwargs.get("betas", (0.9, 0.999)),
                                  weight_decay=kwargs.get("weight_decay", 0.0))
    if name == "full_adagrad":
        return FullMatrixAdaGrad(params, lr=lr)
    if name == "shampoo_2018":
        return Shampoo2018(params, lr=lr)
    if name == "scalable_shampoo":
        return ScalableShampoo(params, lr=lr)
    if name == "soap":
        return SOAP(params, lr=lr)
    if name == "muon":
        # Reference recipe: Muon only for >=2D "hidden" matrices, AdamW for 1D params (biases/norms).
        matrix_params = [p for p in params if p.dim() >= 2]
        other_params = [p for p in params if p.dim() < 2]
        muon = Muon(matrix_params, lr=lr)
        if other_params:
            adamw = torch.optim.AdamW(other_params, lr=kwargs.get("aux_lr", lr * 0.1))
            return _MultiOptimizer([muon, adamw])
        return muon
    if name == "kl_shampoo":
        return KLShampoo(params, lr=lr)
    if name == "online_kl_shampoo":
        return OnlineKLShampoo(params, lr=lr)
    raise ValueError(f"unknown optimizer {name!r}")


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
