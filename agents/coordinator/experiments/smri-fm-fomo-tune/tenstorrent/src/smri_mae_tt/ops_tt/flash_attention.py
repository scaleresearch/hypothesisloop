"""M6 Tier-1 chunked flash-attention op: a hand-derived, FlashAttention-2-style
forward AND backward for plain bidirectional self-attention, built only from
existing `ttnn` primitives called in a Python loop -- no new C++/Metal kernel,
no `tenstorrent/tt-metal/` edits.

## Why this exists

`bidirectional_scaled_dot_product_attention` (`ops_tt/attention.py`) already
gives correct bidirectional attention via `ttml`'s fused `sdpa_fw`/`sdpa_bw`
kernels, and is already used for training. But at the real decoder shape
(`main_pretrain_tt.full_vitl_real_contract()`: batch=24, heads=16,
seq_len=2816, head_dim=32) it measures ~224ms forward+backward per call,
x4 decoder layers = ~896ms of a ~5.8s/step total -- the single largest line
item in a real training step (see `config_tt.SDPAProgramConfigRegistry`'s
docstring for the full measurement history). The cause is structural: the
fused kernel's internal K/V chunk size is hard-capped at 4 tiles by a
`constexpr bool fp32_dest_acc_en = true` in vendored
`sdpa_fw_program_factory.cpp`, with no Python-exposed override -- not fixable
without editing vendored, read-only tt-metal source. A separate, much faster
tunable op (`ttnn.transformer.scaled_dot_product_attention` +
`ttnn.SDPAProgramConfig`, chunk sizes up to 512) exists, but has no backward
anywhere in this tt-metal build (confirmed via `dir(ttnn.transformer)`: every
`*sdpa*`/`*attention*`/`*flash*` entry is forward-only).

`chunked_bidirectional_attention` below is this project's own answer: a
drop-in replacement for `bidirectional_scaled_dot_product_attention`
specifically (same shape/dtype/semantics contract -- BFLOAT16, TILE layout,
every query attends every key, no masking, no causal behavior; not a
general-purpose SDPA, no GQA/causal support), implementing the standard
FlashAttention-2 forward (online-softmax accumulation across K/V chunks) and
its matching recompute-based backward, with caller-chosen `q_chunk_size`/
`k_chunk_size` (unlike the fused kernel's internal, unconfigurable 4-tile
cap).

## Test-first

`ops_tt/test_flash_attention.py` was written before this file (per project
convention: risky rewrites get a test harness written first, by someone not
writing the implementation) and defines the exact API contract this module
must satisfy. Read that file's module docstring for the full parity-testing
rationale.

## Algorithm

Standard FlashAttention-2, notation per query-chunk `i` / key-chunk `j`,
`scale = 1 / sqrt(head_dim)`:

Forward (online softmax, Q outer loop, K inner loop -- for each `i`, `m_i`/
`l_i`/`o_i` are running max / softmax-denominator / unnormalized-output
accumulators updated across `j`):

    S_ij = scale * (Q_i @ K_j^T)
    m_ij = rowmax(S_ij)
    m_new = max(m_i, m_ij)
    P_ij = exp(S_ij - m_new)
    l_new = exp(m_i - m_new) * l_i + rowsum(P_ij)
    o_new = exp(m_i - m_new) * o_i + P_ij @ V_j
    (m_i, l_i, o_i) = (m_new, l_new, o_new)

    O_i = o_i / l_i         (final, after the last K chunk)
    L_i = m_i + log(l_i)    (saved for backward -- the row logsumexp)

The first K-chunk iteration is special-cased (`m_i = m_ij`, `l_i =
rowsum(P_ij)`, `o_i = P_ij @ V_j` directly) rather than relying on an
initial `m_i = -inf` sentinel -- avoids ever computing `exp(-inf - m_new)`
in bf16/fp32 (which underflows to 0 correctly on real hardware, but is
needless numerical-edge-case reliance when the direct special case is no
more code).

Backward (recompute-based, matches FlashAttention-2's backward derivation;
`D_i = rowsum(dO_i * O_i)` is precomputed once over the *full* saved `O`
and incoming `dO`, not chunked, since it is a cheap O(S*D) elementwise+
reduce pass -- only the O(S^2) score-matrix work is chunked):

    D_i = rowsum(dO_i * O_i)                              (once, full tensors)

    for each Q chunk i (outer):
        for each K chunk j (inner):
            S_ij = scale * (Q_i @ K_j^T)
            P_ij = exp(S_ij - L_i)                         (recompute, using saved L_i)
            dV_j += P_ij^T @ dO_i
            dP_ij = dO_i @ V_j^T
            dS_ij = P_ij * (dP_ij - D_i)
            dQ_i += scale * (dS_ij @ K_j)
            dK_j += scale * (dS_ij^T @ Q_i)

`dV_j`/`dK_j` accumulate contributions from every Q chunk (cross-iteration,
so kept as a python list of per-K-chunk running accumulators, added into on
each Q iteration); `dQ_i` accumulates only within its own Q iteration (all
K-chunk contributions for that Q chunk), so it is a fresh accumulator per
`i`. No `ttnn` scatter/slice-assign is used anywhere -- every accumulator is
a plain `ttnn.add`, and the final `dQ`/`dK`/`dV` are built via one
`ttnn.concat` each over the per-chunk list, matching how the forward output
`O` and saved `L` are assembled.

## Shape / chunk-size contract

`query`/`key`/`value`: `[B, H, seq_len, head_dim]`, `BFLOAT16`, `TILE`
layout (matches `bidirectional_scaled_dot_product_attention`'s contract
exactly). `seq_len` must be evenly divisible by both `q_chunk_size` and
`k_chunk_size` -- fails fast (`ValueError`) rather than rounding/padding the
last chunk (project no-fallback convention); a caller with a non-divisible
shape built its call incorrectly and should fix the call, not have this
silently pad.
"""

from __future__ import annotations

import math

import ttml
import ttnn
from ttml.autograd import Function


def _validate_chunking(seq_len: int, q_chunk_size: int, k_chunk_size: int) -> None:
    if seq_len <= 0:
        raise ValueError(f"seq_len must be positive, got {seq_len}")
    if q_chunk_size <= 0 or k_chunk_size <= 0:
        raise ValueError(f"q_chunk_size/k_chunk_size must be positive, got ({q_chunk_size}, {k_chunk_size})")
    if seq_len % q_chunk_size != 0:
        raise ValueError(f"seq_len={seq_len} is not evenly divisible by q_chunk_size={q_chunk_size}")
    if seq_len % k_chunk_size != 0:
        raise ValueError(f"seq_len={seq_len} is not evenly divisible by k_chunk_size={k_chunk_size}")


def _slice_seq(x: "ttnn.Tensor", start: int, length: int) -> "ttnn.Tensor":
    """Slice a `[B, H, S, D]` raw ttnn tensor to `[B, H, start:start+length, D]`."""
    shape = tuple(x.shape)
    b, h, _s, d = shape
    return ttnn.slice(x, [0, 0, start, 0], [b, h, start + length, d])


class ChunkedAttentionFunction(Function):
    """See module docstring for the full forward/backward derivation."""

    @staticmethod
    def forward(
        ctx,
        query: "ttml.autograd.Tensor",
        key: "ttml.autograd.Tensor",
        value: "ttml.autograd.Tensor",
        seq_len: int,
        q_chunk_size: int,
        k_chunk_size: int,
    ):
        q_val = query.get_value()
        k_val = key.get_value()
        v_val = value.get_value()
        head_dim = tuple(q_val.shape)[-1]
        scale = 1.0 / math.sqrt(head_dim)

        num_q_chunks = seq_len // q_chunk_size
        num_k_chunks = seq_len // k_chunk_size

        o_chunks = []
        l_chunks = []
        for i in range(num_q_chunks):
            q_i = _slice_seq(q_val, i * q_chunk_size, q_chunk_size)

            m_i = None
            l_i = None
            o_i = None
            for j in range(num_k_chunks):
                k_j = _slice_seq(k_val, j * k_chunk_size, k_chunk_size)
                v_j = _slice_seq(v_val, j * k_chunk_size, k_chunk_size)

                s_ij = ttnn.multiply(ttnn.matmul(q_i, k_j, transpose_a=False, transpose_b=True), scale)
                m_ij = ttnn.max(s_ij, dim=-1, keepdim=True)

                if j == 0:
                    m_i = m_ij
                    p_ij = ttnn.exp(ttnn.subtract(s_ij, m_i))
                    l_i = ttnn.sum(p_ij, dim=-1, keepdim=True)
                    o_i = ttnn.matmul(p_ij, v_j, transpose_a=False, transpose_b=False)
                else:
                    m_new = ttnn.maximum(m_i, m_ij)
                    correction = ttnn.exp(ttnn.subtract(m_i, m_new))
                    p_ij = ttnn.exp(ttnn.subtract(s_ij, m_new))
                    l_ij = ttnn.sum(p_ij, dim=-1, keepdim=True)
                    l_i = ttnn.add(ttnn.multiply(correction, l_i), l_ij)
                    o_i = ttnn.add(ttnn.multiply(correction, o_i), ttnn.matmul(p_ij, v_j, transpose_a=False, transpose_b=False))
                    m_i = m_new

            o_i_final = ttnn.divide(o_i, l_i)
            l_i_final = ttnn.add(m_i, ttnn.log(l_i))
            o_chunks.append(o_i_final)
            l_chunks.append(l_i_final)

        output = o_chunks[0] if num_q_chunks == 1 else ttnn.concat(o_chunks, dim=2)
        saved_l = l_chunks[0] if num_q_chunks == 1 else ttnn.concat(l_chunks, dim=2)

        ctx.save_for_backward(query, key, value)
        ctx.saved_output = output
        ctx.saved_l = saved_l
        ctx.seq_len = seq_len
        ctx.q_chunk_size = q_chunk_size
        ctx.k_chunk_size = k_chunk_size
        ctx.scale = scale
        return output

    @staticmethod
    def backward(ctx, grad_output):
        query, key, value = ctx.saved_tensors
        q_val = query.get_value()
        k_val = key.get_value()
        v_val = value.get_value()
        o_val = ctx.saved_output
        l_val = ctx.saved_l
        scale = ctx.scale
        seq_len = ctx.seq_len
        q_chunk_size = ctx.q_chunk_size
        k_chunk_size = ctx.k_chunk_size

        num_q_chunks = seq_len // q_chunk_size
        num_k_chunks = seq_len // k_chunk_size

        d_full = ttnn.sum(ttnn.multiply(grad_output, o_val), dim=-1, keepdim=True)

        dk_chunks = [None] * num_k_chunks
        dv_chunks = [None] * num_k_chunks
        dq_chunks = []

        for i in range(num_q_chunks):
            start_i = i * q_chunk_size
            q_i = _slice_seq(q_val, start_i, q_chunk_size)
            l_i = _slice_seq(l_val, start_i, q_chunk_size)
            do_i = _slice_seq(grad_output, start_i, q_chunk_size)
            d_i = _slice_seq(d_full, start_i, q_chunk_size)

            dq_i = None
            for j in range(num_k_chunks):
                start_j = j * k_chunk_size
                k_j = _slice_seq(k_val, start_j, k_chunk_size)
                v_j = _slice_seq(v_val, start_j, k_chunk_size)

                s_ij = ttnn.multiply(ttnn.matmul(q_i, k_j, transpose_a=False, transpose_b=True), scale)
                p_ij = ttnn.exp(ttnn.subtract(s_ij, l_i))

                dv_contrib = ttnn.matmul(p_ij, do_i, transpose_a=True, transpose_b=False)
                dv_chunks[j] = dv_contrib if dv_chunks[j] is None else ttnn.add(dv_chunks[j], dv_contrib)

                dp_ij = ttnn.matmul(do_i, v_j, transpose_a=False, transpose_b=True)
                ds_ij = ttnn.multiply(p_ij, ttnn.subtract(dp_ij, d_i))

                dq_contrib = ttnn.multiply(ttnn.matmul(ds_ij, k_j, transpose_a=False, transpose_b=False), scale)
                dq_i = dq_contrib if dq_i is None else ttnn.add(dq_i, dq_contrib)

                dk_contrib = ttnn.multiply(ttnn.matmul(ds_ij, q_i, transpose_a=True, transpose_b=False), scale)
                dk_chunks[j] = dk_contrib if i == 0 else ttnn.add(dk_chunks[j], dk_contrib)

            dq_chunks.append(dq_i)

        dq = dq_chunks[0] if num_q_chunks == 1 else ttnn.concat(dq_chunks, dim=2)
        dk = dk_chunks[0] if num_k_chunks == 1 else ttnn.concat(dk_chunks, dim=2)
        dv = dv_chunks[0] if num_k_chunks == 1 else ttnn.concat(dv_chunks, dim=2)

        return dq, dk, dv


def chunked_bidirectional_attention(
    query: "ttml.autograd.Tensor",
    key: "ttml.autograd.Tensor",
    value: "ttml.autograd.Tensor",
    *,
    seq_len: int,
    q_chunk_size: int = 512,
    k_chunk_size: int = 512,
) -> "ttml.autograd.Tensor":
    """Chunked, online-softmax bidirectional self-attention -- see module
    docstring. Drop-in replacement for `attention.bidirectional_scaled_dot_
    product_attention` (same `[B, H, seq_len, head_dim]` shape contract, same
    plain-bidirectional semantics), with caller-controlled `q_chunk_size`/
    `k_chunk_size` instead of the fused kernel's internal, unconfigurable
    4-tile cap.

    `seq_len` must be evenly divisible by both chunk sizes (fails fast
    otherwise -- see module docstring).
    """
    _validate_chunking(seq_len, q_chunk_size, k_chunk_size)
    return ChunkedAttentionFunction.apply(query, key, value, seq_len, q_chunk_size, k_chunk_size)
