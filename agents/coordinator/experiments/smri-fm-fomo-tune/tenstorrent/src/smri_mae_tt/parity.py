"""Shared parity-testing harness for the TT-NN MAE port.

TENSTORRENT_PORT.md section 7 ("Parity harness") specifies this as a thing
to "build ... at M2 and keep" -- reusable across M2/M3/M4/M6/M7 gates rather
than reimplemented ad hoc per test file:

    A frozen fixture: 10 real samples + a real checkpoint + fixed
    visible_ids/pred_ids, serialized once.

    A `compare(name, torch_tensor, tt_tensor)` helper reporting PCC, max abs
    error, and relative Frobenius error, with per-layer hooks so a
    regression localizes to a block rather than a model.

No trained checkpoint/real samples exist anywhere in this repo yet (checked
repeatedly by earlier work -- see `params.py`'s and `test_modules_tt.py`'s
module docstrings), so the "frozen fixture" here is the small-dimension
substitute those files already converged on independently: a fixed
`ShapeContract` + seeded `numpy.random.Generator`s that produce shared
random weights and shared random input data, loaded into both a PyTorch
reference model and this port's TT modules via `.copy_()`/`set_value`, so
outputs are directly comparable rather than each test inventing its own
dims/seed. Swap in `packing.py` + a real checkpoint once one exists; the
`compare()`/`ParityReport` half of this module does not change.

Three things live here, all lifted (not reimplemented) from the ad-hoc
copies that had accumulated across `test_modules_tt.py`, `test_model_tt.py`,
and `test_params.py`:

1. Metrics: `pearson_corr`, `ParityReport`, `compare`.
2. The frozen fixture: `ShapeContract` + seeds -> `FrozenFixture`,
   `make_frozen_fixture`, `FROZEN_FIXTURE`.
3. Reference-model plumbing: build a small PyTorch `MaskedEncoder`/
   `MaskedDecoder`, load `modules_tt`'s `EncoderParams`/`DecoderParams`
   arrays into it via `.copy_()`, and run it with plain dense
   `F.scaled_dot_product_attention` in place of the reference's
   jagged/nested-tensor attention path (which has no working CPU backend --
   see `dense_block_forward`'s docstring for why this is mathematically
   identical under this port's static-shape contract, not an approximation).
   Future M4 (gradient parity) work needs exactly this plumbing too.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F

import ttml  # noqa: E402
import ttnn  # noqa: E402

from smri_mae.model_mae import MaskedDecoder, MaskedEncoder
from smri_mae.modules import Patchify3D, SinCosPosEmbed3D

from smri_mae_tt import modules_tt as M
from smri_mae_tt.layout import ShapeContract

# ---------------------------------------------------------------------------
# 1. Metrics
# ---------------------------------------------------------------------------

DEFAULT_PCC_GATE = 0.999


def pearson_corr(a: np.ndarray, b: np.ndarray) -> float:
    """Pearson correlation coefficient between two flattened arrays.

    Raises if either side is constant (zero variance) -- PCC is
    mathematically undefined there, not "1.0" or "0.0" by convention, so
    returning a number would silently hide a degenerate test input.
    """
    af = a.reshape(-1).astype(np.float64)
    bf = b.reshape(-1).astype(np.float64)
    af = af - af.mean()
    bf = bf - bf.mean()
    denom = np.linalg.norm(af) * np.linalg.norm(bf)
    if denom == 0.0:
        raise ValueError("PCC is undefined here: one of the compared tensors is constant (zero variance)")
    return float(np.dot(af, bf) / denom)


@dataclass(frozen=True)
class ParityReport:
    """PCC / max-abs-error / relative-Frobenius-error for one named tensor
    pair -- the three metrics TENSTORRENT_PORT.md section 7 names
    explicitly. Kept dict-subscriptable (`report["pcc"]`) for compatibility
    with the pre-existing call sites this harness replaces."""

    name: str
    pcc: float
    max_abs_err: float
    rel_frob_err: float

    def as_dict(self) -> dict[str, float]:
        return {"pcc": self.pcc, "max_abs_err": self.max_abs_err, "rel_frob_err": self.rel_frob_err}

    def __getitem__(self, key: str) -> float:
        return self.as_dict()[key]

    def assert_gate(self, pcc_gate: float = DEFAULT_PCC_GATE) -> "ParityReport":
        """Fail loud: raise with all three metrics in the message, not just
        the one that tripped the gate -- a reader debugging a failure wants
        the full picture immediately, not a second run with -s."""
        if not (self.pcc >= pcc_gate):
            raise AssertionError(
                f"{self.name}: PCC={self.pcc:.6f} below gate {pcc_gate} "
                f"(max_abs_err={self.max_abs_err:.6e}, rel_frob_err={self.rel_frob_err:.6e})"
            )
        return self


def compare(name: str, torch_tensor: np.ndarray, tt_tensor: np.ndarray, pcc_gate: float = DEFAULT_PCC_GATE) -> ParityReport:
    """The M2/M3/M4 parity-harness entry point (TENSTORRENT_PORT.md section
    7): compute PCC, max absolute error, and relative Frobenius-norm error
    for one named tensor pair, print them, and assert the PCC gate.

    Both inputs are plain numpy arrays (or anything `np.asarray`-convertible
    -- a torch CPU tensor's `.numpy()` is not required first) with the same
    element count. Raises on a shape/size mismatch rather than broadcasting
    past it: a size mismatch here means the port produced a differently
    -shaped tensor than the reference, which is a bug to surface
    immediately, not silently reshape/pad around.

    Pass `pcc_gate=0.0` (never negative -- PCC is bounded below by -1 but a
    genuine parity check is never looking for anti-correlation) to record
    metrics without gating, e.g. `test_params.py`'s lossy round-trip check,
    which asserts its own `rel_frob_err` bound instead.
    """
    ref = np.asarray(torch_tensor).reshape(-1).astype(np.float64)
    tt = np.asarray(tt_tensor).reshape(-1).astype(np.float64)
    if ref.shape != tt.shape:
        raise ValueError(f"{name}: element count mismatch, torch={np.shape(torch_tensor)} tt={np.shape(tt_tensor)}")

    # Check the all-zero-reference degenerate case before pearson_corr: an
    # all-zero reference is always zero-variance, so pearson_corr would
    # raise its generic "PCC is undefined" ValueError first, masking the
    # more specific and more actionable message below.
    ref_norm = float(np.linalg.norm(ref))
    if ref_norm == 0.0:
        raise ValueError(f"{name}: reference tensor is all-zero, relative Frobenius error is undefined")

    pcc = pearson_corr(ref, tt)
    max_abs_err = float(np.max(np.abs(ref - tt)))
    rel_frob_err = float(np.linalg.norm(ref - tt) / ref_norm)

    report = ParityReport(name=name, pcc=pcc, max_abs_err=max_abs_err, rel_frob_err=rel_frob_err)
    print(f"[parity] {name}: PCC={pcc:.6f} max_abs_err={max_abs_err:.6e} rel_frob_err={rel_frob_err:.6e}")
    report.assert_gate(pcc_gate)
    return report


def to_numpy_fp32(tensor: "ttml.autograd.Tensor") -> np.ndarray:
    """Read a device-resident `ttml.autograd.Tensor` back to a fp32 numpy
    array, for feeding into `compare()`."""
    return np.asarray(tensor.to_numpy(ttnn.DataType.FLOAT32), dtype=np.float32)


# ---------------------------------------------------------------------------
# 2. The frozen fixture
# ---------------------------------------------------------------------------

# The seed every test file that wants reproducible-but-independent random
# streams derives from, per project memory ("SEED=2026 was used by one
# agent") and TENSTORRENT_PORT.md section 7's "frozen fixture" instruction.
# Individual tests that need more than one independent stream (e.g. one for
# parameters, one for input data) should use `make_frozen_fixture(seed_offset=...)`
# rather than hand-rolling `SEED + <arbitrary offset>` arithmetic, so the
# seeds in use across the suite stay enumerable from one place.
FROZEN_FIXTURE_SEED = 2026


@dataclass(frozen=True)
class FrozenFixture:
    """A deterministic, small-dimension `ShapeContract` + seeded RNG
    configuration, shared across parity tests so M2/M3/M4 gates are measured
    on consistent, comparable inputs rather than each test inventing its own
    dims/seed.

    `contract` dims are chosen for two reasons, both load-bearing (see
    `modules_tt.py`'s `Attention.__init__` docstring for the full argument):
    `encoder_heads=2` at `embed_dim=64` and `decoder_heads=1` at
    `decoder_embed_dim=32` both give `head_dim=32` -- the tile size --
    because `ttml.ops.multi_head_utils.heads_creation`
    (`ttnn.experimental.nlp_create_qkv_heads`) silently returns garbage
    device memory, not an error, whenever `dim // num_heads` is not a
    multiple of 32. And `l_vis=31, q_pred=32` make `encoder_seq_len=32`,
    `decoder_seq_len=64` tile-aligned, satisfying `ShapeContract`'s own
    validation with `class_token=True`.
    """

    contract: ShapeContract
    encoder_depth: int
    decoder_depth: int
    param_seed: int
    data_seed: int

    def param_rng(self) -> np.random.Generator:
        return np.random.default_rng(self.param_seed)

    def data_rng(self) -> np.random.Generator:
        return np.random.default_rng(self.data_seed)


def make_frozen_fixture(*, seed_offset: int = 0, encoder_depth: int = 2, decoder_depth: int = 1) -> FrozenFixture:
    """Build a `FrozenFixture`. `seed_offset` derives an independent
    `(param_seed, data_seed)` pair from `FROZEN_FIXTURE_SEED` for tests that
    need their own random stream without colliding with another test's (the
    same role `SEED + 100`, `SEED + 200`, ... played ad hoc across the
    pre-existing test files) -- `param_seed`/`data_seed` are always
    consecutive integers so a given `seed_offset` reproduces the exact same
    two streams every time.

    The `ShapeContract` itself is not parameterized: it is the one thing
    this fixture exists to freeze, so every caller gets the identical
    contract `test_modules_tt.py`, `test_model_tt.py`, and `test_params.py`
    already independently converged on.
    """
    contract = ShapeContract(
        batch=2,
        l_vis=31,
        q_pred=32,
        mask_ratio=0.8,
        img_size=(32, 32, 32),
        patch_size=8,
        in_chans=1,
        embed_dim=64,
        decoder_embed_dim=32,
        encoder_heads=2,  # head_dim=32 -- see FrozenFixture docstring
        decoder_heads=1,  # head_dim=32
        class_token=True,
    )
    return FrozenFixture(
        contract=contract,
        encoder_depth=encoder_depth,
        decoder_depth=decoder_depth,
        param_seed=FROZEN_FIXTURE_SEED + seed_offset,
        data_seed=FROZEN_FIXTURE_SEED + seed_offset + 1,
    )


# The default instance: `encoder_depth=2, decoder_depth=1`, matching every
# pre-existing test's small-scale depth choice. Import this directly when a
# test doesn't need its own seed stream; call `make_frozen_fixture(seed_offset=...)`
# when it does.
FROZEN_FIXTURE = make_frozen_fixture()


# ---------------------------------------------------------------------------
# 3. Reference-model plumbing: build, load shared weights, dense-SDPA forward
# ---------------------------------------------------------------------------


def build_reference_encoder(contract: ShapeContract, depth: int) -> MaskedEncoder:
    """A small `smri_mae.model_mae.MaskedEncoder`, library-default init,
    shaped by `contract`. Load real weights into it with
    `load_encoder_into_ref` before comparing against a TT `Encoder` built
    from the same `EncoderParams`."""
    patchify_ref = Patchify3D(contract.img_size, (contract.patch_size,) * 3, in_chans=contract.in_chans)
    patch_embed_ref = nn.Linear(patchify_ref.patch_dim, contract.embed_dim)
    pos_embed_ref = SinCosPosEmbed3D(contract.embed_dim, patchify_ref.grid_size)
    return MaskedEncoder(
        patchify=patchify_ref,
        patch_embed=patch_embed_ref,
        pos_embed=pos_embed_ref,
        depth=depth,
        embed_dim=contract.embed_dim,
        num_heads=contract.encoder_heads,
        qkv_bias=True,
        proj_bias=True,
        mlp_ratio=4,
        class_token=contract.class_token,
        reg_tokens=0,
        no_embed_class=False,
        drop_path_rate=0.0,
    )


def build_reference_decoder(contract: ShapeContract, depth: int) -> MaskedDecoder:
    """A small `smri_mae.model_mae.MaskedDecoder`, library-default init,
    shaped by `contract`. Load real weights into it with
    `load_decoder_into_ref`."""
    patchify_ref = Patchify3D(contract.img_size, (contract.patch_size,) * 3, in_chans=contract.in_chans)
    decoder_pos_embed_ref = SinCosPosEmbed3D(contract.decoder_embed_dim, patchify_ref.grid_size)
    decoder_head_ref = nn.Linear(contract.decoder_embed_dim, patchify_ref.patch_dim)
    return MaskedDecoder(
        pos_embed=decoder_pos_embed_ref,
        head=decoder_head_ref,
        input_dim=contract.embed_dim,
        depth=depth,
        embed_dim=contract.decoder_embed_dim,
        num_heads=contract.decoder_heads,
        qkv_bias=True,
        proj_bias=True,
        mlp_ratio=4,
        class_token=contract.class_token,
        no_embed_class=False,
    )


def _copy_into(param: torch.nn.Parameter, arr: np.ndarray) -> None:
    """Copy `arr` into `param`, reshaping to `param.shape` first.

    `modules_tt.py`'s param arrays carry this port's `(1, 1, ..., D)`
    leading-singleton convention; the reference's `nn.Linear`/`nn.LayerNorm`/
    raw `nn.Parameter`s use plain 1D/2D torch shapes. Both have the same
    total element count, so a reshape (not a transpose -- both sides already
    agree on `(out_features, in_features)` row-major weight layout) is the
    only translation needed.
    """
    with torch.no_grad():
        param.copy_(torch.from_numpy(arr.reshape(tuple(param.shape)).copy()))


def load_block_into_ref(ref_block: nn.Module, params: "M.BlockParams") -> None:
    _copy_into(ref_block.norm1.weight, params.ln1.gamma)
    _copy_into(ref_block.norm1.bias, params.ln1.beta)
    _copy_into(ref_block.attn.qkv.weight, params.qkv.weight)
    _copy_into(ref_block.attn.qkv.bias, params.qkv.bias)
    _copy_into(ref_block.attn.proj.weight, params.proj.weight)
    _copy_into(ref_block.attn.proj.bias, params.proj.bias)
    _copy_into(ref_block.norm2.weight, params.ln2.gamma)
    _copy_into(ref_block.norm2.bias, params.ln2.beta)
    _copy_into(ref_block.mlp.fc1.weight, params.fc1.weight)
    _copy_into(ref_block.mlp.fc1.bias, params.fc1.bias)
    _copy_into(ref_block.mlp.fc2.weight, params.fc2.weight)
    _copy_into(ref_block.mlp.fc2.bias, params.fc2.bias)


def load_encoder_into_ref(ref_encoder: MaskedEncoder, params: "M.EncoderParams") -> None:
    """Share `modules_tt.init_encoder_params`-produced weights into a
    PyTorch reference `MaskedEncoder` via `.copy_()`, so a TT `Encoder`
    built from the same `params` and this reference compute the exact same
    function."""
    _copy_into(ref_encoder.patch_embed.weight, params.patch_embed.weight)
    _copy_into(ref_encoder.patch_embed.bias, params.patch_embed.bias)
    _copy_into(ref_encoder.cls_token, params.cls_token)
    _copy_into(ref_encoder.cls_token_pos, params.cls_token_pos)
    for ref_block, block_params in zip(ref_encoder.blocks, params.blocks):
        load_block_into_ref(ref_block, block_params)
    _copy_into(ref_encoder.norm.weight, params.norm.gamma)
    _copy_into(ref_encoder.norm.bias, params.norm.beta)


def load_decoder_into_ref(ref_decoder: MaskedDecoder, params: "M.DecoderParams") -> None:
    """Share `modules_tt.init_decoder_params`-produced weights into a
    PyTorch reference `MaskedDecoder` via `.copy_()`."""
    _copy_into(ref_decoder.proj.weight, params.proj.weight)
    _copy_into(ref_decoder.proj.bias, params.proj.bias)
    _copy_into(ref_decoder.cls_token, params.cls_token)
    _copy_into(ref_decoder.cls_token_pos, params.cls_token_pos)
    _copy_into(ref_decoder.mask_token, params.mask_token)
    for ref_block, block_params in zip(ref_decoder.blocks, params.blocks):
        load_block_into_ref(ref_block, block_params)
    _copy_into(ref_decoder.norm.weight, params.norm.gamma)
    _copy_into(ref_decoder.norm.bias, params.norm.beta)
    _copy_into(ref_decoder.head.weight, params.head.weight)
    _copy_into(ref_decoder.head.bias, params.head.bias)


def dense_block_forward(block: nn.Module, x: torch.Tensor) -> torch.Tensor:
    """Reference `Block.forward`, with plain dense
    `F.scaled_dot_product_attention` in place of the reference's jagged/
    nested-tensor path (`jagged_scaled_dot_product_attention`,
    `src/smri_mae/modules.py:56-68`).

    Why this exists: `smri_mae.modules.Block.forward` unconditionally routes
    through `torch.nested.nested_tensor_from_jagged` +
    `F.scaled_dot_product_attention` on nested tensors. On a CPU-only torch
    build that path raises `RuntimeError: No viable backend for
    scaled_dot_product_attention was found` (confirmed empirically -- even
    the nested "math" fallback errors, "... must be contiguous after
    transposing") regardless of whether the batch is actually ragged. This
    is a CPU-nested-tensor environment limitation, not something about this
    port's correctness, and not something to work around inside the
    reference `src/smri_mae/modules.py` (read-only reference code, never
    modified by this port).

    Mathematical equivalence: `JaggedBatch`/`nested_tensor_from_jagged` is
    purely a memory-layout device for batching *variable-length* sequences.
    This port's static-shape contract means every sample always contributes
    exactly the same number of tokens (TENSTORRENT_PORT.md section 3: "no
    attention mask exists anywhere in the model") -- i.e. the batch this
    harness ever constructs is never actually ragged, `token_mask` is always
    all-ones. With no raggedness, jagged attention over `[B, S, ...]` and
    plain dense attention over the same `[B, S, ...]` compute the exact same
    numbers; the jagged path just does it through a flattened/packed
    `[B*S, ...]` representation. So swapping in dense SDPA here changes
    nothing about *what* is computed, only how it is represented, and reuses
    every one of `block`'s actual trained submodules/parameters (`norm1`,
    `attn.qkv`, `attn.proj`, `norm2`, `mlp`) unmodified.

    `x`: `[B, S, D]`.
    """
    attn = block.attn
    b, s, d = x.shape
    h, dh = attn.num_heads, attn.head_dim

    residual = x
    y = block.norm1(x)
    qkv = attn.qkv(y).reshape(b, s, 3, h, dh).permute(2, 0, 3, 1, 4)  # [3, B, h, S, dh]
    q, k, v = qkv[0], qkv[1], qkv[2]
    attn_out = F.scaled_dot_product_attention(q, k, v)  # [B, h, S, dh]
    attn_out = attn_out.transpose(1, 2).reshape(b, s, d)
    attn_out = attn.proj(attn_out)
    x = residual + attn_out

    residual = x
    y = block.norm2(x)
    y = block.mlp(y)
    x = residual + y
    return x


def dense_encoder_forward_visible_ids(
    encoder: MaskedEncoder, x_vol: torch.Tensor, visible_ids: torch.Tensor
) -> tuple[torch.Tensor, torch.Tensor | None, torch.Tensor]:
    """Dense-SDPA equivalent of `MaskedEncoder.forward_visible_ids` (see
    `dense_block_forward` for why). Same body, `dense_block_forward` swapped
    in for the block loop; every other line (patchify, patch_embed,
    pos_embed, gather, cat_tokens, norm, chunk_tokens) is untouched
    reference code, called directly on `encoder`'s actual submodules."""
    x = encoder.patchify(x_vol)
    x = encoder.patch_embed(x)
    x = encoder.pos_embed(x)
    x = x.gather(1, visible_ids.unsqueeze(-1).expand(-1, -1, x.shape[-1]))
    x = encoder.cat_tokens(x)
    for block in encoder.blocks:
        x = dense_block_forward(block, x)
    x = encoder.norm(x)
    return encoder.chunk_tokens(x)


def dense_decoder_forward(
    decoder: MaskedDecoder, embeds: torch.Tensor, embed_ids: torch.Tensor, pred_ids: torch.Tensor
) -> torch.Tensor:
    """Dense-SDPA equivalent of `MaskedDecoder.forward` (`embed_token_mask=
    pred_token_mask=None, packed_output=False` -- see `dense_block_forward`
    for why the block loop is swapped)."""
    b, l_vis, _ = embeds.shape
    q_pred = pred_ids.shape[1]
    mask = decoder.mask_token.expand(b, q_pred, -1)
    mask = decoder.pos_embed(mask, pos_ids=pred_ids)

    embeds = decoder.proj(embeds)
    embeds = decoder.pos_embed(embeds, pos_ids=embed_ids)

    x = torch.cat([embeds, mask], dim=1)
    x = decoder.cat_tokens(x)
    for block in decoder.blocks:
        x = dense_block_forward(block, x)
    x = decoder.norm(x)
    _, x = decoder.chunk_tokens(x)

    pred = x[:, l_vis:]
    pred = decoder.head(pred)
    return pred


def numpy_reference_forward_loss(preds: np.ndarray, targets: np.ndarray, voxel_mask: np.ndarray) -> float:
    """Static-shape special case of `model_mae.py::MaskedAutoencoderViT.forward_loss`
    (lines 660-683): every sample contributes exactly `Q_pred` prediction
    slots, so the `scatter_add_`-by-`batch_ids` in the reference degenerates
    to a plain per-sample reduction."""
    scan_errors = ((preds - targets) ** 2 * voxel_mask).sum(axis=(1, 2))
    scan_voxels = voxel_mask.sum(axis=(1, 2))
    if not (scan_voxels > 0).all():
        raise ValueError("numpy_reference_forward_loss: at least one scan has zero valid voxels")
    return float((scan_errors / scan_voxels).mean())
