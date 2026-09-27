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
   placed on, so a cluster missing there is unregistered, another org's, disconnected, or turned off for the org
   (`enabled: false` in `hl cluster list`; `hl cluster enable <name>` turns it on). A job
   with nowhere to go stays QUEUED with `not_admitted_reason: no_cluster_for_org`, or `clusters_disabled` when the org turned off every cluster it could use. To add a
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
3. **Code repository**: a repository on the deployment's git host that the platform experiment
   names. Its default branch starts as a copy of three things from
   `agents/coordinator/experiments/$EXPERIMENT/`: `README.md`, `.hypothesisloop/` and `seed/`,
   with any other source directory its `README.md` lists. An experiment whose `README.md` says
   it runs on another experiment's repository has none of its own: it names that repository.
   The org connects GitHub once: an owner installs the platform's GitHub App on the GitHub
   account and connects that installation in the UI under Settings
   (`POST /orgs/{id}/github-installations`). An operator with the database can connect it with
   `hl-admin github connect ORG INSTALLATION_ID`. `GET /orgs/{id}/github-repositories` lists what
   an experiment of the org may name, and the experiment names one as `repository` (owner/name)
   when it is created (step 2). No address and no git token is handed to anyone: an agent reads
   the address from its platform experiment (`repository_url` in `hl platform-experiments get`).
   On a deployment whose git host is not GitHub nothing is connected: the repository is created
   on the host beforehand and named as `repository` (locally, `make git-repository NAME=...`).
   A job's `code_ref` is that repository's address at a commit, and `hl-clone` checks it out.
   **That repository is a separate repo, and no live mirror of this one** — after editing any
   of those files here, push them to the repository's default branch too, and diff-check before
   spawning agents. `.hypothesisloop/job.yaml` is the one job spec agents submit, and
   `.hypothesisloop/experiment.yaml` states the name, the description and the metrics step 2
   creates the experiment with. `hl repo init` in a clone adds the files it lacks and overwrites
   none. The platform checks no repository at a direct create, so before agents start run
   `hl repo check` in a clone at the baseline commit and fix what it names
   (`agents/coordinator/experiments/template/experiment-checklist.md`, "A ready repository"). It
   checks the layout and that each file is valid, and never that the files describe this
   experiment, so read them against the brief. A job spec here whose `image` is still a
   `REPLACE-WITH` names an address only a deployment knows: fill it in in the clone as step 4
   says, commit and push.
   Agents can push to the repository, so protecting its default branch is the repository
   owner's job, in the repository's own settings on GitHub.
   Agents and jobs clone through `hl git-credential`, git's credential helper: it asks the
   platform for a credential for that one repository, on GitHub a token minted for that call
   and good for about an hour. The agent image and the job images `hl capacity` lists are configured for it. A job that runs
   an image of its own needs `hl` in it and these two lines, or its clone of a private
   repository is refused:

       git config --system credential.helper \
         '!API_URL="$HYPOTHESISLOOP_API_URL" HL_TOKEN="$HYPOTHESISLOOP_TOKEN" hl git-credential'
       git config --system credential.useHttpPath true

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
4. **Job image.** Every job names its image, on CPU, GPU and chip clusters alike, and
   `job.image` is one of two things: the name of a job image exactly as `hl capacity` shows it,
   or the full registry address of an image of the experiment's own, host first
   (`docker.io/library/python:3.13-slim`). Anything else is refused at submit with 422
   `unknown_job_image`. `hl capacity` ends with the job images the deployment offers (`job_image` the
   name, `carries` what is installed), names on each type's line the ones that suit it
   (`job_images`), and marks a cluster that runs one image and no other with `only_job_image`:
   a job meant for that cluster names that image, and one that names another is never placed
   there. A job that no cluster could run with the image it names, or with none, is refused at
   submit with 422 `no_cluster_for_image`. In each of the platform's job
   images a job installs with `uv pip install PACKAGE` or `pip install PACKAGE`, into the Python
   its command runs. `uv run --with PACKAGE script.py` runs in a fresh environment, which suits the
   `python` image and hides the framework a framework image carries. `make images` builds and
   pushes the offered image with no framework, `make job-images` the others. An experiment
   whose job spec runs in an offered image states its name (`dummy-data-search-demo` states
   `image: python`).
   An experiment whose seed builds an image of its own names that one, by its full
   registry address, so it needs a cluster without `only_job_image` that can pull it. Build it before agents start (e.g.
   `make sparse-sdpa-workload-image`) and smoke-test it
   on real hardware (`podman run --device /dev/tenstorrent/0 -v
   /dev/hugepages-1G:/dev/hugepages-1G ...`) — a missing hugepages mount fails with
   `RuntimeError: Querying size for a host channel that does not exist.`, which looks like a
   broken image but isn't.
   **On a k8s cluster ($KUBE_CONTEXT), a podman-level smoke test alone doesn't prove a real job
   pod can run it.** Job images are pushed to the local registry
   (`localdev/controlplane/docker-compose.yml`'s `registry` service) and pulled normally
   (`imagePullPolicy: IfNotPresent`) — a build that never made it past `podman build` (no
   `podman push`) is invisible to every cluster node the same way a typo'd tag is. Build with
   the per-experiment `seed/build_and_push.sh` where one exists, run in the clone of the
   experiment's repository with `REGISTRY` set to the host address in `localdev/.host-address`
   plus `:5000`: it pushes the image and writes its address into `.hypothesisloop/job.yaml`.
   Commit and push that file, and run the script again whenever the host address changes. Use
   `make sparse-sdpa-workload-image` for sparse-sdpa. Verify with `podman pull --tls-verify=false <host address>:5000/<image>:<tag>` (the address in
   `localdev/.host-address`) from any cluster node before spawning any agents.

## 2. Create the platform experiment

The create body is two files laid end to end.
`agents/coordinator/experiments/$EXPERIMENT/.hypothesisloop/experiment.yaml` states what the
experiment is: `name`, `description`, `metrics`. Confirm its `description` is still
`experiment.md`'s current `EXPERIMENT DESCRIPTION` block verbatim (the only thing agents read
about the objective — system prompt is experiment-agnostic). A copy of
`agents/coordinator/experiments/template/run.yaml`, kept outside the tree, states this run:
`repository`, `budget_accelerator_hours`, `max_agents`, `duration_hours`, `coordinator_hosted`. They
depend on `NUM_AGENTS`/`CHIPS_PER_AGENT`/`planned_run_hours`, decided now, not ahead of time.
Fill in every `REPLACE-WITH`, then:

    cat agents/coordinator/experiments/$EXPERIMENT/.hypothesisloop/experiment.yaml /tmp/hlrun/<run>/run.yaml |
      hl platform-experiments create -

`controlplane/settings/examples/experiment.yaml` is the full DSL reference.

Required of a direct create (`$API_URL/explore/coordinator` reads the API out in plain text and
`GET $API_URL/openapi.json` is the source of truth): `name`,
`description`, `metrics`, `budget_accelerator_hours`, a positive `max_agents`, and
`duration_hours`, how long the run lasts from the moment it starts. No request states
`ends_at`: the platform answers it once the run started. A run lasts 30 days at most
(`quota.max_platform_experiment_days`), and one stated longer is refused `run_too_long`.

On GitHub a request with `from_repo: true` reads every field it leaves out from
`.hypothesisloop/experiment.yaml` at the head of the default branch of the repository it names.
`budget_accelerator_hours`, `max_agents` and `duration_hours` are always stated in
the request and never read from the repository. A deployment on a plain git host refuses
`from_repo` (`experiment_file_unsupported`), so the command above states everything and works
on both.

Creating makes the experiment, `open`, and no agent. From there every experiment goes the same
way, whoever does each part: the researchers are added (section 4), a baseline agent measures
the control, its job is declared as the baseline, and the platform starts the run within a
minute of the declaration. Nobody starts it by hand, and `duration_hours` counts from that
start (`started_at`). While it is `open`, `hl platform-experiments get` says in `status_reason`
what the start waits for: `baseline_required`, `starts_at_ahead`, `no_researchers`,
`budget_spent`, or `starting`.

The platform can run the whole experiment. Agents the platform runs join with
`agents add --hosted` (section 4), one call for each, and in the request `coordinator_hosted: true`
has it run the coordinator: one more machine beside the agents', started from the same image with the
platform experiment's coordinator token and briefed with
`agents/experimentator/src/hypothesisloop_agent/prompts/coordinator.md`, which is `supervise.md`
for a coordinator that has `hl`, the repository and no host. It is no agent of the experiment,
holds no quota and is not ranked. `coordinator_model` names its model; omitted, the platform's
default applies. Both keys are read from the request only. With a hosted coordinator and hosted
agents, creating the experiment and adding the researchers is all there is to do: the
coordinator adds the baseline agent, declares its job, keeps the
description true to what is learned and writes its findings as the experiment's summary
(`hl platform-experiments summary`), which `hl platform-experiments get` shows as `summary`
beside `coordinator`: its launcher, its model and `instance`, which reads as an agent's does
(section 4). `hl platform-experiments agents session coordinator` writes out what it said and
saw. Its token writes the summary until two minutes after the close and then stops working. It reads the repository and
pushes nothing. It closes the experiment only when the run can produce nothing more, and says
why in the summary first. It adds agents and removes only the ones it added. It cannot run a
baseline itself, act as an agent, or change anything of the experiment but the description (the
budget, `max_agents`, the schedule and the stages answer 403 to its token); it writes those
down for you.

Without `coordinator_hosted` you are the coordinator, with your own token: you add the baseline
agent and declare its job, as the baseline item below says, and supervise with `supervise.md`.

- `budget_accelerator_hours = NUM_AGENTS * CHIPS_PER_AGENT * planned_run_hours` — not a round
  number, or quota throttles the fleet mid-run. Count every agent you will launch here.
- `max_agents` is how many researchers (agents of role `competitor`, whatever their flavor) may
  join over the whole run. One that finished or was cut still counts. Agents of role
  `baseline`, reviewers, the planner (role `planner`) and the coordinator never count. A PATCH
  below the researchers that joined is refused (409 `max_agents_below_researchers`).
- `metrics`: `{key, direction, role?}[]` — `"role":"ranking"` on the metric `experiment.md` calls
  RANKING METRIC, no role on the rest.
- Every reported metric value has an implicit `metric_basis`, default `"raw"`. If a job's value
  is on a different scale/definition than every other run's (e.g. denormalized against a
  different reference), the job must set a non-`"raw"` `metric_basis` on that value — never
  leave it as `"raw"` and rely on someone noticing the number looks off. A wrongly-"raw" value is
  how a rescaled metric quietly wins a ranking it actually lost.
- A job is read by one value per metric: a ranking metric counts at its best value in its
  direction across what the job reported, a constraint and an attribute at their last. So the
  harness reports a ranking metric as a result, never as a running timer.
- `report_interval_seconds` = the cadence the harness actually reports at.
- `stages` (optional; omit = the control plane config's `stages.default`, 40%/75%-cut then 60%/0%): `length_pct` sums
  to 100, last stage's `evict_pct` = 0, `max_job_hours` caps a stage's job length (0/absent =
  unlimited). Working short-jobs-first example: `[{20,50,0.25},{30,50,1},{50,0}]` as
  `length_pct,evict_pct,max_job_hours`. If `experiment.md` describes a phased method (e.g.
  screen-then-confirm), it should also carry a coordinator note with the matching `stages` values
  to actually enforce that phasing — check for one before defaulting or inventing your own split.
- Record the returned `id` as `PLATFORM_EXPERIMENT_ID`.
- If the experiment holds a `hypothesis.yaml`, it is a seed tree (no `agent_id:`, entries building
  on `root` or on each other's `key`): submit it before spawning: `hl hypothesis submit --platform-experiment
  $PLATFORM_EXPERIMENT_ID --file agents/coordinator/experiments/$EXPERIMENT/hypothesis.yaml`. Submit it
  with your own token and no `--agent`: the hypotheses are then no agent's, recorded as made by
  the coordinator, and any agent may build on, test and settle them. A file that states
  `author:` is refused.
- **Description sync is not optional, ever.** Agents read the live platform experiment's
  `description` field — not `experiment.md`, not `FINAL_RESULT.md`. Any edit to either of those
  files (a resolved question, a new redirect, a stage/direction change) is incomplete until it is
  mirrored: update `agents/coordinator/experiments/$EXPERIMENT/.hypothesisloop/experiment.yaml`'s
  `description` to the refreshed `EXPERIMENT DESCRIPTION` block verbatim, then send that block alone with `hl platform-experiments update
  --platform-experiment $PLATFORM_EXPERIMENT_ID --description-file FILE`, immediately,
  same turn as the file edit — never batched for later. (The update takes only the fields that
  change, and once the platform experiment is `running` only `name`/`description` do, so the
  whole experiment file is refused there.) Then `hl
  platform-experiments list` or `GET $API_URL/platform-experiments/{id}` and diff its
  `description` against the file's block before moving on; treat any mismatch as a blocker, not a
  note-for-later. This has
  caused wasted-retry-loop incidents twice already (agents burning device time re-deriving a
  question the coordinator had already resolved locally, because the live description hadn't
  caught up) — a posted comment redirecting the agent is not a substitute for this and does not
  reliably stop an in-flight retry loop by itself.
- **The baseline is measured and declared before the experiment starts, and the platform holds
  you to it.** The `BASELINE` block lives inside the `EXPERIMENT DESCRIPTION` block you just
  posted (see item 1 of
  `agents/coordinator/experiments/template/experiment-checklist.md` for its four
  lines) and says
  `measured: not yet established` at the create. While the experiment is `open`:
  1. add one agent with `hl platform-experiments agents add --role baseline` (`--hosted` to have
     the platform run it). It holds a quota at once, its jobs are the only ones an open
     experiment accepts, it does not count against `max_agents`, and it takes no share at the
     start;
  2. it runs the repository's job spec, files the job's summary and finishes;
  3. fill the block (`code_ref:`, `metric:`, `measured:` the job's id) and declare it:
     `hl platform-experiments update --description-file FILE --baseline-job JOB`. A refusal
     names what does not hold: the job, the brief, a gate the baseline breaks, or a budget that
     does not cover one such trial for each of `max_agents`.
  A hosted coordinator does steps 1 and 3 itself. Without one you do them. The platform starts
  the run within a minute of a valid declaration, and an experiment not running by its
  `baseline_due_at` (`planning.baseline_limit_minutes`, 90, after it was created or after its
  `starts_at`) is closed (`baseline_limit_reached`).
- Add every researcher (step 4) before the baseline is declared. Agents join only while the
  status is `open`. Quota is divided at the start over the researchers present, and one added
  later is refused `signup_closed`. How long an agent's container takes to boot does not
  matter: an agent is in the experiment from the moment it is added, whenever its harness
  starts.

## 3. Decide the flavor mix and the sweep

Every researcher competes: ranked, cut at stage boundaries, eligible for the top-3 bonus. The
baseline agent of step 2 is the one agent that does not. Every agent's jobs are admitted,
billed, evicted and settled by identical code.

What differs between agents is two independent things, both decided here and set when the agents
are added (step 4):

- The flavor (`--flavor`) picks which specialization the harness inlines into the agent's prompt from
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
- The hyperparameters (`--hyperparameters`) are a small JSON object an agent gets on top of its flavor (batch
  size, a specific point in a search space, ...). A sweep is several agents of the same flavor
  launched with different hyperparameter values, same as XManager's `experiment.add` loop.

If you want a single agent to just replicate the `BASELINE` block's configuration verbatim, launch
it as `generalist` with that config as its hyperparameters (or none at all, and let it read
`BASELINE` from the description) — it competes and is ranked like everyone else.

## 4. Spawn agents

`NUM_AGENTS = floor(available_devices / CHIPS_PER_AGENT)`. An agent joins a platform experiment
in exactly one way: `hl platform-experiments agents add` (`POST /platform-experiments/{id}/agents`),
while the experiment is open. Each call creates one agent with an id of the platform's choosing
and records its flavor (`--flavor`, one of `generalist`, `hyperparameter-search`,
`architecture-search`, `data-search`; omit it and the agent runs `generalist`), hyperparameters
(`--hyperparameters`, a JSON object of at most 4 KiB; omit it and the agent gets `{}`) and model
(`--model`; omit it and the platform's default applies). What the command prints is what the
agent runs: flavor and model are resolved at that moment and never again. One call per agent:

    hl platform-experiments agents add --platform-experiment $PLATFORM_EXPERIMENT_ID --flavor generalist
    hl platform-experiments agents add --platform-experiment $PLATFORM_EXPERIMENT_ID --flavor generalist
    hl platform-experiments agents add --platform-experiment $PLATFORM_EXPERIMENT_ID --flavor hyperparameter-search \
      --hyperparameters '{"batch_size":64}'

Add `--hosted` and the platform runs them: its launcher starts one machine per agent from the
platform's own agent image, tells the agent to take its last turn when the experiment closes or
the agent is cut, and takes the machine down two minutes after. An agent is named by its role and a number
(`Researcher 2`, `Baseline 1`) unless `--name` states one, and a name another agent of the
experiment carries is refused (409 `agent_name_taken`). `agents list` shows in `actor` whose
credential added each agent, and a coordinator's token removes only the ones it added (403
`agent_not_added_by_coordinator`). `--hosted` is refused
(`max_hosted_reached`) beyond the number of machines the platform runs for one
platform experiment, which `hl platform-experiments get` shows as `max_hosted` and which counts
hosted agents of every role and a hosted coordinator. A machine whose harness exits, that the
provider takes away, that does not come up in time, or whose harness stops reporting progress
while the machine still runs is started again up to three times. The new machine takes up the
agent's model session where the last finished turn left it: the harness stores the session with
the platform after each turn and reads it back before its first turn. Work that was not pushed
is not carried over, so an agent pushes its branch as it goes. After the third restart the agent
is stopped. `hl platform-experiments agents list` shows each hosted agent's `instance`:

    state        requested (asked for, not reported by the launcher yet), starting, running,
                 or stopped (no restart left)
    progress_at  when the platform last saw the harness's progress marker change
    seen_at      when the launcher first reported this machine
    restarts     how many times the machine was started again, 3 at most
    end_reason   why the last machine was given up on: exited, gone (the provider no longer
                 lists it), never_seen, still_starting (both past the boot limit), or silent
                 (running with no progress for the progress limit)
    exited_at, exit_log   when that was, and the end of what the harness wrote or the reason
                 there is nothing A stopped agent keeps its place and its quota
and takes no further turn until the platform's operators change the agent image or the
launcher's settings, which starts it again. There is nothing more to do for a hosted agent than
to read that list.

Without `--hosted` you run each agent yourself, which is how an experiment with its own agent
image runs (`make experimentator-image EXPERIMENT=$EXPERIMENT`). Every line the command prints is
one agent with its `agent_id` and its `token`, which acts as that agent in that platform
experiment and nowhere else. The harness starts with `API_URL` and that token as `HL_TOKEN`, and
nothing else. Run outside its image it also needs `AGENT_VENV`, the virtualenv the agent
installs into, which the image sets. The token says which agent of which platform experiment it is, and the harness reads
its flavor, model and hyperparameters from its signup. It registers nothing, defaults nothing, and
exits if the platform does not know the agent. Write the two to one env file per agent, readable by you
alone, instead of putting a token on a command line, where it would sit in the process list and
in the shell's history:

    umask 077
    hl platform-experiments agents add --platform-experiment $PLATFORM_EXPERIMENT_ID --flavor hyperparameter-search \
      --hyperparameters '{"batch_size":64}' |
      jq -r --arg api "$API_URL" '"API_URL=\($api)\nHL_TOKEN=\(.token)"' \
      > /tmp/hlrun/<run>/agent-1.env

    podman run -d --name agent-1 --network host --env-file /tmp/hlrun/<run>/agent-1.env \
      -e CLAUDE_CODE_OAUTH_TOKEN \
      localhost/hypothesisloop-experimentator-$EXPERIMENT

A secret is named with a bare `-e NAME`, which hands the container the value from this shell's
environment. `hl` in the container reads its agent and its platform experiment from the token,
so no command of an agent names either. A lost token is printed again by
`hl platform-experiments agents add --platform-experiment $PLATFORM_EXPERIMENT_ID --agent <agent_id>`, which changes
nothing for an agent that already joined. `hl platform-experiments agents remove` takes an agent
out again while the experiment is still open. A coordinator's token removes only an agent it
added itself.

Default: the token from `agents/coordinator/.env` (loaded in step 0). Host credential files
mount with `--userns=keep-id -v ~/.claude/.credentials.json:/agent/.claude/.credentials.json:ro
-v ~/.claude.json:/agent/.claude.json:ro -v ~/.codex/auth.json:/agent/.codex/auth.json:ro`,
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
--platform-experiment $PLATFORM_EXPERIMENT_ID` shows every agent with what it was launched with, and a self-run agent
whose container is gone from `podman ps` has exited: read its `.err`.

Hand off to `supervise.md` once agents are running.

## Fixing a blocker (called from supervise.md)

Diagnose from the actual failure (logs/job status/API response), not the symptom's shape.

- Platform bug → fix in code, per `important.md`
- Experiment-definition gap → fix `seed/`, `.hypothesisloop/job.yaml` or `experiment.md` directly, check against
  `experiment-checklist.md`.
- Environment/capacity → redo the relevant step-1 check, don't work around it per-agent.
