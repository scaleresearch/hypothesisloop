"""M6 candidate: a tuned GELU op using `ttnn.gelu`'s `variant=FastLut` for
the forward pass instead of `ttml.ops.unary.gelu`'s default `variant=
Accurate` (`tt-train/sources/ttml/ops/unary_ops.cpp:46-57`'s `gelu()` calls
`ttnn::gelu(tensor->get_value())` with no explicit variant, which defaults to
`GeluVariant::Accurate` -- FP32 `erf`, matching `torch.nn.functional.gelu`
exactly). Backward is unchanged: `ttnn.experimental.gelu_bw(..., approximate=
"none")`, the exact same call `unary_ops.cpp:51` already makes -- see "Why
backward stays as-is" below for why this isn't a mismatch.

## Why this needed no vendored-code change at all

Unlike every other op investigated in the "non-SDPA/matmul op survey"
session (`tenstorrent/docs/m6-optimization-results.md`'s new section), GELU's
`variant`/`approximate` knobs are **already exposed at the raw `ttnn` Python
layer** (`help(ttnn.gelu)`, `help(ttnn.experimental.gelu_bw)` on the built
wheel) -- `ttml.ops.unary.gelu`'s C++ wrapper (`unary_ops.cpp`) just never
passes a non-default value through. So the entire fix is a pure-Python
`ttml.autograd.Function` wrapping the already-shipped, already-tested raw
`ttnn` ops with an explicit `variant` argument -- no `tenstorrent/tt-metal`
file is touched, no rebuild is needed. A since-reverted `ops_tt/tuned_linear.py`
tried the analogous "Python-level knob, not a vendored kernel edit" approach
for matmul (real ~2% e2e win, but reverted 2026-08-05 for its added
complexity -- see `TENSTORRENT_PORT.md` section 5); this GELU op measures
as a comparably real win with far less machinery (no cache file, no sweep
script), which is why it stayed.

## Measured: FastLut forward is a real ~1.7-1.8x speedup, PCC comfortably
## clears the forward gate

Isolated timing (`ttnn.synchronize_device` around each call, tensors built
once, same idiom as `sdpa_bw_isolated_bench.py`), real production MLP hidden
shapes (`hidden = dim * mlp_ratio`, `Mlp.__init__`):

```
encoder [24,544,4096]:  Accurate=1.0705ms  FastLut=0.6086ms  (1.76x faster)
decoder [24,2816,2048]: Accurate=2.6620ms  FastLut=1.5098ms  (1.76x faster)
```

(`Tanh` variant was also measured and is *slower* than `Accurate` at both
scales -- 1.5585ms / 3.9406ms -- not used here.)

Correctness (isolated, real production shape, `bfloat16` round-trip,
`torch.nn.functional.gelu(approximate="none")` as ground truth, realistic
activation scale `N(0,1)*2.0`):

```
PCC(Accurate, torch-ref) = 0.99999683   max_abs_err = 0.0312
PCC(FastLut,  torch-ref) = 0.99997677   max_abs_err = 0.0312   (same max err -- both bf16-rounded)
PCC(FastLut,  Accurate)  = 0.99997744   max_abs_err = 0.0239
```

`FastLut`'s error against the exact reference (0.99997677) sits comfortably
above this project's forward PCC gate (`OP_FWD_PCC_GATE = 0.999`,
`perf_parity.py`) -- roughly two orders of magnitude of headroom in
`1 - PCC` terms (0.0000232 vs the 0.001 gate). `test_tuned_gelu.py`'s
`test_run_op_parity_passes_for_tuned_gelu` makes this a standing,
gate-enforced check via the project's standard `run_op_parity` harness
(comparing the full `TunedGeluFunction` forward+backward against
`ttml.ops.unary.gelu`'s forward+backward at real encoder/decoder shapes), not
just this module's ad hoc isolated numbers.

## Why backward stays as-is (not a forward/backward formula mismatch)

`gelu_bw`'s `approximate` argument only takes `"none"`/`"tanh"` -- there is
no LUT option for backward (`help(ttnn.experimental.gelu_bw)` lists exactly
those two strings), so there is nothing to swap there for a speed win; `"none"`
(the exact analytic derivative, already what `ttml.ops.unary.gelu` uses) is
already the faster of the two measured (`"none"`: 1.584ms/4.044ms vs
`"tanh"`: 3.470ms/8.900ms at encoder/decoder scale -- `"tanh"` backward is
markedly *slower*, not a speed lever at all here).

This does mean forward (`FastLut`, a 6-segment piecewise-linear
approximation) and backward (`"none"`, the exact analytic `erf`-based
derivative) are technically computing gradients for two slightly different
functions -- the backward pass differentiates accurate GELU, not the
piecewise-linear function forward actually evaluates. This is deliberate,
not an oversight: `FastLut`'s own docs describe it as "~1% absolute error,"
i.e. a fast *approximation* of accurate GELU meant to be used as a drop-in
for accurate GELU, not a distinct function with its own exact derivative:
using the exact GELU derivative as the gradient is the intended/standard
pairing (the same pattern PyTorch/JAX fused-GELU-approximation kernels use
elsewhere), not a novel or unverified choice -- and it is empirically
verified end-to-end (not just argued) by `test_run_op_parity_passes_for_
tuned_gelu`'s backward PCC check (gated at `OP_BWD_PCC_GATE = 0.99`, the same
standing gate every other op-level change in this project is held to).

## Scope

Opt-in flag (`use_tuned_gelu`, default `True` in `train()`) -- `Mlp.forward`
swaps `ttml.ops.unary.gelu` for `tuned_gelu` when set. No shape restriction
beyond what `ttnn.gelu`/`gelu_bw` themselves require (any tile-aligned
tensor `Mlp` already produces qualifies -- no grid/program-config
divisibility constraint to fail fast on).
"""

from __future__ import annotations

import ttml
import ttnn

from ttml.autograd import Function

# ---------------------------------------------------------------------------
# The Tier-1 custom op: raw ttnn.gelu(variant=FastLut) forward, raw
# ttnn.experimental.gelu_bw(approximate="none") backward -- the exact same
# backward call ttml.ops.unary.gelu already makes, see module docstring's
# "Why backward stays as-is".
# ---------------------------------------------------------------------------


class TunedGeluFunction(Function):
    """`y = gelu(x)`, forward via `ttnn.gelu(..., variant=ttnn.GeluVariant.
    FastLut)`, backward via `ttnn.experimental.gelu_bw(..., approximate=
    "none")` -- see module docstring for why forward/backward use different
    approximation modes and why that's the intended, PCC-gate-verified
    pairing, not a mismatch.
    """

    @staticmethod
    def forward(ctx, x: "ttml.autograd.Tensor"):
        x_val = x.get_value()
        out = ttnn.gelu(x_val, variant=ttnn.GeluVariant.FastLut)
        ctx.save_for_backward(x)
        return out

    @staticmethod
    def backward(ctx, grad_output):
        (x,) = ctx.saved_tensors
        x_val = x.get_value()
        dx = ttnn.experimental.gelu_bw(grad_output, x_val, approximate="none")
        return dx


def tuned_gelu(x: "ttml.autograd.Tensor") -> "ttml.autograd.Tensor":
    """Functional entry point -- see `TunedGeluFunction`. Drop-in
    replacement for `ttml.ops.unary.gelu(x)`."""
    return TunedGeluFunction.apply(x)
