"""Autograd-aware concatenation and slicing for the TT-NN MAE port.

## Why this file exists

Neither `ttnn.concat` nor `ttnn.slice` has an autograd backward registered
anywhere in `ttml`. Confirmed two ways:

1. `grep -rn "concat\\|Concat" tenstorrent/tt-metal/tt-train/sources/ttml/nanobind/nb_ops.cpp`
   finds no bound `concat`/`cat` op in the ops-layer nanobind surface (the
   only bound ops are `linear`, `layernorm`, `embedding`, unary/binary
   elementwise, `multi_head_utils`, `attention`, etc. -- see `nb_ops.cpp`).
2. `tt-train/sources/ttml/ttml/models/deepseek/autograd_ops.py` (a Python
   file shipped inside `ttml` itself, read-only under `tenstorrent/tt-metal/`)
   states this explicitly in its module docstring: "ttnn.slice / ttnn.concat
   / ttnn.sigmoid have no autograd backward in ttml. These wrappers fix that
   using ttml.autograd.Function." That file's `Concat`/`Slice` classes are
   the existing, in-repo precedent for exactly this problem -- this module
   is a same-pattern implementation, kept under
   `tenstorrent/src/smri_mae_tt/ops_tt/` (not importing the deepseek file
   directly, since that lives under the read-only `tenstorrent/tt-metal/`
   tree and is scoped to that model).

This MAE port needs concat for two things the reference PyTorch model does
with `torch.cat` (`modules.py`'s `MaskedEncoder.cat_tokens`,
`MaskedDecoder.cat_tokens`, and `MaskedDecoder.forward`'s
`torch.cat([embeds, mask], dim=1)`):

  - prepending the (learned) cls token to the patch-embedding sequence,
  - concatenating projected visible embeddings with mask-token queries
    before the decoder's cls-token prepend.

Both are on the sequence dimension (dim=2 in this port's `(B, 1, S, D)`
layout -- see `modules_tt.py`'s module docstring for why dim 1 is always 1
here, matching `ttml.ops.multi_head_utils.heads_creation`'s expected
`(B, 1, S, E*3)` QKV layout), so this wrapper is written for a general `dim`
(matching the reference `Concat` op's signature) rather than hardcoding one.

`Slice` is the inverse operation and is needed for the reference's
`chunk_tokens`/slicing (`modules.py`'s `MaskedEncoder.chunk_tokens`,
`MaskedDecoder.chunk_tokens`, and `MaskedDecoder.forward`'s
`pred = x[:, L:]`): splitting the post-block, post-norm sequence back into
`(cls_embed, patch_embeds)` on the encoder side, and slicing out the
prediction region on the decoder side. It is also what `Concat.backward`
itself is built from, so it is exposed as a first-class op here rather than
kept private to this file -- `modules_tt.py` needs a *forward* slice with a
working backward wherever it must consume only part of a tile-aligned
sequence coming out of a block stack (whose parameters require gradients
flowing back through the slice).

## Correctness of the backward

Forward concatenates `tensors` along `dim` via `ttnn.concat`. The backward
of a concat is, exactly, a split of the upstream gradient into the same
size chunks at the same offsets along `dim` -- `d(concat(a,b))/da` selects
exactly the slice of the output gradient covering `a`'s extent, and
similarly for `b`. That is implemented with `ttnn.slice` per input (see
`Concat.backward` below), the same construction as the in-repo
`deepseek/autograd_ops.py::Concat.backward` this mirrors.

Symmetrically, `Slice.backward` is the "un-slice": place the upstream
gradient at the same offset inside a zero tensor shaped like the original
input (zero everywhere the forward slice did not read from -- those
positions provably do not affect the sliced output, so their gradient is
exactly zero), again mirroring `deepseek/autograd_ops.py::Slice.backward`.
"""

from __future__ import annotations

import ttml
import ttnn


class Concat(ttml.autograd.Function):
    """Autograd-aware concat (`ttnn.concat` has no backward -- see module docstring).

    Forward: concatenate `tensors` along `dim` via `ttnn.concat`.
    Backward: slice the upstream gradient back into one gradient per input,
    at the same offsets/sizes the forward concat used.
    """

    @staticmethod
    def forward(ctx, dim, *tensors):
        if len(tensors) < 2:
            raise ValueError(f"concat needs at least 2 tensors, got {len(tensors)}")
        sizes = [list(t.get_value().shape)[dim] for t in tensors]
        ctx.sizes = sizes
        ctx.dim = dim

        raw_tensors = [t.get_value() for t in tensors]
        return ttnn.concat(raw_tensors, dim)

    @staticmethod
    def backward(ctx, grad_output):
        dim = ctx.dim
        sizes = ctx.sizes
        shape = list(grad_output.shape)

        grads = []
        offset = 0
        for size in sizes:
            start = [0] * len(shape)
            end = list(shape)
            start[dim] = offset
            end[dim] = offset + size
            grad_slice = ttnn.slice(grad_output, start, end)
            grads.append(grad_slice)
            offset += size

        return tuple(grads)


def autograd_concat(tensors: list["ttml.autograd.Tensor"], dim: int) -> "ttml.autograd.Tensor":
    """Concat `tensors` along `dim` with a working autograd backward.

    Fails fast (via `Concat.forward`'s check) rather than silently accepting
    a single-tensor "concat" that would just be an identity op with a
    misleading name.
    """
    return Concat.apply(dim, *tensors)


class Slice(ttml.autograd.Function):
    """Autograd-aware slice (`ttnn.slice` has no backward -- see module docstring).

    Forward: `ttnn.slice(input, start, end)` (per-dim half-open `[start, end)`,
    same convention as `ttnn.slice` itself).
    Backward: place the upstream gradient at `[start, end)` inside a
    zero tensor shaped like the (unsliced) input -- the exact input
    gradient, since every element outside `[start, end)` was never read by
    the forward slice and so has zero gradient.
    """

    @staticmethod
    def forward(ctx, input, start, end):
        ctx.input_shape = list(input.get_value().shape)
        ctx.start = list(start)
        ctx.end = list(end)
        return ttnn.slice(input.get_value(), start, end)

    @staticmethod
    def backward(ctx, grad_output):
        input_shape = ctx.input_shape
        start = ctx.start
        end = ctx.end
        device = grad_output.device()

        result = grad_output
        for dim in range(len(input_shape)):
            before = start[dim]
            after = input_shape[dim] - end[dim]
            if before == 0 and after == 0:
                continue

            cur_shape = list(result.shape)
            parts = []

            if before > 0:
                z_shape = list(cur_shape)
                z_shape[dim] = before
                parts.append(ttnn.zeros(z_shape, dtype=result.dtype, layout=result.layout, device=device))

            parts.append(result)

            if after > 0:
                z_shape = list(cur_shape)
                z_shape[dim] = after
                parts.append(ttnn.zeros(z_shape, dtype=result.dtype, layout=result.layout, device=device))

            result = ttnn.concat(parts, dim)

        return result


def autograd_slice(tensor: "ttml.autograd.Tensor", start: list[int], end: list[int]) -> "ttml.autograd.Tensor":
    """Slice `tensor[start[0]:end[0], start[1]:end[1], ...]` with a working backward."""
    return Slice.apply(tensor, start, end)
