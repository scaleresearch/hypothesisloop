"""The one thing both task 3 and task 5 need from Tenstorrent: a 1024-d
mean-pooled embedding per subject.

Upstream's `Task3Method.features` and `Task5Method.features` are the same
function (verified line by line -- they differ only in the sklearn head fitted
on top): transform the t1w, run the frozen encoder once, mean-pool the surviving
patch tokens. So there is one implementation here, not one per task.

Upstream calls `features()` again inside `predict()`, which re-runs the backbone
for every out-of-fold subject -- roughly doubling the backbone work for a result
that is a pure function of the images and therefore identical to the cached one.
`embed_subjects` below runs the encoder exactly once per subject and both task
runners index that table, so the device does 494 forwards for task 3 rather
than 988. That is a change to the *method* side (which upstream marks as the
tunable part), never to the protocol: the numbers scored are the same numbers.
"""

from __future__ import annotations

import time
from dataclasses import dataclass
from pathlib import Path

import nibabel as nib
import numpy as np
import ttml

from fomo_tune.backbone import SmriMaeTransform
from fomo_tune_tt.checkpoint import CheckpointSpec, encoder_params_from_state_dict, load_checkpoint
from fomo_tune_tt.encoder_tt import MaskedInferenceEncoder, build_contract, encoder_seq_len_for
from fomo_tune_tt.packing import key_padding_mask, pack_subject
from smri_mae_tt.layout import ShapeContract


@dataclass
class SubjectEmbedding:
    subject: str
    embedding: np.ndarray  # (D,) float32
    n_visible: int
    seconds: float


class TTBackbone:
    """A frozen sMRI-MAE encoder on Tenstorrent, built for one fixed sequence
    length.

    `l_vis` is a compile-time constant for TT-NN (one program per shape), but
    the number of observed patches is a property of each subject. So the
    encoder is built once for the largest sequence any subject in the run
    needs, and every shorter subject is zero-padded with the padded keys masked
    out of attention (`packing.key_padding_mask`). One shape for the whole run
    means one program-cache entry, not one per subject.
    """

    def __init__(self, ckpt_path: str, l_vis: int):
        state_dict, spec = load_checkpoint(ckpt_path)
        self.spec: CheckpointSpec = spec
        self.contract: ShapeContract = build_contract(
            l_vis=l_vis,
            patch_size=spec.patch_size,
            img_size=spec.img_size,
            embed_dim=spec.embed_dim,
            heads=spec.num_heads,
        )
        params = encoder_params_from_state_dict(state_dict, spec)
        del state_dict
        self.encoder = MaskedInferenceEncoder(self.contract, spec.depth, params, mlp_ratio=spec.mlp_ratio)
        self.transform = SmriMaeTransform(img_size=spec.img_size)
        ttml.autograd.AutoContext.get_instance().set_gradient_mode(ttml.autograd.GradMode.DISABLED)

    def embed(self, t1w_path: Path) -> tuple[np.ndarray, int]:
        """`(D,) float32` mean-pooled embedding for one subject, plus its
        observed-patch count.

        The pooling is upstream's, unchanged: sum over the real tokens divided
        by their count. Padding is excluded by construction here (the padded
        slots are simply not summed) rather than by a `token_mask` multiply,
        which is the same arithmetic without materializing the mask.
        """
        packed = self.pack(t1w_path)
        patch_embeds = self.forward(packed)
        real = patch_embeds[0, 0, : packed.n_visible, :]
        return real.mean(axis=0).astype(np.float32), packed.n_visible

    def pack(self, t1w_path: Path):
        sample = self.transform(nib.load(t1w_path))
        return pack_subject(sample, self.contract.l_vis, self.spec.patch_size, self.spec.mask_drop_scale)

    def forward(self, packed) -> np.ndarray:
        """`[1, 1, l_vis, D]` float32 patch embeddings for a `PackedSubject`."""
        keep = key_padding_mask(packed.n_visible + 1, self.contract.encoder_seq_len)
        out = self.encoder(packed.visible_values, packed.visible_ids, keep)
        return np.asarray(out.to_numpy(), dtype=np.float32)


def choose_l_vis(backbone_free_transform: SmriMaeTransform, t1w_paths: list[Path], patch_size: int) -> int:
    """`l_vis` for a run: the largest observed-patch count over the subjects,
    rounded up so `l_vis + 1` is tile-aligned.

    Measured over the real subjects rather than assumed, because the fomo_tune
    brain mask is a mean-intensity threshold that keeps skull and neck, so its
    occupancy has nothing to do with the SynthSeg-masked occupancy the
    pretraining shape contract was fitted against.
    """
    from smri_mae.modules import patchify3d

    largest = 0
    for path in t1w_paths:
        sample = backbone_free_transform(nib.load(path))
        obs = patchify3d(sample["mask"][None].float(), (patch_size,) * 3)[0]
        largest = max(largest, int((obs.sum(dim=-1) > 0).sum()))
    return encoder_seq_len_for(largest) - 1


def embed_subjects(backbone: TTBackbone, subjects, on_subject) -> dict[str, SubjectEmbedding]:
    """One device forward per subject, in order. `on_subject(done, total, result)`
    is called after each one so a pass that runs for tens of minutes is visibly
    alive rather than silent."""
    table: dict[str, SubjectEmbedding] = {}
    for i, subject in enumerate(subjects):
        start = time.perf_counter()
        embedding, n_visible = backbone.embed(subject.t1w_path)
        result = SubjectEmbedding(subject.subject, embedding, n_visible, time.perf_counter() - start)
        table[subject.subject] = result
        on_subject(i + 1, len(subjects), result)
    return table
