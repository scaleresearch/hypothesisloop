"""FOMO task 1: acute infarct classification, scored by AUROC as the challenge scores it.

`Task1Method` is the part we tune -- features, head, hyperparameters. The protocol below it is
fixed so scores stay comparable across iterations: leave one subject out, pool the out-of-fold
predictions, bootstrap subjects for the CI.

`train` runs that protocol then fits and saves a head; `predict` is the challenge contract,
modality paths in and one probability out. Both go through `Task1Method.predict`, so every fold
exercises the path the submission will run.
"""

import argparse
import json
import logging
import time
from dataclasses import dataclass, field
from pathlib import Path

import joblib
import nibabel as nib
import numpy as np
import torch
from omegaconf import OmegaConf
from sklearn.linear_model import LogisticRegressionCV
from sklearn.metrics import roc_auc_score
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

from fomo_tune.backbone import load_backbone
from fomo_tune.utils import git_sha, set_seed, setup_logging

logger = logging.getLogger("fomo_tune")

Images = dict[str, nib.Nifti1Image]


@dataclass
class Config:
    task: str = "task1"
    ckpt_path: str = "hf://medarc/walnut/checkpoints/pretrain_full_90_10_h100/checkpoint-last.pth"
    modalities: list[str] = field(default_factory=lambda: ["dwi_b1000"])
    output_root: str = "output/fomo_tune"
    name: str = "task1"
    device: str = "cuda"
    seed: int = 4466


# ---- method: the part we tune -----------------------------------------------------------


class Task1Method:
    """Frozen sMRI MAE, mean-pooled tokens per modality concatenated, logistic head."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.backbone, self.transform = load_backbone(cfg.ckpt_path)
        self.device = torch.device(cfg.device)
        self.backbone.to(self.device).eval().requires_grad_(False)
        self.modalities = list(cfg.modalities)
        self.cache: dict[str, np.ndarray] = {}
        self.head = None

    @torch.inference_mode()
    def features(self, images: Images) -> np.ndarray:
        """(D,) per subject. A pure function of the images, so training and inference agree."""
        pooled = []
        for modality in self.modalities:
            sample = self.transform(images[modality])
            batch = {key: value[None].to(self.device) for key, value in sample.items()}

            with torch.autocast("cuda", torch.bfloat16, enabled=self.device.type == "cuda"):
                out = self.backbone(batch)

            patch_embeds = out["patch_embeds"]
            token_mask = out["token_mask"].bool().unsqueeze(-1)
            embed = (patch_embeds * token_mask).sum(dim=1) / token_mask.sum(dim=1)
            pooled.append(embed[0].float().cpu())

        return torch.cat(pooled).numpy()

    def cached_features(self, row: dict) -> np.ndarray:
        if row["subject"] not in self.cache:
            self.cache[row["subject"]] = self.features(row)
        return self.cache[row["subject"]]

    def fit(self, rows: list[dict]) -> None:
        X = np.stack([self.cached_features(row) for row in rows])
        y = np.array([row["label"] for row in rows])

        clf = LogisticRegressionCV(
            Cs=10,
            class_weight="balanced",
            scoring="roc_auc",
            max_iter=1000,
            l1_ratios=(0,),
            use_legacy_attributes=False,
        )
        self.head = make_pipeline(StandardScaler(), clf)
        self.head.fit(X, y)
        self.positive = list(self.head.classes_).index(1)

    def predict(self, images: Images) -> float:
        """Positive-class probability. Indexes `classes_` rather than assuming column 1, which
        would silently score the wrong class if the label order differed."""
        X = self.features(images)[None]
        probs = self.head.predict_proba(X)[0]
        return float(probs[self.positive])

    def save(self, model_dir: Path) -> None:
        """Everything `load` needs but the backbone weights, which stay wherever `ckpt_path`
        points -- a few hundred KB, so a run saves one without copying a 3.7G checkpoint."""
        model_dir.mkdir(parents=True, exist_ok=True)
        OmegaConf.save(self.cfg, model_dir / "config.yaml")
        joblib.dump({"head": self.head, "positive": self.positive}, model_dir / "head.joblib")

    @classmethod
    def load(cls, model_dir: Path, **overrides) -> "Task1Method":
        """Rebuild a fitted method from `save`. Overrides are Config fields, for what differs
        between here and the container -- the backbone path, the device."""
        cfg = OmegaConf.merge(
            OmegaConf.structured(Config), OmegaConf.load(model_dir / "config.yaml"), overrides
        )
        method = cls(cfg)
        state = joblib.load(model_dir / "head.joblib")
        method.head, method.positive = state["head"], state["positive"]
        return method


# ---- protocol: the part we hold fixed ---------------------------------------------------

# Every image the task ships. The method picks which of them it wants, as at inference, where
# the challenge hands over all four modalities whether or not a model uses them.
IMAGE_COLS = ("adc", "dwi_b1000", "flair")


def leave_one_out(rows: list[dict], method: Task1Method) -> tuple[np.ndarray, np.ndarray]:
    """Out-of-fold score for every subject, each predicted by a head fit on the other n-1."""
    y = np.array([row["label"] for row in rows])
    oof = np.zeros(len(rows), dtype=float)
    start = time.perf_counter()
    for held_out, row in enumerate(rows):
        method.fit([r for r in rows if r["subject"] != row["subject"]])
        oof[held_out] = method.predict({key: row[key] for key in IMAGE_COLS})
        logger.info(
            f"fold {held_out + 1}/{len(rows)} {row['subject']} "
            f"y={y[held_out]} p={oof[held_out]:.3f} ({time.perf_counter() - start:.0f}s)"
        )
    return y, oof


def score(
    y: np.ndarray, oof: np.ndarray, seed: int = 0, n_boot: int = 2000, alpha: float = 0.05
) -> dict:
    """AUROC, the challenge metric, plus a percentile CI resampling subjects with replacement."""
    rng = np.random.default_rng(seed)
    samples = []
    for _ in range(n_boot):
        rows = rng.integers(0, len(y), size=len(y))
        if len(np.unique(y[rows])) < 2:
            continue
        samples.append(roc_auc_score(y[rows], oof[rows]))

    low, high = np.percentile(samples, [100 * alpha / 2, 100 * (1 - alpha / 2)])
    return {
        "auroc": float(roc_auc_score(y, oof)),
        "auroc_ci_low": float(low),
        "auroc_ci_high": float(high),
    }


# ---- entrypoints ------------------------------------------------------------------------


def train(args: argparse.Namespace) -> None:
    # imported here, not at the top, so the container needs no dataset stack to run `predict`
    from fomo_tune.datasets import load_fomo_task1

    cfg = OmegaConf.merge(OmegaConf.structured(Config), OmegaConf.from_dotlist(args.overrides))
    run_dir = Path(cfg.output_root) / cfg.name
    run_dir.mkdir(parents=True, exist_ok=True)

    setup_logging(run_dir)
    set_seed(cfg.seed)
    logger.info(f"run {cfg.name} (git {git_sha()})")
    logger.info(f"config:\n{OmegaConf.to_yaml(cfg).rstrip()}")
    OmegaConf.save(cfg, run_dir / "config.yaml")

    # decoded once: leave-one-out revisits every subject n times, and the niftis are small
    rows = list(load_fomo_task1())
    logger.info(f"dataset: {len(rows)} subjects, {sum(r['label'] for r in rows)} positive")

    method = Task1Method(cfg)
    start = time.perf_counter()
    y, oof = leave_one_out(rows, method)
    run_time = time.perf_counter() - start
    summary = score(y, oof)

    # the shipped head sees all n subjects, so it is not any of the models scored above
    method.fit(rows)
    method.save(run_dir / "model")

    preds = [
        {"subject": row["subject"], "label": int(label), "pred": float(pred)}
        for row, label, pred in zip(rows, y, oof)
    ]
    (run_dir / "preds.json").write_text("".join(json.dumps(pred) + "\n" for pred in preds))

    record = {"name": cfg.name, **summary, "run_time": round(run_time, 1)}
    (run_dir / "metrics.json").write_text(json.dumps(record) + "\n")
    scores = "  ".join(f"{k}={v:.4f}" for k, v in summary.items())
    logger.info(f"result: {scores}  ({run_time:.0f}s)")


def predict(args: argparse.Namespace) -> None:
    """The challenge contract: modality paths in, one probability written to `--output`.

    `/app/predict.py` in the container is a shim over this, so what scores the submission is the
    code leave-one-out already ran, not something generated at build time.
    """
    overrides = {"device": args.device}
    if args.ckpt_path:
        overrides["ckpt_path"] = args.ckpt_path
    method = Task1Method.load(args.model_dir, **overrides)

    # every image the challenge hands over, as in `leave_one_out`; the method takes what it uses
    paths = {"adc": args.adc, "dwi_b1000": args.dwi, "flair": args.flair}
    probability = method.predict({key: nib.load(path) for key, path in paths.items()})

    args.output.write_text(f"{probability:.6f}\n")


def main() -> None:
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(required=True)

    train_parser = modes.add_parser("train", help="leave-one-out over the task, then fit and save")
    train_parser.add_argument("overrides", nargs="*", help="config overrides, e.g. device=cpu")
    train_parser.set_defaults(run=train)

    predict_parser = modes.add_parser("predict", help="one subject, one probability")
    for flag in ("--flair", "--adc", "--dwi"):
        predict_parser.add_argument(flag, type=Path, required=True)
    # accepted and ignored: the 4th modality is swi on some subjects and t2s on others
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
