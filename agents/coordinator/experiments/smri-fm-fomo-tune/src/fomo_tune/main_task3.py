"""FOMO task 3: brain age regression, scored by Pearson r and MAE as the challenge scores it.

`Task3Method` is the part we tune -- features, head, hyperparameters. The protocol below it is
fixed so scores stay comparable across iterations: 20-fold over the 494 subjects, pool the
out-of-fold predictions, bootstrap subjects for the CI.

`train` runs that protocol then fits and saves a head; `predict` is the challenge contract, one t1
path in and one age out. Both go through `Task3Method.predict`, so every fold exercises the path
the submission will run.
"""

import argparse
import json
import logging
import time
from dataclasses import dataclass
from pathlib import Path

import joblib
import nibabel as nib
import numpy as np
import torch
from omegaconf import OmegaConf
from sklearn.linear_model import RidgeCV
from sklearn.model_selection import KFold
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

from fomo_tune.backbone import load_backbone
from fomo_tune.utils import git_sha, set_seed, setup_logging

logger = logging.getLogger("fomo_tune")

Images = dict[str, nib.Nifti1Image]


@dataclass
class Config:
    task: str = "task3"
    ckpt_path: str = "hf://medarc/walnut/checkpoints/pretrain_full_90_10_h100/checkpoint-last.pth"
    output_root: str = "output/fomo_tune"
    name: str = "task3"
    device: str = "cuda"
    seed: int = 4466


# ---- method: the part we tune -----------------------------------------------------------


class Task3Method:
    """Frozen sMRI MAE, mean-pooled tokens over the t1w, ridge head."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.backbone, self.transform = load_backbone(cfg.ckpt_path)
        self.device = torch.device(cfg.device)
        self.backbone.to(self.device).eval().requires_grad_(False)
        self.cache: dict[str, np.ndarray] = {}
        self.head = None

    @torch.inference_mode()
    def features(self, images: Images) -> np.ndarray:
        """(D,) per subject. A pure function of the images, so training and inference agree."""
        sample = self.transform(images["t1w"])
        batch = {key: value[None].to(self.device) for key, value in sample.items()}

        with torch.autocast("cuda", torch.bfloat16, enabled=self.device.type == "cuda"):
            out = self.backbone(batch)

        patch_embeds = out["patch_embeds"]
        token_mask = out["token_mask"].bool().unsqueeze(-1)
        embed = (patch_embeds * token_mask).sum(dim=1) / token_mask.sum(dim=1)
        return embed[0].float().cpu().numpy()

    def cached_features(self, row: dict) -> np.ndarray:
        if row["subject"] not in self.cache:
            self.cache[row["subject"]] = self.features(row)
        return self.cache[row["subject"]]

    def fit(self, rows: list[dict]) -> None:
        X = np.stack([self.cached_features(row) for row in rows])
        y = np.array([row["age"] for row in rows], dtype=float)

        # RidgeCV picks alpha by its own efficient leave-one-out, so the fold's own split is
        # never touched by model selection
        self.head = make_pipeline(StandardScaler(), RidgeCV(alphas=np.logspace(-3, 6, 19)))
        self.head.fit(X, y)

    def predict(self, images: Images) -> float:
        """Age in years."""
        X = self.features(images)[None]
        return float(self.head.predict(X)[0])

    def save(self, model_dir: Path) -> None:
        """Everything `load` needs but the backbone weights, which stay wherever `ckpt_path`
        points -- a few hundred KB, so a run saves one without copying a 3.7G checkpoint."""
        model_dir.mkdir(parents=True, exist_ok=True)
        OmegaConf.save(self.cfg, model_dir / "config.yaml")
        joblib.dump(self.head, model_dir / "head.joblib")

    @classmethod
    def load(cls, model_dir: Path, **overrides) -> "Task3Method":
        """Rebuild a fitted method from `save`. Overrides are Config fields, for what differs
        between here and the container -- the backbone path, the device."""
        cfg = OmegaConf.merge(
            OmegaConf.structured(Config), OmegaConf.load(model_dir / "config.yaml"), overrides
        )
        method = cls(cfg)
        method.head = joblib.load(model_dir / "head.joblib")
        return method


# ---- protocol: the part we hold fixed ---------------------------------------------------

# Every image the task ships. The method picks which of them it wants, as at inference, where the
# challenge hands over the modalities whether or not a model uses them.
IMAGE_COLS = ("t1w",)


def cross_validate(
    rows: list[dict], method: Task3Method, seed: int = 0, n_folds: int = 20
) -> tuple[np.ndarray, np.ndarray]:
    """Out-of-fold age for every subject, each predicted by a head fit on the other folds."""
    y = np.array([row["age"] for row in rows], dtype=float)
    oof = np.zeros(len(rows), dtype=float)
    folds = KFold(n_splits=n_folds, shuffle=True, random_state=seed)
    start = time.perf_counter()
    for fold, (train, test) in enumerate(folds.split(rows)):
        method.fit([rows[i] for i in train])
        for i in test:
            oof[i] = method.predict({key: rows[i][key] for key in IMAGE_COLS})
        logger.info(
            f"fold {fold + 1}/{n_folds} n={len(test)} mae={np.abs(y[test] - oof[test]).mean():.2f} "
            f"({time.perf_counter() - start:.0f}s)"
        )
    return y, oof


def metrics(y: np.ndarray, oof: np.ndarray) -> dict:
    return {
        "pearson_r": float(np.corrcoef(y, oof)[0, 1]),
        "mae": float(np.abs(y - oof).mean()),
    }


def score(
    y: np.ndarray, oof: np.ndarray, seed: int = 0, n_boot: int = 2000, alpha: float = 0.05
) -> dict:
    """Both challenge metrics, each with a percentile CI resampling subjects with replacement."""
    rng = np.random.default_rng(seed)
    resamples = rng.integers(0, len(y), size=(n_boot, len(y)))

    summary = {}
    for name, point in metrics(y, oof).items():
        samples = [metrics(y[rows], oof[rows])[name] for rows in resamples]
        low, high = np.percentile(samples, [100 * alpha / 2, 100 * (1 - alpha / 2)])
        summary[name] = point
        summary[f"{name}_ci_low"] = float(low)
        summary[f"{name}_ci_high"] = float(high)
    return summary


# ---- entrypoints ------------------------------------------------------------------------


def train(args: argparse.Namespace) -> None:
    # imported here, not at the top, so the container needs no dataset stack to run `predict`
    from fomo_tune.datasets import load_fomo_task3

    cfg = OmegaConf.merge(OmegaConf.structured(Config), OmegaConf.from_dotlist(args.overrides))
    run_dir = Path(cfg.output_root) / cfg.name
    run_dir.mkdir(parents=True, exist_ok=True)

    setup_logging(run_dir)
    set_seed(cfg.seed)
    logger.info(f"run {cfg.name} (git {git_sha()})")
    logger.info(f"config:\n{OmegaConf.to_yaml(cfg).rstrip()}")
    OmegaConf.save(cfg, run_dir / "config.yaml")

    rows = list(load_fomo_task3())
    ages = np.array([row["age"] for row in rows])
    logger.info(
        f"dataset: {len(rows)} subjects, age {ages.min()}-{ages.max()} mean {ages.mean():.1f}"
    )

    method = Task3Method(cfg)
    start = time.perf_counter()
    y, oof = cross_validate(rows, method)
    run_time = time.perf_counter() - start
    summary = score(y, oof)

    # the shipped head sees all n subjects, so it is not any of the models scored above
    method.fit(rows)
    method.save(run_dir / "model")

    preds = [
        {"subject": row["subject"], "age": float(age), "pred": float(pred)}
        for row, age, pred in zip(rows, y, oof)
    ]
    (run_dir / "preds.json").write_text("".join(json.dumps(pred) + "\n" for pred in preds))

    record = {"name": cfg.name, **summary, "run_time": round(run_time, 1)}
    (run_dir / "metrics.json").write_text(json.dumps(record) + "\n")
    scores = "  ".join(f"{k}={v:.4f}" for k, v in summary.items())
    logger.info(f"result: {scores}  ({run_time:.0f}s)")


def predict(args: argparse.Namespace) -> None:
    """The challenge contract: a t1 path in, one age written to `--output`.

    `/app/predict.py` in the container is a shim over this, so what scores the submission is the
    code cross-validation already ran, not something generated at build time.
    """
    overrides = {"device": args.device}
    if args.ckpt_path:
        overrides["ckpt_path"] = args.ckpt_path
    method = Task3Method.load(args.model_dir, **overrides)

    age = method.predict({"t1w": nib.load(args.t1)})

    args.output.write_text(f"{age:.6f}\n")


def main() -> None:
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(required=True)

    train_parser = modes.add_parser("train", help="cross-validate over the task, then fit and save")
    train_parser.add_argument("overrides", nargs="*", help="config overrides, e.g. device=cpu")
    train_parser.set_defaults(run=train)

    predict_parser = modes.add_parser("predict", help="one subject, one age in years")
    predict_parser.add_argument("--t1", type=Path, required=True)
    predict_parser.add_argument("--output", type=Path, required=True)
    predict_parser.add_argument("--model-dir", type=Path, default=Path("/app/model"))
    predict_parser.add_argument("--ckpt-path", help="overrides the trained config's backbone path")
    predict_parser.add_argument("--device", default="cuda" if torch.cuda.is_available() else "cpu")
    predict_parser.set_defaults(run=predict)

    args = parser.parse_args()
    args.run(args)


if __name__ == "__main__":
    main()
