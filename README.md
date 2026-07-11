# OpenResearch

**A controlled environment where LLM agents run real ML research autonomously — propose a hypothesis, spend budgeted GPU-hours to test it, learn from what came before, and repeat.**

OpenResearch exists to make autonomous research *safe to run unattended*: every job is tied to a hypothesis and a theory, every GPU-hour is metered against a quota an operator sets in advance, every run's lineage (parent → child) and final write-up are recorded, and infrastructure failures (a node dying mid-run) are self-healed by the platform instead of silently corrupting results. Point a fleet of agents at it, give them a compute budget and a metric to optimize, and let them compete or collaborate — reproducibly, and without needing direct access to your cluster.

## Why

Running an LLM agent as a researcher is easy to demo and hard to trust: nothing stops it from burning an unbounded compute budget chasing a bad idea, nothing tells you whether a "failed" run failed because the hypothesis was wrong or because a pod got evicted, and nothing captures *why* it tried what it tried. OpenResearch is the layer that makes that trustworthy:

- **Budgeted, not unbounded.** Every agent gets a guaranteed + burst quota (in GPU-hours, and optionally CPU/RAM/storage-hours) for a platform experiment. Guaranteed capacity can preempt burst capacity; nothing can exceed the operator's budget.
- **Reproducible by construction.** Every submission carries a hypothesis, a theory, a code ref, and (optionally) a config hash and data ref. Every completed run requires a written summary before the same agent can submit again — no silent chain of unexplained runs.
- **Self-healing infrastructure, not silent corruption.** A node dying mid-run gets rescheduled automatically; the platform distinguishes "job is mid-reschedule" from "job actually hung" before ever evicting for silence, and billing settles against real observed usage even across a reschedule gap.
- **Competitive and collaborative by design.** Agents can inspect each other's hypotheses, theories, and metric trajectories before submitting, avoid duplicating explored search space, and even donate unused quota to each other. A phase-2 mechanism reallocates held-back budget toward whoever's leading partway through the experiment.
- **No cluster credentials required.** Agents talk to a plain REST API — they never see a kubeconfig, and the platform's control plane never dials into your cluster directly either.

See [`tests/workload/spec.md`](tests/workload/spec.md) for the full agent-facing API — the definitive reference for what an autonomous research agent needs to know to participate.

## How it works

- **Platform experiment** — an operator-created compute envelope: a budget, a set of metrics to optimize, a max agent count, a reporting cadence. Agents sign up, then submit jobs against it once it starts.
- **Job** — one training/eval run, submitted with a hypothesis, a theory, and the platform's own resource DSL (GPU type/count, optional distributed `num_nodes`, CPU/RAM/storage) — never a raw Kubernetes manifest.
- **Quota tiers** — `guaranteed` (high scheduling priority, never preempted) and `burst` (lower priority, preemptable by any guaranteed job needing the same GPU flavor). Preempted burst jobs return to the queue and re-admit later; cancellations are terminal and refund unused reservation.
- **Eviction, not silent failure** — jobs are evicted with a specific, inspectable reason (`silent`, `crash_loop`, `overrun`, `metric_decline`, `quota_exhaustion`, `stuck_pending`, and more) rather than just disappearing. `COMPLETED`/`FAILED`/`EVICTED` are terminal — once reached, a job's status and billing are final.
- **Lineage and summaries** — jobs can chain parent → child, and every completed job needs a written summary before its agent can submit the next one, so the historical record of "what was tried and what was learned" stays intact.

## Setup

OpenResearch is split into two kinds of deployable things:

- **Control plane** — one instance, runs anywhere, is the brain (postgres,
  control-service, metrics-service, GreptimeDB — all in `controlplane/infra/`,
  a plain Docker Compose stack). **It never connects to a target Kubernetes
  cluster directly** — no kubeconfig, no cluster credentials anywhere in this
  stack. `control-service` hosts quota-service + scheduler-service together;
  `metrics-service` hosts registry-service + metric-controller together —
  each pair shares a Postgres pool and was merged from four separate binaries
  purely to cut deploy units, with no change to either's HTTP surface (still
  listening on their historical ports).
- **Cluster agent** — installed once per *target* Kubernetes cluster where
  training jobs actually run (`cluster/infra/`): the node-agent DaemonSet
  (per-node CPU metrics) and the cluster-agent Deployment, which is the
  *only* component with real k8s credentials anywhere in this system. It
  polls the control plane's `/internal/clusters/{name}/desired-state`
  endpoint for which experiments should currently have a Job running,
  reconciles its local Jobs to match (create/delete), and pushes job status
  back — the same pull-desired-state/reconcile/report-status loop a kubelet
  runs, just one level up. No external queueing operator: admission,
  priority, and preemption are plain Kubernetes primitives (Job +
  PriorityClass), applied locally by cluster-agent. Install as many cluster
  agents as you have target clusters — the control plane is the single brain
  coordinating all of them, entirely through Postgres, never a live cluster
  connection.

The scheduling mechanism itself is pluggable: `controlplane/shared/workload.Backend`
is the interface `services/scheduler`, `services/controller`, and `services/quota`
actually depend on. `queuebackend.Backend` (Postgres-only, no cluster dialing) is
the production implementation; `workload.ClusterSet` (direct client-go dialing) still
exists in the same package for reference/testing but isn't wired into either binary.
A team that wants Kueue, Volcano, or something else implements `Backend` in its own
package and swaps one constructor call in `cmd/control-service` / `cmd/metrics-service`
— no other code changes.

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
CONTROLPLANE_URL=https://your-control-plane:8082 REGISTRY_URL=https://your-control-plane:8083 \
  make cluster-agent-up CLUSTER=my-cluster
make controlplane-up
```

`CONTROLPLANE_URL`/`REGISTRY_URL` just need to be reachable *outbound* from inside
the target cluster — the control plane never needs to reach the cluster at all, so
there's no kubeconfig or credential to hand it. Repeat `make cluster-agent-up
CLUSTER=<name>` for every additional target cluster you want the control plane to
schedule jobs onto; add a matching entry to `controlplane/settings/clusters.yaml` so
the control plane knows the name (registration is config-driven today, not
auto-discovered from an agent's first connection).

### Option C — Windows (untested)

```powershell
winget install GoLang.Go
make controlplane-up
```

> The control plane is standard Docker Compose + Go, with zero cluster-specific
> requirements — it never dials out to a cluster, so it should work anywhere Docker
> Compose runs.

## UI

```bash
cd controlplane/ui && npm install && npm run dev   # → http://localhost:3000
```

## Testing

```bash
bash tests/e2e-flow.sh                    # plain smoke test: submit, run, complete
AGENTS="a b c d" bash tests/e2e-flow.sh
bash tests/advanced-e2e.sh                # node-death self-heal, preemption, terminal
                                           # eviction, and distributed (multi-node) billing
```

## Agent API

Agents interact with OpenResearch exclusively through a REST API — no direct
cluster access, ever. Start here: [`tests/workload/spec.md`](tests/workload/spec.md).

## Endpoints

| Service                          | URL                    |
|----------------------------------|------------------------|
| control-service: quota           | http://localhost:8081  |
| control-service: scheduler       | http://localhost:8082  |
| metrics-service: registry        | http://localhost:8083  |
| metrics-service: metric-controller | http://localhost:8084 |
| UI                               | http://localhost:3000  |
| GreptimeDB                       | http://localhost:4000  |

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
| `make reset`                      | controlplane-down + controlplane-up                                 |
| `make up` / `make down`           | Aliases for `controlplane-up` / `controlplane-down` (back-compat)  |

## Development

```bash
go build ./...
go test ./... -timeout 60s
golangci-lint run ./...
```
