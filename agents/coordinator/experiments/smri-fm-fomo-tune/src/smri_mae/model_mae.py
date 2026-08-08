# Copyright (c) Sophont, Inc
# This source code is licensed under the Apache License, Version 2.0
#
# References:
# capi: https://github.com/facebookresearch/capi/blob/main/model.py
# timm: https://github.com/huggingface/pytorch-image-models/blob/v1.0.20/timm/models/vision_transformer.py

"""
From-scratch re-implementation of the original MAE model.

MaskedEncoder: standard ViT with masking
MaskedDecoder: standard self-attention MAE decoder
MaskedAutoEncoderViT: full MAE model for 3D structural MRI volumes
"""

from collections.abc import Sequence
from typing import Literal

import torch
import torch.nn as nn
import torch.nn.functional as F
from torch import Tensor
from torch.utils.checkpoint import checkpoint
from huggingface_hub import PyTorchModelHubMixin
from jaxtyping import Float, Int

from .modules import (
    AbsolutePosEmbed,
    Block,
    LayerNorm,
    Normalize,
    JaggedBatch,
    Patchify3D,
    SeparablePosEmbed,
    SinCosPosEmbed3D,
    unpack_tokens,
)
from .masking import pad_patch_mask


class MaskedEncoder(nn.Module):
    """
    Masked transformer encoder.
    """

    def __init__(
        self,
        patchify: nn.Module,
        patch_embed: nn.Module,
        pos_embed: nn.Module,
        depth: int = 12,
        embed_dim: int = 768,
        num_heads: int = 12,
        qkv_bias: bool = True,
        proj_bias: bool = True,
        mlp_ratio: int | float = 4,
        class_token: bool = True,
        reg_tokens: int = 0,
        no_embed_class: bool = False,
        final_norm: bool = True,
        drop_path_rate: float = 0.0,
        mask_drop_scale: bool = False,
    ):
        super().__init__()
        self.num_prefix_tokens = int(class_token) + reg_tokens
        self.num_reg_tokens = reg_tokens
        self.has_class_token = class_token
        self.no_embed_class = no_embed_class

        # scale inputs by 1 / observed rate (like dropout)
        self.mask_drop_scale = mask_drop_scale

        # inject tokenization modules, so that the encoder doesn't specifically need to
        # know how the data are tokenized, while still implementing a complete
        # self-contained model.
        self.patchify = patchify
        self.patch_embed = patch_embed
        self.pos_embed = pos_embed

        R = reg_tokens
        self.cls_token = nn.Parameter(torch.empty(1, 1, embed_dim)) if class_token else None
        self.reg_token = nn.Parameter(torch.empty(1, R, embed_dim)) if reg_tokens else None

        if not no_embed_class:
            self.cls_token_pos = nn.Parameter(torch.empty(1, 1, embed_dim)) if class_token else None
            self.reg_token_pos = nn.Parameter(torch.empty(1, R, embed_dim)) if reg_tokens else None
        else:
            self.cls_token_pos = self.reg_token_pos = None

        # stochastic depth decay rule
        dpr = [x.item() for x in torch.linspace(0, drop_path_rate, depth)]

        self.blocks = nn.ModuleList(
            [
                Block(
                    dim=embed_dim,
                    num_heads=num_heads,
                    qkv_bias=qkv_bias,
                    proj_bias=proj_bias,
                    mlp_ratio=mlp_ratio,
                    drop_path=dpr[ii],
                )
                for ii in range(depth)
            ]
        )

        self.norm = LayerNorm(embed_dim) if final_norm else nn.Identity()

        self.reset_parameters()

    def extra_repr(self):
        return (
            f"class_token={self.has_class_token}, reg_tokens={self.num_reg_tokens}, "
            f"no_embed_class={self.no_embed_class}, mask_drop_scale={self.mask_drop_scale}"
        )

    def reset_parameters(self) -> None:
        for p in [self.cls_token, self.cls_token_pos, self.reg_token, self.reg_token_pos]:
            if p is not None:
                nn.init.trunc_normal_(p, std=0.02)

    def cat_tokens(self, x: Tensor) -> Tensor:
        # prepend cls and reg tokens with optional learned position embedding
        # the cls and reg pos embedding is ofc redundant, but included in many other
        # implementations.
        B, _, _ = x.shape

        to_cat = []
        if self.has_class_token:
            cls_token = self.cls_token
            if not self.no_embed_class:
                cls_token = cls_token + self.cls_token_pos
            to_cat.append(cls_token.expand(B, -1, -1))

        if self.num_reg_tokens:
            reg_token = self.reg_token
            if not self.no_embed_class:
                reg_token = reg_token + self.reg_token_pos
            to_cat.append(reg_token.expand(B, -1, -1))

        if to_cat:
            x = torch.cat(to_cat + [x], dim=1)
        return x

    def cat_token_mask(self, token_mask: Tensor, batch_size: int) -> Tensor:
        if self.num_prefix_tokens:
            prefix_mask = torch.ones(
                (batch_size, self.num_prefix_tokens),
                dtype=torch.bool,
                device=token_mask.device,
            )
            token_mask = torch.cat([prefix_mask, token_mask], dim=1)
        return token_mask

    def chunk_tokens(self, x: Tensor) -> tuple[Tensor | None, Tensor | None, Tensor]:
        cls_offset = int(self.has_class_token)
        cls = x[:, :cls_offset] if self.has_class_token else None
        if self.num_reg_tokens:
            reg = x[:, cls_offset : self.num_prefix_tokens, :]
        else:
            reg = None
        patch = x[:, self.num_prefix_tokens :, :]
        return cls, reg, patch

    def forward(
        self,
        x: Tensor,
        mask: Tensor | None = None,
        mask_ratio: float | None = None,
        pad_to_multiple: int | None = None,
    ) -> tuple[
        Float[Tensor, "B 1 D"] | None,
        Float[Tensor, "B R D"] | None,
        Float[Tensor, "B L D"],
        Tensor | None,
        Int[Tensor, "B L"] | None,
        Tensor | None,
    ]:
        """
        x: input data shape [B, C, D, H, W] 
        mask: visible mask, 1 = visible, 0 = invisible. broadcastable shape
        mask_ratio: mask ratio for uniform random masking

        returns:
        - cls_embeds: [B, 1, D]
        - reg_embeds: [B, R, D]
        - patch_embeds: [B, L, D], where L is the number of visible patches
        - mask: observed mask, 1 = observed, 0 = unobserved. same shape as input
        - mask_ids: indices of visible patches [B L]
        - token_mask: valid token mask for padded per-sample masking [B L]
        """
        # apply mask to the input
        if mask is not None:
            mask = mask.to(device=x.device, dtype=torch.bool).expand_as(x)
            x = x.masked_fill(~mask, 0)

        # patchify input
        x = self.patchify(x)
        B, N, P = x.shape

        # patchify mask and apply dropout style scaling
        if mask is not None:
            mask_patches = self.patchify(mask)
            patch_num_obs = mask_patches.sum(dim=-1)
            patch_mask = patch_num_obs > 0
            if self.mask_drop_scale:
                patch_num_obs = patch_num_obs.to(x.dtype)
                x = x * (P / patch_num_obs.unsqueeze(-1).clamp(min=1.0))
        elif mask_ratio is not None:
            patch_mask = torch.ones((B, N), dtype=torch.bool, device=x.device)
            mask_patches = patch_mask.unsqueeze(-1).expand(-1, -1, P)
        else:
            patch_mask = mask_patches = None

        # patch and position embed
        x = self.patch_embed(x)
        x = self.pos_embed(x)

        if mask is not None or mask_ratio is not None:
            mask_ratio = 0.0 if mask_ratio is None else mask_ratio
            patch_mask, mask_ids, token_mask = pad_patch_mask(
                patch_mask,
                mask_ratio=mask_ratio,
                shuffle=mask_ratio > 0,
                pad_to_multiple=pad_to_multiple,
            )

            mask_patches = mask_patches & patch_mask.unsqueeze(-1)
            mask = self.patchify.unpatchify(mask_patches)
            x = x.gather(1, mask_ids.unsqueeze(-1).expand(-1, -1, x.shape[-1]))
        else:
            mask_ids = None
            token_mask = None

        cls_embeds, reg_embeds, patch_embeds = self.forward_patch_embeds(
            x,
            token_mask=token_mask,
        )
        return cls_embeds, reg_embeds, patch_embeds, mask, mask_ids, token_mask

    def forward_patch_embeds(
        self,
        x: Float[Tensor, "B L D"],
        token_mask: Tensor | None = None,
    ) -> tuple[
        Float[Tensor, "B 1 D"] | None,
        Float[Tensor, "B R D"] | None,
        Float[Tensor, "B L D"],
    ]:
        B = x.shape[0]
        if token_mask is None:
            token_mask = torch.ones(x.shape[:2], dtype=torch.bool, device=x.device)
        x = self.cat_tokens(x)
        token_mask = self.cat_token_mask(token_mask, B)
        jagged_batch = JaggedBatch.from_mask(token_mask)
        x = x[token_mask]
        for block in self.blocks:
            x = block(x, jagged_batch=jagged_batch)
        x = self.norm(x)
        x = unpack_tokens(x, token_mask)

        cls_embeds, reg_embeds, patch_embeds = self.chunk_tokens(x)
        return cls_embeds, reg_embeds, patch_embeds

    def forward_visible_ids(
        self,
        x: Tensor,
        visible_ids: Int[Tensor, "B L"],
        img_mask: Tensor | None = None,
    ) -> tuple[
        Float[Tensor, "B 1 D"] | None,
        Float[Tensor, "B R D"] | None,
        Float[Tensor, "B L D"],
    ]:
        if img_mask is not None:
            img_mask = img_mask.to(device=x.device, dtype=torch.bool).expand_as(x)
            x = x.masked_fill(~img_mask, 0)

        x = self.patchify(x)
        if self.mask_drop_scale and img_mask is not None:
            mask_patches = self.patchify(img_mask)
            patch_num_obs = mask_patches.sum(dim=-1).to(x.dtype)
            x = x * (self.patchify.patch_dim / patch_num_obs.unsqueeze(-1).clamp(min=1.0))
        x = self.patch_embed(x)
        x = self.pos_embed(x)
        visible_ids = visible_ids.to(device=x.device)
        x = x.gather(1, visible_ids.unsqueeze(-1).expand(-1, -1, x.shape[-1]))
        return self.forward_patch_embeds(x)

    def forward_embedding(
        self,
        x: Tensor,
        mask: Tensor | None = None,
        mask_ratio: float | None = None,
    ):
        cls_embeds, reg_embeds, patch_embeds, *_ = self.forward(
            x,
            mask=mask,
            mask_ratio=mask_ratio,
        )
        return cls_embeds, reg_embeds, patch_embeds


class MaskedDecoder(nn.Module):
    """Self-attention MAE decoder supporting sparse subset decoding via pred_ids."""

    def __init__(
        self,
        pos_embed: nn.Module,
        head: nn.Module | None = None,
        input_dim: int | None = None,
        depth: int = 12,
        embed_dim: int = 768,
        num_heads: int = 12,
        qkv_bias: bool = True,
        proj_bias: bool = True,
        mlp_ratio: int | float = 4,
        class_token: bool = True,
        no_embed_class: bool = False,
        final_norm: bool = True,
    ):
        super().__init__()
        input_dim = embed_dim if input_dim is None else input_dim
        self.has_class_token = class_token
        self.no_embed_class = no_embed_class

        self.cls_token = nn.Parameter(torch.empty(1, 1, embed_dim)) if class_token else None
        self.cls_token_pos = (
            nn.Parameter(torch.empty(1, 1, embed_dim))
            if class_token and not no_embed_class
            else None
        )
        self.mask_token = nn.Parameter(torch.empty(1, 1, embed_dim))

        # decoder position embedding, encodes query position information into masks
        self.pos_embed = pos_embed

        self.proj = nn.Identity() if input_dim == embed_dim else nn.Linear(input_dim, embed_dim)

        self.blocks = nn.ModuleList(
            [
                Block(
                    dim=embed_dim,
                    num_heads=num_heads,
                    qkv_bias=qkv_bias,
                    proj_bias=proj_bias,
                    mlp_ratio=mlp_ratio,
                )
                for _ in range(depth)
            ]
        )

        self.norm = LayerNorm(embed_dim) if final_norm else nn.Identity()

        # optional injected prediction head
        self.head = nn.Identity() if head is None else head

        self.reset_parameters()

    def extra_repr(self):
        return f"class_token={self.has_class_token}, no_embed_class={self.no_embed_class}"

    def reset_parameters(self) -> None:
        # official mae initializes decoder cls token to zeros
        # although perhaps this was an oversight
        if self.cls_token is not None:
            nn.init.zeros_(self.cls_token)
        if self.cls_token_pos is not None:
            nn.init.trunc_normal_(self.cls_token_pos, std=0.02)
        nn.init.trunc_normal_(self.mask_token, std=0.02)

    def cat_tokens(self, x: Tensor) -> Tensor:
        if not self.has_class_token:
            return x
        cls_token = self.cls_token
        if not self.no_embed_class:
            cls_token = cls_token + self.cls_token_pos
        return torch.cat([cls_token.expand(x.shape[0], -1, -1), x], dim=1)

    def cat_token_mask(self, token_mask: Tensor, batch_size: int) -> Tensor:
        if self.has_class_token:
            cls_mask = torch.ones(
                (batch_size, 1),
                dtype=torch.bool,
                device=token_mask.device,
            )
            token_mask = torch.cat([cls_mask, token_mask], dim=1)
        return token_mask

    def chunk_tokens(self, x: Tensor) -> tuple[Tensor | None, Tensor]:
        cls_offset = int(self.has_class_token)
        cls = x[:, :cls_offset] if self.has_class_token else None
        patch = x[:, cls_offset:, :]
        return cls, patch

    def forward(
        self,
        embeds: Float[Tensor, "B L D"],
        embed_ids: Int[Tensor, "B L"] | None = None,
        pred_ids: Int[Tensor, "B Q"] | None = None,
        embed_token_mask: Tensor | None = None,
        pred_token_mask: Tensor | None = None,
        packed_output: bool = False,
    ) -> Float[Tensor, "B Q P"] | Float[Tensor, "T P"]:
        """
        embeds: input patch embeddings.
        embed_ids: optional patch indices for input embeddings. If not provided, no
            position will be added to the embeddings.
        pred_ids: patch indices of query mask positions. If None, decode *all* patches.

        returns:
        - pred [B, Q, P] where Q is the number of prediction patches and P is the output
            dimension
        """
        B, L, _ = embeds.shape

        Q = self.pos_embed.num_patches if pred_ids is None else pred_ids.shape[1]
        mask = self.mask_token.expand(B, Q, -1)
        mask = self.pos_embed(mask, pos_ids=pred_ids)

        embeds = self.proj(embeds)

        if embed_ids is not None:
            embeds = self.pos_embed(embeds, pos_ids=embed_ids)
        if embed_token_mask is None:
            embed_token_mask = torch.ones((B, L), dtype=torch.bool, device=embeds.device)
        if pred_token_mask is None:
            pred_token_mask = torch.ones((B, Q), dtype=torch.bool, device=embeds.device)
        x = torch.cat([embeds, mask], dim=1)
        token_mask = torch.cat([embed_token_mask, pred_token_mask], dim=1)

        x = self.cat_tokens(x)
        token_mask = self.cat_token_mask(token_mask, B)
        jagged_batch = JaggedBatch.from_mask(token_mask)
        x = x[token_mask]
        # Keep headroom for rare maximum-length PSP batches.
        checkpoint_start = max(0, len(self.blocks) - 2)
        for block_index, block in enumerate(self.blocks):
            if self.training and torch.is_grad_enabled() and block_index >= checkpoint_start:
                x = checkpoint(block, x, jagged_batch, use_reentrant=False)
            else:
                x = block(x, jagged_batch=jagged_batch)

        x = self.norm(x)
        if packed_output:
            pred_offset = int(self.has_class_token) + L
            prediction_mask = F.pad(pred_token_mask, (pred_offset, 0))
            return self.head(x[prediction_mask[token_mask]])

        x = unpack_tokens(x, token_mask)
        _, x = self.chunk_tokens(x)

        pred = x[:, L:]
        pred = pred.masked_fill(~pred_token_mask.unsqueeze(-1), 0)
        pred = self.head(pred)
        return pred


class MaskedAutoencoderViT(nn.Module, PyTorchModelHubMixin):
    def __init__(
        self,
        img_size: int | tuple[int, int, int] = (208, 240, 208),
        patch_size: int | tuple[int, int, int] = (16, 16, 16),
        in_chans: int = 1,
        depth: int = 12,
        embed_dim: int = 768,
        num_heads: int = 12,
        decoder_depth: int = 4,
        decoder_embed_dim: int | None = 512,
        decoder_num_heads: int | None = 16,  # default from mae, head dim = 32
        qkv_bias: bool = True,
        proj_bias: bool = True,
        mlp_ratio: int | float = 4,
        class_token: bool = True,
        reg_tokens: int = 0,
        no_embed_class: bool = False,
        drop_path_rate: float = 0.0,
        mask_drop_scale: bool = False,
        no_decode_pos: bool = False,
        pos_embed: Literal["abs", "sep", "sincos"] = "sincos",
        target_norm: Literal["none", "global", "slice", "patch"] | None = None,
    ):
        super().__init__()
        img_size = _to_3d_tuple(img_size, "img_size")
        patch_size = _to_3d_tuple(patch_size, "patch_size")

        self.no_decode_pos = no_decode_pos  # don't pos encode embeddings in decoder

        # patchify reshapes input into sequence of flattened patches, shape [B, N, P]
        ndim = 3
        patchify = Patchify3D(img_size, patch_size, in_chans=in_chans)

        # linear patch embedding P -> D
        patch_embed = nn.Linear(patchify.patch_dim, embed_dim)

        # position embedding
        # separable position embedding decouples the first spatial axis from the
        # others. Fixed sin/cos embeddings are the default for sMRI volumes.
        if pos_embed == "sincos":
            pos_embed_layer = SinCosPosEmbed3D
        else:
            pos_embed_layer = {"abs": AbsolutePosEmbed, "sep": SeparablePosEmbed}[pos_embed]
        pos_embed = pos_embed_layer(embed_dim, patchify.grid_size)

        # encoder. for inference, this model can be extracted and used like a regular vit
        self.encoder = MaskedEncoder(
            patchify=patchify,
            patch_embed=patch_embed,
            pos_embed=pos_embed,
            depth=depth,
            embed_dim=embed_dim,
            num_heads=num_heads,
            qkv_bias=qkv_bias,
            proj_bias=proj_bias,
            mlp_ratio=mlp_ratio,
            class_token=class_token,
            reg_tokens=reg_tokens,
            no_embed_class=no_embed_class,
            drop_path_rate=drop_path_rate,
            mask_drop_scale=mask_drop_scale,
        )

        self.pred_patchify = patchify

        # fall back to encoder architecture width
        decoder_embed_dim = decoder_embed_dim or embed_dim
        decoder_num_heads = decoder_num_heads or num_heads

        decoder_pos_embed = pos_embed_layer(decoder_embed_dim, self.pred_patchify.grid_size)
        # we might want to try tying the weights of the prediction head to the patch
        # embedding at some point.
        decoder_head = nn.Linear(decoder_embed_dim, self.pred_patchify.patch_dim)

        self.decoder = MaskedDecoder(
            pos_embed=decoder_pos_embed,
            head=decoder_head,
            input_dim=embed_dim,
            depth=decoder_depth,
            embed_dim=decoder_embed_dim,
            num_heads=decoder_num_heads,
            qkv_bias=qkv_bias,
            proj_bias=proj_bias,
            mlp_ratio=mlp_ratio,
            class_token=class_token,
            no_embed_class=no_embed_class,
        )

        # mae style target normalization
        # dim is relative to an unflattened embedding tensor of shape [B, *grid_size, D]
        if target_norm not in {"none", None}:
            norm_dim = {
                "global": tuple(range(1, ndim + 2)),  # full sequence
                "slice": tuple(range(2, ndim + 2)),  # each depth slice along first dim
                "patch": -1,  # normalize each patch independently (mae pix norm loss)
            }[target_norm]
            self.target_norm = Normalize(self.pred_patchify.grid_size, dim=norm_dim)
        else:
            self.target_norm = None

        self.init_weights()

    def extra_repr(self):
        return f"no_decode_pos={self.no_decode_pos}"

    def init_weights(self):
        self.apply(_init_weights)

    def prepare_targets(self, images: Tensor, img_mask: Tensor | None):
        """
        images: [B, C, D, H, W]
        img_mask: mask of valid data. only used for computing correct normalization
            stats. same shape as images.
        """
        targets_patches = self.pred_patchify(images)  # [B, N, P]

        # target normalization
        if self.target_norm is not None:
            # full image data mask used for normalization stats only
            if img_mask is not None:
                img_mask_patches = self.pred_patchify(img_mask)
            else:
                img_mask_patches = None
            targets_patches, *targets_stats = self.target_norm(targets_patches, img_mask_patches)
        else:
            targets_stats = None

        return targets_patches, targets_stats

    def prepare_masks(
        self,
        img_mask: Tensor,
        visible_mask: Tensor | None,
        pred_mask: Tensor | None,
        device: torch.device,
    ):
        img_mask = img_mask.to(device=device, dtype=torch.bool)

        if visible_mask is None:
            visible_mask = img_mask
        else:
            visible_mask = img_mask & visible_mask.to(device=device, dtype=torch.bool)

        if pred_mask is None:
            pred_mask = img_mask
        else:
            pred_mask = img_mask & pred_mask.to(device=device, dtype=torch.bool)

        return img_mask, visible_mask, pred_mask

    def prepare_pred_mask(
        self,
        visible_mask: Tensor,
        pred_mask: Tensor | None = None,
        pred_mask_ratio: float | None = None,
        pad_to_multiple: int | None = None,
    ):
        """
        prepare prediction mask by removing visible content
        visible_mask: [B, C, D, H, W], 1 = visible, 0 = invisible
        pred_mask: same shape, 1 = predict, 0 = don't predict
        """
        if pred_mask is None:
            pred_mask = torch.ones_like(visible_mask)

        pred_mask = pred_mask & ~visible_mask

        pred_mask_patches = self.pred_patchify(pred_mask)
        pred_patch_mask = pred_mask_patches.any(dim=-1)
        # Optionally subsample the prediction candidates.
        mask_ratio = 0.0 if pred_mask_ratio is None else pred_mask_ratio
        pred_patch_mask, pred_ids, pred_token_mask = pad_patch_mask(
            pred_patch_mask,
            mask_ratio=mask_ratio,
            # With per-sample padding every candidate is retained when the ratio
            # is zero, so randomizing their order is pure overhead.
            shuffle=mask_ratio > 0,
            pad_to_multiple=pad_to_multiple,
        )
        pred_mask_patches = pred_mask_patches & pred_patch_mask.unsqueeze(-1)
        return pred_mask_patches, pred_ids, pred_token_mask

    def forward_decoder(
        self,
        patch_embeds: Float[Tensor, "B L D"],
        visible_ids: Int[Tensor, "B L"],
        pred_ids: Int[Tensor, "B Q"] | None,
        visible_token_mask: Tensor | None = None,
        pred_token_mask: Tensor | None = None,
        packed_output: bool = False,
    ) -> Float[Tensor, "B Q P"] | Float[Tensor, "T P"]:
        return self.decoder(
            patch_embeds,
            embed_ids=None if self.no_decode_pos else visible_ids,
            pred_ids=pred_ids,
            embed_token_mask=visible_token_mask,
            pred_token_mask=pred_token_mask,
            packed_output=packed_output,
        )

    def forward_loss(
        self,
        preds: Float[Tensor, "T P"],
        targets_patches: Float[Tensor, "B N P"],
        pred_mask_patches: Float[Tensor, "B N P"],
        pred_ids: Int[Tensor, "B Q"],
        pred_token_mask: Tensor,
    ) -> Tensor:
        """Average valid-voxel MSE within each scan, then average across scans."""
        batch_ids, slot_ids = pred_token_mask.nonzero(as_tuple=True)
        patch_ids = pred_ids[batch_ids, slot_ids]
        targets = targets_patches[batch_ids, patch_ids]
        voxel_mask = pred_mask_patches[batch_ids, patch_ids]

        patch_errors = ((preds - targets) ** 2 * voxel_mask).sum(dim=1)
        patch_voxels = voxel_mask.sum(dim=1).to(dtype=patch_errors.dtype)
        batch_size = targets_patches.shape[0]
        scan_errors = patch_errors.new_zeros(batch_size).scatter_add_(
            0, batch_ids, patch_errors
        )
        scan_voxels = patch_voxels.new_zeros(batch_size).scatter_add_(
            0, batch_ids, patch_voxels
        )
        return (scan_errors / scan_voxels).mean()

    @torch.no_grad()
    def forward_pred_images(
        self,
        preds: Float[Tensor, "B Q P"],
        pred_ids: Int[Tensor, "B Q"],
        pred_token_mask: Tensor | None = None,
        img_mask: Tensor | None = None,
        targets_stats: tuple[Tensor, Tensor] | None = None,
    ) -> Tensor:
        B, _, P = preds.shape
        N = self.pred_patchify.num_patches
        if pred_token_mask is not None:
            preds = preds.masked_fill(~pred_token_mask.unsqueeze(-1), 0)

        preds = torch.zeros((B, N, P), dtype=preds.dtype, device=preds.device).scatter_add_(
            1, pred_ids.unsqueeze(-1).expand(-1, -1, P), preds
        )

        if targets_stats is not None:
            targets_mean, targets_std = targets_stats
            preds = preds * targets_std + targets_mean

        pred_images = self.pred_patchify.unpatchify(preds)
        if img_mask is not None:
            pred_images = pred_images.masked_fill(~img_mask, 0)
        return pred_images

    def forward(
        self,
        images: Tensor,
        img_mask: Tensor,
        mask_ratio: float,
        pred_mask_ratio: float | None = None,
        pad_to_multiple: int | None = None,
        with_state: bool = True,
    ) -> Tensor | tuple[Tensor, dict]:
        img_mask, visible_mask, pred_mask = self.prepare_masks(
            img_mask,
            None,
            None,
            device=images.device,
        )
        targets_patches, targets_stats = self.prepare_targets(images, img_mask)

        (
            cls_embeds,
            reg_embeds,
            patch_embeds,
            visible_mask,
            visible_ids,
            visible_token_mask,
        ) = self.encoder(
            images,
            mask=visible_mask,
            mask_ratio=mask_ratio,
            pad_to_multiple=pad_to_multiple,
        )

        pred_mask_patches, pred_ids, pred_token_mask = self.prepare_pred_mask(
            visible_mask,
            pred_mask=pred_mask,
            pred_mask_ratio=pred_mask_ratio,
            pad_to_multiple=pad_to_multiple,
        )

        preds = self.forward_decoder(
            patch_embeds,
            visible_ids,
            pred_ids,
            visible_token_mask=visible_token_mask,
            pred_token_mask=pred_token_mask,
            packed_output=not with_state,
        )

        loss_preds = preds if not with_state else preds[pred_token_mask]
        loss = self.forward_loss(
            loss_preds,
            targets_patches,
            pred_mask_patches,
            pred_ids,
            pred_token_mask,
        )

        if not with_state:
            return loss

        pred_mask = self.pred_patchify.unpatchify(pred_mask_patches)
        pred_images = self.forward_pred_images(
            preds,
            pred_ids,
            pred_token_mask=pred_token_mask,
            img_mask=img_mask,
            targets_stats=targets_stats,
        )

        state = {
            "targets_patches": targets_patches,
            "targets_stats": targets_stats,
            "patch_embeds": patch_embeds,
            "cls_embeds": cls_embeds,
            "reg_embeds": reg_embeds,
            "visible_mask": visible_mask,
            "visible_ids": visible_ids,
            "visible_token_mask": visible_token_mask,
            "pred_mask": pred_mask,
            "pred_ids": pred_ids,
            "pred_token_mask": pred_token_mask,
            "preds": preds,
            "pred_images": pred_images,
        }
        return loss, state

    def forward_embedding(
        self,
        x: Tensor,
        mask: Tensor | None = None,
        mask_ratio: float | None = None,
    ):
        return self.encoder.forward_embedding(x, mask=mask, mask_ratio=mask_ratio)


class MaskedViT(MaskedEncoder, PyTorchModelHubMixin):
    def __init__(
        self,
        img_size: int | tuple[int, int, int] = (208, 240, 208),
        in_chans: int = 1,
        patch_size: int | tuple[int, int, int] = (16, 16, 16),
        depth: int = 12,
        embed_dim: int = 768,
        num_heads: int = 12,
        qkv_bias: bool = True,
        proj_bias: bool = True,
        mlp_ratio: int | float = 4,
        class_token: bool = True,
        reg_tokens: int = 0,
        no_embed_class: bool = False,
        final_norm: bool = True,
        drop_path_rate: float = 0.0,
        mask_drop_scale: bool = False,
        pos_embed: Literal["abs", "sep", "sincos"] = "sincos",
    ):
        img_size = _to_3d_tuple(img_size, "img_size")
        patch_size = _to_3d_tuple(patch_size, "patch_size")

        patchify = Patchify3D(img_size, patch_size, in_chans=in_chans)
        patch_embed = nn.Linear(patchify.patch_dim, embed_dim)
        if pos_embed == "sincos":
            pos_embed_layer = SinCosPosEmbed3D
        else:
            pos_embed_layer = {"abs": AbsolutePosEmbed, "sep": SeparablePosEmbed}[pos_embed]
        pos_embed = pos_embed_layer(embed_dim, patchify.grid_size)

        super().__init__(
            patchify=patchify,
            patch_embed=patch_embed,
            pos_embed=pos_embed,
            depth=depth,
            embed_dim=embed_dim,
            num_heads=num_heads,
            qkv_bias=qkv_bias,
            proj_bias=proj_bias,
            mlp_ratio=mlp_ratio,
            class_token=class_token,
            reg_tokens=reg_tokens,
            no_embed_class=no_embed_class,
            final_norm=final_norm,
            drop_path_rate=drop_path_rate,
            mask_drop_scale=mask_drop_scale,
        )

        self.init_weights()

    def init_weights(self):
        self.apply(_init_weights)


def _to_3d_tuple(value: int | Sequence[int], name: str) -> tuple[int, int, int]:
    if isinstance(value, int):
        return (value, value, value)
    if len(value) != 3:
        raise ValueError(f"{name} must have exactly 3 spatial dimensions, got {tuple(value)}")
    return tuple(int(item) for item in value)


# JAX ViT xavier uniform init
# https://github.com/facebookresearch/capi/blob/main/model.py
def _init_weights(m: nn.Module) -> None:
    if isinstance(m, nn.Linear):
        nn.init.xavier_uniform_(m.weight)
        if m.bias is not None:
            nn.init.constant_(m.bias, 0)
    elif isinstance(m, nn.LayerNorm) and m.elementwise_affine:
        nn.init.constant_(m.weight, 1.0)
        if m.bias is not None:
            nn.init.constant_(m.bias, 0)


def _create_vit(**kwargs):
    model = MaskedViT(**kwargs)
    return model


def _create_mae_vit(**kwargs):
    model = MaskedAutoencoderViT(**kwargs)
    return model


def mae_vit_small(**kwargs):
    model_args = dict(embed_dim=384, depth=12, num_heads=6)
    return _create_mae_vit(**model_args, **kwargs)


def mae_vit_base(**kwargs):
    model_args = dict(embed_dim=768, depth=12, num_heads=12)
    return _create_mae_vit(**model_args, **kwargs)


def mae_vit_large(**kwargs):
    model_args = dict(embed_dim=1024, depth=24, num_heads=16)
    return _create_mae_vit(**model_args, **kwargs)


def mae_vit_huge(**kwargs):
    model_args = dict(embed_dim=1280, depth=32, num_heads=16)
    return _create_mae_vit(**model_args, **kwargs)


# "patch embed" baseline model, depth 0 ViT (hah)
def patch_embed_small(**kwargs):
    model_args = dict(embed_dim=384, depth=0)
    return _create_vit(**model_args, **kwargs)


def patch_embed_base(**kwargs):
    model_args = dict(embed_dim=768, depth=0)
    return _create_vit(**model_args, **kwargs)
