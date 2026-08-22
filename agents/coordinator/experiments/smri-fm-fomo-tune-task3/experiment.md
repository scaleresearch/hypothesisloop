## EXPERIMENT DESCRIPTION

## STAGE 3 (2026-08-19, extended another 24h -- READ THIS FIRST, round now ends 2026-08-20T20:40Z)

STAGE 2 produced three successive validated promotions before the round went quiet: current
headline is **concat(STOP_BLOCK=17, STOP_BLOCK=23) mean-pool + Cole-style fold-safe CUBIC
age-bias-corrected RidgeCV, ORDER_RESIDUALIZE=1 (linear)** -- pearson_r=0.7609 local/0.3355
external (ABIDE), mae=9.44 local/3.79 external. That's a ~50% external MAE reduction from the
original Stage 1 number (7.63 -> 3.79). Full lineage, every rejected candidate with its mechanism,
and the draft close-out summary are all in `FINAL_RESULT.md` -- READ IT before starting.

BASELINE (the current headline, i.e. the number Stage 3 has to beat)
  config:    concat(STOP_BLOCK=17, STOP_BLOCK=23) mean-pool features + Cole-style fold-safe CUBIC
             age-bias-corrected `RidgeCV`, `ORDER_RESIDUALIZE=1` (linear order-index
             residualization), `KFold(n_splits=20, shuffle=True, random_state=0)` over the 494
             Task_3 subjects, OOF-pooled, scored by `run_task.py`'s `score_task3`
  code_ref:  the shared code repo at `$CODE_REPO_URL` -- the 40-char SHA of the commit that
             produced this headline is not recorded in this file (see `FINAL_RESULT.md` for
             lineage); the accelerator pin is
             tt-metal @ `d9a68815f5fcf08a5bfbffb6f1f811823fba8edd` (`TT_METAL_REF`, shared with
             Task 5 and not to be bumped independently)
  metric:    pearson_r = 0.7609 local (mae 9.44 local); external ABIDE age-matched subset
             pearson_r = 0.3355, mae 3.79
  measured:  not yet established -- no experiment id is recorded here for this headline; it is
             attributed to Stage 2's three successive validated promotions, written up in
             `FINAL_RESULT.md`

**STAGE 3 OBJECTIVE: the round has been extended 24h (new deadline 2026-08-20T20:40Z) specifically
to see if there's more headroom.** No prescribed technique -- same freedom as Stage 2. One
observation worth knowing before you start: local pearson_r gains have been flattening across the
last two promotions (<1% each) while external MAE gains have stayed large (19-23% each step) --
that pattern suggests genuine, generalizable signal may still be findable, not that the pipeline is
tapped out. Same promotion bar as Stage 2: reproduced, checked against the order-index confound,
externally validated on ABIDE before replacing the current headline.

Known not-yet-tried directions from Stage 2's own notes (not a checklist, just what's on record as
untested): other depth-pair combinations beyond concat(17,23); pooling architectures beyond
mean-pool/CLS-token (e.g. attention-weighted pooling); head hyperparameters retuned specifically
for the concatenated 2048-d feature space. IXI (a wider, better age-matched external cohort than
ABIDE) was tried and is genuinely inaccessible (403 from every network path tested) -- don't
re-attempt it, that's a closed dead end, not an untried lever.

## STAGE 2 (2026-08-18, superseded above, kept for reference)

STAGE 1 is done: Task_3's primary result is **pearson_r=0.7319-0.7324, mae=10.42-10.43**
(STOP_BLOCK=23, mean-pool + RidgeCV, fold-safe linear order-index residualization), reproduced
across independent runs and a 5-seed sweep, checked against the order-index confound (which is
real -- order-index alone predicts age at r=0.611 -- and is what the residualizer corrects for),
and externally validated on an independent ABIDE cohort (age-matched subset, n=67: pearson_r=0.364,
real signal on data with no order-index structure at all, no Task_3 subject overlap). This beats
the external FOMO26 leaderboard baseline (CORR 0.426, MAE 12.28) by a wide margin. Full writeup:
`FINAL_RESULT.md` in this directory -- READ IT for complete methodology, the confound investigation,
and what's already been tried and either confirmed or refuted.

**STAGE 2 OBJECTIVE: push pearson_r/mae further, over up to 24h, using whatever method you judge
best.** No prescribed technique -- pooling changes, different heads, ensembling, augmentation,
alternate confound-correction strategies, anything within the CONSTRAINTS below is fair game.

**The bar for a new result to replace the current headline is the same rigor already established,
not a lower one:**
1. **Reproduced** -- same config, different seed/refit, confirm the number holds (not a single
   lucky run).
2. **Checked against the order-index confound directly** -- not just "beats local CV." The
   confound is real and quantified (r=0.611 alone); any candidate must show it isn't riding that
   leak, the same way the current headline was checked (see `FINAL_RESULT.md` section 1-2 for the
   method, and section 2's honest treatment of a correction that turned out to be overcorrection/
   overfitting rather than assume the most aggressive correction is automatically the most honest
   one).
3. **Externally validated** -- ABIDE reuse (`../smri-fm-fomo-tune/scratch_abide_validation/`,
   zero-download, real ages, but pediatric-skewed -- restrict to the age-matched subset the way
   `FINAL_RESULT.md` section 5 does) or a different external cohort if you find a better-matched
   one. Do not promote a new headline without this.

**Known open leads from STAGE 1, worth reading before starting from scratch (not a checklist to
complete in order -- pick what you think is most promising):**
- SVR-RBF on the same order-residualized features reproduced at pearson_r=0.8273/mae=8.47 across
  5 seeds -- better than the RidgeCV headline, but NOT promoted: a nonlinear head paired with a
  linear-only confound correction could be exploiting a real nonlinear order-index-vs-age
  relationship (nonlinear order-anchor r=0.735 vs the linear correction's target of r=0.611) that
  the linear residualizer doesn't remove. The obvious fix (bin the order-index into a nonlinear
  correction) was tried and shown to overfit at n=494 rather than cleanly resolve the question --
  see `FINAL_RESULT.md` section 2's table. A properly regularized nonlinear correction (not naive
  binning) is an open, untried idea, not a dead end.
- No ensemble/augmentation approach (like Task_5's winning `hetero_ensemble_frofa`) has been tried
  on Task_3 at all.
- STOP_BLOCK depths/pooling variants other than 23 were only tested against the confound-INFLATED
  baseline in a prior round -- never individually re-verified debiased.
- IXI (~600 adult subjects, wider/better-matched age range than ABIDE, not yet fetched) would give
  a tighter external validation read than ABIDE's age-mismatched cohort, if pursued.

CONSTRAINTS carry over unchanged from STAGE 1 (frozen encoder, no fine-tuning, shared checkpoint
across all 5 tasks, correctness gate not to be loosened) -- see below.

---

## STAGE 1 (2026-08-17, for reference/history)

OBJECTIVE (2026-08-17 round: TASK 3, not Task 5 -- Task 5 is a separate, closed round)
Get the best possible **age-prediction accuracy on FOMO26 Task 3 (brain age from T1w MRI)** out of
the same frozen sMRI foundation-model encoder used for Task 5, scored exactly the way the challenge
scores it: 20-fold cross-validation (`KFold(shuffle=True, random_state=0)`) over the 494 Task_3
subjects, out-of-fold age predictions pooled, then **Pearson correlation (r) between predicted and
true age is the RANKING METRIC**, with **MAE reported alongside it** (both from `run_task.py`'s
`score_task3`, which calls upstream's own `fomo_tune.main_task3.score` -- the scoring code is not
yours to touch, same rule as Task 5). Pearson r ranks rather than MAE because it's scale-free and
directly tracks whether the encoder's representation actually orders subjects by age -- a technique
that shrinks MAE by predicting close to the sample mean for every subject (regression-to-the-mean)
would look good on MAE alone while adding no real age signal; r is harder to game that way. Both
still gate any claimed win: report both, and treat a win on one without the other as a partial,
not a full, result.

**A winning config must reproduce, not just post a lucky number once.** Rerun any candidate that
beats the baseline (same config, different seed/refit where applicable) and confirm the improvement
holds before reporting it as a result -- one good run at n=494/20-fold is far less likely to be pure
noise than Task 5's n=48 was, but the same discipline applies: a single best-of-N number is not
evidence on its own.

Pin note: this task shares Task 5's `TT_METAL_REF=d9a68815f5fcf08a5bfbffb6f1f811823fba8edd` pin
(see `seed/job.yaml`, `Dockerfile.experimentator`) -- must stay identical across both if either
is ever bumped, or a job silently runs against a different tt-metal build than the agent read.

TARGET TO BEAT: the public FOMO26 Task 3 leaderboard baseline, **CORR 0.426, MAE 12.28** (years).
This is an EXTERNAL reference point, not yet measured through THIS project's own pipeline -- no
prior round has run Task 3 through this encoder/protocol before. **The first job of this round is
establishing our own baseline number** (mean-pooled final-layer features + `RidgeCV`, exactly
`run_task.py`'s existing `head_for("task3")` -- already implemented, zero code changes needed to
get a first number). Do not treat CORR 0.426/MAE 12.28 as validated against our own setup until
that first run reports back; treat it as the goalpost, not a given.

**Task 3 has NOT been checked for the AP-extent (or any other) scan-geometry confound the way
Task 5 was.** Task 5's ~0.995-AUROC bug (see section 1 below and `fomo_tune_tt/confound.py`) was a
real, quantified confound that inflated an apparently-strong result until it was found and fixed.
Task 3's `cross_validate()` call in `run_task.py` deliberately passes `confound=None` for task3 --
this was never verified as safe, only left unexamined. Any candidate that meaningfully beats the
CORR/MAE baseline **must be checked for an analogous shortcut** (e.g.: does a simple scan-geometry
or acquisition-derived proxy alone predict age at a suspiciously high correlation on this 494-subject
set? does the model's residual/error correlate with such a proxy after controlling for true age,
the same partial-correlation-style diagnostic used for Task 5 in `scratch_ppmr_validation/
confound_direct_diagnostic.py`?) before being reported as a real win. This is not optional -- it is
the single most important lesson this project already learned the hard way once.

This is still a **linear-probe / representation-use problem, not a training problem** -- same
frozen 3D ViT-L MAE backbone, same "don't fine-tune it" rule, same shared-checkpoint-across-5-tasks
caution (a hypothesis changes readout/pooling/head, never encoder weights or forward-pass numerics).


CONSTRAINTS (same as Task 5, carried over)
- Frozen encoder, no fine-tuning, no new checkpoints.
- Shared checkpoint across all 5 FOMO26 tasks -- never touch encoder weights or forward-pass
  numerics; only readout/pooling/head choices are in scope. `encoder_tt.py`/`backbone_tt.py`
  already expose every readout point a method-level hypothesis needs (`forward`, `forward_with_cls`,
  `forward_until`, `forward_multi`); reuse them rather than adding new ones.
- The accelerator (Tenstorrent Blackhole, TT-NN port) is a solved black box -- configure via env
  vars, never modify.
- Any claimed improvement over the CORR/MAE baseline must survive a confound check (see above)
  before being reported as a win, exactly the same discipline that caught Task 5's original bug.
- Correctness gate (`run_job.sh`'s PCC parity check against the checkpoint) is owned by upstream;
  do not loosen it to make a run pass.

WHERE THE CODE LIVES (`$CODE_REPO_URL`, same shared repo Task 5 used)
  - `seed/job.yaml` -- the Task 3 job spec (already correctly configured: `TASK=task3`,
    `OUTPUT_DIR=/tmp/fomo-tune-out/task3`). `L_VIS` is deliberately unset for Task 3 (not yet
    measured for its 494 subjects) -- the runner measures it automatically on first use (a few
    minutes, host-side). `seed/job.task5.yaml` is Task 5's twin; not this round's target.
  - `seed/run_job.sh` -- the correctness gate + runner entrypoint, shared across tasks.
  - `seed/metrics.py` -- the metric poster.
  - `tenstorrent/src/fomo_tune_tt/run_task.py` -- the runner. `head_for("task3")` returns the
    baseline `RidgeCV` head; `cross_validate()` is where a hypothesis's fold loop lives;
    `main()` is where task3 vs task5 branches (features, confound, scoring) diverge.
  - `tenstorrent/src/fomo_tune_tt/backbone_tt.py` -- pooling/features, what most hypotheses edit.
  - `tenstorrent/src/fomo_tune_tt/confound.py` -- Task 5's `FoldSafeResidualizer` and `ap_extent()`
    proxy; useful as a template if a Task 3 confound is found and needs the same fix pattern.
  - `tenstorrent/src/fomo_tune_tt/parity_fomo.py` -- the correctness gate.

HOW TO RUN A HYPOTHESIS THAT EDITS CODE (pooling, head, transform -- almost all of them)
  `job.yaml`'s `command` runs the fixed baseline baked into `image` and ignores `code_ref`, so
  submitting it unmodified always replays the seed. To run your edit: keep `image`, `host_mounts`,
  `env`, `cpu`/`memory`/`accelerator_*` unchanged, and replace only `command` with:

      ["bash", "-lc", "url=${HYPOTHESISLOOP_CODE_REF%@*}; sha=${HYPOTHESISLOOP_CODE_REF##*@}; \
       git clone \"$url\" /w && cd /w && git checkout \"$sha\" && \
       PYTHONPATH=/w/tenstorrent/src:/w/src:/build/smri-fm/tenstorrent/tt-metal exec bash seed/run_job.sh"]

  Add `GIT_TOKEN` to `env` so the pod can clone your branch. Zero-build path: nothing compiles,
  PYTHONPATH just makes your checkout shadow the image's baked-in copy. Push and set `code_ref` to
  a full 40-char SHA per the system prompt's standing rule.

WHERE THE DATA AND CHECKPOINT LIVE (node-local, pre-fetched, do not re-download)
  On tt-quietbox: `/home/ttuser/fomo-tune-data`, bind-mounted read-only at `/data/fomo`:
      /data/fomo/Task_3/preprocessed/<sub>/ses-01/t1w.nii.gz    494 subjects (this round's target)
      /data/fomo/Task_3/labels/<sub>/ses-01/labels.txt          age in years
      /data/fomo/Task_5/preprocessed/<sub>/ses_01/t1.nii.gz     48 subjects (other task, not this round)
      /data/fomo/checkpoint/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth   3.9GB, frozen backbone
  A job never fetches or unpacks any of this; a node missing the mount fails admission rather than
  silently downloading it.

WHAT WAS LEARNED ON TASK 5, WORTH CARRYING OVER (full detail: `experiment-task5-archive.md`)
  - A shortcut/confound can inflate a metric dramatically before anyone notices -- Task 5's original
    result (~0.995 AUROC) was almost entirely a scan-geometry artifact, not real signal. Assume
    Task 3 could have an analogous issue (e.g. scanner/site, slice count, resolution correlating
    with age) until checked, not until one shows up by accident.
  - A local-CV win is not sufficient evidence on its own -- Task 5 saw multiple candidates that
    improved local AUROC while carrying a hidden confound in their final score, or that failed an
    external-cohort check despite a strong local number. If/when an external Task 3-comparable
    cohort or held-out split becomes available, treat it as a required check for any claimed win,
    the same way Task 5's external FCD cohort became mandatory mid-round.
  - Report null/refuted results plainly and keep the reasoning for why something failed -- don't
    just drop a promising-looking lead once the confound check fails it; write down the mechanism.

## Infra note (2026-08-18): use all 4 chips, not 1 at a time
This node has 4 accelerator chips and is explicitly sized for 4 concurrent jobs (see seed/job.yaml
comment: 4 * 3800m cpu, 4 * 48Gi memory fits with headroom). There are only 2 agents in this round
-- each agent should keep roughly 2 hypotheses in flight concurrently (submit the next job before
the current one finishes) rather than running one job, waiting for it to complete, then submitting
the next. Idle chips while there's a real hypothesis backlog (and there is one: the order-index
leak root-cause investigation, the order-residualization check, and every method-level candidate
still queued) is wasted budget.
