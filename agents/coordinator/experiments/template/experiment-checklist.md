# Experiment design checklist

What an experiment definition under `agents/coordinator/experiments/<name>/` needs to hold up
at scale (10s-100s of agents, hours-long unattended runs). Use this when designing or reviewing
an experiment definition. Applies to any experiment type — ML training runs, kernel/systems
benchmarks, whatever the platform hosts next — not just the one that happened to prompt a given
item below.

The purpose: everything an agent needs must already be ready before any agent is spawned, so that
when it schedules a job, the job does what it's intended to do right away — no waiting on setup
the experiment definition should have handled once, up front (building packages, fetching a large
input like a dataset/corpus/fixture set from scratch). An agent's entire attention should go to
the objective — forming and testing hypotheses — never to plumbing, environment archaeology, or
working around a gap the experiment definition left for it to discover.

## 1. Zero-setup start: `experiment.md` + three yaml files + `seed/`

An agent that spends its first hour building a harness or discovering a shape isn't competing,
it's re-deriving what every prior agent already derived. An experiment definition needs:

- `experiment.md`'s `EXPERIMENT DESCRIPTION` block — the *only* thing agents read about the objective
  (the system prompt is deliberately experiment-agnostic). It must be fully self-contained: objective,
  ranking metric(s) + how to report them, constraints, and where the pre-built tools live. If it's
  not in this block, no agent sees it.
- Three checked-in yaml files, one per stage of the flow an agent (or the coordinator) drives
  through `hl`, copied from `template/` and never hand-assembled in bash/curl:
  - `experiment.yaml` — the `POST /platform-experiments` body (see `template/experiment.yaml` and
    `controlplane/settings/examples/experiment.yaml` for the full DSL), submitted verbatim with
    `hl platform-experiments create experiment.yaml` in setup.md step 2. Its `description` must be
    `experiment.md`'s `EXPERIMENT DESCRIPTION` block, kept in sync the same way the live platform
    experiment is (setup.md step 2's description-sync rule). Its `budget_accelerator_hours`/
    `max_agents`/`starts_at` are filled in at spawn time (they depend on `NUM_AGENTS`/
    `CHIPS_PER_AGENT`, decided then, not ahead of time) — the checked-in file carries
    `REPLACE-WITH-...` placeholders for those.
  - `hypothesis.yaml` — the `POST /hypotheses` body (see `template/hypothesis.yaml` and
    `controlplane/settings/examples/hypothesis.yaml`), the seed an agent copies and fills in
    before `hl hypothesis submit --file hypothesis.yaml` (system_prompt.md step 5).
  - `seed/job.yaml` — the `POST /experiments` body (see below and
    `controlplane/settings/examples/experiment-submission.yaml`), submitted with
    `hl job submit --agent <id> job.yaml`.
- `seed/` — working, hardware/environment-validated code an agent copies and adapts, not a spec
  it implements from scratch. If every agent independently reinvents the same measurement code,
  slightly differently, that costs real iteration time and makes results harder to compare
  apples-to-apples.
- `Dockerfile.experimentator` — this experiment's own overlay on the shared
  `agents/experimentator/Dockerfile.base` (`FROM localhost/hypothesisloop-experimentator-base`),
  built with `make experimentator-image EXPERIMENT=<name>`. Put here whatever runtime source or
  pin the agent needs to *read* to form hypotheses (a cloned framework repo, a pinned commit) —
  never in the shared base, or one experiment's pin bump silently changes what every other
  experiment's agent reads. An experiment with nothing extra to read can skip this file entirely
  and run straight off the base image; it doesn't need an overlay just to exist. `git add`/commit
  every file it `COPY`s — an untracked file builds fine in your own working tree but is invisible
  to a different agent's own git worktree.
- A `BASELINE` block inside the `EXPERIMENT DESCRIPTION`, so an agent's first job is a
  confirmation and not a fresh discovery. The control an experiment measures against is a
  property of the program, not an object the platform owns — nothing in the domain encodes it,
  and nothing should, because deciding what counts as a control is the judgment this file and
  the coordinator exist to make. What makes it real instead is that it reaches every agent
  verbatim, through the same description-sync rule as the rest of the block:

      BASELINE
        config:    <the exact configuration>
        code_ref:  <repo>@<40-char-sha>
        metric:    <declared ranking metric> = <value>
        measured:  <experiment id that produced it, or "not yet established">

  All four lines are required. `code_ref` is what stops the number rotting silently the moment
  its pin moves, and `measured` is what separates a figure someone ran from one someone
  remembers. A baseline nobody has measured yet says `not yet established` and names
  establishing it as the first task in the description — silence is not an acceptable third
  option, because an agent reading no baseline invents its own and every ranking that follows
  compares against a different one.
- Each ranking metric needs to earn its place, not just be named: pick it (or them) by what the
  experiment's task and objective actually reward, and say why in `experiment.md`, not only what
  it is and how to report it. It must directly track the objective, not just be convenient to
  measure — a metric that's easy to report but a poor proxy for the real goal (e.g. throughput
  when the goal is model quality, or a training-time loss that doesn't track held-out quality)
  produces checklist-compliant results that are still the wrong thing to have optimized.
- Describe the task in the *domain* the agent is actually meant to compete in, not the
  implementation stack underneath it. Name the levers that plausibly move the ranking metric
  (whatever they are for this experiment — hyperparameters, an algorithm, a data pipeline, a
  scheduling policy), and call any layer the agent isn't meant to touch a solved black box it
  configures rather than something it must understand or modify. Naming internals — kernels, a
  build system, vendored patches, a dependency's implementation — that aren't actually part of
  what the agent should vary reads as scope it must engage with, and wastes its first turns on
  stack trivia (or worse, unnecessary rebuilds) unrelated to the metric it's ranked on. Say only
  what's needed to run and vary the thing actually being optimized.

## 2. One correctness gate, owned by upstream/ground-truth, not by the agent

The experiment definition's constraints should pin correctness checks to a fixed, external reference (an upstream
test suite, a golden dataset, a spec) and say explicitly that changing the gate voids the
submission. Without this, an agent under optimization pressure will loosen its own correctness
check to post a better number — indistinguishable from a real win unless the gate is fixed and
external. Reuse the ground truth's own validation code directly rather than a hand-rolled copy of
it, for the same reason.

## 3. Pre-bake a ready-to-submit starting point for each kind of change an agent will make

Most hypotheses an agent tries are "same code, different setting" (learning rate, batch size,
masking ratio, an env var/config field). Some are "the code itself needs to change" (a new kernel,
a different algorithm, a new dependency). These need two different pre-built starting points
prepared up front, not one:

- **Config-only jobs**: submittable straight from the base runtime image, no build step, no
  source checkout — the agent only ever touches env vars/a config file.
- **Structural/code-change jobs**: their own pre-cloned, pre-built starting point with a warm
  cache, so editing code and resubmitting is an incremental build, not a cold one.

If only the second exists, every job — even a one-line config tweak — pays a multi-minute clone +
build, and a fleet spends most of its time compiling instead of iterating. Set both up as part of
this experiment definition, before any agent is spawned, the same way you'd pre-build the job
image or pre-fetch a dataset — this is exactly that same category of "make it a checklist item
here, not something 100 agents each redo for themselves."

## 4. No agent can silently run against a different environment than it thinks it's running against

- Any pinned version/ref (a source commit, a container tag, a dataset snapshot) must match
  identically everywhere it's referenced — the agent's read environment, the job image, the build
  script. A drift here invalidates every result without producing any error, so call out the
  failure mode in comments wherever the pin is set, not just in one place.
- If a fast/prebuilt path can legitimately lag behind what agents read (e.g. a prebuilt artifact
  older than current source), the harness should degrade gracefully — fall back rather than hard
  fail — so a real version skew doesn't look like a broken submission.

## 5. Every job reports the same metrics, the same way, unprompted

The harness should post the declared ranking metric(s) at a steady cadence during the run, not
once at the end, so a coordinator watching periodically gets live signal from a job that might
run for hours — a dead/stuck job is otherwise indistinguishable from a slow-but-fine one. Metric
and log reporting should never raise on failure: a reporting-endpoint hiccup must not take down
the timed work it's reporting on.

## 6. Every agent gets an isolated place to write code, and never collides with another agent

If many agents share one code repo, each should work on its own branch (created once, reused
across restarts), and a job should pin its code by a full commit SHA, never a branch name, pushed
just before submit. This is what lets a fleet of agents share one repo without stepping on each
other's commits or a job silently picking up a teammate's later, unrelated change mid-run.

## 7. A restarted agent must be able to pick up exactly where it left off, cheaply

Runs span long unattended stretches (crashes, redeploys, wall-time caps) and every restart has
zero memory. So state an agent needs to resume correctly must live outside the agent process:
its own branch/commit (survives pod deletion), the shared registry's history (fetched narrow and
never re-fetched once read — context is not durable storage), and a rolling log/state buffer a
coordinator or the agent's next session can read without needing the original process to still be
alive.

## 8. Smoke-test on the real target, at the same layer agents will hit, before spawning any agent

A build succeeding is not proof a job works. Test the actual job-submission path end to end
(the same runtime, mounts, and device access a real job gets), not just that an image builds —
otherwise an environment gap (a missing mount, a missing capability) reads exactly like a broken
image, and 100 agents inherit the same false negative until someone debugs it by hand.

## 9. Immutable, capacity-aware images

`latest`/`:src`-style mutable tags let two agents (or two waves) silently run different binaries
of "the same" image — pin by digest, or at minimum re-verify the pin didn't move before trusting
a comparison across jobs. Separately, a heavy pre-built image (full source tree + build artifacts
+ warm cache) landing on ~100 pods at once can saturate the registry and node disks before any
job reaches hardware — plan for pre-pull/staggered start or an admission throttle, not just image
size on its own.

## 10. Storage and retry costs must be bounded, not just requested

- Size ephemeral storage for the actual worst case (build output + cache + traces + dumps), and
  clean up after a run, not just request a number that happens to work today.
- `max_retries` without failure classification means a deterministic failure (bad patch, OOM,
  disk-full) burns the full retry budget on hardware every time instead of failing fast — classify
  retryable (flaky) vs. non-retryable (deterministic) failures before scaling retries.
- A job that needs a large, shared, mostly-static input (a dataset) shouldn't re-fetch it every
  run — use `JobSpec.host_mounts` to bind-mount an already-fetched, node-local copy instead of
  defaulting every job to fetch-on-start.

## 11. Reporting must scale sublinearly and never lose the final result silently

Per-iteration metric/log POSTs that are fine for one job become real registry load at fleet scale
(iterations × jobs × endpoints) — batch or throttle instead of one request per iteration per job.
And "never raise on a reporting failure" (item 5) must not mean "silently lose the result": if the
final metric POST fails, the run's outcome should still be recoverable (a durable artifact, a
final retry) rather than a job that finished correctly but reported nothing.

## 12. The gate must actually gate, and the metric must resist being gamed by noise

- Reporting a constraint (PCC, cache count) is not the same as *enforcing* it — a harness that logs
  a value but exits 0 regardless lets a run that would fail the gate quietly enter the ranking
  anyway; the enforcement point (harness, coordinator, or scoring) needs to be unambiguous and
  someone's explicit job.
- A ranking metric taken as a single best-of-N minimum, at fleet scale (100 agents × N iterations
  each), makes a lucky outlier measurement likely to win by measurement noise alone rather than a
  real improvement — require the winning config to reproduce (rerun and confirm) before it's
  trusted, not just report the running minimum.

## 13. Record enough environment fingerprint to trust a cross-node comparison

If the fleet runs across more than one physical node/device, selecting an accelerator *type* isn't
enough to make results comparable — record which physical device, firmware/runtime version, and
thermal/power state a result came from, or a real between-node variance can get mistaken for a
genuine improvement (or vice versa).

## Using this checklist

When starting a new experiment definition, work through items 1-13 against its own `experiment.md`,
`experiment.yaml`, `seed/` and `Dockerfile.experimentator` — each item should have a concrete
answer, not just "not applicable." If a new problem
shows up in a live run that this checklist doesn't already cover, add it here as a new item (with
the failure mode it prevents, like the ones above) before building the fix into the experiment
definition — that keeps the next experiment definition benefiting from it too, not just this one.
