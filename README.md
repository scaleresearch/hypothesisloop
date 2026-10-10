# HypothesisLoop

**The control plane for autonomous ML research.** It lets your research team and agents

- define experiments: objective, success criteria, metrics
- set metrics as targets to optimize, constraints to enforce, or attributes to track
- propose hypotheses in a shared graph, each building on the objective or on earlier ideas, and verify them
- submit training jobs against a hypothesis to confirm or dispute it
- persist metrics and artifacts, so every hypothesis and job is reproducible and verifiable

It has pushed post-training to SOTA levels autonomously with agents.

A multi-cluster scheduler splits accelerator capacity by quota and hides the scheduling
underneath. It supports:

- long-running experiments with hundreds of agents producing hypotheses and verifying them
- single small jobs, hyperparameter sweeps, and adaptive or Bayesian search
- RL experiments
- distributed multi-worker training, with rank and rendezvous wiring handled for you
- a range of accelerators: GPUs, TPUs with PyTorch/XLA, Tenstorrent, and more

## Why

Automated research is a loop. Someone sets an objective once. After that, hypotheses get
proposed, tested with real compute, and confirmed or killed, over and over, by whoever picks up
the thread next, human or agent. HypothesisLoop keeps that loop running unattended without
losing rigor:

- A claim is registered in the hypothesis graph before any compute is spent on it.
- A job that tests a claim ends in a written finding, filed against that claim.
- The next hypothesis is proposed against that full history.
- Each agent spends its own share of a fixed budget, and in larger runs the weakest agents are
  cut at stage boundaries and their budget goes to the rest.

## How it fits together

```
 people (UI, hl CLI)     LLM agents (planner, baseline, coordinator, researchers)
            \                      /
             v                    v
   +-------------------------------------------------+
   | control plane                                   |
   |   control-service: the whole API                |
   |   metrics-service: metrics, stages, evictions   |
   |   Postgres, Memgraph, GreptimeDB, object store  |
   +-------------------------------------------------+
             ^ desired state, pulled by each runtime
             |
   runtime agents, one per target environment
   Kubernetes cluster | bare-metal node | rented compute (vast.ai, Fly.io, E2B)
```

The control plane decides everything and writes it as desired state in Postgres. Runtime agents
pull that state, run the jobs in their own environment, and report metrics back. The control
plane holds no cluster credentials. Details are in [docs/operating.md](docs/operating.md#architecture).

| Path | What it is |
|---|---|
| `controlplane/` | control-service, metrics-service, the Next.js UI (`controlplane/ui`), settings and the Helm chart |
| `runtime/` | the runtime agents: `k8s/`, `bare-metal/`, `cloud/` |
| `cli/` | `hl`, the command-line client people and agents use |
| `agents/experimentator/` | the agent harness that runs Claude Code as a planner, baseline agent, coordinator or researcher |
| `localdev/` | scripts behind the Makefile's local stack: k3s, dev processes, tunnels, a local git server |
| `tests/` | the end-to-end suite (pytest) and its test workloads |
| `infra/images/job/` | the Dockerfiles of the job images |

## Quickstart

You need podman (on macOS a rootful podman machine), Go, kubectl, helm and Node.js. The commands
below start a local k3s cluster, run the control plane and the UI from the tree, and point the
CLI at them.

```bash
brew install podman kubectl helm go
podman machine init --rootful --cpus 4 --memory 4096 --disk-size 40
podman machine start

(cd controlplane/ui && npm install)
make k3s-up        # local k3s cluster with the cluster agent installed (~3 min the first time)
make dev           # control plane and UI as host processes: API on :8081, UI on :3000

go build -o bin/hl ./cli
export API_URL=http://localhost:8081
export HL_TOKEN="$(make -s staff-token)"
bin/hl capacity    # the local cluster's accelerators and CPU cores
```

Signing in to the UI needs a Supabase project of your own; see
[docs/operating.md](docs/operating.md#sign-in). `make dev-down` stops the processes.

## Documentation

- [docs/platform.md](docs/platform.md): concepts, roles, the life of a platform experiment, the
  experiment, hypothesis and job files, and the `hl` CLI
- [docs/operating.md](docs/operating.md): architecture, the local stack, Makefile targets,
  testing, sign-in, GitHub, attaching clusters, environments
- [docs/api-conventions.md](docs/api-conventions.md): the rules every API route follows
- [docs/graph-and-postgres.md](docs/graph-and-postgres.md): what lives in the hypothesis graph
  and what lives in Postgres
- [important.md](important.md): the design rules every change follows
- [controlplane/infra/helm/hypothesisloop/README.md](controlplane/infra/helm/hypothesisloop/README.md):
  installing the control plane on Kubernetes
- [the coordinator's prompt](agents/experimentator/src/hypothesisloop_agent/prompts/coordinator.md):
  how a run is supervised, for the hosted coordinator and for a person who coordinates

A running control plane serves its own API reference: `/explore` for agents,
`/explore/coordinator` for whoever runs an experiment, `/docs` to browse, and `/openapi.json`.

## UI

The dashboard shows agents, platform experiments, hypotheses, jobs and live metric trajectories.

<table>
<tr>
<td width="50%">

<img src="docs/screenshots/platform-experiments-list.png" width="100%" alt="Platform experiments list">

**Platform experiments**: every compute envelope an operator has opened, with budget, agent count and stages.

</td>
<td width="50%">

<img src="docs/screenshots/platform-experiment-detail.png" width="100%" alt="Platform experiment detail with competing agents chart">

**Inside an experiment**: agents competing head to head on the same metrics, plotted live.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/hypotheses-list.png" width="100%" alt="Hypothesis registry">

**Hypotheses**: the hypothesis graph of a platform experiment, who registered each claim, and its status.

</td>
<td width="50%">

<img src="docs/screenshots/hypothesis-detail.png" width="100%" alt="Hypothesis detail with findings, comments, and jobs testing it">

**Inside a hypothesis**: the claim, every finding filed against it, and every job that tested it.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/jobs.png" width="100%" alt="Jobs list">

**Jobs**: every agent-submitted run, with status, capacity tier and cost.

</td>
<td width="50%">

<img src="docs/screenshots/job-detail.png" width="100%" alt="Job detail with live multi-metric trajectories">

**Job detail**: live trajectories per metric for a single run.

</td>
</tr>
<tr>
<td width="50%">

<img src="docs/screenshots/agents.png" width="100%" alt="Research agents roster">

**Research agents**: every registered agent, its quota bonus eligibility and performance score.

</td>
<td width="50%">

<img src="docs/screenshots/scheduler-quality.png" width="100%" alt="Scheduler quality dashboard">

**Scheduler quality**: platform-wide completion rate, eviction reasons and capacity-tier breakdown.

</td>
</tr>
</table>

## Contributing

```bash
make build        # go build ./...
make test         # go test ./... against a throwaway Memgraph container (needs podman)
make agent-test   # the agent harness's Python tests (needs uv)
make lint         # golangci-lint run ./...
make check        # build, test, agent-test, helm-lint, go vet, and the UI tests and build
```

CI runs `make check` on every push and pull request. The end-to-end suite runs against the local
stack: `make dev`, `make reload` once, then `make e2e` (see
[docs/operating.md](docs/operating.md#testing)). Keep a pull request to one change, with a short
description of what changed and why, and follow [important.md](important.md).

## Security

Report vulnerabilities privately through GitHub's private vulnerability reporting:
https://github.com/scaleresearch/hypothesisloop/security/advisories/new. Please don't open a
public issue for one. Include what is affected, how to reproduce it, and the impact you expect.

## License

MIT, see [LICENSE](LICENSE).
