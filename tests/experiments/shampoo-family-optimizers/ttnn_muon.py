"""Real on-device (ttnn) Muon optimizer step: Newton-Schulz quintic orthogonalization run via
ttnn.matmul on the real Tenstorrent device, not just forward/backward.

Scope, per Codex review (2026-09-11): bf16 is fine for the 5 matmul-heavy Newton-Schulz
iterations, but the Frobenius-norm reduction that normalizes the input must stay fp32 (bf16
underflow/overflow risk on the norm itself, not the matmuls). The iteration converges the
matrix's singular values toward ~0.87, not exactly 1 -- do not assert ||X@X.T - I|| ~= 0 when
validating; compare relative error / singular values against the CPU torch reference instead.

Everything else about Muon's step (momentum buffer update, the final scale-by-lr, AdamW
fallback for 1D params) still runs on CPU exactly as optimizers.py's Muon class does -- this
module only replaces the _newton_schulz() matmul chain with an on-device equivalent.
"""

import torch


def newton_schulz_ttnn(g: torch.Tensor, device, ttnn, steps: int = 5) -> torch.Tensor:
    """On-device equivalent of optimizers.py's _newton_schulz. g is a 2D matrix on CPU (fp32);
    returns a CPU fp32 tensor. Norm computed in fp32 on the host (cheap, avoids bf16 underflow);
    the steps matmul iterations run via ttnn.matmul on the real device.
    """
    a, b, c = 3.4445, -4.7750, 2.0315
    g = g.float()
    x = g / (g.norm() + 1e-7)

    transposed = False
    if x.shape[0] > x.shape[1]:
        x = x.T.contiguous()
        transposed = True

    x_tt = ttnn.from_torch(x, device=device, layout=ttnn.TILE_LAYOUT, dtype=ttnn.bfloat16)
    for _ in range(steps):
        xt_tt = ttnn.transpose(x_tt, -2, -1)
        a_mat = ttnn.matmul(x_tt, xt_tt)
        a2_mat = ttnn.matmul(a_mat, a_mat)
        b_mat = ttnn.add(ttnn.mul(a_mat, b), ttnn.mul(a2_mat, c))
        bx = ttnn.matmul(b_mat, x_tt)
        x_tt = ttnn.add(ttnn.mul(x_tt, a), bx)

    x = ttnn.to_torch(x_tt).to(torch.float32)
    if transposed:
        x = x.T
    return x.to(g.dtype)
