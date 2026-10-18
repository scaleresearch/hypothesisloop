"""Real on-device (ttnn) linear layer, wired into torch autograd via a custom Function.

Scope, per Codex review (2026-09-11): tt-metalium's release image (our only confirmed-working
ttnn base, see dummy-tt-matmul/seed/Dockerfile.train) has no TT-Train/TTML, so there is no
on-device autograd. Full "train entirely on TT" is not achievable with what's actually installed.
This module is the honestly-scoped alternative Codex approved: a torch.autograd.Function whose
forward AND backward matmuls genuinely run via ttnn.matmul on the real device (not a CPU
fallback), round-tripping tensors CPU<->TT explicitly. Codex's verdict on this pattern: real
device involvement, but for matrices this small (max dim 784) transfer/dispatch overhead will
dominate -- do not claim this is a training speedup. It is an on-device-execution correctness/
integration proof, not an acceleration result. Report FLOPs/step results from this path
separately from the CPU benchmark leaderboard; do not merge them into ranking comparisons.
"""

import torch


def _to_ttnn(x, device, ttnn):
    return ttnn.from_torch(x, device=device, layout=ttnn.TILE_LAYOUT, dtype=ttnn.bfloat16)


def _to_torch(x, ttnn):
    return ttnn.to_torch(x)


class TTLinearFunction(torch.autograd.Function):
    """y = x @ W.T + b, with the two matmuls (fwd) and two matmuls (bwd, for grad_x/grad_W)
    executed on-device via ttnn.matmul. Kept in fp32 on the torch side of the boundary; only
    the ttnn tensors are bf16 (tile layout requirement), so this trades some precision for a
    real device round-trip -- acceptable for an integration proof, not for the ranking leaderboard.
    """

    @staticmethod
    def forward(ctx, x, weight, bias, device):
        import ttnn

        ctx.save_for_backward(x, weight, bias)
        ctx.device = device

        x_tt = _to_ttnn(x, device, ttnn)
        w_tt = _to_ttnn(weight.t().contiguous(), device, ttnn)
        y_tt = ttnn.matmul(x_tt, w_tt)
        y = _to_torch(y_tt, ttnn).to(torch.float32)
        return y + bias

    @staticmethod
    def backward(ctx, grad_output):
        import ttnn

        x, weight, bias = ctx.saved_tensors
        device = ctx.device
        go_tt = _to_ttnn(grad_output.contiguous(), device, ttnn)

        w_tt = _to_ttnn(weight, device, ttnn)
        grad_x_tt = ttnn.matmul(go_tt, w_tt)
        grad_x = _to_torch(grad_x_tt, ttnn).to(torch.float32)

        # grad_w = go.T @ x: (out,batch)@(batch,in) = (out,in), matching weight's shape.
        x_tt = _to_ttnn(x.contiguous(), device, ttnn)
        grad_w_tt = ttnn.matmul(ttnn.transpose(go_tt, -2, -1), x_tt)
        grad_w = _to_torch(grad_w_tt, ttnn).to(torch.float32)

        grad_bias = grad_output.sum(dim=0)
        return grad_x, grad_w, grad_bias, None


class TTLinear(torch.nn.Module):
    """Drop-in for nn.Linear whose matmuls run on a ttnn device via TTLinearFunction."""

    def __init__(self, in_features, out_features, device):
        super().__init__()
        self.weight = torch.nn.Parameter(torch.empty(out_features, in_features))
        self.bias = torch.nn.Parameter(torch.zeros(out_features))
        torch.nn.init.kaiming_uniform_(self.weight, a=5 ** 0.5)
        self.device = device

    def forward(self, x):
        return TTLinearFunction.apply(x, self.weight, self.bias, self.device)
