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

METRICS TO REPORT
  auroc                  (maximize, RANKING METRIC) -- the challenge's own metric for task 5.
      Reported with `auroc_ci_low`/`auroc_ci_high` (2000-sample bootstrap, upstream's own
      `main_task5.score`). Do not substitute a different metric.
  seconds_per_subject     -- DIAGNOSTIC ONLY, never ranks a submission.
  Both post automatically via `fomo_tune_tt/metrics.py`; a config-only job gets this for free.

BASELINE (the number to beat, and what it is stamped against)
  **AUROC 0.995, 95% CI 0.979 - 1.000**, 20-fold over 48 subjects.
  Source: upstream `src/fomo_tune/README.md` task-5 leaderboard, row `walnut-v0.1`, upstream's own
  git `ead1264`, NVIDIA GPU, this same `walnut-v0-1/vitl/sub-52k` checkpoint, mean-pooled tokens,
  `LogisticRegressionCV` head.
  Stamped against: `MedARC-AI/smri-fm` @ `11e53ab1d4bf29d3b44e3b59c7e6166233d414e1` (the commit
  `src/fomo_tune`/`src/smri_mae` are vendored from), checkpoint
  `medarc/walnut/checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth`, and
  `huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip`.
  **Produced by upstream, not reproduced on this platform** -- the port's own reproduction is in
  `PORT_STATUS.md`; a bf16 accelerator and a float32 GPU aren't obliged to agree to the 4th decimal.

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
