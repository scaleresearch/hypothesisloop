"""Encoder-only, inference-only TT-NN forward pass with a key-padding mask.

`smri_mae_tt.modules_tt.Encoder` is the pretraining encoder: every one of its
`l_vis` slots is a real patch (the host sampled exactly that many), so its
attention runs with `AttentionMaskType::None` and it has no notion of padding.
fomo_tune's sequence length is a property of the subject's brain mask, so the
dense batch always has trailing padding, and the padded keys must be kept out
of every softmax.

Everything else -- the parameter modules, the sincos position table, the
fused-QKV attention, the MLP, the tile-alignment rules -- is
`smri_mae_tt.modules_tt`'s, reused unchanged. Only `Attention.forward`'s SDPA
call differs (`AttentionMaskType::Arbitrary` with a `(1, 1, S, S)` bf16 keep
mask instead of `no_mask=True`), so this module subclasses rather than forks.
"""

from __future__ import annotations

import numpy as np
import ttml
import ttnn
from ttml.modules import AbstractModuleBase, Buffer, LinearLayer, ModuleList, Parameter

from smri_mae_tt.layout import TILE, ShapeContract
from smri_mae_tt.modules_tt import (
    Attention,
    BlockParams,
    EncoderParams,
    LayerNorm,
    Mlp,
    _broadcast_batch,
    _const_init,
    _const_tensor,
    _slice_seq,
    _values_to_tensor,
    apply_pos_embed,
    build_pos_embed_table,
)
from smri_mae_tt.ops_tt.concat import autograd_concat


def encoder_seq_len_for(n_visible: int) -> int:
    """Smallest tile-aligned encoder sequence length that fits `n_visible`
    patch tokens plus the cls token."""
    return ((n_visible + 1 + TILE - 1) // TILE) * TILE


def build_contract(l_vis: int, patch_size: int, img_size: tuple[int, int, int], embed_dim: int, heads: int) -> ShapeContract:
    """An encoder-only `ShapeContract`. `q_pred=0`: there is no decoder here,
    and 0 keeps `decoder_seq_len == encoder_seq_len` tile-aligned rather than
    inventing a decoder shape this port never builds."""
    return ShapeContract(
        batch=1,
        l_vis=l_vis,
        q_pred=0,
        mask_ratio=0.0,
        img_size=img_size,
        patch_size=patch_size,
        embed_dim=embed_dim,
        encoder_heads=heads,
    )


class MaskedAttention(Attention):
    """`smri_mae_tt.modules_tt.Attention` with an explicit key-padding mask.

    `ttml.ops.attention.scaled_dot_product_attention(q, k, v, mask, no_mask=False)`
    selects `AttentionMaskType::Arbitrary`; the kernel reads the mask as
    1.0=keep / 0.0=mask and turns the latter into a -1e9 score bias
    (`sdpa_compute_utils.hpp::apply_mask_on_reg`). The kernel requires the mask
    to be `(1, 1, S, S)` bf16 TILE -- one mask shared across batch and heads,
    which is exactly right here: this encoder always runs one subject at a time,
    so there is only ever one padding pattern in flight.
    """

    def forward(self, x: "ttml.autograd.Tensor", seq_len: int, mask: "ttml.autograd.Tensor" = None) -> "ttml.autograd.Tensor":
        if mask is None:
            raise ValueError("MaskedAttention requires a key-padding mask; use modules_tt.Attention for the unmasked path")
        if seq_len % TILE:
            raise ValueError(f"seq_len={seq_len} is not a multiple of the tile height ({TILE}); the fused SDPA kernel requires tile-aligned sequences")
        qkv = self.qkv(x)
        query, key, value = ttml.ops.multi_head_utils.heads_creation(qkv, self.num_heads)
        attn_out = ttml.ops.attention.scaled_dot_product_attention(query, key, value, mask, False)
        fused = ttml.ops.multi_head_utils.heads_fusion(attn_out)
        return self.proj(fused)


class MaskedTransformerBlock(AbstractModuleBase):
    """`modules_tt.TransformerBlock` with the mask threaded through to
    attention. Same submodules, same pre-norm/residual order."""

    def __init__(self, dim: int, num_heads: int, mlp_ratio: float, params: BlockParams) -> None:
        super().__init__()
        self.norm1 = LayerNorm(dim, params.ln1)
        self.attn = MaskedAttention(dim, num_heads, params)
        self.norm2 = LayerNorm(dim, params.ln2)
        self.mlp = Mlp(dim, mlp_ratio, params)

    def forward(self, x: "ttml.autograd.Tensor", seq_len: int, mask: "ttml.autograd.Tensor") -> "ttml.autograd.Tensor":
        residual = x
        x = self.attn(self.norm1(x), seq_len=seq_len, mask=mask)
        x = ttml.ops.binary.add(x, residual)

        residual = x
        x = self.mlp(self.norm2(x))
        x = ttml.ops.binary.add(x, residual)
        return x


class MaskedInferenceEncoder(AbstractModuleBase):
    """Port of `MaskedEncoder.forward(x, mask=...)` from the patch-embedding
    linear onward, for a single subject with trailing padding.

    `class_token=True, reg_tokens=0, no_embed_class=False` -- the only setting
    the walnut checkpoints use, same assumption `smri_mae_tt.modules_tt.Encoder`
    already makes.
    """

    def __init__(self, contract: ShapeContract, depth: int, params: EncoderParams, mlp_ratio: float = 4.0) -> None:
        super().__init__()
        self.contract = contract
        dim = contract.embed_dim
        self.patch_embed = LinearLayer(
            contract.patch_dim, dim, True,
            weight_init=_const_init(params.patch_embed.weight),
            bias_init=_const_init(params.patch_embed.bias),
        )
        self.pos_embed_table = Buffer(build_pos_embed_table(dim, contract.grid_size))
        self.cls_token = Parameter(_const_tensor(params.cls_token))
        self.cls_token_pos = Parameter(_const_tensor(params.cls_token_pos))
        self.blocks = ModuleList([MaskedTransformerBlock(dim, contract.encoder_heads, mlp_ratio, params.blocks[i]) for i in range(depth)])
        self.norm = LayerNorm(dim, params.norm)

    def forward(self, visible_values: np.ndarray, visible_ids: np.ndarray, keep_mask: np.ndarray) -> "ttml.autograd.Tensor":
        """`visible_values` `[1, l_vis, patch_dim]`, `visible_ids` `[1, l_vis]`,
        `keep_mask` `(1, 1, S, S)` float32 with S = l_vis + 1 (cls). Returns the
        post-norm patch embeddings `[1, 1, l_vis, D]`; the cls token is sliced
        off, matching `chunk_tokens`.
        """
        seq_len = self.contract.encoder_seq_len
        batch, l_vis, patch_dim = visible_values.shape
        if batch != 1:
            raise ValueError(f"MaskedInferenceEncoder runs one subject at a time (the SDPA mask is shared across batch), got batch={batch}")
        if l_vis != self.contract.l_vis or patch_dim != self.contract.patch_dim:
            raise ValueError(f"visible_values {visible_values.shape} does not match contract l_vis={self.contract.l_vis}, patch_dim={self.contract.patch_dim}")
        if keep_mask.shape != (1, 1, seq_len, seq_len):
            raise ValueError(f"keep_mask shape {keep_mask.shape} must be (1, 1, {seq_len}, {seq_len})")

        mask = ttml.autograd.Tensor.from_numpy(keep_mask.astype(np.float32), layout=ttnn.Layout.TILE, new_type=ttnn.DataType.BFLOAT16)

        x = _values_to_tensor(visible_values)
        x = self.patch_embed(x)
        x = apply_pos_embed(x, self.pos_embed_table.tensor, visible_ids)

        cls = ttml.ops.binary.add(self.cls_token.tensor, self.cls_token_pos.tensor)
        cls = _broadcast_batch(cls, batch)
        x = autograd_concat([cls, x], dim=2)  # [1, 1, 1 + l_vis, D]

        for block in self.blocks:
            x = block(x, seq_len=seq_len, mask=mask)
        x = self.norm(x)
        return _slice_seq(x, 1, l_vis)
