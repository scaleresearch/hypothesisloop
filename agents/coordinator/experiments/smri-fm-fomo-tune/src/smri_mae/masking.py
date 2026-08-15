import torch
from jaxtyping import Float, Int
from torch import Tensor


def pad_patch_mask(
    patch_mask: Float[Tensor, "B N"],
    mask_ratio: float,
    shuffle: bool = False,
    generator: torch.Generator | None = None,
    pad_to_multiple: int | None = None,
) -> tuple[Float[Tensor, "B N"], Int[Tensor, "B L"], Tensor]:
    """
    Select each row's own mask-ratio count, then pad ids to the batch max length.

    Returns:
    - selected patch mask [B, N]
    - padded selected patch ids [B, Lpad]
    - token mask [B, Lpad], true for real ids and false for padding
    """
    if not 0.0 <= mask_ratio <= 1.0:
        raise ValueError(f"mask_ratio must be in [0, 1], got {mask_ratio}")

    B, N = patch_mask.shape
    device = patch_mask.device
    patch_mask = patch_mask.to(dtype=torch.bool)

    valid_counts = patch_mask.sum(dim=1)
    num_keep = torch.floor(valid_counts.to(torch.float64) * (1.0 - mask_ratio)).to(torch.long)
    if not shuffle:
        selected = patch_mask & (patch_mask.cumsum(dim=1) <= num_keep.unsqueeze(1))
        padded_ids, token_mask = patch_ids_from_mask(
            selected,
            pad_to_multiple=pad_to_multiple,
        )
        return selected, padded_ids, token_mask

    # One masked sort directly produces random valid IDs. The previous
    # shuffle/select/inverse-shuffle path required two full argsorts plus a
    # dynamic nonzero/scatter solely to recover the same selected set.
    noise = torch.rand(B, N, generator=generator, device=device)
    noise.masked_fill_(~patch_mask, torch.inf)
    shuffled_ids = torch.argsort(noise, dim=1)

    max_count = int(num_keep.max().item())
    if pad_to_multiple is not None:
        if pad_to_multiple <= 0:
            raise ValueError(f"pad_to_multiple must be positive, got {pad_to_multiple}")
        max_count = (
            (max_count + pad_to_multiple - 1) // pad_to_multiple * pad_to_multiple
        )
    padded_ids = shuffled_ids[:, :max_count]
    token_mask = torch.arange(max_count, device=device).unsqueeze(0) < num_keep.unsqueeze(1)
    selected = torch.zeros_like(patch_mask).scatter_(1, padded_ids, token_mask)
    return selected, padded_ids, token_mask


def patch_ids_from_mask(
    patch_mask: Tensor,
    pad_to_multiple: int | None = None,
) -> tuple[Int[Tensor, "B L"], Tensor]:
    """Return optionally rounded patch IDs and their token-validity mask."""
    if pad_to_multiple is not None and pad_to_multiple <= 0:
        raise ValueError(f"pad_to_multiple must be positive, got {pad_to_multiple}")

    patch_mask = patch_mask.to(dtype=torch.bool)
    B, N = patch_mask.shape
    device = patch_mask.device
    counts = patch_mask.sum(dim=1)
    max_count = int(counts.max().item())
    if pad_to_multiple is not None:
        max_count = (
            (max_count + pad_to_multiple - 1) // pad_to_multiple * pad_to_multiple
        )

    patch_ids = torch.zeros((B, max_count), dtype=torch.long, device=device)
    token_mask = torch.arange(max_count, device=device).unsqueeze(0) < counts.unsqueeze(1)
    if max_count == 0:
        return patch_ids, token_mask

    batch_ids, selected_ids = patch_mask.nonzero(as_tuple=True)
    slot_ids = patch_mask.cumsum(dim=1)[batch_ids, selected_ids].to(torch.long) - 1
    patch_ids[batch_ids, slot_ids] = selected_ids
    return patch_ids, token_mask
