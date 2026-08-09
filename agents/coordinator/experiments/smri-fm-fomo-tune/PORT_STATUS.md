# smri-fm-fomo-tune: port status and checklist review

Companion to `experiment.md` (which is what agents read). This file is for whoever maintains the
experiment definition: what was built, what was measured, and where each checklist item stands.

## 1. What this is

`src/fomo_tune` (upstream MedARC-AI/smri-fm) evaluates a frozen sMRI-MAE encoder on the FOMO26
downstream tasks. This experiment runs **task 5 (polymicrogyria, AUROC over 48 subjects)** on
Tenstorrent Blackhole, reusing the existing `smri-fm` experiment's TT-NN encoder port. Task 3
(brain age) shares every line of the runner and is wired but not validated end to end -- see
"Task 3" below.

## 2. The one thing that had to be built, not reused

`smri_mae_tt.modules_tt.Encoder` (the pretraining port) has **no concept of padding**. Pretraining
samples exactly `l_vis` visible patches per sample, so every slot is a real token and its
attention runs `AttentionMaskType::None`.

fomo_tune's patch set is not sampled, it is *content-driven*: `MaskedEncoder.forward(x, mask=...)`
zeroes voxels outside the brain mask, patchifies the mask, and drops every patch with zero
observed voxels. How many survive is a property of the subject (measured: 5554 to 12959 across
task 5's 48 subjects). TT-NN compiles one program per shape, so the sequence must be one fixed
length for the whole run, which means real trailing padding that must be kept out of every
softmax.

`fomo_tune_tt/encoder_tt.py` is that, and only that: `MaskedAttention` subclasses the existing
`Attention` and passes a `(1, 1, S, S)` bf16 keep mask (1.0 = attend, 0.0 = -1e9 bias) to the same
fused SDPA kernel via `AttentionMaskType::Arbitrary`. Only key columns are masked, never query
rows, so a padded row still has unmasked keys and no softmax denominator degenerates. Everything
else -- parameter modules, sincos table, fused QKV, MLP, tile rules -- is the pretraining port's,
unchanged.

`fomo_tune_tt/checkpoint.py` builds `EncoderParams` directly from the checkpoint rather than
reusing the pretraining port's `params.py`, which writes a state dict into an already-constructed
Encoder/Decoder pair. There is no decoder here, and constructing 300M Xavier-sampled parameters
purely to overwrite them is waste; the checkpoint is the source, not a later overwrite. So
`params.py` is not vendored here, and neither is `ops_tt/flash_attention.py` (the pretraining
port's alternative attention, which nothing on this path calls) -- vendoring code no import
reaches just makes the tree look bigger than it is.

## 3. Measured results (all on tt-quietbox, real Blackhole, real checkpoint)

Checkpoint: `medarc/walnut` `checkpoints/walnut-v0-1/vitl/sub-52k/checkpoint-last.pth` (3.92GB),
`mae_vit_large`, img 208x240x208, patch 8, D=1024, depth 24, 16 heads, `mask_drop_scale=False`.

| What | Value |
|---|---|
| Pooled-embedding PCC vs upstream float32 encoder | **0.999773** (gate 0.999) |
| Per-token `patch_embeds` PCC | **0.998859** (gate 0.998, see below) |
| Task 5 AUROC, 48 subjects, 20-fold | **0.99306**, CI 0.9722 - 1.0000 |
| Upstream's published task 5 AUROC (same checkpoint, GPU) | 0.995, CI 0.979 - 1.000 |
| Encoder forward, seq_len 12960 | 2.6 - 3.2 s/subject |
| Whole task 5 job (gate + 48 embeddings + CV) | ~4 min |

Three separate jobs were submitted through `POST /experiments` and ran on tt-quietbox
(`exp-fomo-task5-2865be9d`, `-a96b6841`, `-8a726930`); all three COMPLETED and posted
`auroc=0.9930555555555556`, byte-identical, which is the reproduction check item 12 asks for.

The 0.99306 sits inside upstream's own CI and upstream's 0.995 sits inside ours. With 48 subjects
one swapped pair moves AUROC by ~0.002, so this is a reproduction, not a regression.

**Why `patch_embeds` is gated at 0.998 rather than the pretraining port's 0.999.** This was
measured before the threshold was chosen, not fitted to it. Per-block relative Frobenius error vs
the float32 reference: 0.0146 (block 0), 0.0197 (block 4), 0.0268 (block 12), 0.0357 (block 23) --
smooth, monotonic, no jump at any layer, which is accumulated bf16 rounding and not what a wrong
kernel or a broken mask looks like. The final LayerNorm takes block 23's 0.999433 down to
0.998859 by stripping the residual stream's large common component. Pooling averages the noise
back down, which is why the tensor that actually feeds the score is the healthier one. For
calibration, a torch bf16-*autocast* reference on the same subject sits at 0.999988 -- better than
this port because autocast keeps LayerNorm and accumulators in float32 while this port is bf16
throughout, per the pretraining port's own numerics policy.

## 4. Known deviations from upstream, and why

- **`fomo_tune_tt/data.py` reads the unpacked archives directly** instead of upstream's
  `datasets.py`, whose `Nifti()` feature type needs a much newer `datasets` than the tt-metal
  python env ships. Subject ordering, member paths and label parsing are copied verbatim, so the
  KFold splits are identical.
- **`parity_fomo.dense_jagged_sdpa` replaces exactly one function** in the reference,
  `smri_mae.modules.jagged_scaled_dot_product_attention`. Upstream's version uses
  `torch.nested` + `F.scaled_dot_product_attention`, which has only CUDA backends and raises "No
  viable backend" on CPU -- the reference model cannot run on this host as written, and there is
  no GPU here. The substitute computes the same dense per-sequence attention. Nothing else about
  the reference is touched.
- **The backbone runs once per subject, not twice.** Upstream's `cross_validate` calls
  `method.predict()`, which re-runs `features()` for every out-of-fold subject on top of the
  cached call in `fit()`. Since `features()` is a pure function of the images, both calls return
  the same vector; running it once halves device work and changes no prediction. This is on the
  *method* side, which upstream marks as the tunable part -- the frozen protocol is untouched.

## 5. Checklist review (items 1-13 of `template/experiment-checklist.md`)

1. **Zero-setup start.** `experiment.md`'s EXPERIMENT DESCRIPTION is self-contained: objective,
   ranking metric with justification, levers, constraints, pinned refs, node-local data path, and
   the measured `L_VIS` constant so no agent re-derives it. `seed/` holds working, hardware-run
   code (`run_job.sh`, `job.task5.yaml`, `fetch_data.sh`, `build_and_import.sh`, `metrics.py`).
   `Dockerfile.experimentator` builds (`make experimentator-image
   EXPERIMENT=smri-fm-fomo-tune`, verified: both pins resolve inside the image to
   `11e53ab1...` and `d9a68815...`). Baseline is stamped
   against upstream commit + checkpoint + dataset URL. AUROC is justified in the objective block,
   not just named. The description talks about representation/pooling/head choices (the domain)
   and calls the accelerator a solved black box.
2. **One correctness gate, owned upstream.** `parity_fomo.py` runs upstream's own
   `load_backbone`/`SmriMaeBackbone` from the vendored `src/` as the reference. `experiment.md`
   states that lowering `PCC_GATE` voids the submission. **Enforced**: `run_job.sh` runs the gate
   before the task and `compare()` raises, so the job exits non-zero and never scores.
3. **Cheap vs expensive path.** Cheap: every hypothesis knob is an env var in `job.task5.yaml`,
   read at runtime, zero build. Expensive: `seed/build_and_import.sh` -- and it is genuinely cheap
   here, because `Dockerfile.workload` starts `FROM` the already-built smri-fm workload image, so
   a Python change rebuilds one thin COPY layer with no compile, no clone, no cold cache.
4. **No silent environment skew.** `TT_METAL_REF` appears in `Dockerfile.experimentator`,
   `Dockerfile.workload`, both job specs and `experiment.md`, each with a comment naming the
   failure mode. The base image is pinned **by digest**, not `:latest`. `SMRI_FM_REF` pins the
   upstream clone to the commit `src/` was vendored from.
5. **Metrics, unprompted, at a steady cadence.** `run_task.py` posts `seconds_per_subject` per
   subject and per fold and the final `auroc`, via `metrics.py`, which never raises. Confirmed on
   the real job: 210 readings posted.
6. **Isolated code per agent.** Standard platform mechanism: agents branch and jobs pin a full
   40-char SHA (the API rejects anything else -- verified, it rejected a short ref during this
   port's own submission).
7. **Cheap resume.** Nothing in a job is stateful: it is a ~4 minute run whose inputs are all
   node-local and immutable. A restarted agent re-reads `experiment.md`, this file, and the
   registry; nothing lives only in a process.
8. **Smoke-tested on the real target.** A real job (`exp-fomo-task5-2865be9d`) was submitted
   through `POST /experiments`, admitted, scheduled onto tt-quietbox with real device and real
   `host_mounts`, and **COMPLETED**, posting `auroc=0.9930555555555556` -- identical to the local
   run. This caught a genuine gap a podman-only test would have missed: podman's image store is
   invisible to k3s containerd, and the first submission died with `image_pull_failed`. That is
   now `build_and_import.sh`'s second step, with the misleading error message quoted in it.
9. **Immutable, capacity-aware images.** Base pinned by digest; `build_and_import.sh` prints the
   built image id so cross-wave comparisons are checkable. Rollout: the image is a thin layer over
   one the node already has, so a fleet-wide pull moves megabytes, not 30GB.
10. **Bounded storage and retries.** `storage: 20Gi` -- the dataset is mounted, not copied, and
    outputs are a few MB, so this is headroom for the kernel cache, not for data. `max_retries: 1`
    with the reasoning in the file: this job's failure modes (wrong checkpoint path, a subject
    exceeding `L_VIS`, a gate failure) are deterministic and retrying them just burns a chip; one
    retry covers a flaky device open.
11. **Reporting scales and the result is not lost.** ~70 POSTs per job, bounded by subject count,
    not a per-iteration training loop. The final score posts with 5 attempts, and the full result
    record is written to stdout *before* those attempts, so the runtime's log collection preserves
    it even if the endpoint is down for the whole window.
12. **The gate gates; the metric resists noise.** See item 2 for enforcement. `experiment.md`
    states the reproduce-before-trusting rule explicitly and requires reporting the CI rather than
    the point estimate, with the reason (n=48, CI ~0.02 wide, ceiling at 1.000).
13. **Environment fingerprint.** `run_task.py`'s `environment_fingerprint()` records hostname,
    visible `/dev/tenstorrent` devices, `TT_METAL_REF` and job id into `metrics.json` next to the
    score.

## 6. Open items

- `seed/fetch_data.sh`'s Task_3 branch is untested end to end; its Task_5 and checkpoint branches
  produced the tree currently under `/home/ttuser/fomo-tune-data`.
- The three validation jobs all ran on tt-quietbox. Nothing here has been exercised on a second
  node, so `host_mounts`' node-locality (item 10) is correct by construction but untested in the
  "job lands somewhere else" case.

## 7. Task 3 (brain age) -- wired, not validated

Task 3 shares `run_task.py`, `backbone_tt.py`, `data.py` and the gate unchanged; only the head
(`RidgeCV`) and label type differ, and `seed/job.yaml` is its job spec. It has **not** been run
end to end, and two things are deliberately left open:

- `L_VIS` for its 494 subjects is unmeasured. `job.yaml` leaves it unset, which makes the runner
  measure it at startup (one host-side transform pass, a few minutes) rather than guess.
- `Task_3.zip` is downloaded but **not yet unpacked** into `/home/ttuser/fomo-tune-data`; run
  `seed/fetch_data.sh` to finish it.

At ~3s/subject the embedding pass should be ~25 minutes for task 3.
