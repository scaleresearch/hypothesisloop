"""FOMO task 2: meningioma segmentation, scored by per-subject Dice as the challenge scores it.

**`Task2Method` is what we tune.** Today: a frozen sMRI MAE over flair, one token per 8mm patch,
and a logistic head predicting each patch's tumour fraction. `predict_proba` returns that fraction
as a volume on the input's own grid; the method does not decide where to cut it.

**The protocol is held fixed**, or scores stop being comparable across iterations: leave one
subject out, Dice at every threshold in a fixed grid, then the single cut maximizing mean Dice
over the out-of-fold subjects. That cut is tuned on the subjects it is then scored on, so the
number is somewhat inflated -- as is anything else tuned by re-running and reading it.

`train` runs the protocol then fits and saves a head; `predict` is the challenge contract. Both go
through `Task2Method.predict_proba`, so every fold exercises the path the submission runs.
"""

import argparse
import json
import logging
import time
from dataclasses import dataclass
from pathlib import Path
from typing import NamedTuple

import joblib
import nibabel as nib
import numpy as np
import torch
from einops import reduce, repeat
from omegaconf import OmegaConf
from scipy import ndimage
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import Pipeline, make_pipeline
from sklearn.preprocessing import StandardScaler

from fomo_tune.backbone import load_backbone
from fomo_tune.utils import git_sha, set_seed, setup_logging

logger = logging.getLogger("fomo_tune")

Images = dict[str, nib.Nifti1Image]


@dataclass
class Config:
    task: str = "task2"
    ckpt_path: str = "hf://medarc/walnut/checkpoints/pretrain_full_90_10_h100/checkpoint-last.pth"
    modality: str = "flair"
    output_root: str = "output/fomo_tune"
    name: str = "task2"
    inverse_reg: float = 1.0
    largest_component: bool = True
    device: str = "cuda"
    seed: int = 4466


# ---- geometry -----------------------------------------------------------------------------


def repack(img: nib.Nifti1Image) -> nib.Nifti1Image:
    """Round-trip through nibabel: the HF Nifti wrapper's own reorientation is not trustworthy."""
    return nib.Nifti1Image(img.dataobj, img.affine, img.header)


def resample_nearest(
    volume: np.ndarray, source_affine: np.ndarray, target_affine: np.ndarray, target_shape: tuple
) -> np.ndarray:
    """`volume` read at every voxel of the target grid, nearest neighbour, zero outside it."""
    target_to_source = np.linalg.inv(source_affine) @ target_affine
    matrix = target_to_source[:3, :3]
    offset = target_to_source[:3, 3]
    return ndimage.affine_transform(
        volume, matrix, offset, output_shape=target_shape, order=0, mode="constant", cval=0.0
    )


# ---- method: the part we tune ---------------------------------------------------------------


class Patches(NamedTuple):
    """One subject's kept patches, plus the tumour-voxel count of every patch on the grid."""

    features: np.ndarray  # (n_kept, dim)
    patch_ids: np.ndarray  # (n_kept,) indices into the flattened patch grid
    counts: np.ndarray  # (n_patches,)


def fit_head(features: np.ndarray, counts: np.ndarray, voxels: int, inverse_reg: float) -> Pipeline:
    """Voxel-level logistic regression, each patch collapsed to one positive and one negative row.

    Unbalanced on purpose: the fitted probability is then the patch's tumour fraction.
    """
    has_tumour = counts > 0
    rows = np.concatenate([features, features[has_tumour]])
    labels = np.concatenate([np.zeros(len(features)), np.ones(has_tumour.sum())])
    weights = np.concatenate([voxels - counts, counts[has_tumour]])

    keep = weights > 0
    head = make_pipeline(StandardScaler(), LogisticRegression(C=inverse_reg, max_iter=1000))
    head.fit(rows[keep], labels[keep], logisticregression__sample_weight=weights[keep])
    return head


class Task2Method:
    """Frozen sMRI MAE, per-patch tokens, logistic head on the patch tumour fraction."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.backbone, self.transform = load_backbone(cfg.ckpt_path)
        self.device = torch.device(cfg.device)
        self.backbone.to(self.device).eval().requires_grad_(False)
        self.modality = cfg.modality

        patchify = self.backbone.encoder.patchify
        self.grid_size = tuple(patchify.grid_size)
        self.patch_size = tuple(patchify.patch_size)
        self.img_size = tuple(patchify.img_size)
        self.voxels_per_patch = int(np.prod(self.patch_size))

        self.cache: dict[str, Patches] = {}
        self.head = None
        self.threshold = None

    @torch.inference_mode()
    def embed(self, images: Images) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        """The kept patches' features and grid indices, and the affine of the grid they live on."""
        sample = self.transform(images[self.modality])
        batch = {key: value[None].to(self.device) for key, value in sample.items()}

        with torch.autocast("cuda", torch.bfloat16, enabled=self.device.type == "cuda"):
            out = self.backbone(batch)

        keep = out["token_mask"][0].bool()
        features = out["patch_embeds"][0][keep].float().cpu().numpy()
        patch_ids = out["patch_ids"][0][keep].cpu().numpy()
        return features, patch_ids, sample["affine"].numpy()

    def patch_counts(self, seg: nib.Nifti1Image, grid_affine: np.ndarray) -> np.ndarray:
        """Tumour voxels per patch, over the whole grid, in the encoder's flattened order."""
        seg = repack(seg)
        labels = np.asarray(seg.dataobj, dtype=np.float32).round()
        on_grid = resample_nearest(labels, seg.affine, grid_affine, self.img_size)
        px, py, pz = self.patch_size
        return reduce(on_grid, "(gx px) (gy py) (gz pz) -> (gx gy gz)", "sum", px=px, py=py, pz=pz)

    def cached_patches(self, row: dict) -> Patches:
        """Cached: leave-one-out revisits every subject n times."""
        if row["subject"] not in self.cache:
            features, patch_ids, grid_affine = self.embed(row)
            counts = self.patch_counts(row["seg"], grid_affine)
            self.cache[row["subject"]] = Patches(features, patch_ids, counts)
        return self.cache[row["subject"]]

    def fit(self, rows: list[dict]) -> None:
        subjects = [self.cached_patches(row) for row in rows]
        features = np.concatenate([subject.features for subject in subjects])
        counts = np.concatenate([subject.counts[subject.patch_ids] for subject in subjects])
        self.head = fit_head(features, counts, self.voxels_per_patch, self.cfg.inverse_reg)

    def predict_proba(self, images: Images) -> nib.Nifti1Image:
        """Tumour probability per voxel on the input's own grid, constant within each patch."""
        features, patch_ids, grid_affine = self.embed(images)

        # zero where the encoder kept no token, which is a patch with no in-brain voxel
        fractions = np.zeros(int(np.prod(self.grid_size)), dtype=np.float32)
        fractions[patch_ids] = self.head.predict_proba(features)[:, 1]

        gx, gy, gz = self.grid_size
        px, py, pz = self.patch_size
        on_grid = repeat(
            fractions,
            "(gx gy gz) -> (gx px) (gy py) (gz pz)",
            gx=gx,
            gy=gy,
            gz=gz,
            px=px,
            py=py,
            pz=pz,
        )

        image = repack(images[self.modality])
        on_input = resample_nearest(on_grid, grid_affine, image.affine, image.shape)
        return nib.Nifti1Image(on_input, image.affine)

    def binarize(self, probabilities: np.ndarray, threshold: float) -> np.ndarray:
        """Probabilities to a mask. All postprocessing lives here, so the protocol can search it
        by calling this at every candidate threshold rather than knowing what it does."""
        mask = probabilities >= threshold
        if not self.cfg.largest_component or not mask.any():
            return mask
        blobs, _ = ndimage.label(mask)
        sizes = np.bincount(blobs.reshape(-1))
        sizes[0] = 0
        return blobs == sizes.argmax()

    def predict(self, images: Images) -> nib.Nifti1Image:
        """A binary mask on the input's own grid, which is what the challenge scores."""
        assert self.threshold is not None, "threshold is set by `train` or by `load`, not by `fit`"
        probabilities = self.predict_proba(images)
        mask = self.binarize(np.asarray(probabilities.dataobj), self.threshold)
        return nib.Nifti1Image(mask.astype(np.uint8), probabilities.affine)

    def save(self, model_dir: Path) -> None:
        """Config, head and threshold; the weights stay wherever `ckpt_path` points."""
        model_dir.mkdir(parents=True, exist_ok=True)
        OmegaConf.save(self.cfg, model_dir / "config.yaml")
        joblib.dump({"head": self.head, "threshold": self.threshold}, model_dir / "head.joblib")

    @classmethod
    def load(cls, model_dir: Path, **overrides) -> "Task2Method":
        """Rebuild a fitted method from `save`. Overrides are Config fields: ckpt path, device."""
        cfg = OmegaConf.merge(
            OmegaConf.structured(Config), OmegaConf.load(model_dir / "config.yaml"), overrides
        )
        method = cls(cfg)
        state = joblib.load(model_dir / "head.joblib")
        method.head, method.threshold = state["head"], state["threshold"]
        return method


# ---- protocol: the part we hold fixed ---------------------------------------------------

# Every image the task ships. The method picks which of them it wants, as at inference, where the
# challenge hands over all the modalities whether or not a model uses them.
IMAGE_COLS = ("dwi_b1000", "flair")

# Probabilities sit near the 2.4e-4 voxel prevalence, so the grid is geometric rather than linear.
THRESHOLDS = np.logspace(-6, -0.3, 60)


class Curves(NamedTuple):
    """Everything the protocol reports is a read off these, so no fold is ever recomputed."""

    dice: np.ndarray  # (n_subjects, n_thresholds)
    predicted_voxels: np.ndarray  # (n_subjects, n_thresholds)
    true_voxels: np.ndarray  # (n_subjects,)


def subject_curves(
    method: Task2Method, probabilities: np.ndarray, truth: np.ndarray
) -> tuple[np.ndarray, np.ndarray]:
    """One subject's Dice and predicted voxel count at every threshold in THRESHOLDS."""
    true_voxels = int(truth.sum())

    dice = np.zeros(len(THRESHOLDS))
    predicted = np.zeros(len(THRESHOLDS))
    for i, threshold in enumerate(THRESHOLDS):
        prediction = method.binarize(probabilities, threshold)
        predicted_voxels = int(prediction.sum())
        overlap = int(np.logical_and(prediction, truth).sum())
        denominator = predicted_voxels + true_voxels

        predicted[i] = predicted_voxels
        dice[i] = 2 * overlap / denominator if denominator else 1.0
    return dice, predicted


def leave_one_out(rows: list[dict], method: Task2Method) -> Curves:
    """Every subject's threshold curves, predicted by a head fit on the other n-1."""
    dice, predicted, true = [], [], []
    start = time.perf_counter()
    for row in rows:
        method.fit([r for r in rows if r["subject"] != row["subject"]])

        probabilities = method.predict_proba({key: row[key] for key in IMAGE_COLS})
        truth = np.asarray(repack(row["seg"]).dataobj).round() > 0
        assert probabilities.shape == truth.shape, "probabilities are not on the label grid"

        subject_dice, subject_predicted = subject_curves(
            method, np.asarray(probabilities.dataobj), truth
        )
        dice.append(subject_dice)
        predicted.append(subject_predicted)
        true.append(int(truth.sum()))

        best = subject_dice.argmax()
        logger.info(
            f"fold {len(dice)}/{len(rows)} {row['subject']} best={subject_dice[best]:.3f} "
            f"at thr={THRESHOLDS[best]:.2e} vox={true[-1]} "
            f"({time.perf_counter() - start:.0f}s)"
        )
    return Curves(np.stack(dice), np.stack(predicted), np.array(true))


def score(curves: Curves, seed: int = 0, n_boot: int = 2000, alpha: float = 0.05) -> dict:
    """Mean per-subject Dice at the best single threshold, plus a percentile CI over subjects.

    `dice_oracle` lets every subject cut where it likes, which bounds any thresholding rule.
    """
    best = int(curves.dice.mean(axis=0).argmax())
    dice = curves.dice[:, best]

    rng = np.random.default_rng(seed)
    resamples = rng.integers(0, len(dice), size=(n_boot, len(dice)))
    samples = dice[resamples].mean(axis=1)
    low, high = np.percentile(samples, [100 * alpha / 2, 100 * (1 - alpha / 2)])

    return {
        "dice": float(dice.mean()),
        "dice_ci_low": float(low),
        "dice_ci_high": float(high),
        "dice_oracle": float(curves.dice.max(axis=1).mean()),
        "threshold": float(THRESHOLDS[best]),
    }


# ---- entrypoints ------------------------------------------------------------------------


def train(args: argparse.Namespace) -> None:
    # imported here, not at the top, so the container needs no dataset stack to run `predict`
    from fomo_tune.datasets import load_fomo_task2

    cfg = OmegaConf.merge(OmegaConf.structured(Config), OmegaConf.from_dotlist(args.overrides))
    run_dir = Path(cfg.output_root) / cfg.name
    run_dir.mkdir(parents=True, exist_ok=True)

    setup_logging(run_dir)
    set_seed(cfg.seed)
    logger.info(f"run {cfg.name} (git {git_sha()})")
    logger.info(f"config:\n{OmegaConf.to_yaml(cfg).rstrip()}")
    OmegaConf.save(cfg, run_dir / "config.yaml")

    # decoded once: leave-one-out revisits every subject n times, and the niftis are small
    rows = list(load_fomo_task2())
    logger.info(f"dataset: {len(rows)} subjects")

    method = Task2Method(cfg)
    start = time.perf_counter()
    curves = leave_one_out(rows, method)
    run_time = time.perf_counter() - start
    summary = score(curves)

    # the shipped model sees all n subjects, so it is not any of the models scored above
    method.fit(rows)
    method.threshold = summary["threshold"]
    method.save(run_dir / "model")

    record = {"name": cfg.name, **summary, "run_time": round(run_time, 1)}
    (run_dir / "metrics.json").write_text(json.dumps(record) + "\n")
    np.savez(
        run_dir / "curves.npz",
        subjects=[row["subject"] for row in rows],
        thresholds=THRESHOLDS,
        **curves._asdict(),
    )
    scores = "  ".join(f"{k}={v:.4f}" for k, v in summary.items())
    logger.info(f"result: {scores}  ({run_time:.0f}s)")


def predict(args: argparse.Namespace) -> None:
    """The challenge contract: modality paths in, a mask nifti written to `--output`."""
    overrides = {"device": args.device}
    if args.ckpt_path:
        overrides["ckpt_path"] = args.ckpt_path
    method = Task2Method.load(args.model_dir, **overrides)

    # every image the challenge hands over, as in `leave_one_out`; the method takes what it uses
    paths = {"dwi_b1000": args.dwi, "flair": args.flair}
    mask = method.predict({key: nib.load(path) for key, path in paths.items()})

    nib.save(mask, args.output)


def main() -> None:
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(required=True)

    train_parser = modes.add_parser("train", help="leave-one-out over the task, then fit and save")
    train_parser.add_argument("overrides", nargs="*", help="config overrides, e.g. device=cpu")
    train_parser.set_defaults(run=train)

    predict_parser = modes.add_parser("predict", help="one subject, one mask nifti")
    for flag in ("--flair", "--dwi"):
        predict_parser.add_argument(flag, type=Path, required=True)
    # accepted and ignored: the 4th modality is t2s on some subjects and swi on others
    for flag in ("--t2s", "--swi"):
        predict_parser.add_argument(flag, type=Path)
    predict_parser.add_argument("--output", type=Path, required=True)
    predict_parser.add_argument("--model-dir", type=Path, default=Path("/app/model"))
    predict_parser.add_argument("--ckpt-path", help="overrides the trained config's backbone path")
    predict_parser.add_argument("--device", default="cuda" if torch.cuda.is_available() else "cpu")
    predict_parser.set_defaults(run=predict)

    args = parser.parse_args()
    args.run(args)


if __name__ == "__main__":
    main()
