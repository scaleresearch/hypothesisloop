## EXPERIMENT DESCRIPTION

OBJECTIVE
Get the best possible **AUROC on FOMO26 Task 5 (polymicrogyria detection from T1w MRI)** out of a
frozen sMRI foundation-model encoder, scored exactly the way the challenge scores it: 20-fold
cross-validation over 48 subjects, out-of-fold probabilities pooled, AUROC over the pool.

The setup is a **linear-probe / representation-use problem, not a training problem.** The
backbone is a 3D ViT-L masked autoencoder pretrained on 208x240x208 1mm brain volumes; the
challenge freezes it ("the pretrained model is considered frozen, no new pretrain checkpoints
will be accepted"), so you cannot fine-tune it and there is no point trying. What you *can*
change is everything around it: how a volume is turned into an input, which of the encoder's
outputs you keep, how you pool them into a feature vector, and what classifier head you fit on
those features. That is where the entire signal is.

**This backbone is one shared, frozen checkpoint used across all 5 FOMO26 tasks** (this
experiment covers task 5 only; tasks 1/2/3/6/7 are separate future experiments reusing the exact
same encoder). Because of that sharing, the boundary of what you touch matters beyond this one
run: your hypothesis is the *task method* -- which encoder output you read, how you pool/fuse it,
what classifier head you fit -- analogous to upstream's per-task `Task5Method` class, never the
encoder's weights or its forward-pass numerics. `tenstorrent/src/fomo_tune_tt/encoder_tt.py` and
`backbone_tt.py` already expose everything a method-level hypothesis needs (`forward`,
`forward_with_cls`, `forward_until`, `forward_multi` -- different readout points from one frozen
forward pass, not different weights); reuse those rather than adding new ones. If a hypothesis
seems to require changing what the encoder itself computes (not just which of its existing
outputs you read), it is out of scope here -- flag it as a comment, do not implement it, since it
would silently change the shared backbone every other task's future experiment inherits.

The accelerator underneath (a Tenstorrent Blackhole card running a hand-written TT-NN port of the
encoder) is a **solved black box.** It is already built, already validated against the PyTorch
reference, and already wired into the runner. You configure it through env vars; you never need
to read, build, or modify it, and no hypothesis about it will move the metric.

LEVERS THAT PLAUSIBLY MOVE AUROC (all Python, all zero-build)
  - **Pooling.** The runner currently mean-pools the surviving patch tokens into one 1024-d
    vector, which is what upstream does. The encoder also produces a CLS embedding, per-token
    embeddings, and each token's world-space coordinate. CLS alone, CLS concatenated with the
    mean, max-pooling, region-weighted pooling (polymicrogyria is a cortical malformation --
    cortex-weighted pooling is a real, mechanistically motivated idea), or pooling only tokens
    above/below some coordinate are all one-function changes.
  - **The head.** `LogisticRegressionCV(Cs=10, class_weight="balanced", scoring="roc_auc")` is
    upstream's. The `Cs` grid, penalty (l1/l2/elasticnet), whether to PCA/whiten first, whether
    to standardize, and swapping in a different linear model are all fair game. With 48 subjects
    and 1024 features, regularization strength is not a detail.
  - **The input transform.** `SmriMaeTransform` canonicalizes to RAS, rescales to 1mm, centre-fits
    to 208x240x208, and z-scores inside a **mean-intensity threshold mask** -- which, unlike the
    SynthSeg mask used in pretraining, keeps skull and neck. A better brain mask is a legitimate,
    untested direction: it changes both the normalization statistics and which patches survive.
  - **Test-time augmentation.** Left-right flips, small shifts. Averaging embeddings over
    augmentations costs one extra forward per augmentation (~3s each) and nothing else.
  - **Multi-layer features.** Only the final post-norm block output is used today. Intermediate
    blocks often probe better.

METRICS TO REPORT
  auroc                  (maximize, RANKING METRIC) -- the challenge's own metric for this task,
      and the only one it scores task 5 on. Reported by the runner as `auroc`, together with
      `auroc_ci_low`/`auroc_ci_high` (2000-sample percentile bootstrap over subjects, upstream's
      `main_task5.score`, unmodified). It earns its place by being literally the objective: task 5
      is a binary detection task with 24 positives and 24 negatives, ranking quality is what the
      challenge rewards, and accuracy would be both threshold-dependent and less sensitive at
      n=48. Do not substitute a different metric.
  seconds_per_subject     -- DIAGNOSTIC ONLY, never ranks a submission. Posted continuously during
      the embedding pass (once per subject) so a stuck job is visible within seconds rather than
      at the end. A change that improves AUROC at the cost of more seconds/subject is still a win.
  Reporting cadence is already wired: `fomo_tune_tt/run_task.py` posts `seconds_per_subject` per
  subject and per CV fold, and the final `auroc` at the end, via `fomo_tune_tt/metrics.py`'s
  `post_metric()`. A config-only job gets all of this for free; you do not add reporting.

BASELINE (the number to beat, and what it is stamped against)
  **AUROC 0.995, 95% CI 0.979 - 1.000**, 20-fold over 48 subjects.
  Source: upstream `src/fomo_tune/README.md`'s task-5 leaderboard, row `walnut-v0.1`, produced by
  upstream at its own git `ead1264` on an NVIDIA GPU, using this same
  `walnut-v0-1/vitl/sub-52k` checkpoint with mean-pooled tokens and a `LogisticRegressionCV`
  head. Its predecessor row (`baseline`, a different pretrain checkpoint) scored 0.984.
  Stamped against: upstream repo `MedARC-AI/smri-fm` @ `11e53ab1d4bf29d3b44e3b59c7e6166233d414e1`
  (the commit this experiment's `src/fomo_tune` and `src/smri_mae` are vendored from), checkpoint
  `medarc/walnut` `checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth`, and the `Task_5.zip`
  snapshot at `huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip`.
  **This baseline was produced by upstream, not reproduced on this platform.** The port's own
  measured reproduction of it is recorded in `PORT_STATUS.md`; compare against that number as
  well, since a bf16 accelerator and a float32 GPU are not obliged to agree to the fourth decimal.

USE seed/ -- everything below already works on this hardware; none of it is a spec to implement
  - `seed/job.task5.yaml` -- a real, working JobSpec. `host_mounts` bind-mounts this node's
    already-unpacked dataset and checkpoint at `/data/fomo`, so a job pays **zero** fetch cost.
    A hyperparameter sweep is a change to `env` in this file, never to code and never a rebuild.
  - `seed/run_job.sh` -- the job entrypoint. Runs the correctness gate, then the task.
  - `seed/fetch_data.sh` -- one-time node setup. Already run on tt-quietbox; you do not run it.
  - `seed/metrics.py` -- the metric poster the runner already uses.
  - `tenstorrent/src/fomo_tune_tt/` -- the port itself. `run_task.py` is the runner,
    `backbone_tt.py` is where features/pooling live (the thing most hypotheses edit),
    `parity_fomo.py` is the correctness gate.

HOW TO RUN A HYPOTHESIS THAT EDITS CODE (pooling, head, transform -- almost all of them)
  Nearly every real lever above is a Python edit, not an env var: `job.task5.yaml`'s `command`
  runs the fixed baseline baked into `image` and ignores `code_ref` entirely, so submitting it
  unmodified always replays the seed regardless of what you changed. To actually run your edit:
  keep `image`, `host_mounts`, `env`, `cpu`/`memory`/`accelerator_*` from `seed/job.task5.yaml`
  exactly as they are (they already have the built tt-metal/ttnn env and the data/checkpoint
  mount you need), and replace only `command` with:

      ["bash", "-lc", "url=${HYPOTHESISLOOP_CODE_REF%@*}; sha=${HYPOTHESISLOOP_CODE_REF##*@}; \
       git clone \"$url\" /w && cd /w && git checkout \"$sha\" && \
       PYTHONPATH=/w/tenstorrent/src:/w/src:/build/smri-fm/tenstorrent/tt-metal exec bash seed/run_job.sh"]

  Add `GIT_TOKEN` to `env` (same token you push with) so the pod can clone your branch -- the
  base image has `git` but no baked credential. This is still the zero-build/cheap path: nothing
  here compiles, the clone is Python source only, PYTHONPATH just makes your checkout shadow the
  image's baked-in copy of `fomo_tune_tt`/`smri_mae`/`smri_mae_tt`. Push and set `code_ref` per
  the system prompt's standing rule (full 40-char SHA, commit right before each submit).

WHERE THE DATA AND CHECKPOINT LIVE (node-local, pre-fetched, do not re-download)
  On tt-quietbox: `/home/ttuser/fomo-tune-data`, bind-mounted read-only into every job at
  `/data/fomo`. Layout:
      /data/fomo/Task_5/preprocessed/<sub>/ses_01/t1.nii.gz     48 subjects, sub_01 .. sub_48
      /data/fomo/Task_5/labels/<sub>/ses_01/labels.txt          one integer, 0 or 1 (24 positive)
      /data/fomo/Task_3/preprocessed/<sub>/ses-01/t1w.nii.gz    494 subjects (task 3, secondary)
      /data/fomo/Task_3/labels/<sub>/ses-01/labels.txt          age in years
      /data/fomo/checkpoint/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth   3.9GB, the frozen backbone
  A job never fetches or unpacks any of this. If a job lands on a node without it, admission
  fails on the missing mount rather than the job silently downloading 5GB.

SHAPE CONSTANT YOU SHOULD NOT REDISCOVER
  TT-NN compiles one program per tensor shape, so the encoder is built for one fixed sequence
  length for the whole run. Task 5's observed-patch counts under the current transform are
  min 5554 / median 8130 / **max 12959** across all 48 subjects, so `L_VIS=12959`
  (`encoder_seq_len` 12960, tile-aligned) covers every subject. This is already set in
  `seed/job.task5.yaml`. **If you change the transform or the brain mask, this number changes** --
  re-measure it, or a subject that exceeds it fails the run loudly (by design; it is never
  silently truncated, which would drop real tissue from the embedding).

CONSTRAINTS
  - **Correctness gate, owned upstream, not by you.** Every job runs
    `fomo_tune_tt.parity_fomo` before the task: one real subject through both the upstream PyTorch
    `MaskedEncoder` (upstream's own code, from the vendored `src/`) and the TT encoder, same
    checkpoint, compared by PCC. Below `PCC_GATE` (0.999) the job exits non-zero and never
    produces a score. **Lowering `PCC_GATE`, or editing the gate, voids the submission.** Raising
    it is fine.
  - **The backbone checkpoint is frozen** by the challenge. Fine-tuning it, or substituting a
    different pretrained checkpoint, is out of scope and not a valid submission.
  - **The protocol is frozen**: 20-fold `KFold(n_splits=20, shuffle=True, random_state=0)` over the
    48 subjects in `sub_01..sub_48` order, out-of-fold probabilities pooled, AUROC over the pool,
    2000-sample bootstrap CI. Changing the fold count, the split seed, or the pooling of
    out-of-fold predictions makes your number incomparable to every other number here, including
    the baseline.
  - **n is tiny and the CI is wide.** 48 subjects, 24 positive; the baseline's CI is ~0.02 wide
    against a ceiling of 1.000. Most deltas you chase are inside it. A win must reproduce: rerun
    your best config before trusting it, and report the CI, never the point estimate alone.
  - **The metric is bounded above and the baseline is near the ceiling.** 0.995 out of 1.000
    means there are two misranked pairs' worth of headroom in total. Prefer directions with a
    mechanistic reason to help (better mask, cortex-weighted pooling, stronger regularization at
    n=48) over blind hyperparameter scans, and treat a clean, properly-reported null as a real
    result rather than something to hide.
  - **The fleet as a whole overfits the fixed split even when every individual job's CV is
    honest -- run 1 hit this for real.** Run 1 tried ~150 configs against the identical frozen
    48-subject/20-fold split, converged on AUROC 1.0 (RBF-SVM head, block-17+final fused feature),
    and it did not hold: a hidden validation set scored ~0.6, near chance. Rerunning the *same*
    config on the *same* frozen split only proves the number is deterministic, not that it
    generalizes -- with 48 fixed subjects and enough configs tried, some will fit that specific
    sample's noise by chance no matter how correctly each one's own 20-fold CV is done (this is
    leaderboard overfitting: searching many submissions against one small public scoreboard).
    Two changes that follow from that, for this run:
      - **Treat AUROC approaching 1.0 as a red flag, not an achievement.** A genuinely
        generalizable classifier essentially never reaches the literal ceiling on a hard
        structural-anomaly task from a hand-built feature; landing there is what overfitting a
        48-subject sample with a high-capacity non-linear head (RBF-SVM especially) looks like.
        Do not confirm a hypothesis on this alone -- investigate why it's that high before
        trusting it, the way run 1 eventually did for its seed-instability finding.
      - **Before confirming a hypothesis that raises the pool's best score, rerun the same feature
        and head with an alternate KFold seed** (e.g. `random_state=1`) as a *diagnostic-only*
        check -- never for ranking or comparison to the baseline, since that would need protocol
        changes above. If the improvement collapses under a different fold arrangement, that is
        real evidence of overfitting to this particular split's composition, not proof the
        hypothesis is wrong; report it as such rather than silently dropping the diagnostic run.
      - **Favor the simplest config within the confirmed pool's noise band over the pool's
        historical maximum.** A linear head with a well-motivated feature (e.g. an intermediate
        block replacing the final layer) that lands at 0.994-0.997 is more likely to generalize
        than an RBF-SVM ensemble that reaches 0.998-1.0 by searching many feature/head/seed
        combinations against the same 48 subjects. When reporting a "best" result, report the
        search breadth that produced it (how many configs were tried on this axis) alongside the
        number, so a reader can judge how much of the gain is signal versus search.
