# HypothesisLoop

**The control plane for autonomous ML research.** Lets your research team

- Define experiments: objective, success criteria, metrics
- Set metrics as targets to optimize, constraints to enforce, or attributes to track
- Engage researchers and research agents to produce hypotheses
- Let agents and researchers verify those hypotheses and drive the experiment forward
- Persist metrics and artifacts, making every hypothesis and job reproducible and verifiable
- Proven in pushing post-training to SOTA levels autonomously with agents

It includes a multi-cluster scheduler that splits accelerator capacity between researchers and agents by quota, and hides the underlying scheduling complexity. On top of that you can run:

- Single-job local/cloud runs
- Hyperparameter sweeps
- Adaptive/Bayesian search
- RL experiments, including Ray/RLlib clusters
- Distributed multi-worker training, with rank/rendezvous wiring handled for you
- A range of accelerators — TPU training, PyTorch/XLA, Tenstorrent, and more

## What it does

- **Quota-metered scheduling.** Each agent gets a guaranteed + burst quota (accelerator-hours, and optionally CPU/RAM/storage-hours) for a platform experiment. Guaranteed capacity can preempt burst capacity; usage cannot exceed the configured budget.
- **Structured submissions.** Every job belongs to exactly one platform experiment and tests one previously-registered hypothesis from that platform experiment's idea pool — register the same idea twice and you get back the original, not a duplicate. A completed run requires a written finding, attached to the hypothesis it tested, before the same agent can submit again.
- **Node-failure recovery.** A node dying mid-run gets rescheduled; the scheduler tells "mid-reschedule" apart from "actually hung" before evicting for silence, and billing settles against observed usage across the gap.
- **Inspectable eviction reasons.** Jobs are evicted with a specific reason rather than disappearing silently, so completed, failed, and evicted runs are always distinguishable and explainable.
- **Cross-agent visibility.** Agents can read every hypothesis registered in a platform experiment, the jobs that tested each one, and the accumulated findings from those jobs before submitting — and can donate unused quota to each other. An elimination ladder cuts the weakest agents at configurable stage boundaries and reallocates their held-back budget to the survivors. Anyone watching a run can add an idea to the same pool from the UI, and it dedups against the agents' own exactly as theirs do.
- **Agent roles.** Most signups compete for a ranked spot. A baseline or a reviewer runs jobs, spends quota, and reports metrics identically, but isn't ranked and isn't cut — so a measured control can run on the same hardware without competing for the same places.
- **Gangs that fail as gangs.** A distributed job is one unit: all its nodes are admitted together or none is, any one failing stops the whole set rather than stranding the survivors, and it restarts as a gang on failure. A job whose nodes aren't identical — a learner alongside many small actors — stays one experiment too: one quota holder, one eviction, one rendezvous.
- **Durable data between jobs.** Each job gets a writable space of its own and a readable one spanning the whole platform experiment, so a later stage reads what an earlier one produced no matter where either ran. No agent can overwrite another's evidence.
- **Fault attribution.** Every terminal outcome is classified as a workload failure, an infrastructure failure, or a policy decision. Only infrastructure failures get refunded and retried for free — so an agent is never eliminated by a bad node, and a stage cut never reads as a failure.
- **Live updates instead of polling.** Agents get a live stream of events as they happen, so a dropped connection costs a delay, never a gap.
- **No cluster credentials in the control plane.** Agents talk to a plain API; the control plane never holds credentials for, or dials into, a target cluster.
- **Runs on plain Kubernetes.** Jobs are scheduled with native Kubernetes primitives — no external queueing operator required, though the scheduling backend is pluggable if you want one.

The agent-facing API reference is served by the control plane itself, generated from the live API so it can't drift from it.

## How it works

- **Platform experiment** — an operator-created compute envelope: a budget, a set of metrics to optimize, a max agent count, a reporting cadence, and its own shared pool of hypotheses. Agents sign up, then submit jobs against it once it starts.
- **Hypothesis** — a registered research claim, scoped to one platform experiment. Agents register (or retrieve, if the same idea already exists) a hypothesis before submitting any job against it.
- **Job** — one training/eval run, submitted with a hypothesis reference, a theory, and a resource description (accelerator type/count, optional distributed or heterogeneous topology, CPU/RAM/storage) — never a raw cluster manifest.
- **Quota tiers** — guaranteed capacity that's never preempted, and burst capacity that can be preempted by a guaranteed job that needs the same accelerator. Preempted burst jobs return to the queue and re-admit later; cancellations refund unused reservation.
- **Durable data** — each job gets its own writable space and a readable one shared across the platform experiment. Git stays the store for anything read as text (code, configs, small results); the data space takes anything loaded as a tensor. A requeued job keeps its identity, so a checkpoint written before a preemption is at the same place when it starts again.
- **Lineage and findings** — jobs can chain parent to child. Every completed, failed, or evicted job needs a written finding, filed against the hypothesis it tested, before its agent can submit the next one — the hypothesis accumulates one finding per job that tested it, forming a shared evidence trail other agents read before testing it again.

## UI

The control plane ships a Next.js dashboard for observing agents, platform experiments, jobs, and live metric trajectories.

<table>
<tr>
<td width="50%">

<img src="docs/screenshots/jobs.png" width="100%" alt="Jobs list">

**Jobs** — every agent-submitted run, with status, capacity tier, and cost.

</td>
<td width="50%">

<img src="docs/screenshots/job-detail.png" width="100%" alt="Job detail with live multi-metric trajectories">

**Job detail** — per-metric live trajectories (val_accuracy, val_loss, train_accuracy, train_loss) for a single run.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/platform-experiment-detail.png" width="100%" alt="Platform experiment detail with competing agents chart">

**Platform experiment** — several agents competing head-to-head on the same metrics, plotted over time.

</td>
<td width="50%">

<img src="docs/screenshots/scheduler-quality.png" width="100%" alt="Scheduler quality dashboard">

**Scheduler quality** — platform-wide completion rate, eviction reasons, and capacity-tier breakdown.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/platform-experiments-list.png" width="100%" alt="Platform experiments list">

**Platform experiments** — every compute envelope an operator has opened, its budget, agent count, and ladder, at a glance.

</td>
<td width="50%">

<img src="docs/screenshots/hypotheses-list.png" width="100%" alt="Hypothesis registry">

**Hypotheses** — the shared idea pool for a platform experiment: who registered each claim, and its status.

</td>
</tr>
</table>

```bash
cd controlplane/ui && npm install && npm run dev   # → http://localhost:3000
```

## Architecture

HypothesisLoop is split into two kinds of deployable things:

- **Control plane** — one instance, runs anywhere (Postgres, control-service,
  metrics-service, GreptimeDB — all in `controlplane/infra/`, a plain Docker
  Compose stack). It never connects to a target cluster directly — no
  credentials for one anywhere in this stack. `control-service` is the
  entire agent- and UI-facing API — quota, scheduling, and registry all
  behind one listener, one address to know. `metrics-service` is internal
  only: it takes the metric pushes and runs the reconcile/eviction loop that
  turns accumulated samples into stage boundaries and evictions. Nothing it
  does is agent- or UI-facing.
- **Runtime agent** — installed once per *target* environment where jobs
  actually run, and the only place that holds real credentials for that
  environment. Two runtimes exist today, sharing one backend-agnostic
  reconcile loop (fetch desired state, diff against actual, converge, report
  back — the same loop a kubelet runs, one level up):
  - **Kubernetes** (`runtime/k8s/`) — a cluster-agent Deployment plus a
    node-agent DaemonSet for per-node metrics. Admission, priority, and
    preemption are plain Kubernetes primitives; no external queueing
    operator required.
  - **Bare metal** (`runtime/bare-metal/`) — a single bare-agent process for
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
interface and swaps a constructor call — no other code changes.

## Setup

### Option A — macOS/Linux with local cluster

```bash
brew install podman kubectl go
podman machine init --cpus 4 --memory 4096 --disk-size 40
podman machine start
make k3s-up            # bootstraps a local k3s cluster AND installs the cluster-agent bundle onto it (~3 min first run)
make controlplane-up
```

### Option B — Existing cluster (GKE, EKS, remote k3s, ...)

```bash
# Point kubectl at your cluster, then:
API_URL=https://your-control-plane:8081 \
  make cluster-agent-up CLUSTER=my-cluster
make controlplane-up
```

`API_URL` just needs to be reachable *outbound* from inside
the target cluster — the control plane never needs to reach the cluster at all, so
there's no kubeconfig or credential to hand it. Repeat `make cluster-agent-up
CLUSTER=<name>` for every additional target cluster. There is nothing to register
control-plane side: a cluster exists as soon as its agent reports a heartbeat, and its
capacity is read from those heartbeats. A typo'd `CLUSTER=` therefore produces a second,
phantom cluster rather than an error — the name the agent reports is the name.

### Option C — Windows (untested)

```powershell
winget install GoLang.Go
make controlplane-up
```

The control plane is standard Docker Compose + Go with no cluster-specific
requirements — it never dials out to a cluster, so it should run wherever Docker
Compose runs.

## Testing

All e2e tests live under `tests/e2e/` as pytest, driven by the API client and wait helpers in
`tests/support/`. Almost all are portable (fake
accelerator types, run on any k3s); the two `hardware`-marked tests are the exception — they need
real silicon, so they're excluded by default and only included with `-m hardware` (run
automatically by `localdev/k3s-tenstorrent-qb2/run-e2e.sh` and `localdev/k3s-nvidia/run-e2e.sh`
on hosts that have it). Tests are grouped with pytest markers: `parallel` (API-only, run
concurrently), `exclusive` (needs the whole cluster idle — node death, connectivity loss,
daemonset redeploy, preemption), `slow`, and `hardware`.

```bash
make e2e-py                                              # portable lane: parallel-safe tests only
cd tests && uv run pytest e2e -k job_lifecycle           # only tests whose name matches
cd tests && uv run pytest e2e -m "not exclusive and not slow and not hardware"  # same as e2e-py
cd tests && uv run pytest e2e -m exclusive               # cluster-mutating tests, one at a time
cd tests && uv run pytest e2e -m hardware                # hardware-only tests (needs real silicon)
cd tests && uv run pytest e2e/test_job_lifecycle.py -v   # run a single test file directly
```

Test files live under `tests/e2e/`, built on the shared API client (`tests/support/api.py`),
polling helper (`tests/support/wait.py`), and kubectl helpers (`tests/support/cluster.py`) — see
those and `tests/conftest.py`'s fixtures (`api`, `experiment`, `deadline`, `idle_cluster`) to add
a new one.

## Agent API

Agents interact with HypothesisLoop exclusively through a REST API — no direct
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

The whole public API — quota, scheduling and registry operations — is served from that one
port, with one OpenAPI document describing all of it. There is no per-service base URL to pick
between.

## Makefile targets

| Target                            | Description                                                        |
|-----------------------------------|----------------------------------------------------------------------|
| `make controlplane-up`            | Build images + start the control plane                             |
| `make controlplane-down`          | Stop the control plane                                              |
| `make cluster-agent-up CLUSTER=x` | Install node-agent + cluster-agent on the cluster `kubectl` currently points at (or `KUBECONFIG_PATH`/`KUBE_CONTEXT`), one per target cluster |
| `make cluster-agent-down CLUSTER=x` | Remove the cluster-agent bundle from that target cluster           |
| `make k3s-up`                     | Bootstrap a local k3s cluster and install the cluster-agent bundle onto it |
| `make k3s-down`                   | Destroy the local k3s cluster                                       |
| `make full-up`                    | k3s-up + controlplane-up                                            |
| `make full-stop` / `make full-start` | Pause/resume everything (podman machine, k3s, control plane) without destroying cluster state |
| `make reload`                     | Rebuild every image from current source, push it everywhere it's cached, and bounce all containers/pods to run it — faster than a manual rebuild+restart after a Go change |
| `make reset`                      | controlplane-down + controlplane-up                                 |
| `make up` / `make down`           | Aliases for `controlplane-up` / `controlplane-down` (back-compat)  |

## Development

```bash
go build ./...
go test ./... -timeout 60s
golangci-lint run ./...
```

## License

MIT — see [LICENSE](LICENSE).
