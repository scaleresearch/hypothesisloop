"""Host-side: one nifti volume -> the statically-shaped patch tensors the TT
encoder consumes.

This is the fomo_tune counterpart of `smri_mae_tt/packing.py`. The two differ
in *which* patches survive, not in the mechanism:

- pretraining drops patches at random (`mask_ratio`), sampling exactly
  `ShapeContract.l_vis` of them per sample so every sample has one fixed shape;
- fomo_tune drops patches *deterministically*, by content: `MaskedEncoder.
  forward`'s `mask` path zeroes voxels outside the brain mask, patchifies the
  mask, and drops every patch whose observed-voxel count is zero
  (`patch_num_obs > 0`), keeping the survivors in ascending index order
  (`pad_patch_mask(..., mask_ratio=0.0, shuffle=False)` -> `patch_ids_from_mask`).

The survivor count is a property of the subject, so it varies. TT-NN needs one
compile-time sequence length, so `pack_subject` pads the token sequence up to a
caller-chosen `l_vis` and returns the key-padding mask that keeps the padded
slots out of every attention softmax. That is exactly what the reference's
`token_mask` does via its jagged batch -- padding here is not an approximation
of the reference, it is the same semantics expressed densely.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import torch
from torch import Tensor

from smri_mae.modules import patchify3d


@dataclass(frozen=True)
class PackedSubject:
    visible_values: np.ndarray  # [1, l_vis, patch_dim] float32, zero-padded past n_visible
    visible_ids: np.ndarray  # [1, l_vis] uint32, id 0 repeated past n_visible
    n_visible: int  # real patches; slots [n_visible:] are padding
    affine: np.ndarray  # (4, 4) float32, the transform's output affine

    @property
    def l_vis(self) -> int:
        return int(self.visible_values.shape[1])


def pack_subject(
    sample: dict[str, Tensor],
    l_vis: int,
    patch_size: int,
    mask_drop_scale: bool,
) -> PackedSubject:
    """`sample` is one `fomo_tune.backbone.SmriMaeTransform` output
    (`image [1, X, Y, Z]`, `mask [1, X, Y, Z]`, `affine [4, 4]`).

    Mirrors `MaskedEncoder.forward(x, mask=mask)` up to (not including) the
    patch-embedding linear: mask the input, patchify, count observed voxels per
    patch, optionally rescale partially-observed patches, keep the patches with
    at least one observed voxel.
    """
    image = sample["image"][None].float()  # [1, 1, X, Y, Z]
    mask = sample["mask"][None].to(torch.bool)

    # `MaskedEncoder.forward`: `x = x.masked_fill(~mask, 0)`. The transform
    # already writes 0 outside the mask, but the reference does not rely on
    # that and neither does this.
    image = image.masked_fill(~mask, 0.0)

    patch = (patch_size,) * 3
    values = patchify3d(image, patch)[0]  # [N, patch_dim]
    obs = patchify3d(mask.float(), patch)[0]  # [N, patch_dim], 1.0 where observed
    patch_dim = values.shape[-1]

    num_obs = obs.sum(dim=-1)
    if mask_drop_scale:
        values = values * (patch_dim / num_obs.unsqueeze(-1).clamp(min=1.0))

    ids = torch.nonzero(num_obs > 0, as_tuple=False).squeeze(-1)  # ascending, as the reference
    n_visible = int(ids.numel())
    if n_visible == 0:
        raise ValueError("subject has zero observed patches; nothing to encode")
    if n_visible > l_vis:
        raise ValueError(
            f"subject has {n_visible} observed patches but the encoder was built for "
            f"l_vis={l_vis}. Rebuild the encoder with a larger l_vis (see "
            "`fomo_tune_tt.scripts.measure_occupancy`) -- not silently truncated, "
            "dropping real patches would change the embedding."
        )

    visible_values = np.zeros((1, l_vis, patch_dim), dtype=np.float32)
    visible_values[0, :n_visible] = values[ids].numpy()
    visible_ids = np.zeros((1, l_vis), dtype=np.uint32)
    visible_ids[0, :n_visible] = ids.numpy().astype(np.uint32)

    return PackedSubject(
        visible_values=visible_values,
        visible_ids=visible_ids,
        n_visible=n_visible,
        affine=np.asarray(sample["affine"], dtype=np.float32),
    )


def key_padding_mask(n_valid_tokens: int, seq_len: int) -> np.ndarray:
    """The `(1, 1, S, S)` bf16 keep-mask the fused SDPA kernel expects: 1.0 on
    key positions that exist, 0.0 on padding (the kernel turns 0.0 into a
    -1e9 score bias -- see `sdpa_compute_utils.hpp::apply_mask_on_reg`).

    Masking columns only, never rows: a padded *query* row still attends
    normally to the real keys and produces a value nobody reads, which keeps
    every softmax denominator non-degenerate. Masking its row too would leave
    it with no unmasked key at all.
    """
    if not 0 < n_valid_tokens <= seq_len:
        raise ValueError(f"n_valid_tokens={n_valid_tokens} out of range for seq_len={seq_len}")
    keep = np.zeros((1, 1, seq_len, seq_len), dtype=np.float32)
    keep[:, :, :, :n_valid_tokens] = 1.0
    return keep
