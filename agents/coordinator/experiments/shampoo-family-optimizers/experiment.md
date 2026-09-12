## EXPERIMENT DESCRIPTION

```
OBJECTIVE
On a fixed, tiny MLP autoencoder, measure how much the Shampoo family of second-order optimizers
beats Adam, what each successive paper in the family actually improves over the one before it, and
whether a new variant can beat the best of them. This is an optimizer-research experiment, not a
model-architecture one: the network and dataset are fixed and out of scope to change; the only
lever is the optimizer (which one, and its hyperparameters).

MODEL AND TASK (fixed -- do not change)
  AutoencoderMLP in seed/train.py: 784-256-64-256-latent-64-256-784 MLP autoencoder (GELU, no
  BatchNorm -- BN masks preconditioning gains), reconstructing Fashion-MNIST images (MSE loss).
  ~470k params, every weight a 2D matrix -- exactly the shape Shampoo/SOAP/Muon are designed for,
  no conv/embedding special-casing needed. Chosen (Fable design review) as the simplest network
  where the classic second-order-vs-Adam gap is large and reproducible: ill-conditioned loss
  landscape, no augmentation noise, deterministic given a seed.

OPTIMIZERS TO COMPARE (seed/optimizers.py, one entry point `build_optimizer(name, model, lr, ...)`)
  sgd, adamw, full_adagrad, shampoo_2018, scalable_shampoo, soap, muon, kl_shampoo,
  online_kl_shampoo -- see seed/optimizers.py's module docstring for the paper each implements and,
  critically, its FIDELITY NOTES: shampoo_2018/scalable_shampoo/soap/muon are direct unabridged
  implementations of their papers' update rules; kl_shampoo/online_kl_shampoo are BEST-EFFORT
  reconstructions written without the official code (no internet egress when this seed was built).
  FIRST TASK for whoever works kl_shampoo or online_kl_shampoo: clone
  github.com/tilde-research/online-kl-shampoo-release and the KL-Shampoo paper's code if public,
  diff this implementation's update rule against it, and replace it with the official rule if they
  diverge -- note the specific divergence in your hypothesis either way.
  full_adagrad is the "ideal reference" upper bound, not a contender -- only run it at the tiny
  latent=... config where its O(dim^3) state is tractable (see CONSTRAINTS).

RANKING METRIC AND WHY
  optimizer_flops_to_target (MINIMIZE, RANKING METRIC): analytic FLOPs (forward+backward+optimizer
  step, seed/train.py's estimate_optimizer_flops) spent to reach a fixed target test MSE
  (TARGET_LOSS env var, default 0.012 -- AdamW's own tuned loss at a full 3000-step budget; treat
  this as the reference point every other optimizer's speedup/slowdown is reported against).
  Chosen over steps-to-target (hides Shampoo-family's much higher per-step cost -- would flatter
  every second-order method) and over wall-clock-to-target (dominated by this specific hardware's
  host<->device sync and CPU eigendecomp implementation, not the algorithm) -- FLOPs-to-target is
  the implementation-invariant number the papers themselves argue about. Report all three
  (optimizer_flops_to_target, step, wall_clock_seconds) at a steady cadence (REPORT_EVERY_STEPS)
  the whole run, plus `diverged` (1.0 if loss went NaN/Inf -- a diverged run's FLOPs-to-target
  counts as infinite, never dropped from the pool).

USE seed/train.py AND seed/optimizers.py
  seed/train.py is the full harness: loads Fashion-MNIST (torchvision, auto-downloads to
  --data-dir), builds AutoencoderMLP, trains with build_optimizer(...), reports metrics via
  metrics.py at REPORT_EVERY_STEPS cadence, stops at --steps or when TARGET_LOSS is reached.
  Every knob is an env var / CLI flag -- OPT_NAME, OPT_LR, OPT_PRECOND_FREQ, BATCH_SIZE, STEPS,
  TARGET_LOSS, SEED, REPORT_EVERY_STEPS. A hyperparameter sweep or a new optimizer's hyperparameter
  is a change to seed/job.yaml's `command`/env, never to train.py's training loop.
  A new *optimizer variant* (for success criterion 3) is a new class in optimizers.py plus a new
  `build_optimizer` branch -- train.py itself should not need to change for that.

TENSTORRENT INTEGRATION STATUS (read before assuming this runs "on TT" today)
  Per Fable's design review: tt-metal/ttnn has strong coverage for matmul/elementwise/reduction
  (enough for this model's forward/backward) but no production eigh/SVD/matrix-inverse/matrix-root
  op -- Shampoo, SOAP, and KL-Shampoo's preconditioner math cannot run on-device. The only
  tractable architecture is: optimizer *step* (all matrix-power/eigendecomp work) always on host
  CPU in fp32 (seed/optimizers.py already does this unconditionally, regardless of --device);
  forward/backward is the only thing that could plausibly move to TT.
  seed/train.py's --device tt flag exists but its ttnn forward/backward wiring is NOT YET
  IMPLEMENTED -- passing it prints a notice and runs forward/backward on cpu so training is still
  correct, but the on-device-execution question is not yet answerable. IMPLEMENTING THAT WIRING
  (a ttnn forward/backward path for AutoencoderMLP, autograd-compatible or a matching manual
  backward) is in scope as a hypothesis and is the natural first "does the Shampoo-vs-Adam gap
  survive TT's bf16 activations/grads" experiment once it exists -- see CONSTRAINTS for the
  required CPU-fp32 control every job must also report.
  Until that wiring lands, every job runs --device cpu; this does not block starting the sweep
  (the optimizer comparison itself is well-defined on cpu/fp32) but means "which optimizer wins"
  results are not yet validated against TT's actual numerics -- treat that as an open, named risk
  in any FINAL_RESULT.md conclusion until a job with real ttnn forward/backward has run.

CONSTRAINTS
  - full_adagrad only at latent=... configs where flattened per-parameter dim stays under ~300
    (O(dim^3) state) -- do not point it at the default 784-256-64 config, it will not finish.
  - Preconditioner update frequency (OPT_PRECOND_FREQ, Shampoo-family methods only) is a real axis,
    not a knob to fix arbitrarily: sweep k in {1, 10, 100} for every Shampoo-family optimizer and
    report the best-FLOP point per method. Comparing method A at k=1 to method B at k=100 is not a
    comparison of the papers -- always report which k a result used.
  - Tuning budget parity: a claimed win must come from a fixed random search of N=32 trials per
    optimizer over LR (log-uniform, 3 decades) plus that optimizer's own hyperparameters (momentum,
    eps, betas, precondition_freq, grafting on/off) -- same seeds, same trial budget across
    optimizers. Report best-of-N AND median-of-top-5 (best-of-N alone rewards high-variance
    methods) -- a "win" on best-of-N alone that doesn't hold on median-of-top-5 is not a win.
  - >=5 seeds on any config you're claiming a ranked result for; a gap smaller than the seed
    std-dev is a tie, not a win.
  - Numerics: optimizer-step math is fp32 always (seed/optimizers.py already enforces this) --
    never let a job silently run preconditioner math in bf16/fp16.
  - Weight decay: decoupled (AdamW-style) for every optimizer that uses it, including any new
    variant -- do not compare an optimizer with WD against one without.
  - Do not weaken TARGET_LOSS, the metric definition, or the seed/train.py measurement code to
    post a better number -- any change to how the metric itself is computed voids the submission
    the same way a weakened correctness gate would (experiment-checklist.md item 2). This
    experiment's "correctness gate" is the metric computation itself (there is no separate
    held-out validation suite the way a kernel-correctness experiment has upstream tests) --
    changing estimate_optimizer_flops or how test_loss is computed is exactly the kind of change
    that's off limits.
  - A diverged run (NaN/Inf loss) reports optimizer_flops_to_target = inf and diverged = 1.0; it
    stays in the pool, never silently dropped.

METRICS TO REPORT
  optimizer_flops_to_target  (minimize, RANKING METRIC) -- see above.
  step                       (attribute)
  wall_clock_seconds         (attribute)
  diverged                   (attribute) -- 1.0/0.0

RESEARCH QUESTIONS THIS EXPERIMENT MUST ANSWER (success criteria)
  1. How much better is each Shampoo-family optimizer than AdamW, and under what conditions
     (batch size, precondition_freq, seed count) does the advantage appear or vanish? Report a
     verdict per optimizer: confirmed / partially confirmed / not confirmed against its own
     paper's claimed improvement, backed by the measured optimizer_flops_to_target numbers here
     (not the paper's own reported numbers, which used different models/data/hardware).
  2. What does each successive paper improve over the previous one, and by how much? Chain:
     Shampoo 2018 -> Scalable Shampoo 2020 -> SOAP 2024 -> KL-Shampoo 2025 -> Online KL Shampoo
     2026 (Muon is a separate lineage -- compare it to the chain but don't force it into the chain
     order). Each link needs its own confirmed/partially-confirmed/not-confirmed verdict.
  3. Can a new variant beat the best of them? Ends when EITHER a variant reaches
     optimizer_flops_to_target >= 10% better than the best baseline AND that holds on a second,
     larger network (not just this experiment's tiny AE -- flag this as the next experiment's job
     once found, don't try to also build the larger-network experiment inside this one), OR a
     documented negative result after >=5 distinct tried variants (each variant = a new
     optimizers.py class + a fair sweep against it, not a hyperparameter retune of an existing one).

BASELINE
  config:    seed/train.py defaults (AutoencoderMLP, OPT_NAME=adamw, OPT_LR=1e-3, BATCH_SIZE=1024,
             STEPS=3000, TARGET_LOSS=0.012, SEED=0), --device cpu
  code_ref:  not yet established -- no commit has been pushed to $CODE_REPO_URL yet
  metric:    optimizer_flops_to_target = not yet established
  measured:  not yet established -- establishing this AdamW baseline (and confirming
             TARGET_LOSS=0.012 is actually reachable within STEPS=3000 at the default LR, tuning
             LR first if not) is the first task, concurrently with the first wave of agents'
             own hypotheses, not a separate blocking step before spawning.
```

---

## Coordinator notes (not sent to agents)

- Ranking metric: `optimizer_flops_to_target`, minimize. Attributes: `step`, `wall_clock_seconds`,
  `diverged`.
- Design reviewed by Fable (Codex was rate-limited at design time -- see fix-later.md; re-consult
  Codex before the flavor-mix-narrowing decision at the first stage boundary, per the /goal
  directive, once its usage limit resets).
- Flavor mix at launch: broad (`generalist` x2 + `hyperparameter-search` + `architecture-search`)
  -- no promising region yet, and both "tune an existing optimizer" and "build a new variant"
  axes need coverage from wave one. `data-search` is not useful here (dataset is fixed by design)
  -- omit it from this experiment's mix entirely, not just at launch.
- Agents start from this experiment's `seed/` (optimizers.py, train.py, metrics.py, loglines.py)
  plus the baked-in samples. `seed/build_and_push.sh` builds/pushes the job image; smoke-test on
  real Tenstorrent hardware per setup.md step 1.4 before spawning agents.
- The TENSTORRENT INTEGRATION STATUS block above is a live blocker, not resolved yet -- see
  fix-later.md. Do not let an agent claim a TT-hardware-validated result until it is.
