"""Fold-safe confound handling for the task-method head.

Task_5's public 48-subject set carries a real scanner/acquisition confound: PMG-positive
scans have systematically larger physical head coverage (AP-extent = the number of slices
along the anterior-posterior axis times the slice thickness on that axis, read straight off
the T1w NIfTI header) than negative scans. That single scalar alone predicts the label at
AUROC ~0.91-0.93 (see ../../scratch_task5_repro/confound_regression_full_protocol.py and
confound_check.py in the same dir), which is why naive frozen-encoder-features +
LogisticRegressionCV on this dataset scores an inflated ~0.995 AUROC instead of a real
~0.68-0.8.

The fix validated in scratch (see scratch_task5_repro/featurelevel_debias_full_protocol.json,
AUROC=0.795 [0.652,0.912], residual-leak R^2 ~ -0.045) is feature-level fold-safe per-dimension
OLS residualization: for each outer CV fold, fit -- on TRAIN rows only -- a linear regression of
every one of the 1024 encoder feature dims on the scalar confound, then subtract the fitted
trend from BOTH train and test rows of that fold. Never fit on test rows.

Kept separate from run_task.py and small on purpose: a future hypothesis may want a different
confound (a different scalar, a different axis) or no confound correction at all, so this module
exposes plain functions rather than baking a specific confound into the CV loop.
"""

from __future__ import annotations

import nibabel as nib
import numpy as np


def ap_extent(t1w_path) -> float:
    """Physical anterior-posterior head coverage of a T1w NIfTI volume, in mm:
    (voxel count along the AP axis) * (voxel spacing along that axis).

    The AP axis is detected from the affine via `nibabel.aff2axcodes` (looks for 'A' or
    'P' among the three axis codes) rather than hardcoded to a fixed array index, so this
    keeps working if a future preprocessing step changes the volume's on-disk orientation.
    The 48-subject public Task_5 set is uniformly RAS (verified in
    scratch_task5_repro/headers.json), where the AP axis is array index 1 -- the same
    convention scratch_task5_repro/confound_regression_full_protocol.py and analyze.py use.
    """
    img = nib.load(str(t1w_path))
    axcodes = nib.aff2axcodes(img.affine)
    ap_axis = next(i for i, code in enumerate(axcodes) if code in ("A", "P"))
    shape = img.shape
    zooms = img.header.get_zooms()
    return float(shape[ap_axis]) * float(zooms[ap_axis])


class FoldSafeResidualizer:
    """Per-feature-dimension OLS residualization of X on a scalar confound c, fit on
    train rows only and applied to both train and test rows of a fold.

    For a single scalar covariate this is closed-form per dimension (no matrix inverse
    needed even though X has many columns): slope_j = cov(c, X_j) / var(c). Equivalent to
    fitting an independent 1-D OLS per feature dimension.

    Usage per outer CV fold:
        r = FoldSafeResidualizer().fit(X[train], c[train])
        X_train_debiased = r.transform(X[train], c[train])
        X_test_debiased = r.transform(X[test], c[test])
    """

    def __init__(self) -> None:
        self.c_mean_: float | None = None
        self.slope_: np.ndarray | None = None

    def fit(self, X: np.ndarray, c: np.ndarray) -> "FoldSafeResidualizer":
        c = np.asarray(c, dtype=float)
        c_mean = c.mean()
        c_centered = c - c_mean
        denom = float(np.sum(c_centered**2))
        if denom == 0.0:
            # Degenerate fold (constant confound on this train split): nothing to remove.
            self.c_mean_, self.slope_ = c_mean, np.zeros(X.shape[1])
            return self
        x_mean = X.mean(axis=0)
        self.slope_ = (c_centered @ (X - x_mean)) / denom  # shape (n_features,)
        self.c_mean_ = c_mean
        return self

    def transform(self, X: np.ndarray, c: np.ndarray) -> np.ndarray:
        if self.c_mean_ is None or self.slope_ is None:
            raise RuntimeError("FoldSafeResidualizer.transform called before fit")
        c = np.asarray(c, dtype=float)
        return X - np.outer(c - self.c_mean_, self.slope_)

    def fit_transform(self, X: np.ndarray, c: np.ndarray) -> np.ndarray:
        return self.fit(X, c).transform(X, c)
