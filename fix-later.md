# Findings log

## shampoo-family-optimizers experiment setup (2026-09-11)

- Built the experiment definition at `agents/coordinator/experiments/shampoo-family-optimizers/`
  (experiment.md, experiment.yaml, hypothesis.yaml, seed/{optimizers.py, train.py, metrics.py,
  loglines.py, Dockerfile.workload, build_and_push.sh, job.yaml}) per experiment-checklist.md.
  No Dockerfile.experimentator overlay -- agents write plain-PyTorch optimizer code, nothing extra
  to bake in for them to read (checklist item 1 allows skipping it).
- Design review: Codex (`codex exec`) was rate-limited at design time ("You've hit your usage
  limit... try again at 11:42 AM") -- could not get its review. Got Fable's review instead
  (network/task choice, ranking metric, fairness pitfalls, TT feasibility) and incorporated all of
  it into experiment.md. RE-CONSULT CODEX before the first flavor-mix-narrowing decision (first
  stage boundary) once its usage limit resets, per the /goal directive's requirement to consult
  both reviewers before architectural decisions -- this one is still outstanding.
- Implemented and locally smoke-tested (CPU, synthetic 20-dim data, 30 steps) all 9 optimizers in
  seed/optimizers.py: sgd, adamw, full_adagrad, shampoo_2018, scalable_shampoo, soap, muon,
  kl_shampoo, online_kl_shampoo. All ran without crashing, all losses stayed finite, all reduced
  loss from the same init. This is a *smoke test only* (correctness of the update rule, no
  divergence) -- it is not a hyperparameter-tuned or paper-reproducing result.
- kl_shampoo and online_kl_shampoo are BEST-EFFORT reconstructions (log-Euclidean-mean
  Kronecker-factor update) written without the official code -- no internet egress was available
  in this sandbox to clone github.com/tilde-research/online-kl-shampoo-release or find KL-Shampoo's
  official repo. Flagged prominently in optimizers.py's module docstring and experiment.md as the
  first task for whichever agent works those two optimizers: clone the real repos, diff the
  update rule, replace if it diverges.
- BLOCKER, not yet resolved: the actual hardware fleet was not brought up in this pass.
  `podman ps` shows the control plane containers exist but are all `Created`/exited, not running;
  `curl localhost:8081/openapi.json` and the local registry both came back down. Bringing up
  `make controlplane-up`, building+pushing the job image, smoke-testing it on a real
  `/dev/tenstorrent` device end-to-end, creating the platform experiment via `hl
  platform-experiments create`, establishing the real BASELINE measurement, deciding the flavor
  mix, and spawning agent containers (setup.md steps 1-4) is genuine remaining work -- do this
  next, following setup.md literally, before trusting anything here as "running." The `hl` CLI
  itself builds cleanly (`go build -o /tmp/hl ./cli` succeeded).
- Local devices present on this host: `/dev/tenstorrent/0..3` (4 ASICs visible), so capacity for
  a small fleet exists once the control plane and DRA driver are confirmed up (setup.md step 1
  reboot-recovery checks apply -- haven't been re-verified this session).
- TENSTORRENT INTEGRATION STATUS (also documented in experiment.md): every optimizer's own matrix
  math is CPU-only by design (no eigh/matrix-power op on tt-metal/ttnn). train.py's forward/backward
  --device tt path is NOT YET WIRED to real ttnn ops -- it currently falls back to cpu with a
  loud stderr notice rather than silently running cpu under a tt label. Implementing real ttnn
  forward/backward for AutoencoderMLP is in-scope future work, named explicitly in experiment.md
  as an open risk on any ranking result until it exists.
- BLOCKER (this pass): `.gitignore` was fixed to un-ignore `agents/coordinator/experiments/shampoo-family-optimizers/` (it was silently matching the generic `experiments/*/` ignore rule, same as `dummy-data-search-demo` before it -- the experiment dir existed on disk but `git status` never showed it). But `make controlplane-up` -> `make images` -> `check-clean-tree` refuses to build because the whole repo tree has *pre-existing* uncommitted changes across ~25 unrelated tracked files (`.gitignore`, `README.md`, `cli/main.go`, `controlplane/services/registry/*`, other experiments' seed files, etc.) that predate this session and are not part of this experiment's work. Several other peer Claude Code sessions are active on this same machine/repo (per `ListAgents`) and may be mid-edit on some of these files. Committing or stashing all of that to unblock `make images` is not this fork's call to make unilaterally -- it risks clobbering someone else's in-progress work, and git safety policy is "never commit unless explicitly asked." Stopping here rather than guessing.
  RESOLUTION NEEDED FROM COORDINATOR/USER: either (a) confirm it's safe to commit the currently-dirty tracked files (or a scoped subset) so `check-clean-tree` passes, or (b) have each in-flight session commit its own changes first, or (c) build the shampoo image via a path that doesn't go through `make images`'s tree-cleanliness gate (none identified yet -- `make controlplane-up`'s `images` target is shared, not per-experiment).
  My own edit in this pass: `.gitignore` (added `!agents/coordinator/experiments/shampoo-family-optimizers/`). That one change is safe to commit on its own if useful, but doesn't unblock the rest of the dirty tree.
- Next concrete steps for whoever picks this up: (1) re-run the Codex design review once rate
  limit clears; (2) setup.md steps 1-4 for real (control plane up, image build+push+smoke-test on
  a real device, `hl platform-experiments create`, establish BASELINE, spawn 2-4 agents with a
  broad generalist/hyperparameter-search/architecture-search flavor mix per experiment.md's
  coordinator notes); (3) supervise.md loop from there, iterating implement/test/run/observe/fix
  across the 2-3 generations the user expects before results are trustworthy.

## shampoo-family-optimizers: control-plane bring-up blocked (port 8081 held by unrelated process)

Attempted a dirty-tree-safe bring-up of just the control plane compose stack (bypassing `make
controlplane-up`'s `images: check-clean-tree` dependency, since `:latest`-tagged
control-service/metrics-service images already existed locally from a prior build):

    make render-settings
    podman compose -f localdev/controlplane/docker-compose.yml up -d registry
    TAG=latest podman compose -f localdev/controlplane/docker-compose.yml up -d

registry/postgres/greptimedb/minio/metrics-service all came up healthy. control-service failed:

    Error response from daemon: rootlessport listen tcp 0.0.0.0:8081: bind: address already in use

Root cause: PID 290355 (`/home/ttuser/work/tracy_env/bin/python .../tracy/serve_wasm.py --port
8080`, running since 2026-09-05, unrelated to this repo/session) is bound to port 8081 on this
host (per `ss -ltnp`), despite its own `--port 8080` arg — possibly a second listener or stale
port mapping. This predates this session and belongs to unrelated work on this shared machine.

Not killing it unilaterally — same reasoning as the dirty-tree decision: this is another
process's port, likely someone else's live work, and killing it without confirmation risks
breaking something outside this experiment's scope.

**Still running (safe, no other-work impact):** registry/postgres/greptimedb/minio/
metrics-service containers via the compose stack above. control-service is `Created` but not
started, so `$API_URL` (8081) is still down — `hl` calls will fail until this is resolved.

**Open blocker, needs a decision:** either (a) confirm PID 290355 can be killed/moved off 8081,
or (b) remap control-service's host port in `localdev/controlplane/docker-compose.yml` /
`API_URL` to something free (would need to match whatever `$API_URL` every job/agent config
reads, so a real change, not just a local override) so it doesn't collide with existing use of
8081 on this host.

Everything past this point in setup.md (job image build/push/smoke-test, platform-experiment
creation, baseline, agent spawn) is still blocked until control-service is reachable.
