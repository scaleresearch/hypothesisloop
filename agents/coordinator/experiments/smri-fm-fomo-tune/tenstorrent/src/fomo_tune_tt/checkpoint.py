"""Read a walnut sMRI-MAE checkpoint into the numpy `EncoderParams` the TT
encoder is constructed from.

The pretraining port's `params.py` writes a state dict into an *already-built*
`Encoder`/`Decoder` pair. That is the right shape for a training run (build
fresh, then optionally resume), but wrong here twice over: this port has no
decoder at all, and building the encoder from freshly sampled
Xavier/trunc-normal arrays only to overwrite all 300M of them is pure waste. So
the checkpoint is the *source* of `EncoderParams`, not a later overwrite -- one
path, no dead initialization, and `params.py` is not vendored into this
experiment at all.

Two naming conventions have to be bridged, both handled explicitly below:
`torch.nn.LayerNorm` calls its parameters `weight`/`bias` where
`modules_tt.LayerNorm` calls them `gamma`/`beta`, and every TT parameter array
carries leading `(1, 1)` singleton dimensions that the torch tensor lacks.
"""

from __future__ import annotations

import inspect
from dataclasses import dataclass

import numpy as np
import torch

from smri_mae_tt.modules_tt import BlockParams, EncoderParams, LayerNormParams, LinearParams


@dataclass(frozen=True)
class CheckpointSpec:
    """Everything about a checkpoint the TT encoder needs to be built."""

    img_size: tuple[int, int, int]
    patch_size: int
    in_chans: int
    embed_dim: int
    depth: int
    num_heads: int
    mlp_ratio: float
    mask_drop_scale: bool
    class_token: bool
    reg_tokens: int
    no_embed_class: bool
    model_name: str


def resolve_ckpt(ckpt_path: str) -> str:
    """A local path for a checkpoint, downloading it if it is an
    `hf://<org>/<repo>/<file>` URI. Same contract as
    `fomo_tune.backbone.resolve_ckpt`."""
    if not ckpt_path.startswith("hf://"):
        return ckpt_path
    from huggingface_hub import hf_hub_download

    org, repo, *rest = ckpt_path.removeprefix("hf://").split("/")
    return hf_hub_download(f"{org}/{repo}", "/".join(rest))


def load_checkpoint(ckpt_path: str) -> tuple[dict[str, torch.Tensor], CheckpointSpec]:
    """Returns the raw state dict and the architecture it describes.

    The architecture comes from the checkpoint's own `args` (what
    `fomo_tune.backbone.load_backbone` rebuilds the reference model from), never
    from defaults guessed here -- a checkpoint that does not say what it is is a
    checkpoint this port refuses.
    """
    import smri_mae.model_mae as models_mae

    ckpt = torch.load(resolve_ckpt(ckpt_path), map_location="cpu", weights_only=True, mmap=True)
    args = ckpt["args"]

    # Build the reference model the checkpoint describes and read its shape off
    # the constructed module, so `smri_mae.model_mae` itself stays the source of
    # truth for what `args["model"]`'s factory defaults resolve to -- restating
    # the factory's depth/width/head numbers here would be a second place to
    # keep in sync with upstream.
    #
    # Built on CPU, not on the meta device: `MaskedEncoder.__init__` calls
    # `.item()` on its stochastic-depth schedule, which meta tensors do not
    # support. So this really does allocate a ~1.3GB float32 parameter set for a
    # few seconds, once per process, purely to be introspected -- freed below
    # before any device work starts.
    model = _reference_model(models_mae, args)
    encoder = model.encoder

    patch_size = _to_3d(args["patch_size"])
    if len(set(patch_size)) != 1:
        raise ValueError(f"this port assumes a cubic patch (ShapeContract.patch_size is one int), got {patch_size}")

    block = encoder.blocks[0]
    spec = CheckpointSpec(
        img_size=_to_3d(args["img_size"]),
        patch_size=patch_size[0],
        in_chans=args.get("in_chans", 1),
        embed_dim=encoder.cls_token.shape[-1],
        depth=len(encoder.blocks),
        num_heads=block.attn.num_heads,
        mlp_ratio=block.mlp.fc1.out_features / block.mlp.fc1.in_features,
        mask_drop_scale=bool(encoder.mask_drop_scale),
        class_token=bool(encoder.has_class_token),
        reg_tokens=int(encoder.num_reg_tokens),
        no_embed_class=bool(encoder.no_embed_class),
        model_name=args["model"],
    )
    del model, encoder, block
    if not spec.class_token or spec.reg_tokens or spec.no_embed_class:
        raise NotImplementedError(
            f"this port implements class_token=True, reg_tokens=0, no_embed_class=False only; "
            f"checkpoint says class_token={spec.class_token}, reg_tokens={spec.reg_tokens}, "
            f"no_embed_class={spec.no_embed_class}"
        )
    return ckpt["model"], spec


def _reference_model(models_mae, args: dict):
    """`fomo_tune.backbone.load_backbone`'s model construction, verbatim: the
    checkpoint's own `args` pick the factory and the kwargs, and kwargs the
    current `MaskedAutoencoderViT` no longer accepts (older checkpoints carry
    training flags) are dropped."""
    signature = inspect.signature(models_mae.MaskedAutoencoderViT)
    model_kwargs = {k: v for k, v in (args.get("model_kwargs") or {}).items() if k in signature.parameters}
    return models_mae.__dict__[args["model"]](
        img_size=args["img_size"],
        in_chans=args.get("in_chans", 1),
        patch_size=args["patch_size"],
        **model_kwargs,
    )


def _to_3d(value) -> tuple[int, int, int]:
    if isinstance(value, int):
        return (value, value, value)
    value = tuple(int(v) for v in value)
    if len(value) != 3:
        raise ValueError(f"expected a 3-tuple or int, got {value}")
    return value


def encoder_params_from_state_dict(state_dict: dict[str, torch.Tensor], spec: CheckpointSpec) -> EncoderParams:
    """Build `EncoderParams` straight from a `MaskedAutoencoderViT` state dict.

    Fail-fast: every key is required and every shape is checked against `spec`.
    `encoder.pos_embed.weight` is deliberately not consumed -- it is the fixed
    sincos table the TT encoder recomputes (`build_pos_embed_table`) -- but it
    *is* shape-checked, since a mismatch there means the checkpoint's grid does
    not match the one being built.
    """
    dim = spec.embed_dim
    grid = tuple(s // spec.patch_size for s in spec.img_size)
    num_patches = grid[0] * grid[1] * grid[2]
    patch_dim = spec.in_chans * spec.patch_size**3
    hidden = int(dim * spec.mlp_ratio)

    take = _Taker(state_dict)
    params = EncoderParams(
        patch_embed=_linear(take, "encoder.patch_embed", dim, patch_dim),
        cls_token=take("encoder.cls_token", (1, 1, dim)).reshape(1, 1, 1, dim),
        cls_token_pos=take("encoder.cls_token_pos", (1, 1, dim)).reshape(1, 1, 1, dim),
        blocks=[
            BlockParams(
                ln1=_layernorm(take, f"encoder.blocks.{i}.norm1", dim),
                qkv=_linear(take, f"encoder.blocks.{i}.attn.qkv", 3 * dim, dim),
                proj=_linear(take, f"encoder.blocks.{i}.attn.proj", dim, dim),
                ln2=_layernorm(take, f"encoder.blocks.{i}.norm2", dim),
                fc1=_linear(take, f"encoder.blocks.{i}.mlp.fc1", hidden, dim),
                fc2=_linear(take, f"encoder.blocks.{i}.mlp.fc2", dim, hidden),
            )
            for i in range(spec.depth)
        ],
        norm=_layernorm(take, "encoder.norm", dim),
    )

    take.check_shape_only("encoder.pos_embed.weight", (num_patches, dim))
    take.assert_no_unexpected_encoder_keys()
    return params


class _Taker:
    def __init__(self, state_dict: dict[str, torch.Tensor]):
        self._sd = state_dict
        self._consumed: set[str] = set()

    def __call__(self, key: str, shape: tuple[int, ...]) -> np.ndarray:
        tensor = self._require(key, shape)
        self._consumed.add(key)
        return tensor.detach().to(torch.float32).cpu().numpy()

    def check_shape_only(self, key: str, shape: tuple[int, ...]) -> None:
        self._require(key, shape)
        self._consumed.add(key)

    def _require(self, key: str, shape: tuple[int, ...]) -> torch.Tensor:
        if key not in self._sd:
            raise KeyError(f"checkpoint is missing required key {key!r} (expected shape {shape})")
        tensor = self._sd[key]
        if tuple(tensor.shape) != shape:
            raise ValueError(f"{key}: checkpoint tensor has shape {tuple(tensor.shape)}, expected {shape}")
        return tensor

    def assert_no_unexpected_encoder_keys(self) -> None:
        unexpected = sorted(k for k in self._sd if k.startswith("encoder.") and k not in self._consumed)
        if unexpected:
            raise KeyError(f"checkpoint has {len(unexpected)} encoder key(s) this port does not know how to load: {unexpected}")


def _linear(take: _Taker, prefix: str, out_features: int, in_features: int) -> LinearParams:
    return LinearParams(
        weight=take(f"{prefix}.weight", (out_features, in_features)).reshape(1, 1, out_features, in_features),
        bias=take(f"{prefix}.bias", (out_features,)).reshape(1, 1, 1, out_features),
    )


def _layernorm(take: _Taker, prefix: str, dim: int) -> LayerNormParams:
    return LayerNormParams(
        gamma=take(f"{prefix}.weight", (dim,)).reshape(1, 1, 1, dim),
        beta=take(f"{prefix}.bias", (dim,)).reshape(1, 1, 1, dim),
    )
