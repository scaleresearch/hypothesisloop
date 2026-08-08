"""Run FOMO task 3 (brain age) or task 5 (polymicrogyria) end to end on
Tenstorrent.

Structure follows upstream's split between the *method* (what a hypothesis is
allowed to change: features, head, hyperparameters) and the *protocol* (frozen,
so scores stay comparable): 20-fold `KFold(shuffle=True, random_state=0)` over
the subjects, pool the out-of-fold predictions, bootstrap subjects for the CI.

The protocol's scoring is imported from upstream's own task modules rather than
restated here, so "what the number means" is owned by the reference
implementation and cannot drift with a local edit.

The one structural difference from upstream is where the backbone runs: upstream
interleaves `features()` calls with the fold loop (and repeats them in
`predict`). Here every subject is embedded once, up front, and the fold loop is
pure sklearn over a cached matrix -- same predictions, one device pass per
subject, and the expensive part is a single visible phase rather than something
smeared across 20 folds.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
import time
from pathlib import Path

import numpy as np
from sklearn.linear_model import LogisticRegressionCV, RidgeCV
from sklearn.model_selection import KFold
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

from fomo_tune.backbone import SmriMaeTransform
from fomo_tune.main_task3 import score as score_task3
from fomo_tune.main_task5 import score as score_task5
from fomo_tune_tt.backbone_tt import TTBackbone, choose_l_vis, embed_subjects
from fomo_tune_tt.data import load_task
from fomo_tune_tt.metrics import post_metric

logger = logging.getLogger("fomo_tune_tt")

N_FOLDS = 20
PROTOCOL_SEED = 0  # upstream `cross_validate(..., seed=0)`


def head_for(task: str):
    """The method's head. Both are upstream's, verbatim."""
    if task == "task3":
        # RidgeCV picks alpha by its own efficient leave-one-out, so the fold's
        # own split is never touched by model selection.
        return make_pipeline(StandardScaler(), RidgeCV(alphas=np.logspace(-3, 6, 19)))
    return make_pipeline(
        StandardScaler(),
        LogisticRegressionCV(Cs=10, class_weight="balanced", scoring="roc_auc", max_iter=1000, l1_ratios=(0,), use_legacy_attributes=False),
    )


def cross_validate(task: str, features: np.ndarray, y: np.ndarray) -> np.ndarray:
    """Out-of-fold prediction for every subject. Task 3 predicts an age; task 5
    predicts the positive-class probability, indexing `classes_` rather than
    assuming column 1 (upstream's `Task5Method.predict`'s reasoning)."""
    oof = np.zeros(len(y), dtype=float)
    folds = KFold(n_splits=N_FOLDS, shuffle=True, random_state=PROTOCOL_SEED)
    start = time.perf_counter()
    for fold, (train, test) in enumerate(folds.split(features)):
        head = head_for(task)
        head.fit(features[train], y[train])
        if task == "task3":
            oof[test] = head.predict(features[test])
        else:
            positive = list(head.classes_).index(1)
            oof[test] = head.predict_proba(features[test])[:, positive]
        logger.info(f"fold {fold + 1}/{N_FOLDS} n={len(test)} ({time.perf_counter() - start:.0f}s)")
        post_metric((fold + 1) / N_FOLDS, float(time.perf_counter() - start), "cv_seconds")
    return oof


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--task", choices=("task3", "task5"), required=True)
    parser.add_argument("--ckpt-path", required=True, help="local path or hf://org/repo/file")
    parser.add_argument("--data-root", type=Path, required=True, help="the unpacked dataset dir holding Task_3/ or Task_5/")
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument(
        "--l-vis",
        type=int,
        help="encoder sequence budget (l_vis + 1 must be tile-aligned). Omitted: measured over "
        "this task's own subjects, which costs one host-side transform pass over the dataset.",
    )
    parser.add_argument("--limit-subjects", type=int, help="smoke-test knob: embed only the first N subjects")
    args = parser.parse_args()

    args.output_dir.mkdir(parents=True, exist_ok=True)
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(message)s",
        datefmt="%H:%M:%S",
        handlers=[logging.StreamHandler(sys.stdout), logging.FileHandler(args.output_dir / "log.txt")],
    )

    subjects = load_task(args.task, args.data_root)
    if args.limit_subjects:
        subjects = subjects[: args.limit_subjects]
    logger.info(f"{args.task}: {len(subjects)} subjects from {args.data_root}")

    l_vis = args.l_vis
    if l_vis is None:
        start = time.perf_counter()
        l_vis = choose_l_vis(SmriMaeTransform(), [s.t1w_path for s in subjects], patch_size=8)
        logger.info(f"measured l_vis={l_vis} over {len(subjects)} subjects ({time.perf_counter() - start:.0f}s)")

    backbone = TTBackbone(args.ckpt_path, l_vis=l_vis)
    logger.info(f"backbone: {backbone.spec}")
    logger.info(f"contract: l_vis={backbone.contract.l_vis} encoder_seq_len={backbone.contract.encoder_seq_len}")

    embed_start = time.perf_counter()

    def on_subject(done: int, total: int, result) -> None:
        logger.info(f"embed {done}/{total} {result.subject} n_visible={result.n_visible} ({result.seconds:.1f}s)")
        post_metric(done / total, (time.perf_counter() - embed_start) / done, "seconds_per_subject")

    table = embed_subjects(backbone, subjects, on_subject)
    embed_seconds = time.perf_counter() - embed_start
    logger.info(f"embedded {len(table)} subjects in {embed_seconds:.0f}s ({embed_seconds / len(table):.1f}s/subject)")

    features = np.stack([table[s.subject].embedding for s in subjects])
    y = np.array([s.label for s in subjects], dtype=float if args.task == "task3" else int)
    np.savez(args.output_dir / "embeddings.npz", subjects=np.array([s.subject for s in subjects]), features=features, y=y)

    oof = cross_validate(args.task, features, y)
    summary = (score_task3 if args.task == "task3" else score_task5)(y, oof)

    record = {
        "task": args.task,
        "n_subjects": len(subjects),
        "l_vis": backbone.contract.l_vis,
        "encoder_seq_len": backbone.contract.encoder_seq_len,
        "embed_seconds": round(embed_seconds, 1),
        "seconds_per_subject": round(embed_seconds / len(table), 2),
        **summary,
    }
    (args.output_dir / "metrics.json").write_text(json.dumps(record, indent=2) + "\n")
    (args.output_dir / "preds.json").write_text(
        "".join(json.dumps({"subject": s.subject, "label": float(y[i]), "pred": float(oof[i])}) + "\n" for i, s in enumerate(subjects))
    )

    for name in ("pearson_r", "mae", "auroc"):
        if name in summary:
            post_metric(1.0, float(summary[name]), name)
    post_metric(1.0, record["seconds_per_subject"], "seconds_per_subject")

    logger.info("result: " + "  ".join(f"{k}={v}" for k, v in record.items()))


if __name__ == "__main__":
    main()
