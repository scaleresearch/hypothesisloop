## EXPERIMENT DESCRIPTION

```
OBJECTIVE
Maximize held-out validation accuracy (val_accuracy) on a small synthetic two-class dataset. This
is a fast, self-contained shakedown of the coordinator's stage-aware flavor mix and the platform's
three experimentator flavors (hyperparameter-search, architecture-search, data-search) — not a
real research problem. Every job completes in well under a minute.

STARTING POINT
Your shared code repo already contains a working numpy MLP + synthetic-dataset generator
(seed/train.py) and a ready-to-submit job spec (seed/job.yaml, image already built and pushed).
Clone it, branch, and iterate — this is a config-only task: every knob you vary is a job.yaml env
var, never a code change. Submit `hl job submit --agent <id> job.yaml` directly; no build step.

THREE INDEPENDENT AXES — hold two fixed, vary the third, per your flavor brief
  ARCHITECTURE   HIDDEN_LAYERS (comma-separated hidden-layer widths, e.g. "32,16"),
                 ACTIVATION (relu|tanh)
  HYPERPARAMS    LR, BATCH_SIZE, EPOCHS, L2
  DATA           N_SAMPLES, NOISE, N_FEATURES, CLASS_SEP, CURRICULUM (0|1 — easy-to-hard
                 training-set ordering; only meaningful together with epochs > ~10)
Every job.yaml env var not in your flavor's axis must stay at whatever value the pool has already
converged on (or the BASELINE's, before anything has converged) — that is what "hold fixed" means
concretely here.

METRICS TO REPORT
  val_accuracy (maximize, RANKING METRIC) — held-out accuracy, reported as a running max.
  train_loss   — final-epoch mean training loss (attribute, not ranked).

CONSTRAINTS
  - Runs on one fake accelerator per pod (accelerator_type
    nvidia.com/gpu.product=NVIDIA-L40, accelerator_count 1) — the workload itself is plain numpy
    on CPU; the accelerator_type exists only to exercise real scheduling/quota.
  - Report metrics at a steady cadence while the job runs, not once at the end.
  - Never fabricate or inflate a metric; nothing checks this server-side.
  - The dataset generator and training loop in seed/train.py ARE the correctness gate — do not
    change the loss function, the accuracy definition, or the train/val split (first 80%/last
    20% of the generated, pre-shuffled samples) to make a number look better.

FACTS ABOUT THE PROBLEM
  - The task is a noisy two-moons-style binary classification problem; N_FEATURES > 2 adds pure
    noise columns (a real data axis: more noise columns without more capacity or more signal
    should hurt, not help).
  - The run is short and cheap by design (EPOCHS default 40, N_SAMPLES default 800) so a fleet
    can run dozens of jobs in minutes — prefer many small trials over few large ones.

BASELINE
  config:    seed/job.yaml shipped defaults (HIDDEN_LAYERS=16, ACTIVATION=relu, LR=0.05,
             BATCH_SIZE=32, EPOCHS=40, L2=0.0, N_SAMPLES=800, NOISE=0.25, N_FEATURES=2,
             CLASS_SEP=1.0, CURRICULUM=0), one fake NVIDIA-L40 accelerator per pod
  code_ref:  the seeded repo's `main` branch, seeded from this experiment's own seed/ directory —
             no 40-char SHA is recorded for this baseline
  metric:    val_accuracy = not yet established
  measured:  not yet established -- confirming the shipped defaults' val_accuracy with an
             unmodified seed run is the first task
```

---

## Coordinator notes (not sent to agents)

- Ranking metric: `val_accuracy`, maximize. Also declare `train_loss` (attribute).
- Purpose: end-to-end validation of the flavor system (README's "coordinator picks the flavor mix
  deliberately") — three orthogonal axes (architecture, hyperparameters, data) all present in one
  cheap synthetic task, so hyperparameter-search / architecture-search / data-search /
  generalist can all run against the same objective and be judged against evidence (submitted
  jobs' env, hypotheses' text, filed findings), not self-report.
- Deliberately short stage ladder (40%/50% then 60%/0%) so a stage cut happens within minutes,
  giving the coordinator a real "narrow the flavor mix once a promising region is found" decision
  point to make and record, per setup.md step 3's stage-aware guidance.
- The shared repo's `main` branch is seeded with this experiment's own `seed/` directory
  (agents/coordinator/experiments/dummy-data-search-demo/seed/) as the agents' starting point.
