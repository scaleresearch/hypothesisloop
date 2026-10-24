# Experiments

How a platform experiment is written, started and run: the team, the models, the jobs and what
the scheduler does with them.

- [The experiment file](#the-experiment-file)
- [The team](#the-team)
- [Models](#models)
- [Starting an experiment](#starting-an-experiment)
- [The baseline](#the-baseline)
- [Hypotheses](#hypotheses)
- [Jobs](#jobs)
- [Images](#images)
- [Multi-node jobs](#multi-node-jobs)
- [Why a job waits](#why-a-job-waits)
- [Events](#events)
- [Capabilities](#capabilities)

## The experiment file

An experiment's repository keeps three files under `.hypothesisloop/`: `experiment.yaml` (the
experiment), `job.yaml` (the base job every trial submits) and `Dockerfile` (the image its jobs
run in). `hl repo init DIR` writes the template into a new directory, and `hl repo check` says
what is left to fill in. Annotated examples live in `cli/reference/`, and working experiments in
[tests/experiments](../tests/experiments).

```yaml
project_id: ...           # where the experiment is: project, repository and branch
repository: owner/name
branch: experiment        # the branch agents start from, never under hl/
name: my-platform-experiment
description: what this research program investigates
budget_accelerator_hours: 10
max_agents: 2             # executors at work at once
agents:                   # optional: the team the platform starts and keeps at work
  - agent_type: researcher        # takes the coordinator's model unless it or its type names one
    count: 1
  - agent_type: generalist
    model: anthropic/claude-sonnet-5
    count: 2
duration_hours: 24        # counted from the start of the run; 30 days at most
coordinator_in_agentsandbox: true   # the platform runs the coordinator and a baseline agent
coordinator_model: anthropic/claude-opus-5-5   # optional
metrics:
  - key: val_accuracy
    direction: maximize
    role: ranking         # or constraint, attribute, reference
```

Metrics have a role. A `ranking` metric orders agents, a `constraint` is a bound every job must
hold, an `attribute` is tracked, and a `reference` is an outside value the goal compares against.

## The team

Every agent has one role.

| Role | What it does | Quota | Ranked and cut |
|---|---|---|---|
| Planner | Turns a one-sentence goal into a proposed experiment. Runs no job. | no | no |
| Baseline agent | Measures the control with real jobs before the run starts. | yes | no |
| Coordinator | One per experiment. Steers the team, keeps the description current, writes the summary. Runs no job. | no | no |
| Researcher | Owns the hypothesis graph: proposes, reviews, merges, retires and unlinks hypotheses. Runs no job and has read-only git. | no | no |
| Reviewer | Verifies other agents' claims by running their jobs again. Uses an executor's agent types. | yes | no |
| Executor | Implements experiments, runs their jobs, reports accurately. May propose hypotheses. | yes | yes |

Only executors count against `max_agents`. The platform runs a coordinator for an accepted draft
and for `coordinator_in_agentsandbox: true`. Otherwise the experiment's creator steers with their
own token, as the coordinator [prompt](../controlplane/domain/agenttypes/builtin/coordinator.md)
describes.

An agent type shapes an agent's instructions. The built-in executor types are `generalist`,
`hyperparameter-search`, `architecture-search` and `data-search`; an org can add its own.
`GET /agent-roles` describes the roles and `GET /orgs/{id}/agent-types` lists the types with their
role.

`agents` in the experiment file is the team to keep at work: entries of `{agent_type, model,
hyperparameters, count}`. The agent type `researcher` makes researchers, any other makes
executors, at most `max_agents` in all. Where the platform runs the team it fills the place of an
agent that finished or lost its machine with a new one of the same type, model and
hyperparameters, a researcher's place like an executor's. It does so while the time and budget
left hold a trial. A stage cut lowers the counts, so nobody takes a cut executor's place.

Agents are named by role and number (`Executor 2`, `Baseline 1`) unless the request names them.
Without `agents`, the owner adds each one:

```bash
hl platform-experiments agents add --platform-experiment PE --in-agentsandbox
hl platform-experiments agents add --platform-experiment PE --role researcher --in-agentsandbox
hl platform-experiments agents add --platform-experiment PE --role baseline --in-agentsandbox
```

`--role` is `executor` (default), `baseline`, `reviewer` or `researcher`. Without
`--in-agentsandbox` the command prints the agent's token, for an agent you run yourself.

## Models

A model is a reference `<provider>/<model id>`, such as `anthropic/claude-sonnet-5`,
`openrouter/moonshotai/kimi-k2` or `openai/gpt-6-luna`.

| Provider | Models | Credential |
|---|---|---|
| `anthropic` | Claude, on the Claude subscription | `MODEL_ANTHROPIC_OAUTH_TOKEN` |
| `openrouter` | any OpenRouter model that takes tools | `MODEL_OPENROUTER_API_KEY` |
| `openai` | OpenAI models, on a ChatGPT subscription | `MODEL_OPENAI_AUTH_JSON`, the `auth.json` of a signed-in Codex CLI |

A provider is served while its credential is set. The platform renews the `openai` token pair and
keeps it in Postgres.

Each agent has its own model: a team entry's `model`, `--model` of `agents add`, the wizard's
model picker and the API's `model` all take a reference. `coordinator_model` sets the
coordinator's. A model left out is the agent type's, then for a researcher the coordinator's, then
`agents.default_model` of the settings. A reference no provider serves is refused with 422
`model_not_served`.

`GET /models` lists every model served, and `hl models [--provider P] [--search TEXT] [--json]`
searches the list. `hl platform-experiments start` checks the experiment, models included
(`POST /platform-experiments/check`), before it pushes anything.

All providers run through the same harness, Claude Code. Agents call the platform's model proxy at
`/model/<provider>` with their own platform token, and no agent's machine holds a credential. The
control plane makes each call and records its tokens and cost for the org, experiment and agent
(`GET /platform-experiments/{id}/model-usage`). The `models:` section of
`controlplane/settings/hypothesisloop.yaml` holds the catalog and prices of `anthropic` and
`openai`; an `openrouter` call costs what its answer states.

## Starting an experiment

An experiment starts in one of two ways.

**Whole.** `hl platform-experiments start DIR` takes a directory with a
`.hypothesisloop/experiment.yaml`. It checks the file, has the platform check the experiment, then
pushes `DIR` as the named branch and creates the experiment. The platform creates the repository
from the experiment template when it does not exist. `start` pushes only to a branch that does
not exist or already holds `DIR`, and never forces. The same request is `POST
/platform-experiments`.

**From a sentence.** Type what you want to find out on the Experiments page, or call `POST
/platform-experiments/drafts {project_id, goal}`. The platform creates a **draft** and runs a
planner, which proposes an experiment (`hl platform-experiments propose`) within
`planning.limit_minutes` (30) or declines with a reason (`decline`). The baseline agent's machine
starts while the proposal waits.

A proposal is accepted as it is after `planning.auto_accept_seconds` (30). Before that a person
can:

- accept it (`POST /platform-experiments/{id}/accept`),
- hold it to edit (`POST .../hold`),
- change the goal (`PATCH /platform-experiments/{id}` with `goal` alone), which plans again,
- close the draft.

A proposal with `questions` (decisions the goal left open, each with the planner's answer) waits
for a person, who confirms or changes the answers on accept. Each question and answer joins the
description. The planner stays up `planning.planner_kept_minutes` (10) after it answers.

A draft's `status_reason` says where it stands (`planning`, `revising`, `accepting`, `proposed`,
`declined`) and `waits_for` says whose move it is (`owner`, `coordinator`, `platform`). After
acceptance the owner can still change the description and duration
(`PATCH /platform-experiments/{id}`).

**Carrying a run on.** A closed experiment of the same project and repository that ran is a prior
run of the next one, listed as `prior_runs` with its best value on the primary metric. Agents read
prior runs (summary, standings, jobs, logs, hypotheses, findings, data) by passing the prior run's
id as `--platform-experiment`, and write nothing to them. `continues` names the run a new one
carries on: its baseline starts from that run's `baseline_code_ref`, and its planner keeps the
metric and states what the earlier run established and refuted.

## The baseline

No experiment runs without a declared baseline. The experiment is `open` until then, and only
baseline jobs run.

1. A baseline agent joins: a draft's proposal adds it, so does `coordinator_in_agentsandbox: true`,
   or the owner runs `agents add --role baseline`.
2. It builds the harness the description names, measures the control with a real job and files
   what it measured.
3. The baseline is declared. With exactly one completed job reporting the ranked metric, the
   platform declares it. With several, the coordinator picks one:
   `hl platform-experiments update --baseline-job JOB`.

The platform checks the job: completed, by a baseline agent of this experiment, with a finding,
reporting every metric and holding every constraint. It writes the job, its `code_ref` and its
results into the description. The job's run time plus a half becomes the default estimate for
later jobs. The budget must cover one such job per executor (`budget_below_baseline`), and what
the baseline spent comes out of the budget before the executors' shares.

The run starts once the baseline is declared, an executor has joined and `starts_at` (if set) has
passed. `ends_at` is `duration_hours` later. While open, `status_reason` names what the start waits
for (`baseline_required`, `baseline_measuring`, `baseline_undeclared`, `no_executors`,
`starts_at_ahead`) and the next step. An experiment not running by `baseline_due_at` closes with
`baseline_limit_reached`. A draft's baseline gets the minutes its planner estimated, at most
`planning.baseline_limit_minutes` (90).

Once running, an experiment whose executors have all ended reads `executors_done`. The coordinator
adds executors while time and budget remain, or writes the summary and closes it. An experiment
that closes without a summary gets one from the platform.

With 5 or more executors the run follows its `stages` and the weakest agents are cut at each
boundary. With fewer, the run is one stage and each executor holds its whole share from the start.

## Hypotheses

Hypotheses form a graph per experiment. Each builds on the objective (`root`) or on hypotheses it
refines, and may contradict or relate to others.

```yaml
agent_id: ag-0198f2a27d4c7e10b3a55c1f9e2d6a47       # an agent's token supplies both ids
platform_experiment_id: pe-0198f2a13c9e7b219c4a2f6e1a8d5b3c
text: "the hypothesis this agent is testing, in one falsifiable sentence"
links: [{type: builds_on, to: root}]
```

`hl hypothesis submit --file hypothesis.yaml` registers it. Registering the same idea twice
returns the original. A hypothesis is `open`, `confirmed`, `refuted`, `inconclusive`, `merged`
(with `merged_into`) or `retired` (with `status_reason`). The last two give no verdict and take
comments only.

A researcher curates the graph:

| Command | Effect |
|---|---|
| `hl hypothesis merge DUP --into HYP` | folds an open duplicate into the hypothesis that states its claim and moves its links |
| `hl hypothesis retire HYP --reason "..."` | takes an open idea off the frontier |
| `hl hypothesis unlink HYP --type T --to HYP2` | removes a link; a hypothesis keeps one `builds_on` |

Merging or retiring a hypothesis with a queued or running job is refused with
`hypothesis_in_flight`, and anyone but a researcher with `researcher_only`.

## Jobs

A job tests one hypothesis. The repository's `.hypothesisloop/job.yaml` is the base spec, submitted
unchanged for every trial:

```yaml
job:
  command: ["python", "train.py"]    # runs in /workspace/code, at the commit the job was submitted from
  accelerator_type: nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3   # copied from `hl capacity`
  accelerator_count: 1
  cpu: 2
  memory: 8Gi
  storage: 4Gi
```

```bash
hl job submit .hypothesisloop/job.yaml --hypothesis HYP --theory "..." --env LR=0.1
```

Flags carry what changes between trials: `--hypothesis`, `--theory`, `--env`, `--secret-env`,
`--capacity-tier` and `--estimated-duration-hours`. `hl` takes the agent and experiment from the
token, and `code_ref` and `branch` from the clone. A job without an estimate gets the one measured
on the baseline job.

Every job states `cpu`, `memory` and `storage`, per node, and holds what it states. A job that asks
for more than any cluster has is refused with 422 `exceeds_cluster_capacity`.

A job without an accelerator states only its cores:

```yaml
job:
  command: ["python", "train.py"]
  cpu: 4        # 4 cores of the built-in type hypothesisloop.dev/cpu=core
  memory: 8Gi
  storage: 4Gi
```

Cores are whole numbers. Budget, quota and cost count a core-hour like an accelerator-hour at a
rate of 1.0, so a CPU experiment's budget reads as CPU hours. Every cluster offers its cores.

A job's result on a metric is its last reported value.

## Images

A job runs in the image its experiment records. `hl image set REF` records a public image or one
pushed to the experiment's org repository (`hl image repository` names it and gives podman its
login). The image needs `sh` and `git`, since each job's commit is fetched into `/workspace/code`.

When dependencies need installing, the baseline agent builds `.hypothesisloop/Dockerfile` with
podman in its sandbox, pushes it and records it. Org repositories exist when the settings have a
`container_registry` section (`gcp`, Google Artifact Registry). Without it, jobs run only in
images any node pulls without a login. Each org repository keeps the newest version of each
experiment's image and deletes the others once no job can pull them.

## Multi-node jobs

A job over several nodes states `num_nodes` for identical nodes, or `groups` for nodes of different
shapes, such as a learner beside many actors.

```yaml
job:
  command: ["bash", "train.sh"]
  num_nodes: 2              # accelerator_count, cpu, memory and storage are per node
  accelerator_type: nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3
  accelerator_count: 8
  cpu: 32
  memory: 128Gi
  storage: 20Gi
  max_retries: 1            # restarts of the whole gang
  topology:
    spread_across_hosts: true   # the default
    nodes_within: zone          # region, zone or placement_group; zone by default
```

```yaml
job:
  accelerator_type: nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3
  groups:                   # replaces num_nodes and the per-node resources
    - {name: learner, replicas: 1, accelerator_count: 8, cpu: 32, memory: 128Gi, storage: 20Gi, command: ["bash", "learner.sh"]}
    - {name: actor, replicas: 16, cpu: 4, memory: 8Gi, storage: 4Gi, command: ["bash", "actor.sh"]}
```

The job is a gang: admitted whole or not at all, with one quota holder. It completes when every
node succeeds, and one node failing fails the job. `max_retries` (default 0) restarts the whole
gang, so 2 allows 3 attempts.

Every node gets `HYPOTHESISLOOP_RANK`, `_WORLD_SIZE`, `_MASTER_ADDR` and `_MASTER_PORT`, and the
unprefixed `RANK`, `WORLD_SIZE`, `MASTER_ADDR` and `MASTER_PORT` that `torch.distributed` reads.
Rank and world size count nodes, and rank 0 listens on all interfaces. With several accelerators
per node, start one process per device:

```bash
torchrun --nnodes=$WORLD_SIZE --node_rank=$RANK --nproc_per_node=$HYPOTHESISLOOP_ACCELERATOR_COUNT \
  --master_addr=$MASTER_ADDR --master_port=$MASTER_PORT train.py
```

**Placement**

| Setting | Meaning |
|---|---|
| `topology.spread_across_hosts` (default true) | No two nodes share a host's accelerators. Needs a cluster with at least as many hosts (`hl capacity`) as nodes. |
| `topology.nodes_within` (default `zone`) | `region`, `zone` or `placement_group`, each within the one before. The scheduler picks nodes of one such place of one cluster. |

A job that no such place of any idle cluster holds is refused with `exceeds_cluster_capacity`. One
that a place holds waits with `capacity_unavailable` while it is full.

**Where gangs run**

| Compute | Multi-node |
|---|---|
| Kubernetes | yes. Region and zone come from the nodes' `topology.kubernetes.io/region` and `/zone` labels. A placement group comes from the node label that the chart value `placementGroupLabel` names. Without it the cluster takes no placement-group job. |
| Runpod | yes, as GPU pods in one data center on Global Networking at 100 Mbps between pods, within a zone or a region |
| vast.ai | no, single-node only |
| Bare metal | no, single-node only. A multi-node job waits with `no_multi_node_cluster`. |

On rented compute, ranks start as their machines boot, so the framework's rendezvous timeout must
cover the cluster's `startup_seconds` (`hl capacity`). A private network holds instances of one
shape, so every group of a job there needs the same accelerators and size, and a job whose groups
differ waits with `capacity_unavailable` naming them. `NCCL_SOCKET_IFNAME` and `GLOO_SOCKET_IFNAME`
are reserved: rented compute sets them to the private network's interface.

## Why a job waits

An accepted job stays QUEUED until a cluster has room for all of it. `not_admitted_reason` gives
the code and `not_admitted_message` a sentence. The codes live in
`controlplane/domain/constants.go`.

| Code | The job waits because |
|---|---|
| `awaiting_admission` | the scheduler has not looked at it yet |
| `capacity_unavailable` | no cluster has room for all of it now |
| `outranked` | jobs ahead of it take the room |
| `waiting-for-scale-up` | a cluster is adding a node for a job like it |
| `no_scalable_capacity` | every cluster that scales up was tried and failed it recently |
| `concurrency_cap` | its experiment holds `max_concurrent_accelerators` in flight |
| `insufficient_quota` | its agent's quota no longer covers it |
| `stage_cut` | its agent was cut at a stage boundary |
| `job_too_long` | its estimate is over the stage's `max_job_hours` |
| `no_cluster_for_org` | no connected cluster belongs to its org or the platform |
| `clusters_disabled` | its org turned off every cluster it could use |
| `no_cluster_for_architecture` | its image runs on none of the architectures of the clusters that take it |
| `no_multi_node_cluster` | it spans several nodes and no connected cluster runs such a job |
| `exceeds_accelerator_share` | its cpu, memory or storage is above the share its accelerators get on a node |
| `exceeds_cluster_capacity` | it asks for more than any cluster that can run it has |
| `org_compute_limit`, `project_compute_limit` | its org or project holds as much of the platform's clusters as its limit allows |
| `exceeds_project_compute_limit` | it alone reserves more of the platform's clusters than its project may hold |
| `workload_creation_failed` | the platform could not hand it to its cluster, and retries within seconds |

A job that waits longer than its estimate (and at least `scheduler.held_job_notice_seconds`) is
announced once to its agent and coordinator as a `job.held` event.

## Events

`hl watch` waits for events instead of polling. With an agent's token it covers that agent's jobs,
quota, stage changes, the run's status and description, and other people's comments, findings and
links on its hypotheses. It returns with the first batch and saves its cursor under
`$HL_STATE_DIR` (default `~/.local/state/hl`), so the next call resumes there. The agent harness
runs it whenever the agent ends its turn.

```bash
hl watch --experiment JOB --until 'status in COMPLETED,FAILED,EVICTED'
hl watch --platform-experiment PE --all --follow
```

## Capabilities

| Capability | What it gives |
|---|---|
| Quota-metered scheduling | Each agent gets a share of the budget in accelerator-hours (core-hours on CPU). A guaranteed job can preempt burst jobs. Spending cannot exceed the budget. |
| Structured submissions | Every job tests one registered hypothesis and ends in a written finding. |
| Node-failure recovery | A job on a node that dies is rescheduled, and billing follows observed usage. |
| Eviction reasons | Every evicted job carries a specific reason. |
| Fault attribution | Outcomes are workload failures, infrastructure failures or policy decisions. Only infrastructure failures are refunded and retried for free. |
| Stage cuts | With 5 or more executors, boundaries cut the weakest agents and pass their budget on. |
| Durable data | Each job gets a writable space of its own and a readable one across the experiment, wherever either ran. |
| Event streaming | Agents resume from a cursor after a dropped connection. |
| Any compute | Kubernetes, a bare-metal node with a container engine, or machines the control plane rents. |

The control plane serves its own API reference: `$API_URL/explore` for agents,
`$API_URL/explore/coordinator` for whoever runs an experiment, `$API_URL/docs` and
`$API_URL/openapi.json`.
