"""Static shape contract for the TT-NN port.

TT-NN compiles a program per distinct tensor shape, so unlike the PyTorch
reference (which packs a ragged batch with `torch.nested` and runs jagged
Flash SDPA), every sample on TT must contribute exactly the same number of
visible and prediction-query patches. This module is the single source of
truth for those constants, and for deriving them from measured per-shard
patch occupancy.

See TENSTORRENT_PORT.md sections 1 and 3.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

TILE = 32  # ttnn tile height; every sequence length must be a multiple of this.


def round_down_to_multiple(n: int, multiple: int = TILE) -> int:
    return (n // multiple) * multiple


def round_up_to_multiple(n: int, multiple: int = TILE) -> int:
    return ((n + multiple - 1) // multiple) * multiple


@dataclass(frozen=True)
class ShapeContract:
    """Compile-time-constant shapes for one training configuration.

    batch: micro-batch size per device.
    l_vis: visible patches fed to the encoder, per sample (constant across batch).
    q_pred: prediction-query patches fed to the decoder, per sample (constant).
    Every sample contributes exactly l_vis visible and q_pred query patches,
    sampled on the host from its valid set (with replacement if a sample falls
    short -- see `replacement_rate`).
    """

    batch: int
    l_vis: int
    q_pred: int
    mask_ratio: float
    img_size: tuple[int, int, int] = (208, 240, 208)
    patch_size: int = 8
    in_chans: int = 1
    embed_dim: int = 1024
    decoder_embed_dim: int = 512
    encoder_heads: int = 16
    decoder_heads: int = 16
    class_token: bool = True

    def __post_init__(self):
        # NOTE (resolved M2 open question, see TENSTORRENT_PORT.md / project
        # memory "Tenstorrent port"): this used to also require
        # `l_vis % TILE == 0` and `q_pred % TILE == 0` individually. That is
        # not just unnecessary but actively *unsatisfiable* whenever
        # class_token=True (the only setting this project's config ever
        # uses, default_pretrain.yaml): encoder_seq_len = l_vis + 1, and for
        # any integer l_vis, `l_vis % TILE == 0` and `(l_vis + 1) % TILE ==
        # 0` can never both hold (same for decoder_seq_len = l_vis + q_pred
        # + 1). Constructing any ShapeContract with class_token=True
        # unconditionally raised.
        #
        # Measured on real Blackhole hardware
        # (`modules_tt.py`'s module docstring has the exact test): a
        # [B, L_vis=1023, patch_dim] tensor (deliberately not tile-aligned)
        # ran cleanly through `ttml.ops.linear.linear`,
        # `ttml.ops.layernorm.layernorm`, and `ttml.ops.embedding.embedding`
        # -- `ttnn` pads non-tile-aligned intermediate shapes transparently
        # for those ops. Only the fused SDPA kernel actually requires its
        # sequence dimension to be tile-aligned (`ops_tt/attention.py`'s
        # `_validate_seq_len`), and SDPA only ever runs on
        # `encoder_seq_len`/`decoder_seq_len` (after the cls token is
        # concatenated on), never on `l_vis`/`q_pred` directly. So
        # `l_vis`/`q_pred` individually need not be tile-aligned; only the
        # two checks below (on the actual SDPA-facing dimensions) are load
        # -bearing. The individual `l_vis`/`q_pred` checks were removed
        # because they were both redundant and, for this project's actual
        # config, self-contradictory -- not because the shape contract
        # changed.
        if self.encoder_seq_len % TILE:
            raise ValueError(
                f"encoder_seq_len={self.encoder_seq_len} (l_vis={self.l_vis} + "
                f"class_token={int(self.class_token)}) is not a multiple of {TILE}. "
                "Not silently rounded/padded here -- choose an l_vis such that "
                "l_vis + int(class_token) is tile-aligned."
            )
        if self.decoder_seq_len % TILE:
            raise ValueError(
                f"decoder_seq_len={self.decoder_seq_len} (l_vis={self.l_vis} + "
                f"q_pred={self.q_pred} + class_token={int(self.class_token)}) is "
                f"not a multiple of {TILE}."
            )

    @property
    def grid_size(self) -> tuple[int, int, int]:
        t, h, w = self.img_size
        p = self.patch_size
        if t % p or h % p or w % p:
            raise ValueError(f"img_size {self.img_size} not divisible by patch_size {p}")
        return (t // p, h // p, w // p)

    @property
    def num_patches(self) -> int:
        return math.prod(self.grid_size)

    @property
    def patch_dim(self) -> int:
        return self.in_chans * self.patch_size**3

    @property
    def encoder_seq_len(self) -> int:
        return self.l_vis + int(self.class_token)

    @property
    def decoder_seq_len(self) -> int:
        return self.l_vis + self.q_pred + int(self.class_token)


def occupancy_percentiles(valid_patch_counts: np.ndarray) -> dict[str, float]:
    """p1/p50/p99 of per-sample valid-patch counts, for the M1 gate report."""
    return {
        "p1": float(np.percentile(valid_patch_counts, 1)),
        "p50": float(np.percentile(valid_patch_counts, 50)),
        "p99": float(np.percentile(valid_patch_counts, 99)),
        "n": int(valid_patch_counts.shape[0]),
    }


def choose_l_vis(valid_patch_counts: np.ndarray, mask_ratio: float, percentile: float = 1.0) -> int:
    """L_vis = floor(p1_valid * (1 - mask_ratio)), rounded down to a tile multiple,
    so that >= (100 - percentile)% of samples have enough valid patches to fill
    the visible budget without sampling with replacement.

    Raises if the corpus/mask_ratio can't produce a positive tile-aligned
    budget -- that means the occupancy measurement or config is broken, not
    something to silently paper over with an empty batch.
    """
    p = np.percentile(valid_patch_counts, percentile)
    l_vis = round_down_to_multiple(int(math.floor(p * (1.0 - mask_ratio))))
    if l_vis <= 0:
        raise ValueError(
            f"computed l_vis={l_vis} <= 0 from p{percentile}={p} valid patches "
            f"and mask_ratio={mask_ratio}; occupancy measurement or mask_ratio is broken"
        )
    return l_vis


def choose_q_pred(
    valid_patch_counts: np.ndarray,
    mask_ratio: float,
    l_vis: int,
    percentile: float = 1.0,
) -> int:
    """Q_pred = remaining valid candidates after removing L_vis visible patches,
    at the same conservative percentile. This reproduces `pred_mask_ratio: null`
    (decode every non-visible valid patch) as a static budget; see doc section 8
    for lowering this deliberately as a performance lever.

    Raises if L_vis already consumes the p1 budget -- that means L_vis was
    chosen inconsistently with this corpus.
    """
    p = np.percentile(valid_patch_counts, percentile)
    q_pred = round_down_to_multiple(int(math.floor(p)) - l_vis)
    if q_pred <= 0:
        raise ValueError(
            f"computed q_pred={q_pred} <= 0 from p{percentile}={p} valid patches "
            f"and l_vis={l_vis}; l_vis leaves no room for prediction queries"
        )
    return q_pred


def replacement_rate(valid_patch_counts: np.ndarray, mask_ratio: float, budget: int) -> float:
    """Fraction of samples whose own valid-patch keep-count falls short of
    `budget`, i.e. would need sample-with-replacement to fill it.
    """
    keep = np.floor(valid_patch_counts.astype(np.float64) * (1.0 - mask_ratio))
    return float(np.mean(keep < budget))
