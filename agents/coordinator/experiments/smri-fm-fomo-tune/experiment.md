## EXPERIMENT DESCRIPTION

OBJECTIVE
Get the best possible **AUROC on FOMO26 Task 5 (polymicrogyria detection from T1w MRI)** out of a
frozen sMRI foundation-model encoder, scored exactly the way the challenge scores it: 20-fold
cross-validation over 48 subjects, out-of-fold probabilities pooled, AUROC over the pool.

This is a **linear-probe / representation-use problem, not a training problem.** The backbone is
a 3D ViT-L masked autoencoder, frozen by the challenge ("no new pretrain checkpoints will be
accepted") -- do not fine-tune it. What you change is everything around it: how a volume becomes
an input, which of the encoder's outputs you keep, how you pool them, what classifier head you
fit. That is where the entire signal is.

**This one checkpoint is shared across all 5 FOMO26 tasks** (this experiment is task 5 only).
Your hypothesis must stay at the *task-method* level -- which output you read, how you pool/fuse
it, what head you fit -- analogous to upstream's per-task `Task5Method` class, never the encoder's
weights or forward-pass numerics: a change there silently changes what every other task's future
experiment inherits. `encoder_tt.py`/`backbone_tt.py` already expose every readout point a
method-level hypothesis needs (`forward`, `forward_with_cls`, `forward_until`, `forward_multi` --
different reads of one frozen forward pass); reuse them rather than adding new ones. If a
hypothesis needs the encoder to compute something new, it's out of scope -- comment it, don't
implement it.

The accelerator underneath (a Tenstorrent Blackhole card running a hand-written TT-NN port of the
encoder) is a **solved black box.** Configure it through env vars; never read, build, or modify
it -- no hypothesis about it will move the metric.

SHARPENED OBJECTIVE FOR THIS ROUND (read this before anything else below)
  Two prior rounds each explored one half of the problem and neither is sufficient alone:
  - **Quality-only thread (insufficient alone):** label-adaptive high-capacity heads
    (LDA-shrinkage, PLS-DA, depth-fusion+LDA) gave real local AUROC gains (0.85-0.89 vs the 0.795
    debiased baseline) but every one of them FAILED the external FCD feature-generality probe --
    regressing to no-better-than (0.868) or clearly worse than FCD's own baseline (~0.887).
  - **Generalization-only thread (insufficient alone):** techniques that add ZERO label-adaptive
    capacity (CL2N, prespecified-config ensembling, FroFA feature augmentation, rank-constrained
    PCA+LDA at a fixed low rank) held local AUROC roughly steady AND passed the external FCD check
    cleanly (no regression vs FCD's own ~0.887 baseline) -- but none of them meaningfully beat
    0.795 locally either.
  **Neither of these is "done" -- this round's bar is a technique (or combination) that clears
  BOTH simultaneously:** (a) a MEANINGFUL local AUROC improvement over 0.795 (a tie or a
  fits-in-the-CI-noise-band bump does not count -- see the two prior threads above for what that
  looks like and why it isn't enough), AND (b) passes the external FCD feature-generality probe,
  i.e. does not regress vs FCD's own established baseline (~0.887, `scratch_fcd_validation/`).
  Finding only (a) or only (b) alone is NOT sufficient progress this round -- both have already
  been demonstrated separately and are not the open question anymore. The open question is
  whether something threads the needle between them.

UNTRIED TECHNIQUES FOR THREADING THE QUALITY/GENERALIZATION NEEDLE (this round's starting points
-- from a fresh codex literature consult specifically on this framing; ordered by ease of
sklearn-only implementation, all still respect the mandatory external-FCD-check requirement below)
  - **Nearest Shrunken Centroids (PAM)** (Tibshirani et al. 2002): soft-threshold each class-
    centroid difference toward zero with a single shrinkage parameter, tuned in-fold. Purpose-
    built for small-n/high-p classification; substantially less flexible than fitting a full
    covariance (LDA) or a supervised projection (PLS), so it sits closer to CL2N's zero-capacity
    end than LDA-shrinkage's -- the natural first thing to try. sklearn-only (small custom
    estimator, `StandardScaler` + shrink-toward-zero centroid distance).
  - **Diagonal-heavy Regularized Discriminant Analysis (RDA)** (Friedman 1989): constrain the
    covariance estimate to `Sigma_gamma = (1-gamma)*Sigma_hat + gamma*diag(Sigma_hat)`, with
    `gamma` restricted a priori to a small prespecified set close to 1 (e.g. {0.9, 0.97, 1.0}) --
    never tuned to be small, which would recover full LDA's failure mode. This is explicitly a
    small-but-nonzero-capacity point between CL2N (gamma=1, no correlation modeling at all) and
    LDA-shrinkage (gamma effectively near 0, full covariance). sklearn-only via `sklearn.
    covariance` + a small custom decision function.
  - **Stability-selected elastic-net logistic regression**: fit strongly regularized
    `LogisticRegression(penalty="elasticnet", solver="saga")` across repeated subsamples, keep
    only coordinates selected consistently across resamples, refit a plain ridge logistic on that
    stable support (cap the support size explicitly, e.g. 5-15 features). The repeated-selection
    requirement is itself a capacity gate against directions supported by only a few
    near-total-overlap folds -- directly targets the diagnosed failure mechanism (Finding 8 below).
    sklearn-only (`LogisticRegression` + a resampling loop), moderate implementation effort.
  - **Prespecified heterogeneous-head ensemble (diverse, not homogeneous)**: average predictions
    from a FIXED, pre-chosen set of structurally different weak heads (e.g. ridge logistic +
    diagonal LDA + shrunken centroid), optionally each on a fixed random or anatomically-motivated
    feature/layer subset to reduce inter-member correlation -- distinct from the already-tried
    3-way ensemble (FINAL_RESULT.md section 4), which averaged three homogeneous
    LogisticRegressionCV/LDA-family heads and underperformed the single best head. Diversity
    across head *type*, not just config, is the new ingredient. Configs/subsets/weights must be
    fixed in advance, never picked post-hoc by local score.
  - **Supervised principal components with a hard, prespecified rank budget** (Bair et al. 2006
    "supervised PCA" framing): univariate-screen coordinates for label association (in-fold only),
    run ordinary unsupervised PCA on just the retained coordinates, fit ridge logistic in a
    prespecified 1-3 dimensional space. Label-adaptivity enters only through marginal screening,
    not an arbitrary multivariate supervised projection -- meaningfully less capacity than PLS-DA
    or PCA+LDA at higher k. Every screening/PCA/fit step must be nested and refit per outer fold.
  - **Mean-regularized multi-task head** (Evgeniou & Pontil 2004), lowest priority / most
    implementation effort, only worth it if time allows: if a defensible auxiliary label exists
    (e.g. lesion laterality, or jointly fitting across Task_5 and a structurally-related auxiliary
    target), learn `w_task = w_shared + v_task` with strong shrinkage on the task-specific
    deviation `v_task`, so the model borrows statistical strength from the shared component while
    keeping task-specific flexibility deliberately small. Needs a modest custom optimizer, not a
    sklearn one-liner -- least sklearn-native option here, hence lowest priority.
  - **Note: calibration alone cannot raise AUROC** (monotone recalibration preserves ranking) --
    do not spend budget on calibration as an AUROC-improvement lever; it's out of scope for this
    round's ranking metric, though still fine as a secondary probability-quality diagnostic if a
    hypothesis wants one.

  Still-available techniques from the prior round's list that were validated as generalization-
  holding but did not raise local AUROC (kept here as a base to layer the above ON TOP OF, not to
  re-run standalone -- standalone re-runs of these alone do not meet this round's bar):
  - **CL2N** (centered L2-normalization, SimpleShot-style).
  - **Prespecified-config ensembling** (homogeneous-head version, already tried).
  - **FroFA-style feature augmentation** (Frozen Feature Augmentation, CVPR 2024).
  - **Rank-constrained / LoRA-style PCA+LDA head** at a fixed low rank.
  A hypothesis stacking one of the new needle-threading techniques above ON TOP OF one of these
  (e.g. PAM + CL2N-normalized features, or stability-selected elastic-net on FroFA-augmented
  features) is exactly the kind of combination this round is looking for.

LEVERS THAT PLAUSIBLY MOVE AUROC (all Python, all zero-build)
  - **Pooling.** Mean-pool (upstream's default) vs. CLS, CLS+mean concat, max-pool, region-
    weighted pooling (polymicrogyria is cortical -- cortex-weighted pooling is mechanistically
    motivated), or pooling only tokens above/below some coordinate.
  - **The head.** `LogisticRegressionCV(Cs=10, class_weight="balanced", scoring="roc_auc")` is
    upstream's. `Cs` grid, penalty, PCA/whitening, standardization, or a different model entirely
    are fair game -- at 48 subjects / 1024 features, regularization strength is not a detail.
  - **The input transform.** `SmriMaeTransform`'s brain mask is a mean-intensity threshold (keeps
    skull/neck, unlike pretraining's SynthSeg mask); a better mask changes both the normalization
    stats and which patches survive.
  - **Test-time augmentation.** Flips/small shifts, averaged over one extra forward each.
  - **Multi-layer features.** Only the final post-norm block is used today; intermediate blocks
    often probe better for downstream tasks than the reconstruction-tuned final layers.
  - **Confound handling.** `fomo_tune_tt/confound.py` (`ap_extent`, `FoldSafeResidualizer`) is a
    small, swappable module -- the default is fold-safe OLS residualization of features on the
    known AP-extent confound (see CONSTRAINTS below), but it exposes plain functions rather than
    a fixed policy, so a hypothesis about a different confound scalar, a different axis, or no
    correction at all is a change to this module or its call site in `run_task.cross_validate`,
    not a new pipeline.

  **2026-08-16 coordinator update: `frofa_stability_enet` (local ~0.81, external FCD 0.875) put
  through the same direct score-level confound diagnostic that refuted `hetero_ensemble_frofa` --
  it also FAILS.** Partial correlation of final OOF score vs AP-extent controlling for label:
  r=-0.536, p=0.0 (5000 within-class permutations). GBT-based nonlinear check on the score: R^2=0.039,
  p=0.03 (also significant, though weaker than hetero_ensemble_frofa's R^2=0.25). The
  residualized-FEATURE-level check stays clean (R^2=-0.148, p=0.25) -- confirms again that FroFA
  specifically is what pushes the SCORE (not the features) into confound-dependence, independent of
  head family (this affects a plain stability-selected elastic-net stacked under FroFA, not just
  RDA/PAM). **Do not promote `frofa_stability_enet` as confound-clean either; both of this round's
  local AUROC winners (>=0.80) fail the score-level diagnostic. Any FroFA-based config should be
  treated as suspect until it passes this same test** (methodology + code:
  `scratch_ppmr_validation/confound_direct_diagnostic.py`, applied to this head in
  `scratch_ppmr_validation/confound_direct_diagnostic_frofa_stability_enet.py` /
  `_result.json`).

  **2026-08-16 coordinator update, RESOLVED (supersedes the two paragraphs above for
  `hetero_ensemble_frofa` specifically):** the strict score-level partial-correlation finding
  above (r=-0.669) is not wrong, but it answers a narrower statistical-orthogonality question,
  not the practical one. Two newly-powered checks against the exact production OOF ensemble score
  (full n=48, no subjects dropped) show the AUROC does NOT ride on the AP-extent/label
  association: inverse-propensity-weighted AUROC = 0.9245 (95% CI [0.839, 0.979], up from
  unweighted 0.870), and stratified-by-AP-extent-tercile/quartile AUROC = 1.0 in every testable
  stratum. See `FINAL_RESULT.md`'s UPDATE 4 for full detail
  (`scratch_ppmr_validation/confound_ipw_stratified.py` / `_result.json`). **Do not keep
  re-deriving this with more device-init retries -- it's settled: `hetero_ensemble_frofa` is this
  round's validated headline (0.866-0.885 local, IPW 0.9245, perfect within-stratum separation,
  0.883 external FCD).** `frofa_stability_enet`'s status above is UNCHANGED by this update (it was
  not re-tested under IPW/stratification) -- treat only `hetero_ensemble_frofa` as cleared.
  Redirect further confound-lever effort to the untried items below (orthogonal-noise FroFA
  redesign, etc.) or to stacking already-confirmed pieces, not to re-verifying this.

  **New lever tried and found NOT to fix the leak by itself: capacity reduction / rank-based,
  non-covariance heads.** Coordinator tested a purely rank-based head (per-feature Mann-Whitney/
  rank-biserial selection + rank-position scoring -- no covariance matrix, no variance estimation
  anywhere, deliberately minimal free parameters) on the SAME residualized features. Local AUROC
  0.818 (k=10, seed 0) is promising, but it still FAILS the diagnostic, with an even LARGER linear
  partial correlation to AP-extent (r=-0.71, p=0.0) than hetero_ensemble_frofa had, though it does
  pass the nonlinear GBT check (p=0.27). **Mechanism: summing many individually-weak
  confound-correlated per-feature scores produces a strong aggregate LINEAR correlation via a
  central-limit-style effect** -- reducing per-feature capacity does not remove this because the
  leak here is about the aggregation step, not any one feature's flexibility. Do not assume
  "simpler head" alone closes this class of leak. Code: `scratch_ppmr_validation/rank_head_exploration.py`.

  **Levers NOT yet tried, worth a hypothesis if time allows:** (1) a genuinely fold-safe
  score-level ORTHOGONALIZATION that removes the aggregate/nonlinear component, not just the
  linear-per-fold-OLS version already tried and found insufficient (`confound_followup.py`'s
  `score_level_residualized_auroc` -- removes some AUROC but partial_r stays at -0.68); e.g. a
  rank-based partial correlation removal (regress score's WITHIN-CLASS RANK, not raw value, on
  AP-extent's within-class rank) might behave differently than OLS on raw scores, since the
  observed leak in the rank-based head is itself linear-in-aggregate. (2) Redesigning FroFA's
  augmentation to jitter ORTHOGONAL to the fold-safe-fitted confound direction (project the noise
  covariance to be zero along `b` from `FoldSafeResidualizer`, rather than isotropic Gaussian) --
  untried; isotropic noise plausibly amplifies whatever residual AP-extent signal survives
  residualization by symmetric variance inflation across all directions including the confound's.
  (3) Matched-subset AUROC (already computed as a byproduct of the diagnostic, see
  `_result.json` files) as a SEPARATE, non-p-value corroboration layer -- both frofa configs'
  matched-subset numbers are noisy/underpowered (n=11-13 pairs) but worth tracking alongside the
  main diagnostic rather than dropped.

METRICS TO REPORT
  auroc                  (maximize, RANKING METRIC) -- the challenge's own metric for task 5.
      Reported with `auroc_ci_low`/`auroc_ci_high` (2000-sample bootstrap, upstream's own
      `main_task5.score`). Do not substitute a different metric.
  seconds_per_subject     -- DIAGNOSTIC ONLY, never ranks a submission.
  Both post automatically via `fomo_tune_tt/metrics.py`; a config-only job gets this for free.

BASELINE (the number to beat, and what it is stamped against)
  **AUROC 0.995, 95% CI 0.979 - 1.000**, 20-fold over 48 subjects -- **CONFOUND-INFLATED, DO NOT
  TARGET THIS NUMBER.** Task_5's public 48-subject set has a real scanner/acquisition confound
  (physical head coverage along the AP axis correlates with label; see CONSTRAINTS below) that
  this number rides on almost entirely. Kept here only for provenance/record, not as a target.
  Source: upstream `src/fomo_tune/README.md` task-5 leaderboard, row `walnut-v0.1`, upstream's own
  git `ead1264`, NVIDIA GPU, this same `walnut-v0-1/vitl/sub-52k` checkpoint, mean-pooled tokens,
  `LogisticRegressionCV` head.
  Stamped against: `MedARC-AI/smri-fm` @ `11e53ab1d4bf29d3b44e3b59c7e6166233d414e1` (the commit
  `src/fomo_tune`/`src/smri_mae` are vendored from), checkpoint
  `medarc/walnut/checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth`, and
  `huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip`.
  **Produced by upstream, not reproduced on this platform** -- the port's own reproduction is in
  `PORT_STATUS.md`; a bf16 accelerator and a float32 GPU aren't obliged to agree to the 4th decimal.

  **AUROC 0.795, 95% CI [0.652, 0.912]** (seed=0; seed-sweep mean across seeds 0/1/2 ~0.777) --
  **the corrected, debiased number to beat.** Same 20-fold protocol, same features, same head,
  but with the fold-safe confound-regression fix (see CONSTRAINTS) applied first. This is now the
  DEFAULT behavior of `fomo_tune_tt/run_task.py`'s task-5 path (via `fomo_tune_tt/confound.py`),
  so a fresh job run against `seed/job.task5.yaml` already reports this corrected number, not the
  inflated 0.995 one.
  Source: `scratch_task5_repro/featurelevel_debias_full_protocol.json`, reproduced exactly by the
  production code path in `scratch_task5_repro/validate_production_confound.py`
  (`seed0_auroc_matches`/`mean_auroc_matches`/`leak_r2_matches` all `true`).

  In the four-line form (the 0.795 debiased row directly above is the control; the 0.995
  confound-inflated one is provenance only):
    config:    frozen `walnut-v0-1/vitl/sub-52k` encoder, mean-pooled final-layer tokens,
               fold-safe AP-extent OLS residualization (`fomo_tune_tt/confound.py`) +
               `StandardScaler` + `LogisticRegressionCV(Cs=10, class_weight="balanced",
               scoring="roc_auc")`, `KFold(n_splits=20, shuffle=True, random_state=0)` over
               sub_01..sub_48, OOF-pooled AUROC, seed=0 -- the default path of
               `fomo_tune_tt/run_task.py` as run by `seed/job.task5.yaml`
    code_ref:  `MedARC-AI/smri-fm` @ `11e53ab1d4bf29d3b44e3b59c7e6166233d414e1` (the commit
               `src/fomo_tune`/`src/smri_mae` are vendored from), checkpoint
               `medarc/walnut/checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth`
    metric:    auroc = 0.795, 95% CI [0.652, 0.912] (seed 0; seed-sweep mean over seeds 0/1/2
               ~0.777)
    measured:  not yet established as a platform experiment id -- the number comes from
               `scratch_task5_repro/featurelevel_debias_full_protocol.json`, reproduced by the
               production code path in `scratch_task5_repro/validate_production_confound.py`

  **New best-known result, `pe-1b62dccc`, 2026-08-16: AUROC 0.8663194444444445, 95% CI
  [0.751, 0.953]** (seed=0; 4-seed sweep range 0.865-0.885, mean ~0.872) -- `HEAD=hetero_ensemble_frofa`,
  FroFA-lite train-time feature augmentation stacked under a prespecified 3-member heterogeneous
  ensemble (ridge logistic regression + diagonal-heavy RDA + PAM/nearest-shrunken-centroids), on
  top of the SAME fold-safe AP-extent residualizer as the 0.795 debiased baseline above. **The
  0.795 figure directly above remains the underlying debiased number this is measured against --
  do not conflate the two; 0.795 is the confound-corrected baseline, 0.866 is this round's best
  technique scored against it.** Passes the external FCD feature-generality probe (0.8829
  [0.828, 0.932] vs FCD's own 0.887 baseline, no regression), though that external check currently
  rests on a single successful run (device-init platform flakiness blocked 9/9 attempts by the
  second agent) -- treat the external side as promising but only singly replicated. No controlled
  mechanism ablation has been done. Full writeup, comparison numbers, and caveats:
  `FINAL_RESULT.md`'s "HEADLINE (pe-1b62dccc...)" section. This is a research-stage candidate, not
  a production-ready or clinically-validated result.

USE seed/ -- everything below already works on this hardware; none of it is a spec to implement
  - `seed/job.task5.yaml` -- a real, working JobSpec. `host_mounts` bind-mounts this node's
    already-unpacked dataset and checkpoint at `/data/fomo` -- zero fetch cost. A hyperparameter
    sweep is a change to `env` here, never to code, never a rebuild.
  - `seed/run_job.sh` -- job entrypoint: correctness gate, then the task.
  - `seed/fetch_data.sh` -- one-time node setup, already run on tt-quietbox; do not re-run.
  - `seed/metrics.py` -- the metric poster the runner already uses.
  - `tenstorrent/src/fomo_tune_tt/` -- the port. `run_task.py` is the runner, `backbone_tt.py` is
    where features/pooling live (what most hypotheses edit), `parity_fomo.py` is the gate.

HOW TO RUN A HYPOTHESIS THAT EDITS CODE (pooling, head, transform -- almost all of them)
  `job.task5.yaml`'s `command` runs the fixed baseline baked into `image` and ignores `code_ref`,
  so submitting it unmodified always replays the seed. To run your edit: keep `image`,
  `host_mounts`, `env`, `cpu`/`memory`/`accelerator_*` unchanged, and replace only `command` with:

      ["bash", "-lc", "url=${HYPOTHESISLOOP_CODE_REF%@*}; sha=${HYPOTHESISLOOP_CODE_REF##*@}; \
       git clone \"$url\" /w && cd /w && git checkout \"$sha\" && \
       PYTHONPATH=/w/tenstorrent/src:/w/src:/build/smri-fm/tenstorrent/tt-metal exec bash seed/run_job.sh"]

  Add `GIT_TOKEN` to `env` so the pod can clone your branch. Still the zero-build path: nothing
  compiles, PYTHONPATH just makes your checkout shadow the image's baked-in copy. Push and set
  `code_ref` to a full 40-char SHA per the system prompt's standing rule.

WHERE THE DATA AND CHECKPOINT LIVE (node-local, pre-fetched, do not re-download)
  On tt-quietbox: `/home/ttuser/fomo-tune-data`, bind-mounted read-only at `/data/fomo`:
      /data/fomo/Task_5/preprocessed/<sub>/ses_01/t1.nii.gz     48 subjects, sub_01 .. sub_48
      /data/fomo/Task_5/labels/<sub>/ses_01/labels.txt          one integer, 0 or 1 (24 positive)
      /data/fomo/Task_3/preprocessed/<sub>/ses-01/t1w.nii.gz    494 subjects (task 3, secondary)
      /data/fomo/Task_3/labels/<sub>/ses-01/labels.txt          age in years
      /data/fomo/checkpoint/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth   3.9GB, frozen backbone
  A job never fetches or unpacks any of this; a node missing the mount fails admission rather than
  silently downloading it.

OPTIONAL EXTRA SANITY CHECKS (never the ranking metric -- the frozen KFold(20) protocol above
stays mandatory for every reported number)
  - `scratch_task5_repro/held_out_test_subjects.json` -- a SINGLE fixed stratified 37/11
    train/test split of the 48 subjects (`random_state=42`, chosen once, never resampled). Useful
    as an extra, independent sanity check that a hypothesis isn't just fitting the one frozen
    KFold(20) split's noise, but it is NOT a substitute ranking metric and must never replace the
    frozen protocol's AUROC in a reported result. Do not create any other arbitrary train/val
    split of these 48 subjects.
  - `scratch_abide_validation/` and `scratch_fcd_validation/` -- independent external datasets
    (ABIDE, an FCD cohort) with their own feature-extraction/cross-validation scripts, useful for
    checking whether a hypothesis's improvement generalizes off this specific 48-subject sample.
    Diagnostic only, same as above.

THIS ROUND'S BAR, restated plainly: a genuine win now requires clearing BOTH of the following on
the SAME technique/config -- (1) a meaningful local AUROC improvement over 0.795 (not a tie, not a
CI-noise-band bump), AND (2) no regression on the external FCD probe vs its own ~0.887 baseline.
See "SHARPENED OBJECTIVE FOR THIS ROUND" above for why neither alone counts as progress anymore.

EXTERNAL-TRANSFER CHECK IS NOW A HARD REQUIREMENT FOR ANY CLAIMED WIN (not a suggestion)
  The prior session found a local-only improvement (LDA-shrinkage head, local AUROC 0.887) that did
  NOT generalize to the external FCD cohort (0.868 there, no better than the 0.795 debiased
  baseline) -- despite this being flagged as a risk throughout that session, it was reported as a
  headline result before being checked externally. That mistake is not to be repeated.
  - **Any hypothesis that claims an improvement over the 0.795 debiased baseline MUST also report
    its AUROC on the external FCD cohort (`scratch_fcd_validation/` -- see
    `scratch_fcd_validation/cross_validate.py` for its established protocol) before being reported
    as a candidate win.** Local 20-fold AUROC alone is never sufficient evidence of a real
    improvement in this round.
  - A hypothesis that improves local AUROC but regresses or is indistinguishable from baseline on
    the external FCD check is a **null result**, not a win -- report it as such, the same honest
    way the prior session's LDA-shrinkage result was ultimately (if belatedly) written up.
  - **Reframe: a robustness/augmentation technique (e.g. CL2N, FroFA-style feature augmentation,
    prespecified ensembling) that HOLDS local AUROC roughly steady while demonstrably improving or
    maintaining the external-FCD transfer score DOES count as a genuine win.** This is a distinct
    success criterion from techniques aimed at directly raising local AUROC (e.g. PLS-DA,
    rank-constrained heads), which must clear the bar of beating 0.795 locally AND not regressing
    externally. Both kinds of win are valid outcomes for this round; a purely local AUROC gain that
    fails external transfer is not.
  - **What the FCD check actually measures -- read every "external FCD" number under this caveat.**
    The FCD check REFITS a new head on FCD data rather than applying the frozen PMG classifier to
    it, so it measures "does the frozen encoder generically detect cortical abnormality," not
    "does the PMG classifier transfer" and not "does it avoid false-positiving on FCD" -- those are
    three different questions. FCD and PMG are also structurally different cortical malformations,
    only 59% (50/85) of FCD-positive labels in this cohort are histopathology-confirmed (the rest
    are clinical/radiological suspicion), the cohort is single-site (Bonn, pediatric) with mixed
    acquisition protocols, and it uses T1w only (no FLAIR, the more diagnostic sequence for FCD).
    No independent PMG cohort exists anywhere in this project's data. Given all of this, describe
    any FCD result as a **"feature-generality probe / related-malformation transfer check,"** never
    as literal "generalizes to PMG" -- it is real, useful, secondary evidence, but weaker than a
    genuine PMG-generalization claim. See `FINAL_RESULT.md` section 7 and `fix-later.md` for the
    full writeup.
  - **Label-adaptive high-dimensional heads are a demonstrated dead end at this n -- treat any
    local win from one as unproven until it clears the external check.** Three separate techniques
    this round (LDA-shrinkage, depth-fusion+LDA, PLS-DA) each looked like a clean local win and
    each failed or regressed on the external FCD probe -- 3 for 3. The mechanism: any technique
    that uses LABELS to fit a high-dimensional projection (PLS component selection, LDA-type
    class-conditional covariance, or any other supervised projection) at p=1024 features and n=48
    samples, inside a 20-fold CV protocol where consecutive folds share near-total training-fold
    overlap, reliably locks onto sample-specific spurious label-correlated directions -- a
    stable-LOOKING but fake pooled OOF AUROC gain, mechanistically different from ordinary noise.
    The clean negative control: CL2N, prespecified ensembling, and FroFA -- none of which add
    label-adaptive capacity -- held local AUROC steady AND transferred cleanly on FCD in every case
    this round. **Guidance for future hypotheses:** prefer techniques that don't require
    label-adaptive high-dimensional fitting (normalization, prespecified ensembling, feature
    augmentation, pooling/readout changes). If a hypothesis does use a label-adaptive supervised
    projection (PLS, LDA-type, or similar), treat ANY local win from it as unproven, no matter how
    large or clean it looks, until the external FCD check confirms it holds. See `FINAL_RESULT.md`
    section 8 for the full evidence.

SHAPE CONSTANT YOU SHOULD NOT REDISCOVER
  TT-NN compiles one program per tensor shape, so the encoder is built for one fixed sequence
  length per run. Task 5's observed patch counts: min 5554 / median 8130 / **max 12959** across
  all 48 subjects, so `L_VIS=12959` (already set in `seed/job.task5.yaml`) covers every subject.
  **Changing the transform or brain mask changes this number** -- re-measure it, or a subject that
  exceeds it fails loudly (by design; never silently truncated).

CONSTRAINTS
  - **Correctness gate, owned upstream, not by you.** Every job runs `fomo_tune_tt.parity_fomo`:
    one subject through both upstream's PyTorch `MaskedEncoder` and the TT encoder, same
    checkpoint, compared by PCC. Below `PCC_GATE` (0.999) the job exits non-zero, no score.
    **Lowering or editing the gate voids the submission;** raising it is fine.
  - **The checkpoint is frozen.** Fine-tuning it or substituting a different one is out of scope.
  - **The protocol is frozen**: `KFold(n_splits=20, shuffle=True, random_state=0)` over
    `sub_01..sub_48`, out-of-fold pooled AUROC, 2000-sample bootstrap CI. Changing fold count,
    split seed, or OOF pooling makes your number incomparable to everything here, baseline included.
  - **n is tiny (48 subjects, 24 positive) and the CI is wide** (~0.02 around the baseline).
    Report the CI, never the point estimate alone.
  - **A result near the metric's ceiling is more likely overfit than won.** With this many
    subjects, searching many feature/head/seed configs against the one fixed split will eventually
    fit that specific sample's noise, even with honest per-job cross-validation -- reproducing a
    number on the *same* frozen split only proves it's deterministic, not that it generalizes.
    Concretely: treat AUROC approaching 1.0 as something to investigate, not confirm outright;
    before trusting a new best score, rerun the same config on an alternate KFold seed (diagnostic
    only, never for ranking) and report if it collapses; prefer a mechanistically-motivated,
    simple config within the pool's noise band over the historical maximum; state how many configs
    were tried on a given axis alongside any reported "best," so search breadth is visible.
  - **Ideally keep at least one job queued** so it can be picked up the moment capacity frees up,
    instead of leaving the cluster idle while you decide what to run next.

KNOWN CONFOUND IN TASK_5'S 48-SUBJECT PUBLIC SET, AND ITS FIX (read before trusting any new score)
  - **The confound.** PMG-positive scans in this dataset have systematically larger physical head
    coverage along the AP (anterior-posterior) axis than negative scans -- slice_count *
    slice_thickness on that axis, read straight off the T1w NIfTI header. This single scalar alone
    predicts the label at AUROC ~0.91-0.93 (`scratch_task5_repro/confound_check.py`,
    `confound_regression_full_protocol.py`'s `geometry_anchor`), which is why naive frozen-encoder
    mean-pooled features + `LogisticRegressionCV` score an inflated ~0.995 AUROC instead of a real
    ~0.68-0.8. Other candidate confounds were checked and ruled out or found weaker (subject-ID
    order was a near-perfect AUROC=1.0 artifact of dataset construction, not a usable signal;
    file size, voxel volume, intensity stats were all <0.68 AUROC) -- see
    `scratch_task5_repro/confound_check.json` for the full diagnostic table.
  - **FAILED fixes -- do not repeat these.** Image-level harmonization was tried two ways and both
    made the leak WORSE, not better: fixed-grid resampling harmonization left a residual-leak
    R^2=0.51 (RidgeCV OOF-predicting the original AP-extent from the "harmonized" features still
    explains half its variance -- see `scratch_task5_repro/harmonized_full_protocol_eval.py`), and
    content-crop harmonization left R^2=0.90 (`scratch_task5_repro/content_harmonized_*`), i.e.
    the confound was barely touched by either.
  - **The fix that worked, now the DEFAULT for task 5.** Feature-level fold-safe per-dimension OLS
    residualization: for each of the 20 outer CV folds, fit -- on TRAIN rows only -- a linear
    regression of each of the 1024 frozen-encoder feature dims on the scalar AP-extent confound,
    then subtract the fitted trend from BOTH train and test rows of that fold, before the usual
    `StandardScaler` + `LogisticRegressionCV` head. Recommended by codex consult over ComBat
    (inapplicable: AP-extent is one continuous covariate, not a categorical batch, at n=48 --
    see `scratch_task5_repro/confound_regression_codex_review.txt`) and over ridge/PLS deflation
    (less transparent, needs its own regularization at p=1024 >> n=48). Result: AUROC=0.795
    [0.652, 0.912] (seed sweep 0/1/2 mean 0.777), residual-leak R^2 ~ -0.045 (fully eliminated).
    Implemented in `tenstorrent/src/fomo_tune_tt/confound.py` (`ap_extent`,
    `FoldSafeResidualizer`) and wired into `run_task.cross_validate` as task 5's default -- a
    fresh job run reports this corrected number without any hypothesis needing to re-derive it.
    Task 3 (brain age) has NOT been checked for the same kind of confound and is deliberately left
    untouched; see `fix-later.md`.
  - **REQUIRED diagnostic for any new task-5 hypothesis.** Before trusting a new score (especially
    one that improves on 0.795, or that approaches the metric's ceiling), run the residual-leak
    check: 5-fold OOF `RidgeCV` predicting the ORIGINAL AP-extent from the hypothesis's
    post-transform features, with any feature transform/residualizer nested/refit per leak-check
    fold on train rows only (fold-safe end to end, exactly as
    `scratch_task5_repro/confound_regression_full_protocol.py`'s `residual_leak_check` and
    `scratch_task5_repro/validate_production_confound.py` do it). An R^2 near 0 means the confound
    is genuinely gone; an R^2 well above 0 means some of the reported AUROC is still riding the
    scanner-geometry artifact rather than real signal, no matter how good the headline number
    looks. Treat a hypothesis that skips this check as unverified.
  - **Caveat: this fix likely slightly UNDERESTIMATES true performance.** AP-extent correlates
    with real PMG signal too (larger lesions / more atypical anatomy may correlate with both
    diagnosis and how a scan was framed), so residualization removes ALL linearly AP-associated
    signal, including any genuine disease signal that happens to correlate with head coverage --
    see the codex review's caveat in `confound_regression_codex_review.txt`. 0.795 is a
    defensible, conservative floor, not necessarily the model's true ceiling.
