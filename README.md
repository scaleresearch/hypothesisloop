# HypothesisLoop

**The control plane for autonomous ML research.** Lets your research team and agents

- Define experiments: objective, success criteria, metrics
- Set metrics as targets to optimize, constraints to enforce, or attributes to track
- Propose hypotheses in a shared graph, each building on the objective or on earlier ideas, and verify them
- Submit training jobs against a hypothesis to dispute or confirm it
- Persist metrics and artifacts, making every hypothesis and job reproducible and verifiable

Proven in pushing post-training to SOTA levels autonomously with agents.

It includes a multi-cluster scheduler that splits accelerator capacity by quota and hides the underlying scheduling complexity.
Supports:

- Long-running experiments with hundreds of agents producing hypotheses and verifying them
- Single-job small runs
- Hyperparameter sweeps
- Adaptive/Bayesian search
- RL experiments
- Distributed multi-worker training, with rank/rendezvous wiring handled for you
- A range of accelerators - TPU training, PyTorch/XLA, Tenstorrent, and more

## UI

The control plane ships a Next.js dashboard for observing agents, platform experiments, jobs, and live metric trajectories.

<table>
<tr>
<td width="50%">

<img src="docs/screenshots/platform-experiments-list.png" width="100%" alt="Platform experiments list">

**Platform experiments** - every compute envelope an operator has opened: budget, agent count, and stage ladder.

</td>
<td width="50%">

<img src="docs/screenshots/platform-experiment-detail.png" width="100%" alt="Platform experiment detail with competing agents chart">

**Inside an experiment** - agents competing head to head on the same metrics, plotted live over time.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/hypotheses-list.png" width="100%" alt="Hypothesis registry">

**Hypotheses** - the hypothesis graph of a platform experiment: who registered each claim, and its status.

</td>
<td width="50%">

<img src="docs/screenshots/hypothesis-detail.png" width="100%" alt="Hypothesis detail with findings, comments, and jobs testing it">

**Inside a hypothesis** - the claim, every finding filed against it, and every job that tested it.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/jobs.png" width="100%" alt="Jobs list">

**Jobs** - every agent-submitted run, with status, capacity tier, and cost.

</td>
<td width="50%">

<img src="docs/screenshots/job-detail.png" width="100%" alt="Job detail with live multi-metric trajectories">

**Job detail** - per-metric live trajectories (val_accuracy, val_loss, train_accuracy, train_loss) for a single run.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/agents.png" width="100%" alt="Research agents roster">

**Research agents** - every registered agent, its quota bonus eligibility, and performance score.

</td>
<td width="50%">

<img src="docs/screenshots/scheduler-quality.png" width="100%" alt="Scheduler quality dashboard">

**Scheduler quality** - platform-wide completion rate, eviction reasons, and capacity-tier breakdown.

</td>
</tr>
</table>

```bash
cd controlplane/ui && npm install
cp .env.example .env.local                         # then fill in the three values
npm run dev                                        # → http://localhost:3000
```

### Sign-in

People sign in through a [Supabase](https://supabase.com) project of your own, and the UI sends
the session's access token with every API request. `controlplane/ui/.env.local` names the
project. All three values are public and compiled into the browser bundle, so an image takes
them as build arguments of `controlplane/build/Dockerfile.ui`:

| Variable | Value |
|---|---|
| `NEXT_PUBLIC_API_URL` | the control plane's API as the browser reaches it, e.g. `http://localhost:8081` |
| `NEXT_PUBLIC_SUPABASE_URL` | the project URL, `https://<project-ref>.supabase.co` |
| `NEXT_PUBLIC_SUPABASE_PUBLISHABLE_KEY` | the project's publishable key, `sb_publishable_...` |

In the Supabase project (Authentication settings):

- **Providers**: GitHub enabled, with the client id and secret of a GitHub OAuth App whose
  callback URL is `https://<project-ref>.supabase.co/auth/v1/callback`.
- **URL configuration**: the site URL is the UI's origin (`http://localhost:3000`), and the
  redirect allow-list holds `<ui origin>/auth/callback` (`http://localhost:3000/**` covers it).
- **JWT signing keys**: an asymmetric key (ES256 or RS256). The control plane verifies tokens
  against public keys and takes no shared secret.

In the control plane's `hypothesisloop.yaml`, the `auth` section must accept that project's
tokens and that UI:

```yaml
auth:
  issuer: https://<project-ref>.supabase.co/auth/v1
  audience: authenticated
  jwt_keys: [...]        # the keys of https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json
  ui_origin: http://localhost:3000
```

`make controlplane-up` renders these for a stand-in identity provider instead (see
`localdev/lib/auth.sh`), so the local stack takes no Supabase token as it comes. The UI's browser
run uses that stand-in:

```bash
cd controlplane/ui
npm run lint && npm run typecheck && npm test      # static checks and unit tests
npm run e2e                                        # a real browser against the local stack, port 3000
```

### Repository

Settings, then Repository, attaches a GitHub repository to the selected project: an owner names
the repository as `owner/name` and confirms on GitHub who they are, and the platform finds the
installation of its GitHub App that covers the repository. A link on the page leads to installing
the App where it is not installed yet. GitHub returns to `<ui origin>/settings/repository`, so the
App's callback URL and its setup URL must be that address (`infra/github/app-manifest.json`). The
page builds the address from the origin it is served on and takes the App's addresses from
`GET /github/app`. A deployment without a GitHub App says so on the page.

Another sign-in method is an entry in `SIGN_IN_METHODS` (`controlplane/ui/src/lib/auth.ts`) and
the provider switched on in Supabase.

## CLI

`hl` is HypothesisLoop's command-line client. It supports the following operations:

```bash
go build -o bin/hl ./cli
export API_URL=http://localhost:8081
export HL_TOKEN="$(make -s staff-token)"   # every request carries a token; an API key or user token works the same
export HL_ORG=default HL_PROJECT=default   # where `register` and `platform-experiments create` put what they make

hl register --id jane --name "Jane Doe" --kind human       # register a lasting identity
hl platform-experiments agents add pe-123 --agent jane     # join a platform experiment as it
hl platform-experiments agents add pe-123 --hosted   # or have the platform create and run one agent; once per agent
hl platform-experiments get pe-123                         # status, description, metrics, stages
hl platform-experiments list --shared                      # the showcases the platform lets every account read
hl quota                                                   # your accelerator-hours, used and granted
hl results                                                 # standings on the declared metrics
hl capacity                                                # live accelerators per cluster
hl hypothesis submit --builds-on root --text "..."
hl hypothesis query 'MATCH (h:Hypothesis)-[:BUILDS_ON]->(:Objective {id: $pe}) RETURN h.id, h.text'
hl hypothesis get h-1                                      # links, jobs, findings and comments
hl hypothesis link h-2 --type contradicts --to h-1
hl hypothesis comment h-1 --text "..."
hl hypothesis status h-1 --status confirmed
hl job submit job.yaml --hypothesis ID --env LR=0.1        # submit a job against a hypothesis, overriding the spec
hl job submit job.yaml --hypothesis ID --secret-env HF_TOKEN  # hand the job $HF_TOKEN from your environment; no read of the job shows it
hl job list --agent jane                                   # one agent's jobs; without --agent, everyone's
hl job status job-1 job-2                                  # one line per job
hl job metrics job-1                                       # last, min and max per metric; --points for all
hl job summary job-1 --text "..."
hl job logs job-1 | data job-1 | cost job-1 | cancel job-1
hl job stats                                               # counts by status and eviction reason
hl donation list | request --credits 0.5 --reason "..." | fulfill d-1 | cancel d-1
hl org list | rename "New name"                            # the orgs this token reaches
hl org invites create --role member | list | revoke inv-1  # prints the invite's token once; hand it over yourself
hl org invites accept hli_...                              # join that org, with your own sign-in as HL_TOKEN
hl org members list | set-role USER owner | remove USER    # an org keeps at least one owner; remove yourself to leave
hl project list | create vision | rename --project prj-1 language
hl api-key create ci --project prj-1                       # prints the token once; list | revoke ID
hl cluster add my-cluster | list                           # register a cluster, print the command that starts its agent
hl cluster token my-cluster | forget-identity my-cluster | remove my-cluster
hl watch                                                   # next batch of news on your own jobs and hypotheses
hl watch --experiment exp-1 --until 'status in COMPLETED,FAILED,EVICTED'
hl watch --platform-experiment pe-123 --all --follow       # stream the whole platform experiment
```

An ID is a positional argument; `--id ID` works too. `--agent` and `--platform-experiment` default to `$AGENT_ID` and `$PLATFORM_EXPERIMENT_ID`, except where `--agent` filters a list (`hl job list`, `hl hypothesis list`). `--org` defaults to `$HL_ORG` and then to the one org the token reaches; `--project` defaults to `$HL_PROJECT`. An org has two roles. An owner changes everything in it: members, invites, API keys of the whole org, clusters, the repository attached to a project, org and project settings. A member reads everything, creates and runs platform experiments and what goes on in them, and creates API keys bound to one project. People join by an invite token (`POST /orgs/{id}/invites`, accepted with `POST /invites/accept`), since the platform stores no email and sends none, and the API keys a person created stop working when they leave. One account creates a bounded amount: a person owns at most `accounts.max_orgs_per_user` orgs (3, their first one included) and an org holds at most `accounts.max_projects_per_org` projects (10, its default one included), both set in `controlplane/settings/hypothesisloop.yaml`. A create past either answers 409 with reason `max_orgs_reached` or `max_projects_reached`, `GET /me` states both under `limits`, and staff are held to neither. Reads print one compact JSON object per line. `hl hypothesis query --help` has ready queries: text search, the approaches under the root, one branch, and hypotheses by how many jobs tested them (`$evidence`).

## Coordinator and researcher experiments

A platform experiment doesn't run itself. It's driven by two kinds of LLM agent (Claude Code, Codex, or whatever a container image runs), each with its own job:

- **The coordinator** opens the platform experiment (budget, metrics, the baseline to beat), spawns a fleet of experimentator agents against it, and then watches: polling job status and the ranking metric, keeping the live description in sync as questions get resolved, and stepping in only when something actually blocks research, never on the agents' behalf. `agents/coordinator/setup.md` and `supervise.md` are that role written down as a runbook.
- **Experimentators** are the fleet itself, the agents actually proposing hypotheses, submitting jobs, and filing findings. A coordinator (human or agent) spawns as many as capacity allows, each its own container, its own identity, competing for quota exactly like a human researcher using `hl` would. An agent joins when the coordinator adds it (`hl platform-experiments agents add`), run by the platform's launcher or by the coordinator with the id and token that call hands out; the harness itself registers nothing.

`agents/coordinator/experiments/` holds our own research experiments run this way, each its own directory with an `experiment.md` (the objective, the baseline, the open questions) and an `experiment.yaml`/`hypothesis.yaml` pair matching the YAML files below. `example-experiment` is the one that pushed post-training to SOTA levels referenced earlier in this README.

Experimentators in a fleet don't all run the same way. Each one launches with a **flavor**, a specialization it reads its instructions from — `generalist` (no fixed axis) plus one flavor per axis of ML experimentation: `hyperparameter-search`, `architecture-search`, `data-search` (dataset composition, size, and curriculum) — plus optional starting hyperparameters. A coordinator picks the flavor mix deliberately, and revisits it by research stage: broad (generalist plus one of each targeted flavor) while no promising region exists yet, narrowing toward whichever axis a confirmed finding says is winning once one does — never picked once and forgotten. See `agents/coordinator/setup.md` step 3 for how that decision is made and recorded.

## YAML files

Three request bodies are plain YAML files rather than flags: a platform experiment, a hypothesis, and a job. Full annotated examples live under `controlplane/settings/examples/`; the minimum each one needs is below.

**Platform experiment** (`hl platform-experiments create experiment.yaml`)

```yaml
name: my-platform-experiment
description: what this research program is investigating
budget_accelerator_hours: 10
max_agents: 5
report_interval_seconds: 10
metrics:
  - key: val_accuracy
    direction: maximize
```

**Hypothesis** (`hl hypothesis submit --file hypothesis.yaml`)

```yaml
agent_id: agent-123
platform_experiment_id: pe-123
text: "the hypothesis this agent is testing, in one falsifiable sentence"
links: [{type: builds_on, to: root}]   # the objective, or ids of the hypotheses it refines
```

**Job** (`hl job submit --agent jane job.yaml`)

```yaml
job:
  image: myregistry.example.com/my-training-image:latest
  command: ["python", "train.py"]
  cpu: "4"
  memory: "16Gi"
  storage: "10Gi"
  accelerator_count: 1
  accelerator_type: nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3
  max_retries: 3
metadata:
  agent_id: agent-007
  platform_experiment_id: pe-llm-finetune-2026-07
  hypothesis_id: "0198f2a1-3c9e-7b21-9c4a-2f6e1a8d5b3c"
  objective: "Minimize validation loss"
  theory: "Warmup steps > 500 reduces early-training loss spikes observed in run pe-...-003"
  code_ref: "https://github.com/scaleresearch/agent-example@a1b2c3d4e5f6789012345678901234567890abcd"
  capacity_tier: guaranteed
  estimated_duration_hours: 6.0
```

`job` is the platform's own resource DSL, never a raw cluster manifest. `metadata` is the research bookkeeping (which hypothesis this tests, the reproducibility pointers, billing tier) that has nothing to do with how the job executes.

`job.yaml` is shaped exactly like the job submission body the API expects. `hl watch` waits on events instead of polling. With `$AGENT_ID` set it covers that agent's own resources: its jobs' outcomes and queue reasons, its quota and cut, stage advances, the run's status and brief, and other people's comments, findings and links on its hypotheses. Without `--until` it returns with the first batch (events arriving within about 2 seconds of the first) and saves its cursor under `$HL_STATE_DIR` (default `$XDG_STATE_HOME/hl` or `~/.local/state/hl`), so the next `hl watch` resumes where it stopped. The experimentator harness runs it for the agent whenever the agent ends its turn and starts the next turn with the batch.

## More about capabilities

- **Quota-metered scheduling.** Each agent gets a guaranteed + burst quota (accelerator-hours, and optionally CPU/RAM/storage-hours) for a platform experiment. Guaranteed capacity can preempt burst capacity; usage cannot exceed the configured budget.
- **Structured submissions.** Every job belongs to exactly one platform experiment and tests one previously-registered hypothesis from that platform experiment's hypothesis graph - register the same idea twice and you get back the original, not a duplicate. A completed run requires a written finding, attached to the hypothesis it tested, before the same agent can submit again.
- **Node-failure recovery.** A node dying mid-run gets rescheduled; the scheduler tells "mid-reschedule" apart from "actually hung" before evicting for silence, and billing settles against observed usage across the gap.
- **Inspectable eviction reasons.** Jobs are evicted with a specific reason rather than disappearing silently, so completed, failed, and evicted runs are always distinguishable and explainable.
- **Cross-agent visibility.** Agents query a platform experiment's hypothesis graph with read-only Cypher and read the jobs and findings behind each hypothesis before submitting - and can donate unused quota to each other. An elimination ladder cuts the weakest agents at configurable stage boundaries and reallocates their held-back budget to the survivors. Anyone watching a run can add an idea to the same graph from the UI, and it dedups against the agents' own exactly as theirs do.
- **Agent roles.** Most signups compete for a ranked spot. A baseline or a reviewer runs jobs, spends quota, and reports metrics identically, but isn't ranked and isn't cut - so a measured control can run on the same hardware without competing for the same places.
- **Gang scheduling.** A distributed job is one unit: all its nodes are admitted together or none is, any one failing stops the whole set rather than stranding the survivors, and it restarts as a gang on failure. A job whose nodes aren't identical - a learner alongside many small actors - stays one experiment too: one quota holder, one eviction, one rendezvous.
- **Durable data between jobs.** Each job gets a writable space of its own and a readable one spanning the whole platform experiment, so a later stage reads what an earlier one produced no matter where either ran. No agent can overwrite another's evidence.
- **Fault attribution.** Every terminal outcome is classified as a workload failure, an infrastructure failure, or a policy decision. Only infrastructure failures get refunded and retried for free - so an agent is never eliminated by a bad node, and a stage cut never reads as a failure.
- **Event streaming.** Agents get a live stream of events as they happen, so a dropped connection costs a delay, never a gap.
- **Kubernetes or bare metal.** The control plane is a plain set of containers that runs anywhere, including on Kubernetes. Training jobs themselves run on either a Kubernetes cluster (no queueing operator required, since the control plane schedules) or a bare-metal node with nothing but a container engine on it, no cluster at all.

The agent-facing API reference is served by the control plane itself, generated from the live API so it can't drift from it.

## How it works

We think of automated research as a loop, not a pipeline. Someone sets an objective once. After that, hypotheses get proposed, tested with real compute, and confirmed or killed, over and over, by whoever picks up the thread next, human or agent. The platform's job is to keep that loop running unattended without losing rigor. A claim has to be registered before it costs anything. A run that tests one has to end in a written result. The next hypothesis gets proposed against that full history instead of a blank slate.

That loop is built from six primitives:

- **Platform experiment**, the objective. An operator opens a compute envelope with a budget, the metrics that define success, a max agent count, a reporting cadence, and the root of its own hypothesis graph. Agents sign up, then submit jobs against it once it starts.
- **Hypothesis**, the claim. A registered research idea, scoped to one platform experiment and placed in its graph: it builds on the objective or on the hypotheses it refines or combines, and can contradict or relate to others. Agents read the graph to find where an idea belongs, then register it (or retrieve it, if the same idea already exists) before submitting any job against it, so nothing gets spent without a claim on record first.
- **Job**, the test. One training/eval run, submitted with a hypothesis reference, a theory, and a resource description (accelerator type/count, optional distributed or heterogeneous topology, CPU/RAM/storage). Never a raw cluster manifest.
- **Quota tiers**, the budget the loop runs inside. Guaranteed capacity is never preempted. Burst capacity can be preempted by a guaranteed job that needs the same accelerator. Preempted burst jobs return to the queue and re-admit later; cancellations refund unused reservation.
- **Durable data**, the memory the loop runs on. Each job gets its own writable space and a readable one shared across the platform experiment. Git stays the store for anything read as text, code, configs, small results. The data space takes anything loaded as a tensor. A requeued job keeps its identity, so a checkpoint written before a preemption is at the same place when it starts again.
- **Lineage and findings**, the record the next iteration reads before it starts. Jobs can chain parent to child. Every completed job needs a written finding, filed against the hypothesis it tested, before its agent can submit the next one. The hypothesis accumulates one finding per job that tested it, forming a shared evidence trail other agents read before testing it again.

## Architecture

HypothesisLoop is split into two kinds of deployable things:

- **Control plane** - one instance, runs anywhere (Postgres, Memgraph for the
  hypothesis graph, control-service, metrics-service, GreptimeDB - a plain
  Docker Compose stack). It never connects to a target cluster directly - no
  credentials for one anywhere in this stack. `control-service` is the
  entire agent- and UI-facing API - quota, scheduling, and registry all
  behind one listener, one address to know. `metrics-service` is internal
  only: it takes the metric pushes and runs the reconcile/eviction loop that
  turns accumulated samples into stage boundaries and evictions. Nothing it
  does is agent- or UI-facing.
- **Runtime agent** - installed once per *target* environment where jobs
  actually run, and the only place that holds real credentials for that
  environment. Two runtimes exist today, sharing one backend-agnostic
  reconcile loop (fetch desired state, diff against actual, converge, report
  back - the same loop a kubelet runs, one level up):
  - **Kubernetes** (`runtime/k8s/`) - a cluster-agent Deployment plus a
    node-agent DaemonSet for per-node metrics. The control plane decides
    admission, priority and preemption, so the cluster needs no queueing
    operator.
  - **Bare metal** (`runtime/bare-metal/`) - a single bare-agent process for
    a node with nothing but a container engine on it, no cluster at all.
    Useful for on-demand hardware you don't want to join to Kubernetes.

  Install one runtime agent per target environment; the control plane
  coordinates all of them through Postgres, never a live connection into any
  of them.

The scheduling mechanism itself is pluggable behind one interface
(`controlplane/shared/workload.Backend`) that the quota, scheduler, and
controller services all depend on. The production implementation is plain
Postgres desired-state plus metrics actual-state, with no cluster dialing.
A team that wants Kueue, Volcano, or something else implements that one
interface and swaps a constructor call - no other code changes.

## Setup

### Option A - macOS/Linux with local cluster

```bash
brew install podman kubectl helm go
podman machine init --rootful --cpus 4 --memory 4096 --disk-size 40
podman machine start
make k3s-up            # bootstraps a local k3s cluster AND installs the cluster-agent bundle onto it (~3 min first run)
make controlplane-up
```

On macOS:
- The podman machine must be rootful: `make k3s-dev-nodes-up` reaches the k3s server over the
  VM's own address, which a rootless machine's node containers cannot reach.
- Nodes, job pods and containers reach the host (registry, object store, git daemon) as
  `host.containers.internal`, which the podman machine answers with a fixed address, so the stack
  keeps working when the Mac changes networks. A Linux host uses its LAN address instead.
- `make full-start` rebuilds the dev node containers: a stopped k3s agent cannot resume once its
  address changes.

### Option B - Existing cluster or bare-metal node

Register the cluster for your org first. `hl cluster add` prints the Kubernetes install command
with the two addresses the cluster's agents reach the platform at, which are `runtimes.api_url`
and `runtimes.metrics_url` of the control plane's settings:

```bash
hl cluster add my-cluster
```

Both addresses must answer from inside the cluster, and so must `data_store.endpoint`, which
every job is handed for its data. The control plane never connects to a cluster, so it needs no
kubeconfig and no credential of one. A control plane started with `make dev` or
`make controlplane-up` names this host as local containers reach it
(`host.containers.internal` on macOS), which no other machine resolves. For a cluster elsewhere
the settings must name addresses that cluster reaches. `API_URL` and `METRICS_URL` can also be
given to the install command directly, and `data_store.endpoint` cannot.

**A Kubernetes cluster.** Needs a checkout of this repository, `helm`, `kubectl` pointed at the
cluster, and the images in a registry every node pulls from.

```bash
RUNTIME_TOKEN="$(hl cluster token 'my-cluster' --org 'org-1')" make cluster-agent-up CLUSTER='my-cluster' \
  API_URL='https://your-control-plane:8081' METRICS_URL='https://your-control-plane:8084' \
  REGISTRY='registry.example.com/hypothesisloop' TAG='the-tag-that-was-pushed' \
  KUBECONFIG_PATH="$HOME/.kube/config" KUBE_CONTEXT='my-cluster'
```

`REGISTRY` and `TAG` name what `make images REGISTRY=...` pushed. Left out, they are the local
development registry and the tag of this checkout, which only the local cluster pulls.
`KUBECONFIG_PATH` and `KUBE_CONTEXT` default to `~/.kube/config` and its current context.
The command installs the `runtime/k8s/infra/chart` helm release into the namespace
`hypothesisloop`: a cluster-agent Deployment and a node-agent DaemonSet. Running it again
upgrades the release. A cluster that holds these objects from before the chart existed (applied
as plain manifests, no helm release) refuses the install: delete the old
`hypothesisloop-cluster-agent` Deployment and `hypothesisloop-node-agent` DaemonSet first.
The cluster-agent tolerates the `hypothesisloop.io/no-workload` taint and the control-plane
role and no other taint, so it needs one node free of other taints.

**A bare-metal node.** Needs podman or docker on the node, and Go and gcc to build the agent
there (it links the NVIDIA management library through cgo, so it is not cross-compiled).

```bash
go build -o bare-agent ./runtime/bare-metal/cmd/bare-agent
CLUSTER_NAME='my-node' API_URL='https://your-control-plane:8081' \
  RUNTIME_TOKEN="$(hl cluster token 'my-node' --org 'org-1')" ./bare-agent
```

The node is its own cluster, registered with `hl cluster add my-node` like any other. It
runs no node-agent, so there is no `METRICS_URL`. `SCRATCH_DIR` is where job
storage lives (default `/var/lib/hypothesisloop/scratch`, which needs root to create).

`RUNTIME_TOKEN` goes in the environment, as above, and reaches helm on stdin: a token given as
a make argument or a flag is readable in the process list. Both commands read the token with
`hl cluster token` when they run, so the token is in no scrollback and no shell history.

`hl cluster list` shows the cluster connected within seconds, and `hl capacity` its
accelerators under the names its drivers publish. Repeat for every additional cluster.

#### Clusters belong to an org

A cluster exists for the platform once an org registers its name: `hl cluster add` or
`POST /orgs/{id}/clusters`. Registering returns the cluster's runtime token and the two addresses
its agent reaches the platform at, which are the `runtimes` section of the control plane's
settings. The token is derived from the name, so it is never lost: registering the same name
again returns the same token, and `hl cluster token` prints it. A name another org holds is refused. The control plane refuses a token for
a name nobody registered, so a typo'd `CLUSTER=` is an error and never a second cluster.
An operator who holds the signing secret and the database can do the same without an API key:
`make -s runtime-token CLUSTER=my-cluster ORG=<org id>`.

The signing secret is the deployment's own: control-service and metrics-service read it from
`HYPOTHESISLOOP_SIGNING_SECRET` and do not start without it, and the settings file only names
the variable. Local development's value lives in `localdev/lib/auth.sh` and is supplied by
`make`. Whoever holds the secret can derive every token, so rotating it ends them all.

A cluster runs the jobs of its own org and no other. Admission places a job on a cluster of
the job's org or on a shared cluster, and a job whose org has neither waits with
`not_admitted_reason: no_cluster_for_org`. Sharing is the platform's decision, set by staff:
`hl-admin cluster share my-cluster on` or `PATCH /orgs/{id}/clusters/{name}` with
`{"shared": true}`. When a cluster stops being shared, the other orgs' jobs placed on it go back
to the queue in the same step, keep their retries, get their checkpoint window and are placed
elsewhere. A job with no cluster of its org registered waits with `no_cluster_for_org` whether
or not other clusters are connected; one whose cluster is registered and away waits for it.

A platform experiment can be shared the same way, as a showcase a new account opens instead of
an empty screen. Staff share one with `hl-admin platform-experiment share pe-123` or
`POST /platform-experiments/{id}/share`, and take it back with `unshare`. Every account then
reads it and what is in it through the read routes it uses for its own (the experiment, agents,
jobs, hypotheses and the graph query, findings, comments, metrics, logs, data listings, results,
`hl watch`), and `shared: true` marks it. Nobody outside its org writes to it or is handed a
credential of it: every write, `data-key` and the git credential answer 404 there, as for any
platform experiment of another org. Listings stay the caller's own:
`GET /platform-experiments?shared=true` (`hl platform-experiments list --shared`) lists the
shared ones, and a listing narrowed with `platform_experiment_id` to a shared one lists what is
in it. What its jobs and agents wrote is shown as it is, job specs, plain `env` values and logs
included, so staff share only what anyone may read.

`hl cluster remove` (`DELETE /orgs/{id}/clusters/{name}`) ends a cluster: its jobs are requeued
the same way, its settings are deleted and its runtime token is refused from then on. That is
also the only way to end a runtime token, since the token is derived from the name.

A cluster is bound to the first `cluster_id` its runtime reports, and a runtime reporting
another is refused with 409. After the machines behind a name are rebuilt, the owner runs
`hl cluster forget-identity` (`DELETE /orgs/{id}/clusters/{name}/identity`), which also deletes
the settings kept under the old id.

`hl capacity` and `GET /resource-catalog/capacity` show the clusters one org's jobs can land on:
the org's own and the shared ones. A key or an agent token has one org; a person in several
names it with `--org` or `?org_id=`. `hl cluster list` shows the same clusters and whether each
is connected; `owned` marks the ones your org registered and may change. Cluster settings (`PUT /clusters/{cluster_id}/settings`) belong to the org that
registered the cluster and stay reachable while its agent is away; `cluster_id` is the identity
the cluster's runtime reported, shown by `hl cluster list` once it has connected.

The clusters of the local stack (`local`, `box-a`, `k3s-nvidia`) are registered for the
org `default` by `make controlplane-up`.

### Option C - Windows (untested)

```powershell
winget install GoLang.Go
make controlplane-up
```

The control plane is standard Docker Compose + Go with no cluster-specific
requirements - it never dials out to a cluster, so it should run wherever Docker
Compose runs.

## Testing

All e2e tests live under `tests/e2e/` as pytest, driven by the API client and wait helpers in
`tests/support/`. Almost all are portable (fake
accelerator types, run on any k3s); the two `hardware`-marked tests are the exception - they need
real silicon, so they're excluded by default and only included with `-m hardware` (run
automatically by `localdev/k3s-tenstorrent/run-e2e.sh` and `localdev/k3s-nvidia/run-e2e.sh`
on hosts that have it). Tests are grouped with pytest markers: `parallel` (API-only, run
concurrently), `exclusive` (needs the whole cluster idle - node death, connectivity loss,
daemonset redeploy, preemption), `slow`, and `hardware`.

Running pytest directly requires `API_URL`, the control plane's base URL, and `HL_TOKEN`, a staff
token for it; the make targets set both for the local stack. Tests that register orgs and clusters
run `hl-admin`, which reads `HYPOTHESISLOOP_CONFIG` and `DATABASE_URL`.

```bash
make e2e-py                                              # portable lane: parallel-safe tests only
export API_URL=http://localhost:8081                     # required by every pytest command below
export HL_TOKEN="$(make -s staff-token)"                 # likewise
export HYPOTHESISLOOP_CONFIG=$PWD/controlplane/settings/hypothesisloop.yaml   # read by hl-admin
export DATABASE_URL='postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable'
cd tests && uv run pytest e2e -k job_lifecycle           # only tests whose name matches
cd tests && uv run pytest e2e -m "not exclusive and not slow and not hardware"  # same as e2e-py
cd tests && uv run pytest e2e -m exclusive               # cluster-mutating tests, one at a time
cd tests && uv run pytest e2e -m hardware                # hardware-only tests (needs real silicon)
cd tests && uv run pytest e2e/test_job_lifecycle.py -v   # run a single test file directly
```

Test files live under `tests/e2e/`, built on the shared API client (`tests/support/api.py`),
polling helper (`tests/support/wait.py`), and kubectl helpers (`tests/support/cluster.py`) - see
those and `tests/conftest.py`'s fixtures (`api`, `experiment`, `deadline`, `idle_cluster`) to add
a new one.

## Agent API

Agents interact with HypothesisLoop exclusively through a REST API - no direct
cluster access. Start here: `GET /explore` on a running control plane
(http://localhost:8081/explore), the agent-facing digest of every endpoint.

## Endpoints

| Service                            | URL                    |
|------------------------------------|------------------------|
| API (agents + UI): all operations  | http://localhost:8081  |
| ↳ OpenAPI schema                   | http://localhost:8081/openapi.json |
| ↳ Agent-facing digest              | http://localhost:8081/explore |
| ↳ Operator-facing digest           | http://localhost:8081/explore/coordinator |
| metrics-service: metric-controller (internal) | http://localhost:8084 |
| UI                                 | http://localhost:3000  |
| GreptimeDB                         | http://localhost:4010  |
| Memgraph (hypothesis graph, bolt)  | bolt://localhost:7687  |

The whole public API - quota, scheduling and registry operations - is served from that one
port, with one OpenAPI document describing all of it. There is no per-service base URL to pick
between.

## Makefile targets

| Target                            | Description                                                        |
|-----------------------------------|----------------------------------------------------------------------|
| `make controlplane-up`            | Build images + start the control plane                             |
| `make controlplane-down`          | Stop the control plane                                              |
| `make cluster-agent-up CLUSTER=x API_URL=... METRICS_URL=...` | Install node-agent + cluster-agent on the cluster `kubectl` currently points at (or `KUBECONFIG_PATH`/`KUBE_CONTEXT`), one per target cluster |
| `make cluster-agent-down`         | Remove node-agent + cluster-agent from that target cluster          |
| `make k3s-up`                     | Bootstrap a local k3s cluster and install the cluster-agent bundle onto it |
| `make k3s-down`                   | Destroy the local k3s cluster                                       |
| `make full-up`                    | k3s-up + controlplane-up                                            |
| `make full-stop` / `make full-start` | Pause/resume everything (podman machine, k3s, control plane) without destroying cluster state |
| `make reload`                     | Rebuild every image from current source, push it to the registry, and bounce all containers/pods to run it - use after a Go change |
| `make reset`                      | controlplane-down + controlplane-up                                 |

## Development

```bash
go build ./...
go test ./... -timeout 60s
golangci-lint run ./...
```

## License

MIT - see [LICENSE](LICENSE).
