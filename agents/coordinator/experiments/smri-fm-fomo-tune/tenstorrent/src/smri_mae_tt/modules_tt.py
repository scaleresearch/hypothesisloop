"""Transformer building blocks for the TT-NN MAE port: encoder/decoder blocks,
attention, MLP, and the fixed 3D sin/cos position embedding, composed from
`ttml.autograd`-bound ops (Tier 1 in TENSTORRENT_PORT.md section 4) plus the
`ops_tt.concat` helpers this module needs for cls-token/mask-token
concatenation.

Ports (structurally, not line-for-line) `src/smri_mae/modules.py`'s
`Attention`/`Mlp`/`Block`/`SinCosPosEmbed3D`/`apply_pos_embed` and
`src/smri_mae/model_mae.py`'s `MaskedEncoder`/`MaskedDecoder`, minus all the
jagged-batch/token-mask/padding machinery documented as gone in
TENSTORRENT_PORT.md section 3 -- every sample on this port always
contributes exactly `contract.l_vis` visible and `contract.q_pred`
prediction patches (`layout.ShapeContract`, `packing.PackedBatch`), so there
is no attention mask, no `JaggedBatch`, and no `unpack_tokens` anywhere
here.

## Tensor layout convention

Every activation tensor in this module is 4D, `(B, 1, S, D)` -- batch, a
singleton "head" axis, sequence, feature. This is not a stylistic choice:
it is the shape `ttml.ops.multi_head_utils.heads_creation` requires of its
QKV input (`ops/multi_head_utils.cpp`: "qkv shape is (B, 1, S, E * 3)"),
`ttml.ops.embedding.embedding` produces (`ops/embedding_op.cpp` reshapes its
output to `(batch, 1, sentence, dim)`), and the idiomatic tt-train Python
model code already uses (`ttml/models/nanogpt/gpt_block.py`'s `GPTBlock`,
`ttml/models/nanogpt/multi_head_attention.py`'s `MultiHeadAttention`) --
matching it means every op in this file composes directly with
`ttml.ops.linear.linear`/`layernorm`/`embedding`/`multi_head_utils.*`
without extra reshapes at each call site.

## Empirical note on the static-shape contract (see TENSTORRENT_PORT.md /
`layout.ShapeContract.__post_init__`)

`layout.py`'s `ShapeContract` validates that `encoder_seq_len = l_vis + 1`
and `decoder_seq_len = l_vis + q_pred + 1` are tile-aligned (multiples of
32), but *not* `l_vis`/`q_pred` individually. Whether that is sufficient --
i.e. whether `ttnn.embedding`/`ttml.ops.linear.linear`/
`ttml.ops.layernorm.layernorm` require every intermediate sequence length to
itself be tile-aligned, or transparently pad -- was an open question this
module resolves by measurement, not assumption:

    B, L_vis, patch_dim, D = 2, 1023, 512, 1024  # L_vis deliberately NOT a
                                                   # multiple of 32
    x = ttml.autograd.Tensor.from_numpy(..., layout=ttnn.Layout.TILE, ...)
    y = ttml.ops.linear.linear(x, w, b)       # -> shape [2, 1, 1023, 1024], no error
    z = ttml.ops.layernorm.layernorm(y, gamma, beta)  # -> shape [2, 1, 1023, 1024], no error
    g = ttml.ops.embedding.embedding(ids, table)      # -> shape [2, 1, 1023, 1024], no error

All three ran cleanly on real Blackhole hardware at `L_vis=1023` (not a
multiple of 32). **Result: `ttnn` pads non-tile-aligned intermediate shapes
transparently for `linear`/`layernorm`/`embedding`.** Only the fused SDPA
kernel actually requires its `S` to be tile-aligned (already enforced by
`ops_tt/attention.py`'s `_validate_seq_len`, called on exactly
`ShapeContract.encoder_seq_len`/`decoder_seq_len`). This confirms
`layout.py`'s existing validation (only on `encoder_seq_len`/
`decoder_seq_len`, not `l_vis`/`q_pred` individually) is correct and
sufficient as written -- `layout.py` is intentionally left unmodified by
this change. It also means the cls token can be concatenated onto the
patch-embedding sequence *after* `patch_embed`/`pos_embed` run (exactly
where the PyTorch reference does it, `MaskedEncoder.forward_patch_embeds`'s
`cat_tokens`), rather than needing to be folded into a single tile-aligned
gather from the first op, because the only op that actually requires tile
alignment is the one running on the already-cls-concatenated sequence.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np
import ttml
import ttnn

from smri_mae_tt.layout import ShapeContract
from smri_mae_tt.ops_tt.attention import bidirectional_scaled_dot_product_attention
from smri_mae_tt.ops_tt.concat import autograd_concat, autograd_slice
from smri_mae_tt.ops_tt.tuned_gelu import tuned_gelu

from ttml.modules import AbstractModuleBase, Buffer, LinearLayer, ModuleList, Parameter

# ---------------------------------------------------------------------------
# Fixed 3D sin/cos position embedding (numpy, deterministic) -- verbatim port
# of src/smri_mae/modules.py:330-378 (`get_3d_sincos_pos_embed` /
# `get_1d_sincos_pos_embed_from_grid`). No torch, no ttml: this is exactly
# the host-side table computation the reference does with numpy already
# (`SinCosPosEmbed3D.__init__` calls it directly, before ever touching
# torch), we just build a `ttml.autograd.Tensor` constant from the resulting
# table instead of a `torch.Tensor`.
# ---------------------------------------------------------------------------


def _get_1d_sincos_pos_embed_from_grid(embed_dim: int, pos: np.ndarray) -> np.ndarray:
    """Port of `modules.py:360-378`. `pos`: positions of shape (M,) (or
    broadcastable to it). Returns (M, embed_dim)."""
    if embed_dim % 2 != 0:
        raise ValueError(f"embed_dim must be even, got {embed_dim}")
    omega = np.arange(embed_dim // 2, dtype=np.float64)
    omega /= embed_dim / 2.0
    omega = 1.0 / 10000**omega  # (D/2,)

    pos = pos.reshape(-1).astype(np.float64)  # (M,)
    out = np.einsum("m,d->md", pos, omega)  # (M, D/2)

    emb_sin = np.sin(out)
    emb_cos = np.cos(out)
    return np.concatenate([emb_sin, emb_cos], axis=1)  # (M, D)


def sincos_pos_embed_3d(embed_dim: int, grid_size: tuple[int, int, int]) -> np.ndarray:
    """Port of `modules.py:330-357` (`get_3d_sincos_pos_embed`), specialized
    to `uniform_power=True, cls_token=False` -- the only combination this
    project's config ever uses: `SinCosPosEmbed3D` (the only `pos_embed`
    variant `default_pretrain.yaml` selects, `pos_embed: sincos`) always
    calls the reference with `uniform_power=True`, and always prepends the
    cls token itself (`MaskedEncoder.cat_tokens`/`MaskedDecoder.cat_tokens`)
    rather than asking the pos-embed table to reserve a row for it -- so the
    `uniform_power=False` and `cls_token=True` branches of the reference
    function are dead code for this project and are not ported.

    `grid_size` is `(grid_depth, grid_height, grid_width)` == `T, H, W`
    patch-grid extents, matching `ShapeContract.grid_size` and the
    `(t h w)` patch flattening order `patchify3d` uses
    (`modules.py:218`'s `rearrange(..., "b c (t u) (h p) (w q) -> b (t h w) (c u p q)")`).

    Returns an `[T*H*W, embed_dim]` float32 table, row `i` giving the
    position embedding for patch index `i` in that same `(t h w)` flatten
    order (verified: `np.meshgrid(..., indexing="ij")` over
    `(T, H, W)`-shaped index grids flattens in exactly that order under
    `.reshape(-1)`, matching the reference).
    """
    grid_depth, grid_h, grid_w = grid_size
    grid_d_idx = np.arange(grid_depth, dtype=np.float64)
    grid_h_idx = np.arange(grid_h, dtype=np.float64)
    grid_w_idx = np.arange(grid_w, dtype=np.float64)
    grid_d_m, grid_h_m, grid_w_m = np.meshgrid(grid_d_idx, grid_h_idx, grid_w_idx, indexing="ij")

    # uniform_power=True branch of the reference.
    h_embed_dim = w_embed_dim = d_embed_dim = int(math.ceil(embed_dim / 6) * 2)

    emb_h = _get_1d_sincos_pos_embed_from_grid(h_embed_dim, grid_h_m)
    emb_w = _get_1d_sincos_pos_embed_from_grid(w_embed_dim, grid_w_m)
    emb_d = _get_1d_sincos_pos_embed_from_grid(d_embed_dim, grid_d_m)
    pos_embed = np.concatenate([emb_d, emb_h, emb_w], axis=1)
    pos_embed = pos_embed[:, :embed_dim]
    return pos_embed.astype(np.float32)


def build_pos_embed_table(
    embed_dim: int,
    grid_size: tuple[int, int, int],
    dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16,
) -> "ttml.autograd.Tensor":
    """Compute the fixed sincos table (`sincos_pos_embed_3d`) once and wrap
    it as a `[1, 1, num_patches, embed_dim]` device constant, ready for
    `ttml.ops.embedding.embedding` gathers (`apply_pos_embed` below).

    Not wrapped in `ttml.modules.Parameter`/registered as trainable: the
    reference builds this table with `requires_grad=False`
    (`modules.py`'s `SinCosPosEmbed3D.__init__`:
    `nn.Parameter(torch.from_numpy(weight).float(), requires_grad=False)`).
    Callers should hold the returned tensor in a `ttml.modules.Buffer` (not
    a `Parameter`) when storing it on an `AbstractModuleBase`, so
    `AbstractModuleBase.__setattr__`'s auto-registration does not register
    it with the optimizer (see `module_base.py`: `Buffer` values are kept in
    a private `_buffers` dict via `_bind_buffer`, never `register_tensor`).
    """
    table_np = sincos_pos_embed_3d(embed_dim, grid_size)  # [N, D]
    n, d = table_np.shape
    return ttml.autograd.Tensor.from_numpy(
        table_np.reshape(1, 1, n, d), layout=ttnn.Layout.TILE, new_type=dtype
    )


def apply_pos_embed(
    x: "ttml.autograd.Tensor",
    table: "ttml.autograd.Tensor",
    ids: np.ndarray,
) -> "ttml.autograd.Tensor":
    """Gather rows `ids` from the `[1, 1, N, D]` position table and add them
    to `x`. Mirrors `modules.py:381-391`'s `apply_pos_embed`
    (`weight.gather(1, pos_ids...); x = x + weight`), specialized to the
    case this port always has real `pos_ids` (`ShapeContract`'s static
    `visible_ids`/`pred_ids` -- there is no "no pos_ids, add the whole table
    broadcast over B" branch here because every call site on this port
    already knows which patch indices it wants).

    `ids`: host numpy array, shape `[B, L]`, any integer dtype (cast to
    `uint32`, the dtype `ttml.ops.embedding.embedding` expects -- see
    `tests/python/test_embedding.py`). `x`: `[B, 1, L, D]`.
    """
    if ids.ndim != 2:
        raise ValueError(f"pos-embed ids must be [B, L], got shape {ids.shape}")
    batch, seq_len = ids.shape
    ids_np = np.ascontiguousarray(ids).astype(np.uint32).reshape(batch, 1, 1, seq_len)
    ids_tensor = ttml.autograd.Tensor.from_numpy(ids_np, layout=ttnn.Layout.ROW_MAJOR, new_type=ttnn.DataType.UINT32)
    gathered = ttml.ops.embedding.embedding(ids_tensor, table)
    return ttml.ops.binary.add(x, gathered)


# ---------------------------------------------------------------------------
# Small host-side helpers: constant/parameter tensor construction, batch
# broadcast (via a broadcasting add -- see below for why), and sequence
# slicing.
# ---------------------------------------------------------------------------


def _const_tensor(arr: np.ndarray, dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16) -> "ttml.autograd.Tensor":
    """Build a `ttml.autograd.Tensor` constant from a numpy array.

    `ttml.autograd.Tensor.from_numpy(..., new_type=ttnn.DataType.BFLOAT8_B)`
    is not supported for any source numpy dtype -- confirmed both
    empirically (`TypeError: Unsupported type: BFLOAT8_B` from
    `nanobind/nb_util.cpp`) and in-repo: `tt-train/tests/python/
    test_autograd.py`'s `unsupported_format_cases` lists every
    `(numpy_dtype, ttnn.DataType.BFLOAT8_B, layout)` combination (float32,
    int32, uint32, bfloat16 sources; `None`/`ROW_MAJOR`/`TILE` layouts) as
    expected-to-raise. The working path (also confirmed empirically) is:
    build a BFLOAT16 tile tensor via `from_numpy` as usual, then
    `ttnn.typecast` its raw `ttnn.Tensor` value to `BFLOAT8_B`, then
    re-wrap with `ttml.autograd.create_tensor(..., requires_grad=True)` --
    verified end-to-end through `ttml.ops.linear.linear` forward+backward
    with a `BFLOAT8_B` weight on real hardware.
    """
    bf16 = ttml.autograd.Tensor.from_numpy(arr.astype(np.float32), layout=ttnn.Layout.TILE, new_type=ttnn.DataType.BFLOAT16)
    if dtype == ttnn.DataType.BFLOAT16:
        return bf16
    casted = ttnn.typecast(bf16.get_value(), dtype)
    return ttml.autograd.create_tensor(casted, requires_grad=True)


def _const_init(arr: np.ndarray, dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16):
    """A `weight_init`/`bias_init` callable (see `ttml.modules.LinearLayer`)
    that always returns the same fixed numpy array, cast to `dtype`.

    This is the seam a future `params.py` checkpoint loader hooks into: a
    torch `state_dict` tensor reshaped/transposed to this port's `(1, 1,
    out, in)` weight-shape convention and handed to `LinearLayer(...,
    weight_init=_const_init(that_array))` assigns it directly, no different
    from how this file uses it for from-scratch numpy init.

    Fails fast on a shape mismatch between the array and what the layer
    asks for, rather than reshaping/broadcasting it into place -- a mismatch
    here means the init table and the layer's declared dims disagree, which
    is a bug to surface immediately, not paper over.
    """

    def init_fn(shape, mapper=None):
        if tuple(shape) != tuple(arr.shape):
            raise ValueError(f"parameter shape mismatch: layer requested {tuple(shape)}, init array is {tuple(arr.shape)}")
        if mapper is not None:
            raise NotImplementedError(
                "mesh sharding mappers are not supported by this port's constant "
                "initializer yet (TENSTORRENT_PORT.md section 5 targets a 1x1 mesh "
                "on this development box; multi-card sharded init is out of scope "
                "for modules_tt.py)."
            )
        return _const_tensor(arr, dtype)

    return init_fn


def _broadcast_batch(
    tensor: "ttml.autograd.Tensor",
    batch: int,
    seq_len: int = 1,
    dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16,
) -> "ttml.autograd.Tensor":
    """Broadcast a `[1, 1, 1, D]` parameter (cls token, mask token, ...) to
    `[batch, 1, seq_len, D]`, with a correct backward.

    Reference does this with `Tensor.expand` (`modules.py`'s
    `cls_token.expand(B, -1, -1)`, `mask_token.expand(B, Q, -1)`), which has
    no `ttnn`/`ttml` equivalent exposed to Python. Instead of writing a
    dedicated broadcast `Function` (which would just reinvent gradient
    reduce-summation over the broadcast dims), this uses
    `ttml.ops.binary.add` against an explicit `[batch, 1, seq_len, D]` zero
    tensor: `ttml.ops.binary.add` already broadcasts mismatched dims of size
    1 (confirmed empirically: `add([B,1,1,D]_zeros, [1,1,1,D]_param)` ->
    `[B,1,1,D]`, and *also* over two simultaneous broadcast dims,
    `add([B,1,Q,D]_zeros, [1,1,1,D]_param)` -> `[B,1,Q,D]`, both on real
    hardware) and its backward correctly reduce-sums the upstream gradient
    back down to the parameter's original `[1,1,1,D]` shape (verified: the
    `[1,1,1,D]` operand's accumulated gradient after `add`+`mean`+`backward`
    has shape `(1,1,1,D)`, not `(B,1,Q,D)`) -- exactly the semantics a
    `Tensor.expand`+backward would have, achieved with an op already proven
    to exist and be correct (`ttml.ops.binary.add`) rather than a new
    hand-written `Function`.
    """
    dim = tensor.shape()[-1]
    zeros_np = np.zeros((batch, 1, seq_len, dim), dtype=np.float32)
    zeros = ttml.autograd.Tensor.from_numpy(zeros_np, layout=ttnn.Layout.TILE, new_type=dtype)
    return ttml.ops.binary.add(zeros, tensor)


def _slice_seq(x: "ttml.autograd.Tensor", start_seq: int, length: int) -> "ttml.autograd.Tensor":
    """Slice `x[:, :, start_seq:start_seq+length, :]` with a working backward
    (`ops_tt.concat.autograd_slice`)."""
    shape = x.shape()
    start = [0, 0, start_seq, 0]
    end = [shape[0], shape[1], start_seq + length, shape[3]]
    return autograd_slice(x, start, end)


def _values_to_tensor(values: np.ndarray, dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16) -> "ttml.autograd.Tensor":
    """`[B, L, P]` host numpy patch values -> `[B, 1, L, P]` device tensor."""
    if values.ndim != 3:
        raise ValueError(f"expected [B, L, P] patch values, got shape {values.shape}")
    batch, seq_len, dim = values.shape
    return ttml.autograd.Tensor.from_numpy(
        values.astype(np.float32).reshape(batch, 1, seq_len, dim), layout=ttnn.Layout.TILE, new_type=dtype
    )


# ---------------------------------------------------------------------------
# Numpy weight initialization -- deterministic, seeded, no torch/ttml.
#
# Matches `model_mae.py`'s `_init_weights` (Xavier-uniform for every Linear
# weight, zero bias, LayerNorm weight=1/bias=0, applied globally via
# `self.apply(_init_weights)`) plus the per-module `reset_parameters` calls
# for the learned tokens (`trunc_normal_(std=0.02)` for
# `cls_token`/`cls_token_pos`/`mask_token`, *except* the decoder's
# `cls_token`, which `MaskedDecoder.reset_parameters` initializes to zeros
# -- "official mae initializes decoder cls token to zeros" -- ported here
# verbatim as the same asymmetry, not smoothed over).
# ---------------------------------------------------------------------------


@dataclass
class LinearParams:
    weight: np.ndarray  # (1, 1, out_features, in_features)
    bias: np.ndarray  # (1, 1, 1, out_features)


@dataclass
class LayerNormParams:
    gamma: np.ndarray  # (1, 1, 1, dim)
    beta: np.ndarray  # (1, 1, 1, dim)


@dataclass
class BlockParams:
    ln1: LayerNormParams
    qkv: LinearParams
    proj: LinearParams
    ln2: LayerNormParams
    fc1: LinearParams
    fc2: LinearParams


@dataclass
class EncoderParams:
    patch_embed: LinearParams
    cls_token: np.ndarray  # (1, 1, 1, D), trunc_normal(std=0.02)
    cls_token_pos: np.ndarray  # (1, 1, 1, D), trunc_normal(std=0.02)
    blocks: list[BlockParams] = field(default_factory=list)
    norm: LayerNormParams = None


@dataclass
class DecoderParams:
    proj: LinearParams
    cls_token: np.ndarray  # (1, 1, 1, D), ZEROS -- see module docstring above
    cls_token_pos: np.ndarray  # (1, 1, 1, D), trunc_normal(std=0.02)
    mask_token: np.ndarray  # (1, 1, 1, D), trunc_normal(std=0.02)
    head: LinearParams
    blocks: list[BlockParams] = field(default_factory=list)
    norm: LayerNormParams = None


def _xavier_uniform_linear(rng: np.random.Generator, out_features: int, in_features: int) -> LinearParams:
    """`nn.init.xavier_uniform_` for the weight (bound = sqrt(6/(fan_in+fan_out)),
    gain=1, matching the reference's default-gain call) + zero bias, matching
    `model_mae.py::_init_weights`'s `nn.Linear` branch."""
    limit = math.sqrt(6.0 / (in_features + out_features))
    weight = rng.uniform(-limit, limit, size=(1, 1, out_features, in_features)).astype(np.float32)
    bias = np.zeros((1, 1, 1, out_features), dtype=np.float32)
    return LinearParams(weight=weight, bias=bias)


def _layernorm_params(dim: int) -> LayerNormParams:
    """weight=1, bias=0, matching `_init_weights`'s `nn.LayerNorm` branch."""
    return LayerNormParams(gamma=np.ones((1, 1, 1, dim), dtype=np.float32), beta=np.zeros((1, 1, 1, dim), dtype=np.float32))


def _trunc_normal(rng: np.random.Generator, shape: tuple[int, ...], std: float = 0.02, a: float = -2.0, b: float = 2.0) -> np.ndarray:
    """Truncated-normal sample via rejection sampling, matching the support
    (`[a*std, b*std]`, default `a=-2, b=2`) of `torch.nn.init.trunc_normal_`
    (the reference's `nn.init.trunc_normal_(p, std=0.02)` uses those same
    defaults). Not bit-identical to PyTorch's erfinv-based sampler -- this
    module's parity test loads its own generated numpy arrays directly into
    both the TT and the PyTorch reference model's state, so the two never
    need to agree on *how* a value was sampled, only on what the resulting
    array is.
    """
    lo, hi = a * std, b * std
    size = int(np.prod(shape))
    out = np.empty(size, dtype=np.float64)
    filled = 0
    while filled < size:
        batch = rng.normal(0.0, std, size=size - filled)
        accepted = batch[(batch >= lo) & (batch <= hi)]
        n = min(accepted.shape[0], size - filled)
        out[filled : filled + n] = accepted[:n]
        filled += n
    return out.reshape(shape).astype(np.float32)


def init_block_params(rng: np.random.Generator, dim: int, mlp_ratio: float = 4.0) -> BlockParams:
    """`qkv_bias=True, proj_bias=True` unconditionally: this project's
    config never overrides `MaskedAutoencoderViT`'s defaults
    (`default_pretrain.yaml`'s `model_kwargs` sets neither), so the
    `bias=False` code path is not built here."""
    hidden = int(dim * mlp_ratio)
    return BlockParams(
        ln1=_layernorm_params(dim),
        qkv=_xavier_uniform_linear(rng, 3 * dim, dim),
        proj=_xavier_uniform_linear(rng, dim, dim),
        ln2=_layernorm_params(dim),
        fc1=_xavier_uniform_linear(rng, hidden, dim),
        fc2=_xavier_uniform_linear(rng, dim, hidden),
    )


def init_encoder_params(rng: np.random.Generator, contract: ShapeContract, depth: int, mlp_ratio: float = 4.0) -> EncoderParams:
    dim = contract.embed_dim
    return EncoderParams(
        patch_embed=_xavier_uniform_linear(rng, dim, contract.patch_dim),
        cls_token=_trunc_normal(rng, (1, 1, 1, dim), std=0.02),
        cls_token_pos=_trunc_normal(rng, (1, 1, 1, dim), std=0.02),
        blocks=[init_block_params(rng, dim, mlp_ratio) for _ in range(depth)],
        norm=_layernorm_params(dim),
    )


def init_decoder_params(rng: np.random.Generator, contract: ShapeContract, depth: int, mlp_ratio: float = 4.0) -> DecoderParams:
    dim = contract.decoder_embed_dim
    input_dim = contract.embed_dim
    if input_dim == dim:
        # Reference: `self.proj = nn.Identity() if input_dim == embed_dim else nn.Linear(...)`.
        # This project's config always has embed_dim=1024 != decoder_embed_dim=512
        # (mae_vit_large), so the identity-proj branch never triggers and is not
        # implemented here -- see `Decoder.__init__`'s matching check.
        raise NotImplementedError(
            "decoder_embed_dim == embed_dim (identity decoder input projection) is "
            "not used by this project's config and is not implemented"
        )
    return DecoderParams(
        proj=_xavier_uniform_linear(rng, dim, input_dim),
        cls_token=np.zeros((1, 1, 1, dim), dtype=np.float32),
        cls_token_pos=_trunc_normal(rng, (1, 1, 1, dim), std=0.02),
        mask_token=_trunc_normal(rng, (1, 1, 1, dim), std=0.02),
        head=_xavier_uniform_linear(rng, contract.patch_dim, dim),
        blocks=[init_block_params(rng, dim, mlp_ratio) for _ in range(depth)],
        norm=_layernorm_params(dim),
    )


# ---------------------------------------------------------------------------
# Modules. Follow the idiomatic tt-train Python pattern found in
# `ttml/models/nanogpt/{gpt_block,gpt_mlp,multi_head_attention}.py`:
# `AbstractModuleBase` subclasses holding `ttml.modules.Parameter`-wrapped
# tensors, auto-registered via `__setattr__`.
# ---------------------------------------------------------------------------


class LayerNorm(AbstractModuleBase):
    """`ttml.ops.layernorm.layernorm` (eps hardcoded to 1e-6 in
    `ops/layernorm_op.cpp:146`, already matching the reference's
    `partial(nn.LayerNorm, eps=1e-6)` -- see `modules.py:131` -- with no
    workaround needed) wrapped with gamma/beta parameters, matching
    `GPTBlock`'s private `LayerNorm` in `gpt_block.py` but taking explicit
    init arrays instead of `ttml.init.ones()`/`zeros()`, so a future
    checkpoint loader can assign real trained weights the same way
    `_const_init` does for `LinearLayer`.
    """

    def __init__(self, dim: int, params: LayerNormParams) -> None:
        super().__init__()
        if params.gamma.shape != (1, 1, 1, dim) or params.beta.shape != (1, 1, 1, dim):
            raise ValueError(f"LayerNorm params must be shaped (1,1,1,{dim}), got gamma={params.gamma.shape} beta={params.beta.shape}")
        self.gamma = Parameter(_const_tensor(params.gamma))
        self.beta = Parameter(_const_tensor(params.beta))

    def forward(self, x: "ttml.autograd.Tensor") -> "ttml.autograd.Tensor":
        return ttml.ops.layernorm.layernorm(x, self.gamma.tensor, self.beta.tensor)


class Attention(AbstractModuleBase):
    """Port of `modules.py`'s `Attention`: fused QKV linear -> per-head split
    -> bidirectional SDPA -> head fusion -> output projection. No jagged
    batch: `bidirectional_scaled_dot_product_attention` (`ops_tt/attention.py`)
    already runs plain dense bidirectional attention over the fixed `seq_len`
    this port's static shape contract fixes ahead of time.
    """

    def __init__(
        self,
        dim: int,
        num_heads: int,
        params: BlockParams,
        qkv_mlp_dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16,
    ) -> None:
        super().__init__()
        if dim % num_heads != 0:
            raise ValueError(f"dim={dim} must be divisible by num_heads={num_heads}")
        head_dim = dim // num_heads
        if head_dim % 32 != 0:
            # Empirically confirmed on real hardware, not documented anywhere
            # in `nlp_create_qkv_heads`'s Python-visible docstring/signature:
            # `ttnn.experimental.nlp_create_qkv_heads` (what
            # `ttml.ops.multi_head_utils.heads_creation` calls,
            # `ops/multi_head_utils.cpp`) computes
            # `head_dim = qkv.padded_shape()[-1] / (num_q_heads + 2*num_kv_heads)`
            # (`nlp_create_qkv_heads.cpp`) and, when that arithmetic head_dim
            # is not a multiple of the tile width (32), does **not** raise --
            # it silently returns tensors of a *different*, wrong head_dim
            # containing uninitialized/garbage device memory (verified with a
            # column-marker tensor: dim=64, num_heads=4 -> mathematically
            # head_dim=16, but the op returned shape [...,32] full of
            # large-magnitude garbage floats unrelated to the input, e.g.
            # -1.07e8, on head 0). The same op at dim=64, num_heads=2
            # (head_dim=32, tile-aligned) returns exactly the expected
            # contiguous-column split with correct data. So this is a real,
            # silent-corruption hardware/kernel constraint -- not a shape
            # error to catch downstream -- and is checked here, eagerly, at
            # module construction time, rather than letting it surface as
            # wrong-but-plausible numbers deep in a parity test. This does
            # not affect this project's real config (ViT-L D=1024/16 heads
            # -> head_dim=64; decoder D=512/16 heads -> head_dim=32, both
            # already tile-aligned) -- it only constrains what `num_heads`
            # values a small-scale test may use.
            raise ValueError(
                f"dim={dim} // num_heads={num_heads} = head_dim={head_dim}, which is not a "
                "multiple of 32. ttnn.experimental.nlp_create_qkv_heads (used by "
                "ttml.ops.multi_head_utils.heads_creation) silently returns garbage "
                "device memory instead of raising when head_dim is not tile-aligned "
                "-- verified empirically, see the comment above this check. Choose "
                "dim/num_heads so head_dim is a multiple of 32."
            )
        self.num_heads = num_heads
        # qkv (and, in Mlp below, fc1/fc2) take `qkv_mlp_dtype` -- BFLOAT8_B on
        # the decoder, BFLOAT16 on the encoder (TENSTORRENT_PORT.md section 6:
        # "Keep decoder MLP/QKV weights in bfloat8_b ... encoder weights and
        # SDPA stay bf16"). `proj` (the attention output projection) is not
        # named in that policy and stays BFLOAT16 unconditionally -- it is not
        # "QKV" or "MLP", and applying bfp8 to it would be an undocumented
        # extension of the doc's guidance, not a literal reading of it.
        self.qkv = LinearLayer(dim, 3 * dim, True, weight_init=_const_init(params.qkv.weight, qkv_mlp_dtype), bias_init=_const_init(params.qkv.bias))
        self.proj = LinearLayer(dim, dim, True, weight_init=_const_init(params.proj.weight), bias_init=_const_init(params.proj.bias))

    def forward(self, x: "ttml.autograd.Tensor", seq_len: int) -> "ttml.autograd.Tensor":
        qkv = self.qkv(x)
        query, key, value = ttml.ops.multi_head_utils.heads_creation(qkv, self.num_heads)
        attn_out = bidirectional_scaled_dot_product_attention(query, key, value, seq_len=seq_len)
        fused = ttml.ops.multi_head_utils.heads_fusion(attn_out)
        return self.proj(fused)


class Mlp(AbstractModuleBase):
    """Port of `modules.py`'s `Mlp`: fc1 -> GELU -> fc2. No dropout: the
    reference's `Mlp` never had any (only `Block`'s residual paths do, via
    `DropPath`, itself omitted below since `drop_path_rate: 0.0` always in
    this project's config).

    `use_tuned_gelu` (M6, default `False`): when `True`, the GELU activation
    between `fc1`/`fc2` uses `ops_tt.tuned_gelu.tuned_gelu` (forward via
    `ttnn.gelu(..., variant=ttnn.GeluVariant.FastLut)`, a measured ~1.76x
    forward speedup over the default `Accurate` variant at real encoder/
    decoder MLP-hidden shapes, PCC-gate-verified -- see `tuned_gelu.py`'s
    module docstring) instead of `ttml.ops.unary.gelu`'s default `Accurate`
    variant. `False` preserves prior behavior exactly.
    """

    def __init__(
        self,
        dim: int,
        mlp_ratio: float,
        params: BlockParams,
        qkv_mlp_dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16,
        use_tuned_gelu: bool = False,
    ) -> None:
        super().__init__()
        hidden = int(dim * mlp_ratio)
        self.fc1 = LinearLayer(dim, hidden, True, weight_init=_const_init(params.fc1.weight, qkv_mlp_dtype), bias_init=_const_init(params.fc1.bias))
        self.fc2 = LinearLayer(hidden, dim, True, weight_init=_const_init(params.fc2.weight, qkv_mlp_dtype), bias_init=_const_init(params.fc2.bias))
        self._gelu = tuned_gelu if use_tuned_gelu else ttml.ops.unary.gelu

    def forward(self, x: "ttml.autograd.Tensor") -> "ttml.autograd.Tensor":
        x = self.fc1(x)
        x = self._gelu(x)
        x = self.fc2(x)
        return x


class TransformerBlock(AbstractModuleBase):
    """Port of `modules.py`'s `Block`: pre-norm attention + residual,
    pre-norm MLP + residual. No `drop_path`/`DropPath`: this project's
    config always has `drop_path_rate: 0.0` (`default_pretrain.yaml`'s
    `model_kwargs`, and it is the `MaskedAutoencoderViT` default too), so
    `DropPath(0.0)` is `nn.Identity()` in the reference already -- building
    a real drop-path code path here would be dead code for this port.

    `use_tuned_gelu` (M6, default `False`): forwarded to `Mlp` unchanged --
    see `Mlp`'s docstring.
    """

    def __init__(
        self,
        dim: int,
        num_heads: int,
        mlp_ratio: float,
        params: BlockParams,
        seq_len: int,
        qkv_mlp_dtype: "ttnn.DataType" = ttnn.DataType.BFLOAT16,
        use_tuned_gelu: bool = False,
    ) -> None:
        super().__init__()
        self.seq_len = seq_len
        self.norm1 = LayerNorm(dim, params.ln1)
        self.attn = Attention(dim, num_heads, params, qkv_mlp_dtype=qkv_mlp_dtype)
        self.norm2 = LayerNorm(dim, params.ln2)
        self.mlp = Mlp(dim, mlp_ratio, params, qkv_mlp_dtype=qkv_mlp_dtype, use_tuned_gelu=use_tuned_gelu)

    def forward(self, x: "ttml.autograd.Tensor") -> "ttml.autograd.Tensor":
        residual = x
        x = self.norm1(x)
        x = self.attn(x, seq_len=self.seq_len)
        x = ttml.ops.binary.add(x, residual)

        residual = x
        x = self.norm2(x)
        x = self.mlp(x)
        x = ttml.ops.binary.add(x, residual)
        return x


class Encoder(AbstractModuleBase):
    """Port of `model_mae.py`'s `MaskedEncoder.forward_visible_ids` (the
    "no ragged batch" path, per TENSTORRENT_PORT.md's guidance on which
    reference method to follow): patchify is done host-side already
    (`packing.py`), so this starts from patch values.

    `class_token=True, reg_tokens=0, no_embed_class=False` unconditionally
    -- this project's config (`default_pretrain.yaml`'s `model_kwargs`)
    never varies these, so the reg-token and `no_embed_class` branches of
    the reference are not built here (see task instructions / TT port
    memory: "don't design for hypothetical future requirements").

    `use_tuned_gelu` (M6, default `False`): forwarded to every block's `Mlp`
    -- see `modules_tt.Mlp`'s docstring.
    """

    def __init__(
        self,
        contract: ShapeContract,
        depth: int,
        params: EncoderParams,
        mlp_ratio: float = 4.0,
        use_tuned_gelu: bool = False,
    ) -> None:
        super().__init__()
        self.contract = contract
        dim = contract.embed_dim
        self.patch_embed = LinearLayer(
            contract.patch_dim, dim, True, weight_init=_const_init(params.patch_embed.weight), bias_init=_const_init(params.patch_embed.bias)
        )
        self.pos_embed_table = Buffer(build_pos_embed_table(dim, contract.grid_size))
        self.cls_token = Parameter(_const_tensor(params.cls_token))
        self.cls_token_pos = Parameter(_const_tensor(params.cls_token_pos))
        self.blocks = ModuleList(
            [
                TransformerBlock(
                    dim,
                    contract.encoder_heads,
                    mlp_ratio,
                    params.blocks[i],
                    seq_len=contract.encoder_seq_len,
                    use_tuned_gelu=use_tuned_gelu,
                )
                for i in range(depth)
            ]
        )
        self.norm = LayerNorm(dim, params.norm)

    def forward(
        self, visible_values: np.ndarray, visible_ids: np.ndarray
    ) -> tuple["ttml.autograd.Tensor", "ttml.autograd.Tensor"]:
        """`visible_values`: `[B, L_vis, patch_dim]` host numpy (packing.py's
        `PackedBatch.visible_values`). `visible_ids`: `[B, L_vis]` host numpy
        (`PackedBatch.visible_ids`). Returns `(cls_embed [B,1,1,D],
        patch_embeds [B,1,L_vis,D])`, matching
        `forward_visible_ids`'s `(cls_embeds, reg_embeds, patch_embeds)`
        minus the always-`None` `reg_embeds` (`reg_tokens=0` here).
        """
        batch, l_vis, patch_dim = visible_values.shape
        if l_vis != self.contract.l_vis:
            raise ValueError(f"visible_values has L_vis={l_vis}, contract expects l_vis={self.contract.l_vis}")
        if patch_dim != self.contract.patch_dim:
            raise ValueError(f"visible_values has patch_dim={patch_dim}, contract expects patch_dim={self.contract.patch_dim}")
        if visible_ids.shape != (batch, l_vis):
            raise ValueError(f"visible_ids shape {visible_ids.shape} does not match visible_values batch/L_vis ({batch},{l_vis})")

        x = _values_to_tensor(visible_values)
        x = self.patch_embed(x)
        x = apply_pos_embed(x, self.pos_embed_table.tensor, visible_ids)

        cls = ttml.ops.binary.add(self.cls_token.tensor, self.cls_token_pos.tensor)  # [1,1,1,D]
        cls = _broadcast_batch(cls, batch)  # [B,1,1,D]
        x = autograd_concat([cls, x], dim=2)  # [B,1,1+L_vis,D]

        for block in self.blocks:
            x = block(x)
        x = self.norm(x)

        cls_embed = _slice_seq(x, 0, 1)
        patch_embeds = _slice_seq(x, 1, l_vis)
        return cls_embed, patch_embeds


class Decoder(AbstractModuleBase):
    """Port of `model_mae.py`'s `MaskedDecoder.forward`, `embed_ids`/`pred_ids`
    always given (this port never decodes "all patches" -- `pred_ids` is
    always the host-sampled `PackedBatch.pred_ids`) and
    `embed_token_mask`/`pred_token_mask`/`packed_output` dropped (every slot
    is always valid on this port, TENSTORRENT_PORT.md section 3).

    `use_tuned_gelu` (M6, default `False`): forwarded to every block's `Mlp`
    -- see `modules_tt.Mlp`'s docstring.
    """

    def __init__(
        self,
        contract: ShapeContract,
        depth: int,
        params: DecoderParams,
        mlp_ratio: float = 4.0,
        use_tuned_gelu: bool = False,
    ) -> None:
        super().__init__()
        self.contract = contract
        dim = contract.decoder_embed_dim
        input_dim = contract.embed_dim
        if input_dim == dim:
            raise NotImplementedError(
                "decoder_embed_dim == embed_dim (identity decoder input projection) is "
                "not used by this project's config and is not implemented (see "
                "init_decoder_params)"
            )
        self.proj = LinearLayer(input_dim, dim, True, weight_init=_const_init(params.proj.weight), bias_init=_const_init(params.proj.bias))
        self.pos_embed_table = Buffer(build_pos_embed_table(dim, contract.grid_size))
        self.cls_token = Parameter(_const_tensor(params.cls_token))
        self.cls_token_pos = Parameter(_const_tensor(params.cls_token_pos))
        self.mask_token = Parameter(_const_tensor(params.mask_token))
        self.blocks = ModuleList(
            [
                TransformerBlock(
                    dim,
                    contract.decoder_heads,
                    mlp_ratio,
                    params.blocks[i],
                    seq_len=contract.decoder_seq_len,
                    qkv_mlp_dtype=ttnn.DataType.BFLOAT8_B,
                    use_tuned_gelu=use_tuned_gelu,
                )
                for i in range(depth)
            ]
        )
        self.norm = LayerNorm(dim, params.norm)
        self.head = LinearLayer(dim, contract.patch_dim, True, weight_init=_const_init(params.head.weight), bias_init=_const_init(params.head.bias))

    def forward(
        self,
        patch_embeds: "ttml.autograd.Tensor",
        visible_ids: np.ndarray,
        pred_ids: np.ndarray,
    ) -> "ttml.autograd.Tensor":
        """`patch_embeds`: `[B, 1, L_vis, embed_dim]`, the encoder's output
        (`Encoder.forward`'s second return value). `visible_ids`/`pred_ids`:
        `[B, L_vis]`/`[B, Q_pred]` host numpy (`PackedBatch.visible_ids`/
        `pred_ids`). Returns `pred [B, 1, Q_pred, patch_dim]`.
        """
        batch = visible_ids.shape[0]
        l_vis = self.contract.l_vis
        q_pred = self.contract.q_pred
        if visible_ids.shape != (batch, l_vis):
            raise ValueError(f"visible_ids shape {visible_ids.shape} != ({batch},{l_vis})")
        if pred_ids.shape != (batch, q_pred):
            raise ValueError(f"pred_ids shape {pred_ids.shape} != ({batch},{q_pred})")

        dim = self.contract.decoder_embed_dim
        mask = _broadcast_batch(self.mask_token.tensor, batch, seq_len=q_pred)  # [B,1,Q,D]
        mask = apply_pos_embed(mask, self.pos_embed_table.tensor, pred_ids)

        embeds = self.proj(patch_embeds)  # [B,1,L_vis,D]
        embeds = apply_pos_embed(embeds, self.pos_embed_table.tensor, visible_ids)

        x = autograd_concat([embeds, mask], dim=2)  # [B,1,L_vis+Q,D]

        cls = ttml.ops.binary.add(self.cls_token.tensor, self.cls_token_pos.tensor)  # [1,1,1,D]
        cls = _broadcast_batch(cls, batch)
        x = autograd_concat([cls, x], dim=2)  # [B,1,1+L_vis+Q,D]

        for block in self.blocks:
            x = block(x)
        x = self.norm(x)

        pred = _slice_seq(x, 1 + l_vis, q_pred)  # skip cls and the visible-embed region
        pred = self.head(pred)
        return pred
