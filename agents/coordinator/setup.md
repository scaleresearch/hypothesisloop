# Coordinator: setup & debugging

Get from nothing to "agents running real jobs." Then hand off to `supervise.md`. Come back here
when supervising surfaces an environment/platform problem.

## Variables

Set by the caller (e.g. `repeatable/oversee-experiment-record-all-issues.md`) or default as
shown. Resolve against the live environment, never guess.

    EXPERIMENT=<name>                          # required — agents/coordinator/experiments/<name>/
    NUM_AGENTS=2
    CHIPS_PER_AGENT=2                          # NUM_AGENTS * CHIPS_PER_AGENT <= available capacity
    CLAUDE_CODE_OAUTH_TOKEN=<auto-loaded from agents/coordinator/.env, see step 1>
    KUBE_CONTEXT=k3s-tt
    NODE_NAME=box-a
    CHIP_ARCH=tenstorrent.com/chipArch=blackhole
    API_URL=http://localhost:8081              # the whole API: platform experiments, jobs,
                                               # hypotheses, metrics, resource catalog. Export
                                               # it: `hl` reads it from the environment and
                                               # exits with an error when it is unset
    HL_TOKEN=<org API key, hlk_...>            # `hl api-key create coordinator`, run once by a
                                               # member of the org. Keep it in
                                               # agents/coordinator/.env (gitignored) and load it
                                               # with step 0: every request carries it and `hl`
                                               # exits with an error when it is unset. Never put
                                               # it on a command line. A staff token is for
                                               # operating the platform itself, not for a run
    HL_PROJECT=default                         # the project the platform experiment is created in
    METRICS_URL=http://localhost:8084
    STAGES=<[]domain.Stage or unset>           # unset = stages.default in the control plane config
    FINDINGS_FILE=fix-later.md
    PLATFORM_EXPERIMENT_ID=<set in step 2>

You never act on agents' behalf (no SSH into their jobs, no hand-editing their branch/metrics) —
capacity, environment, unblocking is your job; the research is theirs. Platform code changes
follow `important.md`. If a step below can't be done as written, stop and say why — don't
silently improvise.

## 1. Prepare the environment (all six, every time — don't assume from a prior session)

0. **Tokens.** `set -a; source agents/coordinator/.env 2>/dev/null; set +a` — loads `HL_TOKEN`
   (the org API key) and the persistent `CLAUDE_CODE_OAUTH_TOKEN` (gitignored) if present. Missing file → step 4 falls back
   to mounted dotfiles.
1. **Capacity.** `lib_attach_node $KUBE_CONTEXT $NODE_NAME` (`localdev/lib/node.sh`, not manual
   kubectl). Confirm `GET $API_URL/resource-catalog/capacity` shows `$CHIP_ARCH` with >=
   `NUM_AGENTS*CHIPS_PER_AGENT` devices. `lib_detach_node` again once done.
   **Note on board topology:** some Tenstorrent boards physically pair multiple ASICs on one card
   (e.g. P300 = 2 ASICs per physical board). Exposing a single `/dev/tenstorrent/N` node or
   setting a single-chip env var (e.g. `TT_METAL_VISIBLE_DEVICES`) does not give true isolation —
   the runtime still detects the sibling ASIC and may reject the topology (e.g. "Board has 1
   chips, but expected 2 chips for board type p300") or fall back to a custom cluster type
   requiring an explicit mesh-graph descriptor. When planning per-chip capacity/allocation, treat
   paired-ASIC boards as a single allocatable unit unless a validated mesh-graph descriptor for
   the split configuration exists.
2. **Control plane + agents up**: `make controlplane-up` (podman compose, see
   `localdev/controlplane/docker-compose.yml`), cluster-agent DaemonSet/Deployment on
   `$KUBE_CONTEXT`.
   **A cluster runs the jobs of the org that registered it, and no other.** The platform
   experiment's project decides the org of every job in it, so the cluster must be registered
   for that org, or be shared by staff. `hl cluster list` shows the clusters the org's jobs run
   on and whether each is connected; the capacity check in step 1 lists only clusters the token's jobs can be
   placed on, so a cluster missing there is unregistered, another org's, or disconnected. A job
   with nowhere to go stays QUEUED with `not_admitted_reason: no_cluster_for_org`. To add a
   cluster, `hl cluster add <name>` registers the name and prints the `make cluster-agent-up`
   command with the platform's addresses filled in; run it with `kubectl` pointed at the cluster (it reads the runtime token with `hl cluster token` when it runs). `hl cluster remove <name>` ends a cluster and requeues its jobs. The local stack registers its own clusters for the
   org `default`.
   **After any host reboot, check these too — none of them auto-restart:** the Tenstorrent DRA
   driver pod (kubelet sometimes fails to re-register it; `kubectl delete pod -n
   tt-operator-system -l app.kubernetes.io/name=tt-dra-driver` forces a clean re-register — job
   pods fail with `CDI device injection failed` until this is done), and any
   bare-metal node's `bare-agent` process (no systemd unit — restart its `start.sh` by hand; if it
   used an ephemeral cloudflare tunnel URL, prefer a plain LAN URL instead since that survives
   reboots without needing a new tunnel).
3. **Code repository**: a GitHub repository attached to the project `$HL_PROJECT`, holding
   `$EXPERIMENT`'s own `seed/` (never `tests/workloads/`). A missing `seed/` isn't automatically
   a bug — some experiments point at a sibling's `seed/` in prose; check `experiment.md` first.
   Attach it once per project: in the UI under Settings, or with `POST /projects/{id}/repo`,
   after installing the platform's GitHub App on the repository. An operator with the database
   can record it with `hl-admin repo attach PROJECT INSTALLATION REPOSITORY_ID`.
   `GET /projects/{id}/repo` shows what is attached. No address and no git token is handed to
   anyone: an agent reads the address from its platform experiment (`code_repo_url` in
   `hl platform-experiments get`) and does not start while the project has no repository.
   **The attached repository is a separate repo, and no live mirror of this one** — after editing
   `seed/`, push it to the repository's default branch too, and diff-check before spawning
   agents. The experiment base lives there as `.hypothesisloop/job.yaml`, the job spec agents
   submit, and optionally `.hypothesisloop/experiment.yaml`, the create body step 2 can read.
   Agents can push to the repository, so protecting its default branch is the repository
   owner's job, in the repository's own settings on GitHub.
   Agents and jobs clone through `hl git-credential`, git's credential helper: it asks the
   platform for a token minted for that call, limited to the one repository and good for about
   an hour. The agent image and the platform's job image are configured for it. A job that runs
   an image of its own needs `hl` in it and these two lines, or its clone of a private
   repository is refused:

       git config --system credential.https://github.com.helper \
         '!API_URL="$HYPOTHESISLOOP_API_URL" HL_TOKEN="$HYPOTHESISLOOP_TOKEN" hl git-credential'
       git config --system credential.https://github.com.useHttpPath true

   Confirm the clone from *inside a real job pod*, not just from the coordinator container.
   **`data_store.endpoint` in `controlplane/settings/hypothesisloop.yaml` must use the host
   address in `localdev/.host-address` (written by `make render-settings`:
   `host.containers.internal` on macOS, the LAN IP on a Linux host), never `127.0.0.1`.** The
   coordinator/agent containers run `--network host` so `127.0.0.1` resolves there, but a job
   pod (k3s) is in a separate network namespace and can't reach the host's loopback. Job pods
   reach the object store directly to write checkpoints (the platform
   is not in the data path) — the shipped value is the
   placeholder `http://REPLACE-WITH-HOST-ADDRESS:9000`, and loading a config whose endpoint is
   loopback is refused outright rather than failing later inside a job. `make controlplane-up`
   publishes the S3 store (Silo) on `:9000` and creates the bucket; the host address is reachable from the
   control-plane containers as well, so one value serves both.
4. **Job image** built before agents start (e.g. `make sparse-sdpa-workload-image`), smoke-tested
   on real hardware (`podman run --device /dev/tenstorrent/0 -v
   /dev/hugepages-1G:/dev/hugepages-1G ...`) — a missing hugepages mount fails with
   `RuntimeError: Querying size for a host channel that does not exist.`, which looks like a
   broken image but isn't.
   **On a k8s cluster ($KUBE_CONTEXT), a podman-level smoke test alone doesn't prove a real job
   pod can run it.** Job images are pushed to the local registry
   (`localdev/controlplane/docker-compose.yml`'s `registry` service) and pulled normally
   (`imagePullPolicy: IfNotPresent`) — a build that never made it past `podman build` (no
   `podman push`) is invisible to every cluster node the same way a typo'd tag is. Build with
   the per-experiment `seed/build_and_push.sh` where one exists (it takes the registry from `localdev/.host-address`, pushes and renders the
   job.yaml's placeholder image ref into `job.rendered.yaml`; push that to the attached repository with
   the rest of `seed/`, and run it again whenever the host address changes), or `make <name>-workload-image` for the platform's
   own images. Verify with `podman pull --tls-verify=false <host address>:5000/<image>:<tag>` (the address in
   `localdev/.host-address`) from any cluster node before spawning any agents.

## 2. Create the platform experiment

`agents/coordinator/experiments/$EXPERIMENT/experiment.yaml` is the checked-in template for this
(copied from `template/experiment.yaml` when the experiment was defined — see
`experiment-checklist.md` item 1). Fill in its `REPLACE-WITH-...` placeholders
(`budget_accelerator_hours`, `max_agents`, `starts_at` — these depend on `NUM_AGENTS`/
`CHIPS_PER_AGENT`/`planned_run_hours`, decided now, not ahead of time) and confirm its
`description` is still `experiment.md`'s current `EXPERIMENT DESCRIPTION` block verbatim (the only
thing agents read about the objective — system prompt is experiment-agnostic), then:
`hl platform-experiments create agents/coordinator/experiments/$EXPERIMENT/experiment.yaml` (see
`controlplane/settings/examples/experiment.yaml` for the full DSL reference).

Required fields (`GET $API_URL/openapi.json` is the source of truth): `name`, `description`,
`budget_accelerator_hours`, `max_agents`, `metrics`, `report_interval_seconds`, `starts_at`,
`ends_at` (RFC3339).

With `from_repo: true` in the file, every field it leaves out is read from
`.hypothesisloop/experiment.yaml` at the head of the default branch of the project's repository.
`budget_accelerator_hours`, `max_agents` and `ends_at` are always stated in the request and
never read from the repository.

The platform can run the whole experiment. In the request, `hosted_agents: N` joins N agents the
platform runs (as `agents add --hosted` in section 4 does) and `coordinator_hosted: true` has it
run the coordinator: one more machine beside the agents', started from the same image with the
platform experiment's coordinator token and briefed with
`agents/experimentator/src/hypothesisloop_agent/prompts/coordinator.md`, which is `supervise.md`
for a coordinator that has `hl`, the repository and no host. It is no agent of the experiment,
holds no quota and is not ranked. `coordinator_model` names its model; omitted, the platform's
default applies. Both keys need the project's repository (`no_repository` without one) and are
read from the request only. The experiment and its hosted agents are created together or not at all. With them, creating the
experiment and `hl platform-experiments start` is all there is to do: the coordinator keeps the
description true to what is learned and writes its findings as the experiment's summary
(`hl platform-experiments summary`), which `hl platform-experiments get` shows as `summary`
beside `coordinator`: its launcher, its model and `instance`, which reads as an agent's does
(section 4). Its token still writes the summary after the close. It reads the repository and
pushes nothing. It closes the experiment only when the run can produce nothing more, and says
why in the summary first. It cannot start the experiment, run a baseline itself, act as an
agent, or change anything of the experiment but the description (the budget, `max_agents`, the
schedule and the stages answer 403 to its token); it writes those down for you.

- `budget_accelerator_hours = NUM_AGENTS * CHIPS_PER_AGENT * planned_run_hours` — not a round
  number, or quota throttles the fleet mid-run. Count every agent you will launch here.
- `max_agents` is the size of the ranked field — every agent that signs up competes and counts
  against it, there is no separate role that doesn't.
- `metrics`: `{key, direction, role?}[]` — `"role":"ranking"` on the metric `experiment.md` calls
  RANKING METRIC, no role on the rest.
- Every reported metric value has an implicit `metric_basis`, default `"raw"`. If a job's value
  is on a different scale/definition than every other run's (e.g. denormalized against a
  different reference), the job must set a non-`"raw"` `metric_basis` on that value — never
  leave it as `"raw"` and rely on someone noticing the number looks off. A wrongly-"raw" value is
  how a rescaled metric quietly wins a ranking it actually lost.
- `report_interval_seconds` = the cadence the harness actually reports at.
- `stages` (optional; omit = the control plane config's `stages.default`, 40%/75%-cut then 60%/0%): `length_pct` sums
  to 100, last stage's `evict_pct` = 0, `max_job_hours` caps a stage's job length (0/absent =
  unlimited). Working short-jobs-first example: `[{20,50,0.25},{30,50,1},{50,0}]` as
  `length_pct,evict_pct,max_job_hours`. If `experiment.md` describes a phased method (e.g.
  screen-then-confirm), it should also carry a coordinator note with the matching `stages` values
  to actually enforce that phasing — check for one before defaulting or inventing your own split.
- Record the returned `id` as `PLATFORM_EXPERIMENT_ID`.
- If the experiment's `hypothesis.yaml` is a seed tree (`author:` set, entries building on `root`
  or on each other's `key`), submit it before spawning: `hl hypothesis submit --platform-experiment
  $PLATFORM_EXPERIMENT_ID --file agents/coordinator/experiments/$EXPERIMENT/hypothesis.yaml`. A
  human-authored seed needs `hypothesis_submit_policy: mixed`.
- **Description sync is not optional, ever.** Agents read the live platform experiment's
  `description` field — not `experiment.md`, not `FINAL_RESULT.md`. Any edit to either of those
  files (a resolved question, a new redirect, a stage/direction change) is incomplete until it is
  mirrored: update `agents/coordinator/experiments/$EXPERIMENT/experiment.yaml`'s `description` to
  the refreshed `EXPERIMENT DESCRIPTION` block verbatim, then `hl platform-experiments update
  $PLATFORM_EXPERIMENT_ID agents/coordinator/experiments/$EXPERIMENT/experiment.yaml`, immediately,
  same turn as the file edit — never batched for later. (While the platform experiment is
  `running`, only `name`/`description` may change; any other field that differs from the live
  value is rejected and the error names it, so keep the file's other fields as created, or set
  the named ones to the live values.) Then `hl
  platform-experiments list` or `GET $API_URL/platform-experiments/{id}` and diff its
  `description` against the file's block before moving on; treat any mismatch as a blocker, not a
  note-for-later. This has
  caused wasted-retry-loop incidents twice already (agents burning device time re-deriving a
  question the coordinator had already resolved locally, because the live description hadn't
  caught up) — a posted comment redirecting the agent is not a substitute for this and does not
  reliably stop an in-flight retry loop by itself.
- **The `BASELINE` block must be present and answered before any agent is spawned.** It lives
  inside the `EXPERIMENT DESCRIPTION` block you just posted, so it reaches agents verbatim through
  the sync rule above — see `experiment-checklist.md` item 1 for the four required lines. Check
  the posted `description`, not just the file. Either `metric:` is a real number with an
  experiment id on `measured:`, or `measured:` says `not yet established` **and** the description
  names establishing it as the first task. A block that is absent, or present with all four lines
  blank, is a blocker — fix `experiment.md`, re-`PUT`, and only then spawn. An agent that reads no
  baseline invents one, and every result it ranks is measured against a different reference than
  its neighbour's.
- `starts_at` must leave time to add every agent (step 4), never "now". Agents join only while
  the status is `open`, and it flips to `running` within a minute of `starts_at` passing once at
  least one agent has joined (`SweepAutoStart` in
  `controlplane/services/quota/platform_experiments_lifecycle.go`). Quota is divided at that
  moment over the agents present, and one added later is refused `signup_closed`. How long an
  agent's container takes to boot no longer matters: an agent is in the experiment from the
  moment it is added, whenever its harness starts. Add the whole roster in one go, or keep
  `starts_at` a few minutes out until it is complete.

## 3. Decide the flavor mix and the sweep

Every agent that signs up competes — ranked, cut at stage boundaries, eligible for the top-3
bonus. There is no other role, and the platform never branches on one: every agent's jobs are
admitted, billed, evicted and settled by identical code.

What differs between agents is two independent things, both decided here and set when the agents
are added (step 4):

- `AGENT_FLAVOR` picks which specialization the harness inlines into the agent's prompt from
  `agents/experimentator/src/hypothesisloop_agent/prompts/flavors/<flavor>.md` — what kind of
  trial it runs and what it varies (`generalist`, `hyperparameter-search`, `architecture-search`,
  `data-search`; a new specialization is a new file there and its name in `AgentFlavors` in
  `controlplane/shared/domain/agent_signup.go`, which is the set the platform accepts). Decide the flavor mix before spawning
  and write the reason into the run's notes, since it's what the results mean.
- **The mix is a per-stage decision, not a one-time one.** Pick it deliberately for where the
  research actually is, not once at launch and then forgotten:
  - **Early / no promising region yet** (before the first stage cut, or whenever no branch of the
    hypothesis graph has a finding worth converging on): bias toward breadth — `generalist`
    agents plus one of each targeted flavor, so the fleet explores hyperparameters, architecture,
    and data at once instead of guessing which axis matters before any evidence exists. E.g. for
    4 agents: 1 generalist + 1 hyperparameter-search + 1 architecture-search + 1 data-search.
  - **Once a promising region is found** (a confirmed branch, or a clear leader the coordinator
    can name with a hypothesis_id) — narrow: spawn the *next* wave's agents weighted toward the
    axis that region's own finding says moved the metric, and drop or reduce `generalist` slots.
    If the leading result was a data change, the next wave is mostly `data-search` (plus a
    `hyperparameter-search` or two to harden it), not another blind spread.
  - Re-decide at every stage boundary (`GET .../stages` — see supervise.md), not just once:
    a stage cut is a checkpoint to read which branches of the graph are confirmed or refuted and
    adjust the *next* wave's flavor mix accordingly, the same way it decides who gets cut.
  - You cannot change a running agent's flavor (it's read once at launch) — narrowing means new
    agents spawned with a different mix, or letting a cut thin out the earlier, broader wave
    rather than retroactively editing it. Existing agents keep competing under their original
    flavor until they finish or are cut.
  - Record every mix decision (initial and any narrowing) in the run's notes with the reasoning —
    what evidence prompted it, which hypothesis or finding motivated a narrower slate — since
    that reasoning is what makes the results interpretable afterward, exactly like the flavor mix
    itself.
- `AGENT_HYPERPARAMETERS` is a small JSON object handed to an agent on top of its flavor (batch
  size, a specific point in a search space, ...). A sweep is several agents of the same flavor
  launched with different hyperparameter values, same as XManager's `experiment.add` loop.

If you want a single agent to just replicate the `BASELINE` block's configuration verbatim, launch
it as `generalist` with that config as its hyperparameters (or none at all, and let it read
`BASELINE` from the description) — it competes and is ranked like everyone else.

## 4. Spawn agents

`NUM_AGENTS = floor(available_devices / CHIPS_PER_AGENT)`. An agent joins a platform experiment
in exactly one way: `hl platform-experiments agents add` (`POST /platform-experiments/{id}/agents`),
while the experiment is open. It creates the agents with ids of the platform's choosing and
records each one's flavor (`--flavor`, one of `generalist`, `hyperparameter-search`,
`architecture-search`, `data-search`; omit it and the agent runs `generalist`), hyperparameters
(`--hyperparameters`, a JSON object of at most 4 KiB; omit it and the agent gets `{}`) and model
(`--model`; omit it and the platform's default applies). What the command prints is what each
agent runs: flavor and model are resolved at that moment and never again. One call per group
that shares those values:

    hl platform-experiments agents add $PLATFORM_EXPERIMENT_ID --count 2 --flavor generalist
    hl platform-experiments agents add $PLATFORM_EXPERIMENT_ID --flavor hyperparameter-search \
      --hyperparameters '{"batch_size":64}'

Add `--hosted` and the platform runs them: its launcher starts one machine per agent from the
platform's own agent image, tells the agent to take its last turn when the experiment closes or
the agent is cut, and takes the machine down two minutes after. It is refused (`no_repository`)
while the project has no repository attached, since a hosted agent works in the project's
repository, and (`max_hosted_reached`) beyond the number of machines the platform runs for one
platform experiment, which `hl platform-experiments get` shows as `max_hosted` and which counts
hosted agents of every role and a hosted coordinator. A machine whose harness exits, that the
provider takes away, or that does not come up in time is started again up to three times; after
that the agent is stopped, and `hl platform-experiments agents list` shows it with
`instance.stopped`, `instance.exited_at` and `instance.exit_log`, the end of what the harness
wrote or why the platform gave up on the machine. A stopped agent keeps its place and its quota
and takes no further turn until the platform's operators change the agent image or the
launcher's settings, which starts it again. There is nothing more to do for a hosted agent than
to read that list.

Without `--hosted` you run each agent yourself, which is how an experiment with its own agent
image runs (`make experimentator-image EXPERIMENT=$EXPERIMENT`). Every line the command prints is
one agent with its `agent_id` and its `token`, which acts as that agent in that platform
experiment and nowhere else. The harness starts only with both, and with the flavor and the model
the same line shows (`AGENT_FLAVOR`, `AGENT_MODEL`): it registers nothing, defaults nothing, and
exits if the platform does not know the agent. Write them to one env file per agent, readable by you
alone, instead of putting a token on a command line, where it would sit in the process list and
in the shell's history:

    umask 077
    hl platform-experiments agents add $PLATFORM_EXPERIMENT_ID --flavor hyperparameter-search \
      --hyperparameters '{"batch_size":64}' |
      jq -r '"AGENT_ID=\(.agent_id)\nHL_TOKEN=\(.token)\nAGENT_FLAVOR=\(.flavor)\nAGENT_MODEL=\(.model)\nAGENT_HYPERPARAMETERS=\(.hyperparameters | tojson)"' \
      > /tmp/hlrun/<run>/agent-1.env

    podman run -d --name agent-1 --network host --env-file /tmp/hlrun/<run>/agent-1.env \
      -e PLATFORM_EXPERIMENT_ID=$PLATFORM_EXPERIMENT_ID \
      -e API_URL=$API_URL -e CLAUDE_CODE_OAUTH_TOKEN \
      localhost/hypothesisloop-experimentator-$EXPERIMENT

A secret is named with a bare `-e NAME`, which hands the container the value from this shell's
environment. `hl` in the container reads `AGENT_ID` and `PLATFORM_EXPERIMENT_ID` as its `--agent`
and `--platform-experiment` defaults. A lost token is printed again by
`hl platform-experiments agents add $PLATFORM_EXPERIMENT_ID --agent <agent_id>`, which changes
nothing for an agent that already joined. `hl platform-experiments agents remove` takes an agent
out again while the experiment is still open.

Default: the token from `agents/coordinator/.env` (loaded in step 0). Host credential files
mount with `--userns=keep-id -v ~/.claude/.credentials.json:/home/agent/.claude/.credentials.json:ro
-v ~/.claude.json:/home/agent/.claude.json:ro -v ~/.codex/auth.json:/home/agent/.codex/auth.json:ro`,
on rootless podman only: a rootful podman machine (macOS) rejects `--userns=keep-id` with
`crun: mount sysfs to sys: Operation not permitted`. The SDK prefers the token when both are set.
Never use `--bare`; it ignores the token.

Each agent prints its activity log to stdout: one line per action (UTC time, agent id, kind,
summary), where the kind is `say` (its own text), `tool` (a tool call, with the Bash command),
`error` (the first line of a failed tool result) or `event` (a platform update the harness handed
the agent). The agent does not run `hl watch` itself: whenever it ends its turn, the harness runs
it for the agent's own resources and starts the next turn with the batch it returns, and nudges the
agent after 30 minutes without one. The cursor lives in `.hl-state/` in the agent's workdir, so a
restarted agent resumes where it stopped. Harness logs (turn results, rate limits,
reconnects) go to stderr and the full transcript stays in the Claude Code session jsonl under
`~/.claude/projects/` inside the container. For a per-agent activity file, run the container
attached in the background instead of `-d` and redirect its streams:

    podman run --name agent-1 ... localhost/hypothesisloop-experimentator-$EXPERIMENT \
      > /tmp/hlrun/<run>/agent-1.activity.log 2> /tmp/hlrun/<run>/agent-1.err &

One private repository per project; each agent works on branch
`hl/$PLATFORM_EXPERIMENT_ID/<agent_id>` (platform experiment id included so a fresh experiment always
starts from a fresh branch off the current default branch — see the agent's own system prompt for why) so
`code_ref` (`<clone address>@<sha>`) always resolves to a real commit. An agent's credential
can push, a job's and a coordinator's can only read, and none is handed out once the platform
experiment is closed or the agent cut.

Confirm the roster before handing off: `hl platform-experiments agents list
$PLATFORM_EXPERIMENT_ID` shows every agent with what it was launched with, and a self-run agent
whose container is gone from `podman ps` has exited: read its `.err`.

Hand off to `supervise.md` once agents are running.

## Fixing a blocker (called from supervise.md)

Diagnose from the actual failure (logs/job status/API response), not the symptom's shape.

- Platform bug → fix in code, per `important.md`
- Experiment-definition gap → fix `seed/`/`job.yaml`/`experiment.md` directly, check against
  `experiment-checklist.md`.
- Environment/capacity → redo the relevant step-1 check, don't work around it per-agent.
