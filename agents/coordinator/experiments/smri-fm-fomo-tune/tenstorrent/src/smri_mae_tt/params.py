"""Checkpoint <-> TT parameter conversion for the MAE port (`params.py`,
TENSTORRENT_PORT.md section 6: "`params.py` # torch .pth checkpoint ->
ttnn/ttml tensors, dtype policy").

No trained PyTorch checkpoint exists anywhere in this repo yet (confirmed by
extensive search -- see project memory / `test_modules_tt.py`'s module
docstring), so this module is not "load a specific known checkpoint": it is
the general, checkpoint-ready machinery that will consume one once it
exists -- a torch `state_dict` from `torch.save`/`torch.load` of
`src/smri_mae/model_mae.py`'s `MaskedAutoencoderViT` (whose top-level
submodules are literally named `encoder`/`decoder`, i.e. its `state_dict()`
keys are `"encoder.<...>"`/`"decoder.<...>"` -- exactly the two prefixes
this module expects).

## What this module does NOT do

Fresh-parameter initialization already fully exists and is not
reimplemented here: `modules_tt.py`'s `Encoder`/`Decoder` classes already
build their entire parameter set (matching `model_mae.py`'s
`_init_weights`/`reset_parameters` scheme bit-for-bit in *scheme*, not
value) from an `EncoderParams`/`DecoderParams` numpy-array pair at
construction time -- that is "the seam a future `params.py` checkpoint
loader hooks into", per `modules_tt.py`'s own `_const_init` docstring.
`init_tt_params` below is a two-line convenience wrapper over
`modules_tt.init_encoder_params`/`init_decoder_params` so a caller building
a fresh (untrained) model doesn't need to import `modules_tt` directly for
that step; it is not a second init implementation.

## What this module does

1. `named_tt_parameters(encoder, decoder)` -- flatten both modules'
   `named_parameters()` into one `{"encoder.<name>": tensor, "decoder.<name>":
   tensor}` dict, for `config_tt.build_param_group_specs`/`MultiGroupAdamW`
   and for the state_dict conversion below to share one naming scheme.
2. `state_dict_to_tt(torch_state_dict, encoder, decoder)` -- write a torch
   `state_dict`'s values into an already-constructed `Encoder`/`Decoder`'s
   parameter tensors in place (via `ttml.autograd.Tensor.set_value`, the same
   primitive `ttml.modules.LinearLayer.__setstate__` uses for pickle
   deserialization -- confirmed empirically on real hardware, see
   `test_params.py`). Fail-fast: raises on any missing key, any shape
   mismatch, or any unexpected/unrecognized key, *before* writing anything
   (validated in a first pass; device tensors are only mutated in a second
   pass once the whole state_dict has been checked against the module) --
   never silently skips or clamps a malformed checkpoint.
3. `tt_to_state_dict(encoder, decoder)` -- the reverse: read every TT
   parameter back to a torch-compatible `state_dict`, suitable for
   checkpointing a training run. Also emits the (deterministically
   recomputed, not device-read) fixed sin/cos position-embedding buffer
   under `"<prefix>.pos_embed.weight"`, so the result is a *complete*,
   strict-`load_state_dict`-able state dict for `MaskedEncoder`/
   `MaskedDecoder`, matching what a real checkpoint of those classes
   contains (`SinCosPosEmbed3D.weight` is a non-trainable
   `requires_grad=False` `nn.Parameter`, so it *is* part of their
   `state_dict()`, even though it is never learned or loaded -- TT-side it
   lives in a `ttml.modules.Buffer`, which `named_parameters()` never yields
   at all, see `modules_tt.py`'s `build_pos_embed_table` docstring).

## Naming convention

TT-side parameter names come directly from `AbstractModuleBase.named_parameters()`
(`ttml/modules/module_base.py`), which already mirrors the PyTorch reference's
attribute paths closely: `"blocks.3.attn.qkv.weight"`, `"patch_embed.bias"`,
`"cls_token"`, etc. The *only* divergence from the reference is `LayerNorm`:
`modules_tt.py`'s `LayerNorm` names its two parameters `gamma`/`beta`
(`modules_tt.py`'s `LayerNorm.__init__`), where `torch.nn.LayerNorm` names the
same two tensors `weight`/`bias`. `torch_key_and_shape` below is the one
place that translation happens (`...norm1.gamma` <-> `...norm1.weight`,
`...norm1.beta` <-> `...norm1.bias`); every other parameter name is identical
in both worlds. This project's convention (matching `named_tt_parameters`'s
docstring on `config_tt.build_param_group_specs`) is to prefix the flattened
name with `"encoder."`/`"decoder."`, mirroring `MaskedAutoencoderViT`'s own
two submodule attribute names.

## Dtype policy (TENSTORRENT_PORT.md section 6) and what precision survives a round trip

Every TT parameter is stored as either `BFLOAT16` (the port's default
activation/weight dtype) or `BFLOAT8_B` (decoder MLP/QKV weights only, per
section 6: "Keep decoder MLP/QKV weights in `bfloat8_b`"). Both directions
below introspect each parameter's *current* stored dtype
(`tensor.get_value().get_dtype()`) rather than hardcoding which parameter
names are bf8b -- this automatically tracks whatever `modules_tt.py`'s
`Attention`/`Mlp` construction decided (`qkv_mlp_dtype` in `Block.__init__`),
so `params.py` never has to duplicate that policy.

- **`state_dict_to_tt` (torch fp32 -> TT bf16/bf8b): lossy, by design.**
  `BFLOAT16` keeps a 7-bit mantissa (~2-3 decimal digits); `BFLOAT8_B` keeps
  even less (a 3-bit mantissa per element, shared per-block exponent, roughly
  1-2 decimal digits). Whatever precision the source fp32 checkpoint has
  beyond that is discarded and **not recoverable** -- this is the same "bf16
  activations, no loss scaler" numerics policy the rest of the port already
  runs under (section 6), not a `params.py`-specific loss.
- **`tt_to_state_dict` (TT bf16/bf8b -> torch fp32): exact, lossless
  widening.** Reading a bf16/bf8b value back out to fp32 just represents
  whatever is already stored, verbatim, in a wider format -- no additional
  rounding happens on the way out. So a full round trip
  (`state_dict_to_tt` then `tt_to_state_dict`) reproduces the *original* fp32
  values only up to the write-side dtype's precision, not bit-exactly -- this
  is expected and is exactly what `test_params.py`'s round-trip test checks
  (a bounded relative-error gate, not exact equality).
- The recomputed `pos_embed.weight` entry `tt_to_state_dict` emits is the one
  exception: it is never read off the (bf16-stored) device buffer at all, so
  it is bit-for-bit float32, matching `modules_tt.sincos_pos_embed_3d`'s
  already-verified-bit-exact-vs-reference table
  (`test_modules_tt.py::test_sincos_pos_embed_matches_reference`).
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Mapping

import numpy as np
import torch
import ttml
import ttnn

from smri_mae_tt.modules_tt import (
    DecoderParams,
    EncoderParams,
    init_decoder_params,
    init_encoder_params,
    sincos_pos_embed_3d,
)

if TYPE_CHECKING:
    from smri_mae_tt.layout import ShapeContract
    from smri_mae_tt.modules_tt import Decoder, Encoder

# Attribute names that hold a bare `[1, 1, 1, D]` token parameter directly on
# `Encoder`/`Decoder` (no LinearLayer/LayerNorm wrapper in between) -- these
# map to a torch shape of `(1, 1, D)`, not `(D,)` or `(out, in)`. See
# `model_mae.py`: `cls_token`/`cls_token_pos` are `nn.Parameter(torch.empty(1,
# 1, embed_dim))` on both `MaskedEncoder` and `MaskedDecoder`; `mask_token` is
# the same shape, decoder-only.
_TOKEN_LEAF_NAMES = frozenset({"cls_token", "cls_token_pos", "mask_token"})

# `SinCosPosEmbed3D.weight`'s state_dict key relative to its owning
# `MaskedEncoder`/`MaskedDecoder` -- present in a real checkpoint, never
# loaded (fixed, computed table -- see module docstring), always allowed
# to be present-but-unconsumed in `state_dict_to_tt`, always re-emitted
# (recomputed) by `tt_to_state_dict`.
_POS_EMBED_SUFFIX = "pos_embed.weight"

_KNOWN_PREFIXES = ("encoder.", "decoder.")


def init_tt_params(
    rng: np.random.Generator,
    contract: "ShapeContract",
    encoder_depth: int,
    decoder_depth: int,
    mlp_ratio: float = 4.0,
) -> tuple[EncoderParams, DecoderParams]:
    """Fresh (untrained) parameters for `modules_tt.Encoder`/`Decoder`,
    matching `model_mae.py::_init_weights`'s scheme (see `modules_tt.py`'s
    `init_encoder_params`/`init_decoder_params`, which this thin wrapper
    delegates to unchanged -- not reimplemented here, per this module's own
    docstring)."""
    encoder_params = init_encoder_params(rng, contract, depth=encoder_depth, mlp_ratio=mlp_ratio)
    decoder_params = init_decoder_params(rng, contract, depth=decoder_depth, mlp_ratio=mlp_ratio)
    return encoder_params, decoder_params


def named_tt_parameters(encoder: "Encoder", decoder: "Decoder") -> dict[str, "ttml.autograd.Tensor"]:
    """Flatten `encoder.named_parameters()`/`decoder.named_parameters()` into
    one `{"encoder.<name>": tensor, "decoder.<name>": tensor}` mapping.

    This is the single naming scheme both `config_tt.build_param_group_specs`
    (optimizer grouping -- its `.bias`/`"norm"`/`"gamma"`/`"patch_embed"`
    substring rules all still match correctly under the `"encoder."`/
    `"decoder."` prefix, since substring checks are prefix-agnostic) and
    `state_dict_to_tt`/`tt_to_state_dict` below are built around: e.g.
    `"encoder.blocks.0.attn.qkv.weight"`, `"decoder.head.bias"`.

    Every entry is guaranteed trainable (`requires_grad=True`) by
    construction: `AbstractModuleBase.named_parameters()` only ever yields
    tensors registered via `ttml.modules.Parameter` (never `Buffer`s, e.g.
    the sincos position-embedding table -- see `modules_tt.py`'s
    `build_pos_embed_table` docstring on why that is deliberately a `Buffer`,
    not a `Parameter`), and every `Parameter` `modules_tt.py` constructs is
    `requires_grad=True`.
    """
    named: dict[str, "ttml.autograd.Tensor"] = {}
    for name, tensor in encoder.named_parameters():
        named[f"encoder.{name}"] = tensor
    for name, tensor in decoder.named_parameters():
        named[f"decoder.{name}"] = tensor
    return named


def torch_key_and_shape(tt_name: str, tt_shape: tuple[int, ...]) -> tuple[str, tuple[int, ...]]:
    """Translate one TT-side parameter name (module-local, e.g.
    `"blocks.0.norm1.gamma"`, `"cls_token"`, `"patch_embed.weight"`) and its
    TT tensor shape (this port's `(1, 1, ...)`-leading-singleton convention,
    `modules_tt.py`'s module docstring) into the corresponding PyTorch
    `state_dict` key and shape.

    Dispatches purely on the final path component (the "leaf" attribute
    name a `ttml.modules.Parameter` was registered under), which is
    unambiguous in this codebase: `LayerNorm` (`modules_tt.py`) is the only
    module that names its parameters `gamma`/`beta` (mapping to torch's
    `weight`/`bias`); every `LinearLayer` names its parameters `weight`/
    `bias` directly (identical in both worlds, only the leading `(1, 1)`
    torch lacks needs squeezing); and `cls_token`/`cls_token_pos`/
    `mask_token` are bare token parameters with no wrapping submodule at all.

    Raises `ValueError` on a leaf name none of those categories cover --
    fail-fast: an unrecognized parameter category is a bug in this mapping
    (e.g. `modules_tt.py` grew a new parameter kind) to surface immediately,
    not to guess about.
    """
    leaf = tt_name.rsplit(".", 1)[-1]
    if leaf == "gamma":
        # LayerNorm weight: TT (1,1,1,D) -> torch (D,).
        return tt_name[: -len("gamma")] + "weight", tuple(tt_shape[-1:])
    if leaf == "beta":
        # LayerNorm bias: TT (1,1,1,D) -> torch (D,).
        return tt_name[: -len("beta")] + "bias", tuple(tt_shape[-1:])
    if leaf in _TOKEN_LEAF_NAMES:
        # Bare token parameter: TT (1,1,1,D) -> torch (1,1,D).
        return tt_name, tuple(tt_shape[1:])
    if leaf == "weight":
        # LinearLayer weight: TT (1,1,out,in) -> torch (out,in).
        return tt_name, tuple(tt_shape[2:])
    if leaf == "bias":
        # LinearLayer bias: TT (1,1,1,out) -> torch (out,).
        return tt_name, tuple(tt_shape[3:])
    raise ValueError(
        f"params.py does not know the torch-side shape convention for TT parameter "
        f"leaf name {leaf!r} (full name {tt_name!r}, tt_shape={tt_shape}). This means "
        "modules_tt.py registered a parameter category torch_key_and_shape has never "
        "seen -- add a case here rather than guessing a shape."
    )


def _get_dtype(tensor: "ttml.autograd.Tensor") -> "ttnn.DataType":
    return tensor.get_value().get_dtype()


def _read_tensor_value(tensor: "ttml.autograd.Tensor") -> np.ndarray:
    """Read a TT parameter back to a float32 numpy array, exactly (see module
    docstring: reading out is lossless widening, never a further rounding).

    `ttml.autograd.Tensor.to_numpy(FLOAT32)` raises `TypeError: Unsupported
    type: BFLOAT8_B` when the tensor's *stored* dtype is `BFLOAT8_B`
    (confirmed empirically on real hardware -- the same
    `nanobind/nb_util.cpp` limitation `modules_tt.py`'s `_const_tensor`
    docstring documents for the write direction). The workaround, also
    confirmed empirically: `ttnn.typecast` the raw value to `FLOAT32` first
    (a real, supported device op), re-wrap with `ttml.autograd.create_tensor`,
    *then* call `to_numpy`.
    """
    dtype = _get_dtype(tensor)
    if dtype == ttnn.DataType.BFLOAT8_B:
        fp32_value = ttnn.typecast(tensor.get_value(), ttnn.DataType.FLOAT32)
        host_view = ttml.autograd.create_tensor(fp32_value, requires_grad=False)
        return np.asarray(host_view.to_numpy(ttnn.DataType.FLOAT32), dtype=np.float32)
    return np.asarray(tensor.to_numpy(ttnn.DataType.FLOAT32), dtype=np.float32)


def _write_tensor_value(tensor: "ttml.autograd.Tensor", arr: np.ndarray) -> None:
    """Overwrite a TT parameter's value in place from a float32 numpy array,
    casting to whatever dtype the parameter is *currently* stored as.

    Uses `ttml.autograd.Tensor.set_value`, the exact primitive
    `ttml.modules.LinearLayer.__setstate__` uses for pickle deserialization
    (`ttml/modules/linear.py`) -- confirmed on real hardware to mutate the
    tensor already bound into the owning module's parameter registry
    in-place (no re-registration needed).

    `ttml.autograd.Tensor.from_numpy(..., new_type=BFLOAT8_B)` is
    unsupported for any source numpy dtype (`modules_tt.py`'s
    `_const_tensor` docstring, confirmed empirically); the working path,
    identical to `_const_tensor`'s, is to build a `BFLOAT16` tile tensor via
    `from_numpy` first, then `ttnn.typecast` its raw value to `BFLOAT8_B`.
    """
    dtype = _get_dtype(tensor)
    bf16 = ttml.autograd.Tensor.from_numpy(
        arr.astype(np.float32), layout=ttnn.Layout.TILE, new_type=ttnn.DataType.BFLOAT16
    )
    if dtype == ttnn.DataType.BFLOAT16:
        tensor.set_value(bf16.get_value())
    elif dtype == ttnn.DataType.BFLOAT8_B:
        tensor.set_value(ttnn.typecast(bf16.get_value(), ttnn.DataType.BFLOAT8_B))
    else:
        raise NotImplementedError(
            f"params.py only knows how to write BFLOAT16/BFLOAT8_B parameters (this "
            f"port's only two parameter storage dtypes, TENSTORRENT_PORT.md section 6), "
            f"found stored dtype {dtype!r}. Not silently coercing -- this means either a "
            "parameter was constructed with an unexpected dtype, or this port's dtype "
            "policy grew a third storage dtype that params.py needs an explicit case for."
        )


def state_dict_to_tt(
    torch_state_dict: Mapping[str, torch.Tensor],
    encoder: "Encoder",
    decoder: "Decoder",
) -> None:
    """Write a PyTorch `state_dict` (e.g. `torch.load(checkpoint_path)` of a
    `MaskedAutoencoderViT`, or the merged `encoder.state_dict()` +
    `decoder.state_dict()` of a standalone `MaskedEncoder`/`MaskedDecoder`
    pair, each re-prefixed `"encoder."`/`"decoder."`) into an
    already-constructed TT `Encoder`/`Decoder`'s parameter tensors, in place.

    Fail-fast, two-pass: pass 1 walks every TT parameter, resolves its
    expected torch key/shape (`torch_key_and_shape`), and raises immediately
    on a **missing key** (`KeyError`) or a **shape mismatch**
    (`ValueError`) -- collecting the planned writes but not yet touching any
    device tensor. Only if every TT parameter resolved cleanly does pass 2
    run, actually calling `set_value` on each. This means a malformed
    `state_dict` never leaves the model half-updated: either every parameter
    is consistently overwritten, or none are (device tensors from `pass 1`
    are read-only there; nothing is mutated until pass 2 starts). After
    loading, this also raises (`KeyError`) if `torch_state_dict` contains
    **unexpected keys**: anything under `"encoder."`/`"decoder."` that no TT
    parameter claimed (except each module's `pos_embed.weight` -- the fixed,
    computed sincos table, deliberately never loaded, see module docstring),
    and anything outside the `"encoder."`/`"decoder."` namespace entirely
    (e.g. a `MaskedAutoencoderViT` checkpoint's `target_norm.*`, or a
    `DistributedDataParallel`-wrapped checkpoint's leftover `"module."`
    prefix -- neither is silently ignored).
    """
    encoder_plan = _validate_module_against_state_dict("encoder", encoder, torch_state_dict)
    decoder_plan = _validate_module_against_state_dict("decoder", decoder, torch_state_dict)

    stray = sorted(k for k in torch_state_dict if not k.startswith(_KNOWN_PREFIXES))
    if stray:
        raise KeyError(
            f"state_dict has {len(stray)} key(s) outside the 'encoder.'/'decoder.' "
            f"namespaces state_dict_to_tt knows how to consume: {stray}"
        )

    for tensor, arr in encoder_plan:
        _write_tensor_value(tensor, arr)
    for tensor, arr in decoder_plan:
        _write_tensor_value(tensor, arr)


def _validate_module_against_state_dict(
    prefix: str,
    module: "Encoder | Decoder",
    torch_state_dict: Mapping[str, torch.Tensor],
) -> list[tuple["ttml.autograd.Tensor", np.ndarray]]:
    """Pass 1 of `state_dict_to_tt` for one module (`"encoder"`/`"decoder"`):
    validate every parameter resolves to a present, correctly-shaped
    `torch_state_dict` entry, and that no unexpected keys remain under this
    module's prefix. Returns the write plan (`tensor, arr` pairs); writes
    nothing itself.
    """
    plan: list[tuple["ttml.autograd.Tensor", np.ndarray]] = []
    consumed: set[str] = set()

    for tt_name, tensor in module.named_parameters():
        tt_shape = tuple(tensor.shape())
        torch_suffix, torch_shape = torch_key_and_shape(tt_name, tt_shape)
        torch_key = f"{prefix}.{torch_suffix}"
        consumed.add(torch_key)

        if torch_key not in torch_state_dict:
            raise KeyError(
                f"state_dict is missing required key {torch_key!r} "
                f"(TT parameter {prefix}.{tt_name}, expected torch shape {torch_shape})"
            )
        torch_tensor = torch_state_dict[torch_key]
        actual_shape = tuple(torch_tensor.shape)
        if actual_shape != torch_shape:
            raise ValueError(
                f"{torch_key}: state_dict tensor has shape {actual_shape}, expected "
                f"{torch_shape} (TT parameter {prefix}.{tt_name}, tt_shape={tt_shape})"
            )

        arr = torch_tensor.detach().to(dtype=torch.float32).cpu().numpy().reshape(tt_shape)
        plan.append((tensor, arr))

    provided = {k for k in torch_state_dict if k.startswith(f"{prefix}.")}
    pos_embed_key = f"{prefix}.{_POS_EMBED_SUFFIX}"
    if pos_embed_key in provided:
        _validate_pos_embed_shape(prefix, module, torch_state_dict[pos_embed_key])
    allowed_unconsumed = {pos_embed_key} & provided
    unexpected = sorted(provided - consumed - allowed_unconsumed)
    if unexpected:
        raise KeyError(
            f"state_dict has {len(unexpected)} unexpected key(s) under {prefix!r} that no "
            f"TT parameter of this module claims: {unexpected}"
        )

    return plan


def _pos_embed_dim(prefix: str, module: "Encoder | Decoder") -> int:
    contract = module.contract
    return contract.embed_dim if prefix == "encoder" else contract.decoder_embed_dim


def _validate_pos_embed_shape(prefix: str, module: "Encoder | Decoder", torch_tensor: torch.Tensor) -> None:
    """`pos_embed.weight` is never loaded (module docstring), but a grossly
    wrong shape here (e.g. a checkpoint from a different `img_size`/
    `patch_size`/embed dim) is exactly the kind of silent mismatch this
    project's fail-fast rule exists to catch -- so its shape, if present, is
    still checked against this `ShapeContract`, even though its values are
    not used.
    """
    contract = module.contract
    expected_shape = (contract.num_patches, _pos_embed_dim(prefix, module))
    actual_shape = tuple(torch_tensor.shape)
    if actual_shape != expected_shape:
        raise ValueError(
            f"{prefix}.{_POS_EMBED_SUFFIX}: state_dict tensor has shape {actual_shape}, "
            f"but this module's ShapeContract implies {expected_shape} -- this state_dict "
            "looks like it was produced by a different shape configuration. (This "
            "parameter is never loaded -- it is a fixed, recomputed table -- but a shape "
            "mismatch here means the checkpoint doesn't match this model.)"
        )


def tt_to_state_dict(encoder: "Encoder", decoder: "Decoder") -> dict[str, torch.Tensor]:
    """The reverse of `state_dict_to_tt`: read every TT parameter of
    `encoder`/`decoder` back into a torch-compatible `state_dict`, for
    checkpointing a training run.

    The result is *complete* and strict-`load_state_dict`-able against a
    matching `MaskedEncoder`/`MaskedDecoder` pair: alongside every learned
    parameter (read from the device, see module docstring on precision), it
    also includes each module's `pos_embed.weight` -- recomputed on the host
    via `modules_tt.sincos_pos_embed_3d` (bit-exact vs. the PyTorch
    reference's own table, see `test_modules_tt.py`), not read off the
    (bf16-stored) device buffer, since it is fully and exactly reproducible
    from `contract` alone and reading it back would only add needless
    precision loss to a value nothing ever learns.
    """
    state_dict: dict[str, torch.Tensor] = {}
    _dump_module_to_state_dict("encoder", encoder, state_dict)
    _dump_module_to_state_dict("decoder", decoder, state_dict)
    return state_dict


def _dump_module_to_state_dict(prefix: str, module: "Encoder | Decoder", state_dict: dict[str, torch.Tensor]) -> None:
    for tt_name, tensor in module.named_parameters():
        tt_shape = tuple(tensor.shape())
        torch_suffix, torch_shape = torch_key_and_shape(tt_name, tt_shape)
        arr = _read_tensor_value(tensor).reshape(torch_shape)
        state_dict[f"{prefix}.{torch_suffix}"] = torch.from_numpy(arr.copy())

    contract = module.contract
    dim = _pos_embed_dim(prefix, module)
    table = sincos_pos_embed_3d(dim, contract.grid_size)  # [num_patches, dim], exact float32
    state_dict[f"{prefix}.{_POS_EMBED_SUFFIX}"] = torch.from_numpy(table.copy())
