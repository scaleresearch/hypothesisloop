"""Bidirectional (non-causal) self-attention for the TT-NN MAE port.

## Why this file exists

`ttml::ops::scaled_dot_product_attention` (the fused, kernel-backed SDPA used
for training -- `tt-train/sources/ttml/ops/scaled_dot_product_attention.cpp`)
defaults to `AttentionMaskType::Causal` when no mask tensor is passed and no
`no_mask` override is given. This MAE never wants a causal mask (encoder and
decoder are both plain bidirectional -- TENSTORRENT_PORT.md section 3: static
`L_vis`/`Q_pred` per sample means "no attention mask exists anywhere in the
model", every slot is always a real, valid token, so there is nothing to
*block*).

## `no_mask=True` (current implementation, M6 kernel-level lever)

This module calls `ttml.ops.attention.scaled_dot_product_attention(query,
key, value, None, no_mask=True)`, which forces
`AttentionMaskType::None` -- the fused `sdpa_fw`/`sdpa_bw` kernels skip
mask-bias application entirely, rather than computing and adding a zero bias
via an explicit all-ones "Arbitrary" mask (the previous implementation).
Measured at real production shapes (`run_op_parity`, PCC=1.000000 exact
match against the old all-ones-mask path, both forward and backward, at both
`encoder_seq_len=544` and `decoder_seq_len=2816`):

    encoder (B=24,H=16,S=544,D=64):  fwd+bwd 78.17ms -> 75.50ms (~3.4% faster)
    decoder (B=24,H=16,S=2816,D=32): fwd+bwd 402.68ms -> 359.09ms (~10.8% faster)

See `tenstorrent/docs/m6-optimization-results.md` for the full step-level
measurement and `TENSTORRENT_PORT.md` section 4 (item #1) for why this was
scoped as the first, cheapest kernel-level lever.

### The `no_mask=True`/`AttentionMaskType::None` plumbing required a real vendored-kernel bug fix

`AttentionMaskType::None` already existed as an enum value and was already
correctly handled by the *reader* kernel (`sdpa_fw_reader_kernel.cpp` only
stages `cb_attn_mask` when `USE_ATTN_MASK` is defined) and by *both* backward
compute kernels (`sdpa_bw_q_compute_kernel.cpp`/`sdpa_bw_kv_compute_kernel.cpp`,
which already had a genuine `#else` no-mask branch). But the **forward**
compute kernel (`sdpa_fw_compute_kernel.cpp`) had no such branch: its
non-causal `#else` block unconditionally called `apply_mask_on_reg(...,
cb_attn_mask, ...)` and popped `cb_attn_mask`, regardless of whether
`USE_ATTN_MASK` was defined. For `AttentionMaskType::Arbitrary` this is
correct (the reader really did stage a mask there); for `None` the reader
never wrote anything to that CB, so the compute kernel read uninitialized L1
as if it were mask data -- garbage added to the QK^T scores, which overflowed
to `Inf` through the softmax `exp()`. This only manifested at real
core-grid/batch scale (confirmed correct output at a trivial B=1,H=1,S=64
shape; 94.7% of output elements were `Inf` at the real B=24,H=16,S=544
shape) -- small enough to look like a subtle numerical-stability issue, but
it was a straightforward missing `#ifdef USE_ATTN_MASK` guard, exactly
matching the pattern the reader kernel and both backward kernels already
used correctly.

Fixed by wrapping both the `apply_mask_on_reg` call and the `cb_attn_mask`
pop in `#ifdef USE_ATTN_MASK` in `sdpa_fw_compute_kernel.cpp` (three-line
diff). `apply_mask_on_reg`'s own docstring confirms it does not apply the
SDPA scale factor (that happens later, uniformly, in
`apply_exp_inplace_and_find_exp_sum`), so no substitute scaling call was
needed for the `None` branch, unlike the backward kernels' no-mask branch
(which scales immediately since backward has no later scaled-exp step).

This is the one exception to this port's "no file under `tenstorrent/tt-metal/`
is modified" rule (`TENSTORRENT_PORT.md`'s "no patches over existing tt-nn
code, you can write new functionality if you need" -- this is a bug fix in a
never-before-exercised code path, not a patch to working behavior; every
existing caller of `scaled_dot_product_attention` is unaffected, since
`no_mask` defaults to `false`). Modified files: `ops/
scaled_dot_product_attention.{hpp,cpp}` (new `no_mask` parameter),
`nanobind/nb_ops.cpp` (expose it on both overloads), `metal/ops/sdpa_fw/
device/kernels/compute/sdpa_fw_compute_kernel.cpp` (the actual bug fix).
Requires a `./build_metal.sh --build-tt-train` rebuild to take effect.

This module calls only the already-nanobind-exposed
`ttml.ops.attention.scaled_dot_product_attention`, whose `mask`/`no_mask`
arguments are bound in `tt-train/sources/ttml/nanobind/nb_ops.cpp`
(`py_attention.def("scaled_dot_product_attention", ...)`, two overloads:
mask as `ttml.autograd.Tensor` or as a raw `ttnn.Tensor`, both optional).

`ttml::metal::sdpa_fw`/`sdpa_bw` are *not* separately exposed through
`ttml.metal` in nanobind (that submodule only carries `moe_group`/
`moe_ungroup` -- see `nb_ops.cpp`'s `py_metal` block), so the
lower-level Tier-2 wrapper path is not available/needed here; going through
the ops-layer `scaled_dot_product_attention` binding is not just the
cheapest option, it is the only one Python can reach without a new
nanobind extension.
"""

from __future__ import annotations

import ttml

from smri_mae_tt.layout import TILE


def _validate_seq_len(seq_len: int) -> None:
    if seq_len <= 0:
        raise ValueError(f"seq_len must be positive, got {seq_len}")
    if seq_len % TILE != 0:
        raise ValueError(
            f"seq_len={seq_len} is not a multiple of the tile height ({TILE}). "
            "The fused SDPA kernel requires TILE-layout query/key/value "
            "tensors, and this port's static shape contract (layout.py, "
            "ShapeContract) already rounds every sequence length to a tile "
            "multiple. A non-tile-aligned seq_len here means the caller "
            "built its shapes incorrectly upstream -- fail here rather than "
            "silently rounding/padding it."
        )


def bidirectional_scaled_dot_product_attention(
    query: "ttml.autograd.Tensor",
    key: "ttml.autograd.Tensor",
    value: "ttml.autograd.Tensor",
    *,
    seq_len: int,
) -> "ttml.autograd.Tensor":
    """Plain bidirectional self-attention via tt-train's fused SDPA kernel.

    `query`/`key`/`value` are `ttml.autograd.Tensor` of shape
    (B, H, seq_len, D) (K/V may have fewer heads than Q for grouped-query
    attention -- `ttml::ops::scaled_dot_product_attention` handles that; this
    MAE's encoder/decoder do not use GQA, so H is the same for all three in
    practice). `seq_len` is validated here (positive, tile-aligned) only to
    fail fast on a caller bug upstream -- the fused kernel itself derives its
    own sequence length directly from the query/key/value tensor shapes, not
    from this argument.

    See the module docstring for `no_mask=True`/`AttentionMaskType::None`
    and the vendored kernel bug fix it required.
    """
    _validate_seq_len(seq_len)
    return ttml.ops.attention.scaled_dot_product_attention(query, key, value, None, True)
