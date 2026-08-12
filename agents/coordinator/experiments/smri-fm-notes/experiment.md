## EXPERIMENT DESCRIPTION

OBJECTIVE
Make the sMRI MAE (masked-autoencoder) model actually smarter: minimize held-out validation
reconstruction loss (`val_loss`) at the production `full-vitl-real` shape contract (batch=24,
encoder D=1024/depth=24/16 heads, decoder D=512/depth=4/16 heads, 316.2M trainable params). This
is a real production pretraining workload, not a synthetic microbenchmark, and it already runs
end to end (see BASELINE for a completed real run). Quality is the only lever worth pulling: LR
schedule, warmup, weight decay, masking ratio, optimizer settings, and model
architecture/hyperparameters (depth, width, heads, patch/masking strategy) that could plausibly
help the model learn. Device speed (`ms_per_sample`) is tracked only as a secondary/diagnostic
signal so a stalled or badly-regressed job is visible; it never ranks a submission and is not a
lever worth pulling on its own. The runtime/hardware stack underneath is a solved, working
implementation detail -- you are not expected to touch it; treat it as a black box you configure
through the entrypoint's CLI flags and Python-level model code, not something you rebuild.

USE seed/
  - The entrypoint already posts every metric for you (`seed/metrics.py`'s `post_metric()` is
    wired into training/eval unmodified) -- a config-only job gets metrics for free.
  - `seed/job.yaml` -- a starting JobSpec for a real job. It requests `host_mounts` (a read-only,
    node-local bind mount of this host's already-fetched dataset) so a config-only job pays zero
    dataset-fetch cost on this node. `seed/job.tt-small.yaml` is the equivalent for the second
    cluster -- same dataset, different node-local mount path (host_mounts can't be shared
    verbatim across nodes). Pick whichever job spec matches the cluster your job actually lands
    on; both are real, working starting points.
  - Every knob is a CLI flag: `--base-lr`, `--weight-decay`, `--warmup-steps`, `--min-lr`,
    `--clip-grad`, `--accum-iter`, `--seed`, `--log-every`, `--eval-every`, `--checkpoint-every`.
    A hyperparameter sweep is a change to the job's `env`/`args`, never to code.
  - Model architecture and training-loop Python (encoder/decoder depth & width, masking
    strategy, data augmentation/prefetching) is plain, editable Python -- change it freely if you
    have a reason to expect it helps the metric. No rebuild step of any kind is needed for any
    change described here.

METRICS TO REPORT
  val_loss               (minimize, RANKING METRIC) -- held-out reconstruction quality.
      `main_pretrain_tt.py --eval-every N` already runs a full no-grad pass over the held-out
      val shards (`real_data.RealValSource`, 2550 samples across 17 shards never seen in
      training) and reports mean per-scan valid-voxel MSE. This directly tracks the objective
      (a smarter model), not a proxy for it -- report every eval, not just at the end.
  train_loss              -- cheap, high-frequency signal (posted every --log-every steps) so a
      stuck/dead job is visible fast; noisy step-to-step (see BASELINE) and NOT the ranking
      metric on its own -- a lower training loss with no held-out val_loss confirmation is not
      a legitimate win, since it doesn't rule out overfitting.
  ms_per_sample           -- DIAGNOSTIC ONLY, never ranks a submission. Wall-clock ms/sample at
      steady state (warm steps only), computed as step_time_ms / (batch_size * accum_iter).
      Useful for noticing a change made the job impractically slow to iterate on, nothing more --
      a config that improves val_loss but costs more ms/sample is still a legitimate win.
  Why these three, not more: a downstream linear-probe eval already exists in this repo
      (`src/evaluation/main_linear.py`) but only against the PyTorch reference model's
      checkpoints, not wired to TT checkpoint format -- building that bridge is real, separate
      work and out of scope here; val_loss (held-out reconstruction MSE, the MAE's own training
      objective) is the best quality proxy available today without inventing new eval
      infrastructure. Flagged here so a future experiment definition doesn't silently forget
      this gap exists.
  Report all three at a steady cadence while the job runs (train_loss every --log-every steps,
  val_loss every --eval-every steps, ms_per_sample once per warm step or your own steady
  cadence), not once at the end.

METHOD -- two phases, matched to this platform experiment's own stage boundaries. Multiple jobs
can run at once (one per available accelerator, per the live resource catalog) -- plan concurrent
probes/runs to use that, not one job at a time.
  Worth a quick search for relevant prior work (papers, established MAE/ViT pretraining recipes)
  before proposing a hypothesis, when it's cheap to check -- prefer a grounded guess over a blind
  one, but don't let this become its own overhead.
  Phase 1 (research + broad screening): screen each hypothesis with a probe job -- this stage's
      own `max_job_hours` cap (raised to 3.5h; the earlier 2h cap evicted every 3500-6000-step
      probe before completion in practice, so treat this as the real, verified ceiling rather than
      re-deriving one from step count) gives real room to see LR-decay-phase signal on a probe.
      **Coordinator note, 2026-08-11T18:20Z:** stage 1's `length_pct` was widened from 35 to 99
      live on `pe-d564bb90` (stage 2 shrunk from 65 to 1) so phase 1's screening window covers
      essentially the whole 24h run instead of cutting 50% of agents at the ~8.4h mark -- with
      only one hypothesis (min_lr=1e-5, +4.39% at T=2800) confirmed at that point, the original
      35% cutoff would have ended screening too early. `max_job_hours=3.5` on stage 1 is
      unaffected by this change. If this experiment is ever restarted fresh, set `stages` at
      creation time to match (e.g. `[{99,50,3.5},{1,0}]`) rather than relying on a live DB patch.
      that actually finishes, not just early noise. Compare probes to each
      other at a matched step count (e.g. all at step 1000), never to the full BASELINE number
      below -- an early checkpoint always looks worse than a finished run regardless of whether
      the config is actually better, so an early-step val_loss is only meaningful relative to
      another early-step val_loss from the same step count, not as an absolute claim. A probe that
      only wins late in training (past this stage's step ceiling) can still look like a loss here
      -- if a hypothesis has a real mechanistic reason to expect a late payoff (e.g. it interacts
      with LR decay), say so and give it a stage-2 slot anyway rather than trusting the phase-1
      number alone to kill it.
  Phase 2 (confirm + consolidate): take the best-performing direction(s) from phase 1 --
      including combining complementary ones (e.g. the best LR schedule plus the best warmup, if
      they were screened independently) -- and run them at full length under this stage's most
      generous cap, room enough for the reproduce-before-trusting rerun in the same or a
      follow-up job. A phase-1 win is a lead, not a result, until it survives this full-length
      reproduction.

CONSTRAINTS
  - Correctness gate, owned upstream, not by you: the project's own test suite must stay green
    after any change. Editing a test to make it pass voids the submission.
  - Dataset: 67 shards, 150 samples each, 50 shards/7500 samples train + 17 shards/2550 samples
    val, already present on this host -- every job re-fetches into its own storage idempotently.
  - A win must reproduce: rerun your best config once more at the same step count/seed before
    trusting it -- these metrics are not noise-free.

BASELINE (a completed real run at the production contract, this host, real hardware)
  - **val_loss BASELINE (this is the number to beat):** run `fleet-baseline-d0-v2`, 6000 steps,
    `--eval-every 500`: **val_loss (eval/loss) = 0.47145** at step 6000 (down from train_loss
    2.050212 at step 0), final train_loss 0.455414. A submission's val_loss must be reported at a
    matched step count against this run to be a legitimate comparison -- an earlier-step
    checkpoint will trivially look worse.
