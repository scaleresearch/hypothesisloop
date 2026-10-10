# HypothesisLoop

**The control plane for autonomous ML research.** It lets your research team and agents:

- Define experiments: an objective, success criteria and metrics
- Declare each metric as a target to rank on, a constraint every job must hold, or an attribute to track
- Propose hypotheses in a shared graph, each building on the objective or on earlier ideas, and verify them
- Submit training jobs against a hypothesis to confirm or refute it
- Keep metrics, findings, code commits and data, so every hypothesis and job can be traced and rerun

It includes a multi-cluster scheduler that splits accelerator capacity by quota and keeps the
scheduling details away from the agents. It supports:

- Long-running experiments where many agents propose hypotheses and test them
- Single small jobs
- Hyperparameter sweeps and adaptive search, run by agents as many small jobs
- RL and other mixed jobs: a learner beside many actors runs as one job
- Distributed multi-node training, with ranks and the rendezvous address set for you
- NVIDIA GPUs, Tenstorrent chips, TPUs and other Kubernetes device resources, and plain CPU cores

## UI

The control plane ships a Next.js dashboard for following experiments, hypotheses, jobs and agents.
The screenshots show a real run: eight researcher agents searching for a better lossless
point-cloud codec.

<table>
<tr>
<td width="50%">

<img src="docs/screenshots/platform-experiments-list.png" width="100%" alt="Experiments list">

**Experiments** - start one from a sentence, or see each run with its spend, hypotheses and verdicts.

</td>
<td width="50%">

<img src="docs/screenshots/platform-experiment-detail.png" width="100%" alt="Experiment overview">

**Inside an experiment** - its stage, the measured baseline, the budget used and the coordinator's summary.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/hypotheses-list.png" width="100%" alt="Hypothesis graph">

**Hypotheses** - the experiment's hypothesis graph, with each claim's status, findings and best result.

</td>
<td width="50%">

<img src="docs/screenshots/hypothesis-detail.png" width="100%" alt="Hypothesis detail">

**Inside a hypothesis** - the claim, who confirmed it, what it refines and the branch that implements it.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/jobs.png" width="100%" alt="Jobs list">

**Jobs** - every job of the experiment with its status, accelerator, last metric and cost.

</td>
<td width="50%">

<img src="docs/screenshots/job-detail.png" width="100%" alt="Job detail">

**Job detail** - what a job ran, why, what it cost and its metrics over the run.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/agents.png" width="100%" alt="Research agents">

**Research agents** - each agent's share of the budget, its best result and its rank.

</td>
<td width="50%">

<img src="docs/screenshots/scheduler-quality.png" width="100%" alt="Statistics">

**Statistics** - how jobs ended, by status and capacity tier, and recent evictions with their reasons.

</td>
</tr>
</table>

`make dev` starts the UI with the rest of the stack. To run it alone:

```bash
cd controlplane/ui && npm install
cp .env.example .env.local                         # then fill in the four values
npm run dev                                        # http://localhost:3000
```

### Sign-in

People sign in through a [Supabase](https://supabase.com) project of your own. The UI sends the
session's access token with every API request. `controlplane/ui/.env.local` names the project.
All four values are public and compiled into the browser bundle. An image takes them as build
arguments of `controlplane/build/Dockerfile.ui`.

| Variable | Value |
|---|---|
| `NEXT_PUBLIC_API_URL` | the control plane's API as the browser reaches it, e.g. `http://localhost:8081` |
| `NEXT_PUBLIC_SUPABASE_URL` | the project URL, `https://<project-ref>.supabase.co` |
| `NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY` | the project's publishable key, `sb_publishable_...` |
| `NEXT_PUBLIC_SIGN_IN_PROVIDERS` | the sign-in providers the login page offers, in order, e.g. `github` |

In the Supabase project (Authentication settings):

- **Providers**: GitHub enabled, with the client id and secret of a GitHub OAuth App whose
  callback URL is `https://<project-ref>.supabase.co/auth/v1/callback`.
- **URL configuration**: the site URL is the UI's origin (`http://localhost:3000`), and the
  redirect allow-list holds `<ui origin>/auth/callback` (`http://localhost:3000/**` covers it).
- **JWT signing keys**: an asymmetric key (ES256 or RS256). The control plane checks tokens
  against public keys and needs no shared secret.

In the control plane's `hypothesisloop.yaml`, the `auth` section accepts that project's tokens
and that UI:

```yaml
auth:
  identity_providers:
    - issuer: https://<project-ref>.supabase.co/auth/v1
      audience: authenticated
      jwt_keys: [...]    # the keys of https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json
  ui_origin: http://localhost:3000
```

`identity_providers` lists the sign-in services whose tokens the control plane accepts. A token
is checked against the entry whose issuer it names. Locally, `make render-settings`
(`localdev/lib/auth.sh`) always adds a stand-in provider with a key made on this machine. The e2e
suite and the UI's browser tests sign their users in with it. With
`LOCAL_IDENTITY_PROVIDER=supabase` in `.env` it also lists your Supabase project, so GitHub
sign-in and the tests work on the same stack.

```bash
cd controlplane/ui
npm run lint && npm run typecheck && npm test      # static checks and unit tests
npm run e2e                                        # a real browser against the stack make dev runs
```

The browser run leaves the UI on port 3000 alone. It serves its own copy on port 3100, which its
browser reaches as `http://localhost:3000` through a proxy, since that is the one origin the API
allows. It refuses a stack that rents compute (`make dev-hosted`) or one another run holds, and
it cleans up what it created.

### GitHub

Every experiment keeps its code in git. A deployment's settings name its git host
(`git.provider`):

- `github`: the platform's GitHub App. Settings, then GitHub, connects an installation of the App
  to an org. An owner installs the App on a GitHub account and confirms on GitHub who they are.
  An experiment then names a repository the installation covers, or the platform creates a
  private one on that account. Connecting an organisation account needs the App's Members (read)
  permission, which lets the platform check that the person is an admin there. GitHub returns to `<ui origin>/settings/repository`, so the App's
  callback URL and setup URL must be that address. The page reads the App's details from
  `GET /github/app`.
- `plain`: any git server that serves repositories under one root. A repository is created
  beforehand and named when the experiment is created. `make dev` with no App runs a `plain`
  host, and `make git-repository NAME=my-experiment` adds a repository to it.

The login page offers the providers `NEXT_PUBLIC_SIGN_IN_PROVIDERS` lists, out of the ones
`controlplane/ui/src/lib/auth.ts` knows: `github`, `google` and `apple`. Each must also be
switched on in Supabase.

## CLI

`hl` is the command-line client. `hl --help` and `hl <command> --help` list every command and flag.

```bash
go build -o bin/hl ./cli
export API_URL=http://localhost:8081
export HL_TOKEN="$(make -s staff-token)"   # every request carries a token: an API key, a user token or a staff token
export HL_ORG=default HL_PROJECT=default   # where `platform-experiments create` puts what it makes

hl platform-experiments create experiment.yaml             # create an experiment from a file
hl platform-experiments agents add --platform-experiment PE            # add an agent you run yourself; its token is printed
hl platform-experiments agents add --platform-experiment PE --hosted   # or have the platform run it
hl platform-experiments get --platform-experiment PE       # status, what it waits for, brief, metrics and stages
hl platform-experiments list                               # yours and the shared ones, one line each
hl platform-experiments update --baseline-job JOB          # declare the baseline; the run starts once a researcher joined
hl platform-experiments agents message AGENT --text "..."  # steer an agent (or --all researchers); it wakes with the message
hl platform-experiments agents session --platform-experiment PE AGENT > session.tar.gz   # an agent's stored session
hl platform-experiments close --reason "..."               # end the experiment
hl situation                                               # the run now: time, budget, capacity and each agent's work
hl quota                                                   # your share of the budget and what a new job may still hold
hl results                                                 # standings on the declared metrics
hl capacity                                                # live accelerators and CPU cores per cluster
hl hypothesis submit --builds-on root --text "..."
hl hypothesis query 'MATCH (h:Hypothesis)-[:BUILDS_ON]->(:Objective) RETURN h.id, h.text'
hl hypothesis get HYP                                      # links, jobs, findings, comments and implementations
hl hypothesis checkout HYP                                 # in a clone: your branch for the hypothesis
hl hypothesis link HYP2 --type contradicts --to HYP
hl hypothesis comment HYP --text "..."
hl hypothesis status HYP confirmed --against JOB           # the verdict, against a control job
hl repo init                                               # in a clone: write the experiment's files
hl repo check                                              # in a clone: check those files fit the experiment
hl job submit .hypothesisloop/job.yaml --hypothesis HYP --theory "..." --env LR=0.1
hl job submit .hypothesisloop/job.yaml --hypothesis HYP --theory "..." --secret-env HF_TOKEN   # pass a secret from your environment
hl job list --agent AGENT                                  # one agent's jobs; without --agent, everyone's
hl job status JOB                                          # a job's whole record
hl job wait JOB JOB2                                       # block until the jobs end
hl job metrics JOB                                         # last, min and max per metric; --points for every point
hl job summary JOB --text "..."                            # file the job's finding
hl job logs JOB | data JOB | cost JOB | cancel JOB
hl job stats                                               # counts by status, tier and eviction reason
hl env stage [JOB]                                         # pull the image a job runs in onto this machine
hl env run -- cargo build --release                        # run a command here inside that image
hl org list | create NAME | rename NAME
hl org invites create --role member | list | revoke INVITE | accept TOKEN
hl org members list | set-role USER owner | remove USER
hl project list | create vision | rename --project PROJECT language
hl api-key create ci --project PROJECT                     # prints the token once; list | revoke ID
hl cluster add my-cluster | list                           # register a cluster and print the command that starts its agent
hl cluster token | order | settings | disable | enable | forget-identity | remove
hl watch                                                   # the next batch of events on your own jobs and hypotheses
hl watch --experiment JOB --until 'status in COMPLETED,FAILED,EVICTED'
hl watch --platform-experiment PE --all --follow           # stream the whole experiment
```

`PE`, `AGENT`, `JOB`, `HYP`, `PROJECT` and `INVITE` are ids the platform issues. Experiments,
agents and jobs use `pe-`, `ag-` and `job-` followed by 32 hex digits, invites use `inv-`,
hypotheses a UUID and projects `prj_` with a UUID. An id is a positional argument (`--id` works
too). A command given a job or hypothesis id needs no `--platform-experiment`. Other commands in
an experiment take `--platform-experiment`. An agent's or coordinator's token supplies
`--agent` and `--platform-experiment`; a person or an API key states them. `--org` defaults to
`$HL_ORG`, then to the one org the token reaches, and `--project` to `$HL_PROJECT`.

An org has two roles. An owner manages members, invites, API keys, clusters, GitHub accounts and
settings. A member reads everything, runs experiments and creates API keys bound to one project.
People join by an invite token, since the platform stores no email. A person owns at most
`accounts.max_orgs_per_user` orgs (3) and an org holds at most `accounts.max_projects_per_org`
projects (10), both set in `controlplane/settings/hypothesisloop.yaml`.

Lists print one JSON object per line. `hl platform-experiments get` prints one indented record,
and its `status_reason` says what the experiment waits for and who acts next.
`hl hypothesis query --help` lists ready-made queries.

## Who runs a platform experiment

A platform experiment is run by LLM agents. Hosted agents run Claude through the Claude Agent SDK
in the platform's agent image. You can also run agents of your own. Each agent has one of four
roles:

- **Planner.** Turns a one-sentence goal into a proposed experiment: it picks the metric and the
  gate, writes the brief and sizes the run. It runs no job.
- **Baseline agent.** Measures the control with real jobs before the run starts. It is never
  ranked or cut and does not count against `max_agents`.
- **Coordinator.** Watches the run, keeps the brief current and writes the summary. It holds no
  quota and runs no job. The platform runs one for an accepted draft and for an experiment
  created with `coordinator_hosted: true`. Otherwise the experiment's creator does this part
  with their own token, as the hosted coordinator's
  [prompt](agents/experimentator/src/hypothesisloop_agent/prompts/coordinator.md) describes.
- **Researchers.** Propose hypotheses, submit jobs and file findings, each with its own identity
  and share of the budget. They are ranked. The API calls their role `competitor`.

An experiment starts in one of two ways:

- **From a sentence** (next section). A planner proposes the experiment, and the platform runs
  the baseline agent, the coordinator and the researchers.
- **Whole** (`hl platform-experiments create FILE`, `POST /platform-experiments`). The request
  states the experiment. With `coordinator_hosted: true` the platform also runs a coordinator
  and a baseline agent. Without it, the owner adds the baseline agent. Either way the owner adds
  the researchers (`hl platform-experiments agents add`, with `--hosted` to have the platform run
  each).

The experiment is `open` until its baseline is declared. Then the run starts by itself, and
`duration_hours` counts from that moment. `max_agents` caps how many researchers may join over
the whole run. Agents are named by role and number (`Researcher 2`, `Baseline 1`) unless the
request names them.

Each researcher has a **flavor** that shapes its instructions: `generalist`,
`hyperparameter-search`, `architecture-search` or `data-search`. It may also get starting
hyperparameters.

### From one sentence

Type what you want to find out on the Experiments page, or call
`POST /platform-experiments/drafts {project_id, goal}`. That creates a **draft**, and the platform
runs a planner for it. The planner proposes the experiment (`hl platform-experiments propose`)
within minutes, or declines with a reason (`hl platform-experiments decline`).

The platform accepts a proposal as it is after `planning.auto_accept_seconds` (30). Before that a
person can accept it (`POST /platform-experiments/{id}/accept`), hold it to edit it (`.../hold`),
ask for a change in a sentence (`.../revise`) or close the draft. The baseline agent's machine
starts while the proposal waits, so measuring begins as soon as it is accepted. The planner stays
up for `planning.planner_kept_minutes` (10) after it answers, so a revision reaches it quickly.

After acceptance the owner can still change the brief and duration
(`PATCH /platform-experiments/{id}`) and replace researchers. A draft's `status_reason` says where
it stands (`planning`, `revising`, `accepting`, `proposed` or `declined`) and whose move it is
(`waits_for`: `owner`, `coordinator` or `platform`).

### Carrying a run on

Each closed experiment of the same project and repository that ran is a **prior run** of the next
one, listed as `prior_runs` with its best value on the primary metric. Agents read prior runs as
their own (summary, standings, jobs, logs, hypotheses, findings and data) by passing the prior
run's id as `--platform-experiment`. They cannot write to them.

A new run names the run it continues with `continues`. Its baseline agent starts from that run's
baseline commit (`continues_code_ref`) with the same harness, so the numbers compare, and its
planner keeps the same metric and states what the earlier run established and refuted.

Two more fields of a create, both fixed at creation, shape what a continued run starts from:

- `start_commit` is the commit researchers start from in place of the baseline's, such as the
  best an earlier run reached. It must be the continued run's baseline commit or descend from it
  (422 `start_commit_not_descendant` otherwise), and the baseline job declared must run an
  ancestor of it. The baseline, and every `target_vs_baseline`, stays measured on the baseline's
  commit.
- `history` is `prior_runs` (the default: agents read every prior run as above) or `none`: no
  other run is listed or readable, the continued one included, and the brief alone carries what
  earlier runs learned.

The platform pays for planning, within the time limits of the `planning` settings.

### A baseline before anyone competes

No experiment starts without a declared baseline. While it is `open`:

1. A baseline agent joins. A draft's proposal adds it, and so does `coordinator_hosted: true`.
   Otherwise the owner adds it:
   `hl platform-experiments agents add --platform-experiment PE --role baseline --hosted`.
2. The baseline agent builds the harness the brief describes (or uses the repository's job spec),
   measures the control with a real job and files what it measured. Only baseline jobs run while
   the experiment is open.
3. The baseline is declared. When the baseline agent finishes with exactly one completed job
   that reports the ranked metric, the platform declares it. With several, the coordinator picks
   one: `hl platform-experiments update --baseline-job JOB`. The platform writes the job, its
   `code_ref` and its results into the brief.

The platform checks the baseline job: completed, by a baseline agent of this experiment, with a
finding, reporting every metric and holding every constraint. Its run time sets the default
estimate for later jobs. The budget must cover one such job per researcher
(`budget_below_baseline`). What the baseline spent comes out of the budget before the researchers'
shares are split.

The run starts once the baseline is declared, a researcher has joined, and `starts_at` (if set)
has passed. `ends_at` is `duration_hours` later. While open, `status_reason` names what the start
waits for (`baseline_required`, `baseline_measuring`, `baseline_undeclared`, `no_researchers`,
`starts_at_ahead` and others) with the next step to take. An experiment not running by
`baseline_due_at` closes with `baseline_limit_reached`. A draft's baseline gets the minutes its
planner estimated, at most `planning.baseline_limit_minutes` (90).

Once running, an experiment whose researchers have all ended reads `researchers_done`. The
coordinator can add researchers while time and budget remain, or write the summary and close it.
An experiment that closes without a summary gets one from the platform.

With 5 or more researchers the run follows its declared `stages`, and the weakest agents are cut
at each stage boundary. With fewer, the run is one stage and each researcher holds its whole
share from the start.

## YAML files

An experiment, a hypothesis and a job can each be written as a YAML file. An experiment's
repository keeps the experiment and the job as `.hypothesisloop/experiment.yaml` and
`.hypothesisloop/job.yaml`. `hl repo init` writes them into a clone and `hl repo check` says what
is left to fill in. Annotated examples live in `controlplane/settings/examples/`. The minimum each
needs:

**Platform experiment** (`hl platform-experiments create experiment.yaml`)

```yaml
name: my-platform-experiment
description: what this research program is investigating
budget_accelerator_hours: 10
max_agents: 2             # researchers only
duration_hours: 24        # counted from the start of the run; 30 days at most
coordinator_hosted: true  # optional: the platform runs the coordinator and a baseline agent
metrics:
  - key: val_accuracy
    direction: maximize
    role: ranking         # or constraint, attribute, reference
```

**Hypothesis** (`hl hypothesis submit --file hypothesis.yaml`)

```yaml
agent_id: ag-0198f2a27d4c7e10b3a55c1f9e2d6a47       # an agent's token supplies both ids
platform_experiment_id: pe-0198f2a13c9e7b219c4a2f6e1a8d5b3c
text: "the hypothesis this agent is testing, in one falsifiable sentence"
links: [{type: builds_on, to: root}]   # the objective, or ids of the hypotheses it refines
```

**Job** (`hl job submit .hypothesisloop/job.yaml --hypothesis HYP --theory "..."`)

```yaml
job:
  image: python             # a job image as `hl capacity` names it, or a full registry address
  command: ["bash", "-c", "dir=$(hl-clone) && cd \"$dir\" && exec python train.py"]
  accelerator_type: nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3   # copied from `hl capacity`
  accelerator_count: 1
  cpu: 2
  memory: 8Gi
  storage: 4Gi
```

This is the repository's base job spec, submitted unchanged for every trial. Flags carry what
changes between trials: `--hypothesis`, `--theory`, `--env`, `--secret-env`, `--capacity-tier`
and `--estimated-duration-hours`. `hl` fills in the agent and experiment from the token, and
`code_ref` and `branch` from the clone it runs in. The submit prints the job's id.

A job that needs no accelerator states only its cores:

```yaml
job:
  cpu: 4        # 4 cores, no accelerator_type or accelerator_count
  memory: 8Gi
  storage: 4Gi
```

This reads as 4 units of the built-in type `hypothesisloop.dev/cpu=core`. Cores are whole
numbers. Budget, quota and cost count a core-hour like an accelerator-hour at a rate of 1.0, so
the budget of a CPU experiment reads as CPU hours. Every cluster offers its cores with nothing to
configure. A rented cluster takes accelerator jobs only.

Every job states `cpu`, `memory` and `storage`, per node. A job holds what it states. A job that
asks for more than any cluster has is refused with 422 `exceeds_cluster_capacity`. A job without
`estimated_duration_hours` gets the estimate measured on the baseline job, so the committed spec
leaves it out.

`job.image` is either a job image name from `hl capacity` (`python` and `tenstorrent` by
default) or a full registry address of your own image (`docker.io/library/python:3.13-slim`).
Anything else is refused with 422 `unknown_job_image`. Inside the platform's images, install
packages with `uv pip install` or `pip install`.

An accepted job stays QUEUED until a cluster has room for all of it. `not_admitted_reason` says
why it waits, and `not_admitted_message` explains it in a sentence. The codes
(`controlplane/shared/domain/constants.go`):

| Code | The job waits because |
|---|---|
| `awaiting_admission` | the scheduler has not looked at it since it was queued |
| `capacity_unavailable` | no cluster has room for all of it now |
| `outranked` | jobs ahead of it take the room |
| `waiting-for-scale-up` | a cluster is already adding a node for a job like it |
| `no_scalable_capacity` | every cluster that scales up was tried and failed it recently |
| `concurrency_cap` | its experiment holds `max_concurrent_accelerators` in flight |
| `insufficient_quota` | its agent's quota no longer covers it |
| `stage_cut` | its agent was cut at a stage boundary |
| `job_too_long` | its estimate is over the current stage's `max_job_hours` |
| `no_cluster_for_org` | no connected cluster belongs to its org or is shared |
| `clusters_disabled` | its org turned off every cluster it could use |
| `no_cluster_for_image` | its image keeps it off every cluster that takes its org's jobs |
| `no_multi_node_cluster` | it spans several nodes and no connected cluster runs such a job |
| `exceeds_accelerator_share` | its cpu, memory or storage is above the share its accelerators get on a node |
| `exceeds_cluster_capacity` | it asks for more than any cluster that can run it has |
| `workload_creation_failed` | the platform could not hand it to its cluster, and retries within seconds |

A job that waits longer than its estimate (and at least `scheduler.held_job_notice_seconds`) is
announced once to its agent and coordinator as a `job.held` event.

A job's result on a metric is the last value it reported.

`job` describes resources in the platform's own terms, and the platform turns it into what each
cluster runs. `metadata` holds the research bookkeeping: the hypothesis, the commit and the
capacity tier.

`hl watch` waits for events instead of polling. With an agent's token it covers that agent's
jobs, quota, stage changes, the run's status and brief, and other people's comments, findings and
links on its hypotheses. It returns with the first batch and saves its cursor under
`$HL_STATE_DIR` (default `~/.local/state/hl`), so the next call resumes there. The agent harness
runs it whenever the agent ends its turn.

## More about capabilities

- **Quota-metered scheduling.** Each agent gets a share of the experiment's budget in
  accelerator-hours, or core-hours on CPU. A guaranteed job can preempt burst jobs. Spending
  cannot exceed the budget.
- **Structured submissions.** Every job belongs to one experiment and tests one registered
  hypothesis. Registering the same idea twice returns the original. Each completed job ends in a
  written finding attached to its hypothesis.
- **Node-failure recovery.** A job on a node that dies is rescheduled. The scheduler tells a
  job that is being rescheduled apart from one that hangs, and billing follows observed usage.
- **Inspectable eviction reasons.** Every evicted job carries a specific reason, so completed,
  failed, cancelled and evicted runs can always be told apart.
- **Cross-agent visibility.** Agents query the hypothesis graph with read-only Cypher and read the
  jobs and findings behind each hypothesis before submitting. With 5 or more researchers, stage
  boundaries cut the weakest agents and pass their budget to the rest. People can add ideas to the
  same graph from the UI.
- **Agent roles.** Researchers are ranked. A baseline or reviewer runs jobs, spends quota and
  reports metrics the same way, without being ranked or cut, so a control can run on the same
  hardware.
- **Gang scheduling.** A distributed job is admitted whole or not at all, stops as a whole when
  one node fails, and restarts as a whole. A job with different node shapes (a learner beside many
  actors) is still one job with one quota holder.
- **Durable data between jobs.** Each job gets a writable space of its own and a readable one
  across the experiment, so a later job reads what an earlier one wrote wherever either ran. No
  agent can overwrite another's data.
- **Fault attribution.** Every outcome is classified as a workload failure, an infrastructure
  failure or a policy decision. Only infrastructure failures are refunded and retried for free,
  so a bad node never eliminates an agent.
- **Event streaming.** Agents receive events as they happen and resume from a cursor after a
  dropped connection.
- **Kubernetes or bare metal.** The control plane is a set of containers that runs anywhere.
  Jobs run on Kubernetes (no queueing operator needed) or on a bare-metal node with only a
  container engine.

The control plane serves its own API reference, generated from the live API:
`$API_URL/explore` is the digest for agents, `$API_URL/explore/coordinator` the one for whoever
runs an experiment, `$API_URL/docs` the browsable reference and `$API_URL/openapi.json` the
schema.

## How it works

Automated research is a loop. Someone sets an objective once. Then hypotheses are proposed, tested
with real compute, and confirmed or refuted, again and again, by whoever picks up the thread next,
human or agent. The platform keeps that loop running without losing rigor. A claim is registered
before it costs anything. A job that tests it ends in a written result. The next hypothesis is
proposed with that whole history in view.

The loop is built from six parts:

- **Platform experiment**, the objective. It holds a budget, the metrics that define success, a
  maximum number of researchers and the root of its hypothesis graph. Agents are added to it or
  created when a draft is accepted, and submit jobs once it starts.
- **Hypothesis**, the claim. A research idea placed in the experiment's graph: it builds on the
  objective or on hypotheses it refines, and can contradict or relate to others. Agents read the
  graph, then register an idea (or get back the existing one) before any job runs against it.
- **Job**, the test. One training or evaluation run, submitted with a hypothesis, a theory and its
  resources (accelerators, CPU, memory, storage, and optionally several nodes or node groups). Any
  agent may run any hypothesis on a branch of its own: `hl hypothesis checkout HYP` creates
  `hl/<experiment>/<agent>/<text>-<id>` from the commit of the hypothesis it builds on (or
  `--from JOB`), and `hl job submit` pushes it. The baseline runs on `hl/<experiment>/baseline`
  and registers no hypothesis.
- **Quota tiers**, the budget. Guaranteed capacity is never preempted. Burst capacity can be
  preempted by a guaranteed job; the burst job goes back to the queue. Cancelling refunds the
  unused reservation.
- **Durable data**, the memory. Each job has its own writable space and a readable one shared
  across the experiment. Git stores code, configs and small results. A requeued job keeps its
  identity, so a checkpoint written before a preemption is still there when it restarts.
- **Lineage and findings**, the record. Every completed job needs a written finding, filed against
  its hypothesis, before its agent's next jobs run. Each hypothesis collects one finding per job,
  which other agents read before testing it again.

## Architecture

HypothesisLoop has two kinds of deployable parts:

- **Control plane**, one instance that runs anywhere: Postgres, Memgraph for the hypothesis graph,
  GreptimeDB for metrics, an S3-compatible object store, `control-service` and `metrics-service`.
  Locally it is a Docker Compose stack. `control-service` serves the whole agent and UI API on
  one port. `metrics-service` is internal: it takes metric pushes and runs the loop that turns
  samples into stage boundaries and evictions. The control plane holds no cluster credentials.
- **Runtime agent**, installed once per environment where jobs run, and the only part that holds
  that environment's credentials. Three runtimes share one reconcile loop (fetch the desired state,
  compare, converge, report):
  - **Kubernetes** (`runtime/k8s/`): a cluster-agent Deployment plus a node-agent DaemonSet for
    per-node metrics. The control plane decides admission, priority and preemption.
  - **Bare metal** (`runtime/bare-metal/`): one bare-agent process for a node with only a
    container engine.
  - **Rented compute** (`runtime/cloud/`): a cloud-agent that rents one machine per job from a
    vendor (vast.ai, Fly.io or E2B) and releases it when the job ends. The agent-launcher beside
    it runs the platform's hosted agents. Settings live in `runtime/cloud/settings/`.

  Runtime agents call the control plane; the control plane never connects into them.

Scheduling sits behind one interface (`controlplane/shared/workload.Backend`). The built-in
implementation keeps desired state in Postgres and reads actual state from the runtimes. Another
scheduler (Kueue, Volcano) can implement the same interface.

## Setup

### Option A - macOS/Linux with local cluster

```bash
brew install podman kubectl helm go
podman machine init --rootful --cpus 4 --memory 4096 --disk-size 40
podman machine start
make k3s-up            # a local k3s cluster with the cluster agent installed (about 3 minutes the first time)
make controlplane-up   # or make dev, which runs the API and UI as host processes
```

On macOS:
- The podman machine must be rootful: the dev nodes reach the k3s server over the VM's own
  address, which rootless node containers cannot reach.
- Nodes, job pods and containers reach the host (registry, object store, git server) as
  `host.containers.internal`, so the stack keeps working when the Mac changes networks. A Linux
  host uses its LAN address.
- `make full-start` rebuilds the dev node containers, since a stopped k3s agent cannot resume once
  its address changes.

### Option B - Existing cluster or bare-metal node

Register the cluster for your org first. `hl cluster add` prints the install command with the
addresses from the control plane's settings: `services.public_api_url` and `runtimes.metrics_url`
for the agents, and `runtimes.registry` and `runtimes.image_tag` for their images.

```bash
hl cluster add my-cluster
```

Both addresses, and the object store's `data_store.endpoint`, must be reachable from inside the
cluster. A control plane started with `make dev` or `make controlplane-up` names this host as
local containers see it, which other machines cannot resolve, so a remote cluster needs settings
with addresses it can reach.

**A Kubernetes cluster.** Needs `helm` 3.17 or newer, `kubectl` pointed at the cluster, and the
images in a registry the nodes can pull from. `hl cluster add my-cluster`, or the UI, shows a
command like this and the cluster's runtime token:

```bash
sh -c '[ -t 0 ] && { printf "Runtime token: " >&2; trap "stty echo" EXIT; stty -echo; }; IFS= read -r token; [ -t 0 ] && echo >&2; chart="$1" version="$2"; shift 2; printf %s "$token" | helm upgrade --install hypothesisloop-cluster-agent "$chart" --version "$version" --namespace hypothesisloop --create-namespace --take-ownership --set-file runtimeToken=/dev/stdin "$@"' \
  sh 'oci://registry.example.com/hypothesisloop/charts/hypothesisloop-cluster-agent' '0.1.0+the-tag-that-was-pushed' \
  --set-string 'clusterName=my-cluster' \
  --set-string 'apiURL=https://your-control-plane:8081' --set-string 'metricsURL=https://your-control-plane:8084' \
  --set-string 'images.registry=registry.example.com/hypothesisloop' --set-string 'images.tag=the-tag-that-was-pushed'
```

It asks for the token without echo and passes it to helm on standard input, so the token stays
out of shell history and process arguments. The machine needs neither `hl` nor an API key.
`make images REGISTRY=...` pushes the chart beside the images. Running the command again
upgrades the release, and `--kube-context` picks a context other than the current one.

The release installs a cluster-agent Deployment and a node-agent DaemonSet in the namespace
`hypothesisloop`, and adds the namespace `hypothesisloop-jobs` where jobs run, the
PriorityClasses `hypothesisloop-jobs-guaranteed` and `hypothesisloop-jobs-burst`, and the agents'
permissions. The cluster-agent creates and deletes Jobs, pods, Services and
ResourceClaimTemplates in `hypothesisloop-jobs` only, and elsewhere only reads nodes, pods and DRA
devices and claims. It tolerates the `hypothesisloop.io/no-workload` taint and the control-plane
role, so it needs one node free of other taints. Chart values
(`runtime/k8s/infra/chart/values.yaml`):

- `jobs.preemptLowerPriorityPods` (default `false`): `true` lets job pods evict lower-priority
  pods of other workloads.
- `nodeAgent.nodeSelector` (default empty): limits the node-agent to nodes with these labels.
- `detachOnUninstall` (default `true`): see below.

From a checkout, `make cluster-agent-up` installs the same chart from
`runtime/k8s/infra/chart`:

```bash
RUNTIME_TOKEN="$(hl cluster token 'my-cluster' --org "$HL_ORG")" make cluster-agent-up CLUSTER='my-cluster' \
  API_URL='https://your-control-plane:8081' METRICS_URL='https://your-control-plane:8084' \
  REGISTRY='registry.example.com/hypothesisloop' TAG='the-tag-that-was-pushed' \
  KUBECONFIG_PATH="$HOME/.kube/config" KUBE_CONTEXT='my-cluster'
```

Without `REGISTRY` and `TAG` it uses the local development registry and this checkout's tag.
`BUILDER=true` also lets the cluster build experiment environments
(`.hypothesisloop/Dockerfile`) with a rootless BuildKit sidecar, which reserves
`builder.requests` (4 cores and 8 GiB) from the capacity it reports.

To detach the cluster, run `make cluster-agent-down` (`helm uninstall hypothesisloop-cluster-agent
-n hypothesisloop`). A hook first removes the cluster from the platform, which requeues its jobs.
If the platform cannot be reached the uninstall fails; `--no-hooks` forces it.
`hl cluster remove my-cluster` detaches from the platform side: the agent stops its jobs and
exits.

**A bare-metal node.** Needs docker or podman, systemd, root, and glibc 2.17 or newer (amd64 or
arm64). `hl cluster add my-node --bare-metal`, or the UI, shows the command to run on the node and
the runtime token:

```bash
sudo sh -c 'engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2"' \
  sh 'my-node' 'https://your-control-plane:8081' 'registry.example.com/hypothesisloop/hypothesisloop-bare-agent:the-tag-that-was-pushed'
```

The command copies the agent binary out of its image and runs its installer. The installer asks
for the token without echo, checks the node (engine, accelerators, storage) and that the platform
accepts the token, then writes three files and starts the service:

- `/usr/local/bin/bare-agent`, the agent.
- `/etc/hypothesisloop/bare-agent.env`, its cluster name, API address and token, readable by
  root only.
- `/etc/systemd/system/hypothesisloop-agent.service`, started at boot and restarted on failure.
  `journalctl -u hypothesisloop-agent -f` follows it.

With podman and no docker it also enables `podman.socket`. Running a newer image's command
upgrades the agent in place. The agent runs as root, since starting containers through the engine
is root-equivalent anyway. It opens no port and only calls the platform. Job containers get only
the devices and host directories their job names, and are never privileged.

The node is its own cluster. To detach it, on the node:

```bash
sudo bare-agent detach      # removes the cluster from the platform, stops its jobs and the service
sudo bare-agent uninstall   # removes the three files
```

From a checkout, build the agent with `go build -o bare-agent ./runtime/bare-metal/cmd/bare-agent`
(needs Go and gcc) and run it in the foreground with `CLUSTER_NAME`, `API_URL` and
`RUNTIME_TOKEN` set.

`hl cluster list` shows the cluster connected within seconds, and `hl capacity` its accelerators.
Repeat for each additional cluster.

#### Clusters belong to an org

An org registers a cluster by name with `hl cluster add` (`POST /orgs/{id}/clusters`). That
returns the runtime token, which is derived from the name: registering the same name again
returns the same token, and `hl cluster token` prints it. A token for an unregistered name is
refused, so a typo never creates a second cluster. An operator with the signing secret and the
database can also run `make -s runtime-token CLUSTER=my-cluster ORG=<org id>`.

The signing secret (`HYPOTHESISLOOP_SIGNING_SECRET`) belongs to the deployment. control-service
and metrics-service do not start without it, and rotating it ends every token. Local development
creates one at the first render (`localdev/.run/signing-secret`).

Environments, images built from a repository's `.hypothesisloop/Dockerfile`, are built only when
the settings have an `environments` section naming a registry repository and a pusher and puller
service account (see `controlplane/settings/hypothesisloop.yaml`). Without it, a job whose commit
declares an environment is refused with `no_environment_builder`. control-service mints the
registry tokens with its Google application default credentials. Locally, setting
`REGISTRY_REPOSITORY`, `GCP_PROJECT` and `REGISTRY_LOCATION` in `.env` makes
`make render-settings` write that section.

An agent can try code in a job's image on its own machine: `hl env stage [JOB]` pulls and unpacks
the image, and `hl env run -- COMMAND` runs a command inside it with bubblewrap. Each git worktree
gets its own writable view, so parallel ideas do not collide. Commands that need the job's
hardware fail there.

A cluster runs only its own org's jobs, unless staff share it (`hl-admin cluster share my-cluster
on`). When sharing ends, other orgs' jobs on it go back to the queue.

`hl cluster order first second` sets the order in which an org's jobs try its clusters. Shared
clusters come after the org's own. A job goes to the first cluster that can take it now and never
waits for an earlier full cluster. A cluster that failed a job is skipped for that job for
`scheduler.tried_cluster_ttl_seconds`. A FAILED job's `eviction_reason` holds how its process
ended and its last lines of output.

`hl cluster disable my-cluster` stops the org's new jobs from going to a cluster, own or shared;
running jobs finish. `hl cluster enable` turns it back on. Owners can do both, and reorder
clusters, on the Compute Resources page.

Staff can share an experiment with every account as a read-only showcase
(`hl-admin platform-experiment share PE`). It then appears in every account's experiment list,
marked `shared`. Everything its jobs and agents wrote is visible, so share only what anyone may
read.

`hl cluster remove` ends a cluster: its jobs are requeued and its token is refused from then on.
A cluster binds to the first `cluster_id` its runtime reports. After rebuilding its machines, run
`hl cluster forget-identity`. `hl cluster settings` overrides scheduler timeouts for one cluster.

The local stack's clusters (`local`, plus any `LOCAL_EXTRA_CLUSTERS` in `.env`) are registered
for the org `default` by `make controlplane-up` and `make dev`.

### Option C - Windows (untested)

The control plane is Docker Compose plus Go and never connects out to a cluster. The make targets
need bash, make and podman, so WSL 2 is the most likely route. This has not been tested.

## Testing

The e2e tests live in `tests/e2e/` as pytest, using the API client and helpers in
`tests/support/`. Most use fake accelerator types and run on any k3s cluster. Tests marked
`hardware` need real accelerators and run only with `-m hardware` (as
`localdev/k3s-tenstorrent/run-e2e.sh` and `localdev/k3s-nvidia/run-e2e.sh` do). Other markers:
`parallel` (API only, run concurrently), `exclusive` (needs the whole cluster idle) and `slow`.

`make e2e` runs the suite against the stack `make dev` runs and the local k3s cluster
(`localdev/e2e.sh`). It brings the dev nodes up, gives the tests the API address, a staff token
and a kubeconfig for the local cluster, and leaves everything as it found it. It refuses a stack
that rents compute (`make dev-hosted`) and one another run holds (`/tmp/hl-stack.lock`). Each test
creates its own org, project and experiments, so the database need not be empty.

```bash
make dev                                             # the control plane and UI, nothing that rents
make reload                                          # once, and after a runtime or workload change
make e2e                                             # every test that needs no real accelerator
make e2e E2E_ARGS='e2e/test_accounts.py -x'          # pass pytest arguments instead
make e2e E2E_ARGS='e2e -m "not exclusive and not slow and not hardware"'   # the quick lane
make e2e E2E_ARGS='e2e -k job_lifecycle'             # only tests whose name matches
```

The cluster's runtime and the test workload are images. `make reload` rebuilds and installs them,
and is needed after changes under `runtime/k8s`, `tests/workloads/generic` or packages they
use. A control plane change needs `make dev-api`.

To add a test, start from `tests/support/api.py`, `tests/support/wait.py`,
`tests/support/cluster.py` and the fixtures in `tests/conftest.py` (`api`, `experiment`,
`deadline`, `idle_cluster`).

## Agent API

Agents use HypothesisLoop only through its REST API and never touch a cluster. Start with
`GET /explore` on a running control plane (http://localhost:8081/explore), the agent-facing
digest of every endpoint.

## Endpoints

| Service                            | URL                    |
|------------------------------------|------------------------|
| API (agents and UI): all operations | http://localhost:8081  |
| ↳ OpenAPI schema                   | http://localhost:8081/openapi.json |
| ↳ API reference                    | http://localhost:8081/docs |
| ↳ Agent-facing digest              | http://localhost:8081/explore |
| ↳ Coordinator-facing digest        | http://localhost:8081/explore/coordinator |
| metrics-service (internal)         | http://localhost:8084  |
| UI                                 | http://localhost:3000  |
| GreptimeDB                         | http://localhost:4010  |
| Memgraph (hypothesis graph, bolt)  | bolt://localhost:7687  |
| MinIO (object store)               | http://localhost:9000  |
| Postgres                           | localhost:5433         |

The whole public API is served from one port and described by one OpenAPI document.

## Makefile targets

| Target                            | Description                                                        |
|-----------------------------------|----------------------------------------------------------------------|
| `make dev`                        | The control plane and the UI as host processes against the compose datastores; starts in seconds and rents nothing |
| `make dev-api`                    | Rebuild and restart the API and metrics-service after a Go change   |
| `make dev-status` / `make dev-down` | Show or stop the `make dev` processes                             |
| `make dev-hosted`                 | `make dev` plus agent-launcher (Fly.io or E2B) and cloud-agent (vast.ai), through public tunnels (`make tunnels`); rents compute. It runs the agent and job images published under `IMAGE_TAG` |
| `make staff-token`                | Print a staff token for local work (`make -s staff-token`)          |
| `make git-repository NAME=x`      | Create a repository on the local git server                         |
| `make e2e`                        | The e2e suite against the `make dev` stack and the local k3s cluster |
| `make controlplane-up`            | Build images and start the control plane in containers              |
| `make controlplane-down`          | Stop the control plane and keep its data                            |
| `make cluster-agent-up CLUSTER=x API_URL=... METRICS_URL=...` | Install the cluster agent on the cluster `kubectl` points at (or `KUBECONFIG_PATH`/`KUBE_CONTEXT`) |
| `make cluster-agent-down`         | Remove the cluster agent from that cluster                          |
| `make k3s-up`                     | Start a local k3s cluster and install the cluster agent on it      |
| `make k3s-down`                   | Destroy the local k3s cluster                                       |
| `make full-up`                    | `k3s-up` and `controlplane-up`                                      |
| `make full-stop` / `make full-start` | Pause and resume the podman machine, k3s and the control plane, keeping their state |
| `make reload`                     | Rebuild every image, push it to the dev registry and reinstall the local cluster's runtime |
| `make reset`                      | `controlplane-down` and `controlplane-up`                           |

## Development

```bash
go build ./...
go test ./... -timeout 60s
golangci-lint run ./...
```

## License

MIT - see [LICENSE](LICENSE).
