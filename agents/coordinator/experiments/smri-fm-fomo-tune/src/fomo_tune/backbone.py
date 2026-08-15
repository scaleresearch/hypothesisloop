import inspect

import nibabel as nib
import numpy as np
import torch
import torch.nn as nn
import torch.nn.functional as F
from einops import rearrange
from torch import Tensor

import smri_mae.model_mae as models_mae


class SmriMaeBackbone(nn.Module):
    grid_coords: Tensor

    def __init__(self, encoder: models_mae.MaskedEncoder):
        super().__init__()
        self.encoder = encoder
        self.img_size = self.encoder.patchify.img_size

        grid_size = self.encoder.patchify.grid_size
        patch_size = np.array(self.encoder.patchify.patch_size)
        grid_coords = rearrange(np.indices(grid_size), "c x y z -> (x y z) c")
        grid_coords = grid_coords * patch_size + (patch_size - 1) / 2
        grid_coords = torch.as_tensor(grid_coords, dtype=torch.float32)
        self.register_buffer("grid_coords", grid_coords)

    def forward(self, batch: dict[str, Tensor]) -> dict[str, Tensor]:
        images = batch["image"]
        mask = batch["mask"]
        affine = batch["affine"]

        B, C, X, Y, Z = images.shape
        assert (X, Y, Z) == self.img_size, f"expected {self.img_size}, got {(X, Y, Z)}"

        _, _, patch_embeds, _, patch_ids, token_mask = self.encoder(images, mask=mask)

        # [B, L, 3] world xyz coords of embeddings
        patch_coords = self.grid_coords[patch_ids, :]
        rot = affine[:, :3, :3]
        trans = affine[:, :3, 3]
        patch_coords = patch_coords @ rot.transpose(1, 2) + trans[:, None, :]

        return {
            "patch_embeds": patch_embeds,
            "patch_ids": patch_ids,
            "token_mask": token_mask,
            "patch_coords": patch_coords,
        }


class SmriMaeTransform:
    def __init__(
        self,
        img_size: tuple[int, int, int] = (208, 240, 208),
        spacing: tuple[float, float, float] = (1.0, 1.0, 1.0),
    ):
        self.img_size = img_size
        self.spacing = spacing

    def __call__(self, img: nib.Nifti1Image) -> dict[str, Tensor]:
        # repack image to handle incomplete hf Nifti interface
        img = nib.Nifti1Image(img.dataobj, img.affine, img.header)
        # a converter's trailing singleton axis is common and would otherwise reach `fit_to_shape`
        img = nib.funcs.squeeze_image(img)
        img = nib.as_closest_canonical(img)

        data = torch.from_numpy(np.ascontiguousarray(img.get_fdata(dtype=np.float32)))
        assert data.ndim == 3, f"expected a 3D volume, got {tuple(data.shape)}"
        affine = np.asarray(img.affine)

        spacing = img.header.get_zooms()
        if max(abs(s - s_) for s, s_ in zip(spacing, self.spacing)) > 0.05:
            data, affine = rescale(data, affine, spacing, self.spacing)

        data, affine = fit_to_shape(data, affine, target_shape=self.img_size)

        # mean threshold, not the SynthSeg mask used in pretraining, so skull and neck stay in
        mask = data > data.mean()
        brain = data[mask]
        mean = brain.mean()
        # population std (correction=0) to match the pretraining normalization
        std = brain.std(correction=0).clamp_min(1e-6)
        data = torch.where(mask, (data - mean) / std, 0.0)

        return {
            "image": data.unsqueeze(0),
            "mask": mask.unsqueeze(0),
            "affine": torch.as_tensor(affine, dtype=torch.float32),
        }


def rescale(
    x: torch.Tensor,
    affine: np.ndarray,
    spacing: tuple[float, ...],
    target_spacing: tuple[float, ...] = (1.0, 1.0, 1.0),
) -> tuple[torch.Tensor, np.ndarray]:
    scales = tuple([current / target for current, target in zip(spacing, target_spacing)])
    resampled = F.interpolate(x[None, None], scale_factor=scales, mode="trilinear").squeeze(0, 1)

    # align_corners=False reads output voxel j from input voxel (j + 0.5) / scale - 0.5
    scale = np.asarray(scales, dtype=float)
    step = np.diag([*(1 / scale), 1.0])
    step[:3, 3] = 0.5 / scale - 0.5
    return resampled, affine @ step


def fit_to_shape(
    x: torch.Tensor, affine: np.ndarray, target_shape: tuple[int, ...]
) -> tuple[torch.Tensor, np.ndarray]:
    """Centre the volume in `target_shape`, padding the short axes and cropping the long ones."""
    pads = [target - size for size, target in zip(x.shape, target_shape)]
    padding = [side for pad in reversed(pads) for side in (pad // 2, pad - pad // 2)]

    # a crop is a negative pad, so output voxel k came from input voxel k - pad // 2 either way
    step = np.eye(4)
    step[:3, 3] = [-(pad // 2) for pad in pads]
    return F.pad(x, padding), affine @ step


def resolve_ckpt(ckpt_path: str) -> str:
    """A local path for a checkpoint, downloading it if it is an hf://<org>/<repo>/<file> URI."""
    from huggingface_hub import hf_hub_download

    if ckpt_path.startswith("hf://"):
        org, repo, *rest = ckpt_path.removeprefix("hf://").split("/")
        return hf_hub_download(f"{org}/{repo}", "/".join(rest))

    return ckpt_path


def load_backbone(ckpt_path: str) -> tuple[SmriMaeBackbone, SmriMaeTransform]:
    path = resolve_ckpt(ckpt_path)
    ckpt = torch.load(path, map_location="cpu", weights_only=True, mmap=True)
    args = ckpt["args"]

    model_fn = models_mae.__dict__[args["model"]]
    model: models_mae.MaskedAutoencoderViT = model_fn(
        img_size=args["img_size"],
        in_chans=args.get("in_chans", 1),
        patch_size=args["patch_size"],
        # older checkpoints carry training flags the current model_mae no longer takes
        **filter_kwargs(models_mae.MaskedAutoencoderViT, args.get("model_kwargs") or {}),
    )
    model.load_state_dict(ckpt["model"])
    backbone = SmriMaeBackbone(model.encoder)
    transform = SmriMaeTransform(img_size=args["img_size"])
    return backbone, transform


def filter_kwargs(func, kwargs):
    signature = inspect.signature(func)
    kwargs = {k: v for k, v in kwargs.items() if k in signature.parameters}
    return kwargs
