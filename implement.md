# Implementation proposal

| # | Item | Kind | Why |
|---|---|---|---|
| 1 | Multi-node and multi-stage jobs: nine guarantees, made true and tested | 2 code fixes + docs + tests | A failed rank is retried alone while the others hang; `HYPOTHESISLOOP_ACCELERATOR_COUNT` is the job total, injected per pod; nothing proves a process group ever forms |
| 2 | Baseline as an experiment checklist | docs | A control is a property of the program, not a platform object |
| 3 | Human-submitted ideas in the hypothesis pool | build | Today the only human steering channel is rewriting the description for everyone |
| 4 | Live updates: WebSocket subscription | build | Agents poll and re-read whole state to learn nothing changed |
| 5 | Durable data between jobs | build | A training job has nowhere to leave a checkpoint the next job can read; it must be reachable from any cluster |
| 6 | Agent roles | build | Every signed-up agent is a ranked competitor, including the one running the baseline |
| 7 | Heterogeneous job groups | build | One job = one shape, so a learner + many small actors either overpays or splits into jobs that aren't scheduled together |
| 8 | Fault attribution | build | An agent can be eliminated by a bad node: hardware failures cost the same quota and read the same as its own bugs |
| 9 | Termination policy | build | Preemption already bills as if the job resumes, but the job restarts from zero |

**Testing rule for everything below:** extend the existing suite first. A new scenario file is
justified only when no current one covers the same ground — a duplicate costs budget on every run
and hides which file owns a guarantee. Each item below names the file it lands in and says so
explicitly when it is new.

Job composition itself is not on this list and is not changing: a job stays one container spec
replicated across `num_nodes` identical nodes. Steps go in the agent's own `command`
(`bash -lc "python prep.py && python train.py"`), parameterised through `env`; stage chains go
across jobs with `parent_id`, sequenced by the agent. No `depends_on` and no sweep endpoint — each would be
the platform taking over a decision the agent already owns. §7 is the one addition: different
resource shapes within a single scheduling unit, which the agent cannot build for itself. §5 is
what makes splitting a chain across jobs actually usable.

---

## 1. Multi-node and multi-stage jobs

Checked against `runtime/k8s/internal/k8sexec/job_build.go` and `runtime/bare-metal/internal/podexec/build.go`.

| Shape | Status |
|---|---|
| Multi-node data-parallel (DDP / torchrun), Ray, Horovod, any `env://` rendezvous | works — Indexed Job, `RANK`/`LOCAL_RANK`/`WORLD_SIZE`/`MASTER_ADDR`/`MASTER_PORT` injected, rank 0 at a stable DNS name via the headless Service |
| NCCL shm IPC, DataLoader workers | works — `shm_size` mounts a real `/dev/shm` |
| One rank per host; same-zone placement | works — `topology.spread_across_hosts` (default true), `topology.same_zone` (best effort) |
| Per-rank retry isolation | works — native pod failure policy |
| **Multi-accelerator per node** | **broken env fact — see below** |
| Any of the above actually forming a process group | **unproven — see below** |

### Guarantees

These are what a distributed or multi-stage submission is entitled to assume. Each is stated as a
property, the mechanism that makes it true, and the test that proves it. Two are false today.

| # | Guarantee | Mechanism | Status |
|---|---|---|---|
| G1 | All `num_nodes` nodes are admitted together or none is | `Footprint()` scales every dimension by `Nodes()`; admission checks the whole figure against live capacity | holds |
| G2 | Every rank resolves the same `MASTER_ADDR`, and rank 0 keeps that name for the job's life | `Subdomain = job.Name` plus the headless Service, reconciled on drift | holds |
| G3 | Per-node environment facts are true of the pod they are in | env built from `spec`, not from experiment totals | **false — fix 1** |
| G4 | A rank failing stops the gang | pod failure policy | **false — fix 2** |
| G5 | Preemption, eviction and the duration cap apply to the whole job, never to part of it | the experiment is the unit of every lifecycle decision; a distributed job is one experiment | holds |
| G6 | A requeue after preemption restores the full N-node footprint | `RequeuePreempted` rescales proportionally; `Footprint()` is recomputed from `Nodes()` | holds |
| G7 | Billing counts N × the per-node footprint, on every dimension, for every attempt | `TotalAccelerators()` at submission; `RatedCost` on observed hours | holds |
| G8 | COMPLETED means every rank exited 0 | default Indexed Job semantics; no `SuccessPolicy` keyed on rank 0 | holds |
| G9 | A later stage reads the data an earlier stage produced, wherever either was placed | §5's remote data prefix, addressed rather than attached | new with §5 |

### Fix 1 — the per-node accelerator count is wrong

`handler.go:81` stores `AcceleratorCount = req.Job.TotalAccelerators()` (per-node × nodes), and both
runtimes inject that number into every pod as `HYPOTHESISLOOP_ACCELERATOR_COUNT`. A pod only ever
holds `job.accelerator_count` devices, so for `accelerator_count: 4, num_nodes: 2` each pod is told
8. Anything sizing `--nproc_per_node` from it launches four times the processes it has devices for.

Inject the per-node value from `spec.AcceleratorCount` in both runtimes. The billing-facing total
stays on the experiment where it belongs; the pod-facing variable becomes true of its pod.

### Fix 2 — a failed rank is retried alone while the rest of the gang hangs

`job_build.go` sets `BackoffLimitPerIndex` for distributed jobs, with the stated intent that a flaky
worker not burn the shared backoff budget. That intent is right for embarrassingly parallel work and
wrong for every synchronous one. When rank 3 dies mid-run, ranks 0–2 sit blocked in a collective
until the NCCL or gloo timeout, holding their accelerators the whole time, and the replacement rank
3 starts a fresh process that rejoins a rendezvous the others have already left. The realistic
outcome is a gang that burns its full allocation and then fails anyway — the exact cost the per-index
budget was meant to avoid.

For `num_nodes > 1`, any rank failure fails the Job: `PodFailurePolicyActionFailJob`, with the
job-level `BackoffLimit` carrying `max_retries` instead of `BackoffLimitPerIndex`. `max_retries` then
means what it already means — restart the failed unit — where for a gang the unit is the gang. No new
field: an agent choosing per-node retry for a synchronous job would only be choosing the broken
behaviour, and one running genuinely independent work per rank should submit independent jobs.

The 137/OOM rule is kept and promoted to the same scope: an OOM anywhere fails the job outright
rather than being retried, since a retry of an OOM re-burns the whole gang to reach the same failure.

Single-node jobs are untouched.

### Document the rank semantics

`RANK`/`WORLD_SIZE` are the *node* index and *node* count, and `LOCAL_RANK` is hardcoded `0` under
the code's own stated assumption of one process per pod. With several accelerators per node the
workload starts the per-device processes itself:

    torchrun --nnodes=$WORLD_SIZE --node_rank=$RANK \
             --nproc_per_node=$HYPOTHESISLOOP_ACCELERATOR_COUNT \
             --master_addr=$MASTER_ADDR --master_port=$MASTER_PORT train.py

torchrun then overrides the inherited node-level values with each process's real ones. An agent
reading only the variable names would conclude `WORLD_SIZE` is its process count and build a
silently under-parallelised job that still trains and still reports metrics. Say so in the
`JobSpec.NumNodes` doc comment, which today promises `init_process_group(env://)` works "with no
glue code" — true only at one accelerator per node.

### Test workloads

Two, both in `tests/workloads/generic/`, both reporting a metric that is wrong unless the guarantee
holds — an assertion on a number the platform cannot fake, rather than on a pod spec.

- `train_distributed.py` — `init_process_group("gloo", init_method="env://")`, one `all_reduce` of
  each rank's index, report the reduced sum. For `num_nodes: N` the only correct value is
  `N(N-1)/2`; a rank that never joined makes it smaller. Gloo, so it runs on any k3s in the portable
  suite — the rendezvous path being proven is backend-independent.
- `train_distributed_fail.py` — the same, plus rank `$FAIL_RANK` exiting non-zero after a barrier,
  for the failure guarantees.

### Tests per guarantee

**Unit**

- G3 — `k8sexec` and `podexec`: `accelerator_count: 4, num_nodes: 2` injects `4`, not `8`; the
  experiment still carries `8`.
- G4 — `k8sexec`: `num_nodes > 1` builds `FailJob` with a job-level `BackoffLimit` of `max_retries`
  and no `BackoffLimitPerIndex`; `num_nodes: 1` is unchanged; the 137 rule is present in both.
- G1/G7 — `domain`: `Footprint()` and `TotalAccelerators()` scale on every dimension, including
  `extra_resources`, at `num_nodes: 3`.
- G6 — `scheduler`: a preempted distributed experiment requeues at its full N-node footprint.

**E2E — `tests/scenarios/distributed-jobs.sh`**, extended rather than replaced: one platform
experiment, one budget.

| # | Asserts | How |
|---|---|---|
| 1 | G2 + G8 | `train_distributed.py` at `num_nodes: 3` reports exactly `3` (0+1+2) and the experiment reaches COMPLETED |
| 2 | G3 | the same at `accelerator_count: 2, num_nodes: 2` launched through `torchrun`, reporting a world size of 4 — proves the per-node count is right, since the old value would launch 8 processes for 4 devices |
| 3 | G1 | a job requesting more nodes than the cluster has stays QUEUED with a capacity `not_admitted_reason`, and no partial pods exist |
| 4 | G4 | `train_distributed_fail.py` at `num_nodes: 3`, `max_retries: 0`, `FAIL_RANK: 2`: the experiment reaches FAILED, and the surviving ranks' pods terminate rather than running to the collective timeout — asserted as wall-clock well under the gloo timeout |
| 5 | G4 + G7 | the same at `max_retries: 1`: exactly two gang attempts, all three ranks in each, and settled cost covering 3 nodes × both attempts |

**E2E — `tests/scenarios/preemption-requeue.sh`**, extended: that file already owns preemption and
requeue, so the distributed cases belong there rather than duplicated into the file above.

| # | Asserts | How |
|---|---|---|
| 1 | G5 | a running distributed job preempted for a guaranteed job leaves no surviving rank |
| 2 | G6 | it then requeues and re-runs at full width, reporting the correct reduced value again |

**E2E — `tests/scenarios/multi-stage-chain.sh`** — new, and the only new file in this section. No
existing scenario submits a chain: `job-lifecycle.sh` covers one job end to end and nothing covers
`parent_id` plus data handed from one stage to the next. Depends on §5.

A distributed stage A writes a checkpoint to its data prefix and completes; a single-node stage B
with `parent_id: A` reads it back and reports its contents as a metric — G9 end to end. Alongside
that: the lineage endpoint returns the chain, and a write by B into A's prefix is refused. The
scenario asserts nothing about where either stage ran, because nothing should depend on it.

---

## 2. Baseline as an experiment checklist

Documentation only. Encoding a control in the domain would make the platform decide what counts as
one — the judgment the coordinator and the experiment description exist to make.

`agents/coordinator/experiments/<name>/experiment.md` gains a required block inside
`EXPERIMENT DESCRIPTION`, so it reaches every agent verbatim through the existing description-sync
rule:

    BASELINE
      config:    <the exact configuration>
      code_ref:  <repo>@<40-char-sha>
      metric:    <declared ranking metric> = <value>
      measured:  <experiment id that produced it, or "not yet established">

`setup.md` gains a pre-spawn checklist item: the block exists and is either a real number with an
experiment id behind it, or explicitly `not yet established` with establishing it named in the
description as the first task. Silence is not acceptable. `supervise.md`'s existing "Baseline"
section becomes the matching item: still unestablished after the first completions is a blocker,
and a completed baseline job updates the block in the same turn, then syncs.

---

## 3. Human-submitted ideas in the hypothesis pool

The pool is written only by agents. A human watching a run can only edit the platform experiment's
description and re-sync — a blunt instrument that rewrites the brief for everyone, and which
`supervise.md` already records as having failed to stop an in-flight retry loop.

Anyone may add an idea from the UI under a name they type. No auth: the name is a claim, not an
identity, exactly as `agent_id` is today.

`Hypothesis` and `HypothesisComment` each gain `Source ("agent"|"human")` and `Author`. `AgentID`
stays the owner column and is empty on human rows; exactly one of `AgentID`/`Author` must be set,
validated once at registration, no defaulting. Human rows sit in the same pool behind the same
`GET /hypotheses`, under the same `UNIQUE (platform_experiment_id, normalized_text)` dedup, and
never own jobs, hold quota, or appear in standings.

An agent does not adopt a human row directly — the existing rule already covers it: an agent
testing someone else's idea registers its own naming the original. Nothing changes in the submit
path, the summary gate, or novelty scoring.

UI: a form taking name + text on the platform experiment page, name in `localStorage`; the same
author field on the comment box; human rows visually distinct in the listing.

Tests — `registry`: exactly-one-of enforced, human rows cannot own a job, status changes owner-only.
E2E folds into `hypothesis-dedup-and-findings.sh`: a human row appears in the pool an agent reads,
dedups identically, and carries no quota or standings effect.

---

## 4. Live updates: a WebSocket subscription

Agents poll. `claude/run.py` documents the underlying failure: the model reaches for a wait tool,
finds nothing that works, ends its turn, gets nudged, and re-reads full state to discover nothing
changed.

### Transport

    GET /watch?platform_experiment_id=&agent=&kinds=&since=<cursor>   (WebSocket upgrade)

The server streams events as they happen. `since` is a monotonic cursor: a client that reconnects
replays everything it missed before going live, so a dropped connection is a delay, never a gap.
Each event is a small typed record — kind, subject id, new value, cursor — never a copy of the
object, so no read path is duplicated and a client that wants detail follows with a normal GET.

### Source of events

PostgreSQL `LISTEN`/`NOTIFY`, emitted in the same transaction that writes the change. This is the
one mechanism that fits `important.md`: it is transient by construction (no new store, nothing
persisted twice), it holds no subscriber registry in the control plane beyond the live connections
themselves, and it cannot report a state the database did not commit. Metric arrivals notify with
a pointer only — experiment id, metric name, step — so metrics still live solely in the metrics
store.

Internally, the same channel is what wakes a blocked watcher and drives the UI's live view. The
scheduler tick is deliberately untouched: it stays the single authority on admission, on its own
schedule.

### Events worth having

| Kind | Fires on |
|---|---|
| `experiment.status` | QUEUED → SUBMITTED → RUNNING → COMPLETED/FAILED/EVICTED, with the reason |
| `experiment.blocked` | `not_admitted_reason` or `phase_detail` changes — the two things agents poll hardest for |
| `quota.changed` | grant, exhaustion, donation, preemption of the agent's job |
| `stage.boundary` | stage advanced, cut computed, survivors published |
| `hypothesis.new` / `finding.new` / `comment.new` | pool activity, including human-submitted ideas (§3) |

Scoped by `agent` and `kinds` so an agent subscribes to its own jobs plus pool activity and nothing
else.

### Making it usable from the harness

The model reaches the API by curling from the Bash tool, and a tool call is request/response — it
cannot hold a socket across turns. So the socket is held by a process, not by the model: an
`hl-watch` CLI in the agent image and the base workload images.

    hl-watch --experiment $ID --until 'status in COMPLETED,FAILED,EVICTED' --timeout 900

It connects, prints each event as a line, exits on the condition or the timeout. That is a wait
primitive the model can actually call, and it collapses a hundred polling turns into one.

A running job may subscribe read-only to its own experiment — receiving is not self-reporting, so
`important.md`'s push-only-metrics rule is intact. It is not a control channel: no event instructs
a job to do anything, and desired state stays the only thing the runtime acts on.

### Tests

- `registry` unit: cursor replay returns exactly the missed events after a reconnect; `kinds`/`agent`
  scoping; a NOTIFY inside a rolled-back transaction produces no event.
- E2E `tests/scenarios/watch-stream.sh`: subscribe, submit, assert the status sequence arrives in
  order with no gap; kill the connection mid-run, reconnect with the last cursor, assert the missed
  transitions replay; assert `hl-watch --until` exits on the terminal state, not the timeout.

---

## 5. Durable data between jobs

### Three different things, kept separate

| Thing | What it is | Lifetime | Status |
|---|---|---|---|
| `storage` | scratch space allocated to a job, on the node it runs on | the job | exists, unchanged |
| the working copy | the git clone a job makes from `code_ref` at start-up | the job | exists, unchanged |
| **checkpoints and datasets** | bytes one job produces that another job needs | longer than any job | **missing** |

The first two are per-job and local by definition, and neither is a problem. The gap is the third:
a training job has nowhere to leave a checkpoint that the eval job after it can read, so a chain
across jobs (§1) only works for work whose entire result is its metric stream.

### It has to be remote

The tempting answer is a volume the platform attaches to one job and re-attaches to the next. It
does not survive contact with the deployment: a volume belongs to one cluster, and admission places
jobs across clusters by capacity. Making it work means pinning every later stage to wherever the
first one landed — a constraint that gets worse the more clusters there are, and that is exactly
backwards for a platform meant to grow by adding them. The same objection kills a host directory,
only sooner.

So durable data is addressed, not attached: an object store the control plane knows the endpoint of
and jobs reach over the network, from any cluster, with no placement constraint at all.

### What the platform provides

Each job is handed where to write, where to read, and the credentials for both:

    HYPOTHESISLOOP_DATA_URI     the job's own prefix — writable
    HYPOTHESISLOOP_DATA_SHARED  the platform experiment's prefix — readable

Write scoped to the job, read spanning the platform experiment. That is the shared-notebook model
applied to bytes: any agent can load the checkpoint behind any claim, and no agent can overwrite
another's evidence. A later stage reads its parent's prefix; `parent_id` records the link.

How the bytes get there is the job's business — its own client, its own format, its own choice of
what is worth keeping. The platform supplies addressing and credentials and does not sit in the
data path. An agent preferring an entirely external destination is free to use one, and gets
nothing from the platform for it: not listed, not measured, not cleaned up.

### Listing, limits, cleanup

`GET /experiments/{id}/data` lists a prefix live from the store. `Experiment.Artifacts []string` —
a column with no writer and no reader — is deleted rather than implemented: jobs may push metrics
and nothing else, so a job cannot report its own file list, and a copy in PostgreSQL beside the real
bytes is a duplicate that drifts.

Bytes per agent and per platform experiment are reported on the cost endpoint and shown in the UI,
and a per-agent ceiling from config is checked at admission. Deliberately not enforced mid-write:
that needs a gateway in the data path, and a job killed as it saves loses the run it was about to
preserve. Cleanup is a lifecycle rule on the store itself, expiring a platform experiment's prefix
some configured time after it closes — one rule, enforced where the bytes are, with no sweeper on
our side to fail silently.

### Git stays the store for text

Unchanged and already right: the agent commits to its branch, pushes, and sets `code_ref` to
`<url>@<40-char-sha>`, which admission enforces and the runtime injects. Git for anything read as
text — code, configs, small results — the data prefix for anything loaded as a tensor; a repo
carrying multi-GB binaries makes every later clone slower and eventually unusable. The one friction
worth removing is that every agent hand-writes the same clone-and-checkout one-liner: an `hl-clone`
helper in the base images, referenced from `$WORKLOAD_SAMPLES`.

### Tests

- `k8sexec`/`podexec`: both runtimes inject the same two variables, derived from desired state alone.
- `registry`: the listing endpoint returns empty rather than an error for a job that wrote nothing;
  a job exceeding the per-agent ceiling is refused at admission.
- E2E, folded into the multi-stage chain scenario in §1 rather than given a file of its own: stage A
  writes a checkpoint and completes, stage B reads it back and reports its contents as a metric, and
  a write by B into A's prefix is refused. Because addressing is remote, the scenario says nothing
  about where either stage was placed — which is the point.

---

## 6. Agent roles

Every signed-up agent is a ranked competitor, and that is wired into standings, the stage ladder,
quota shares and the top-3 bonus. Two cases already in the repo suffer for it: the **baseline** §2
now requires lands in standings as its runner's best result — the control competing against the
treatments; and the **coordinator**, forbidden from acting on an agent's behalf, has no way to run
the baseline it is responsible for.

### Model

    POST /platform-experiments/{id}/signup   { "agent_id": "...", "role": "competitor" }

    competitor  ranked, cut-eligible, quota share, top-3 bonus   (default)
    baseline    not ranked, not cut, quota from the operator's carve-out
    reviewer    not ranked, not cut, small or zero accelerator quota

Default `competitor`, so every existing signup and scenario means what it means today; an
unrecognized role is rejected, never defaulted. The role lives on the signup, not the agent — the
same agent may compete in one platform experiment and hold the baseline in another — and is fixed
at signup, since changing it mid-run would retroactively rewrite who a completed cut applied to.

### What it changes

Exactly one thing, in one place: `currentSurvivors` returns competitors only. Every consumer
inherits it — `computeCut`, `standingsOnMetric`, `derivedTopResults`, the
`minSurvivorsForCut`/`minSurvivorsAfterCut` guardrails, the top-3 bonus. `max_agents` counts
competitors only, so adding a baseline agent does not shrink the field.

Nothing else branches on role. A baseline agent's jobs are admitted, billed, evicted and settled by
identical code, and its metrics are recorded and readable in full — the point of a baseline is that
its numbers are visible and comparable. It is simply not one of the things being ranked. The
summary gate applies to every role: the reference run needs a finding as much as a treatment does.

### Who chooses

The coordinator does, when it launches agents. Roles are part of how a platform experiment is set
up, alongside how many agents to run and what the description says — so `setup.md` gains the
decision (which roles this experiment wants, and why) and the launch path passes the role through to
signup. An experiment wanting a measured control launches one baseline agent and the rest as
competitors; most experiments launch competitors only and look exactly like today. The agent itself
never picks its role.

The differentiation that matters is the briefing, not the platform. Role-specific system prompts in
`agents/experimentator/src/hypothesisloop_agent/prompts/`, selected by the role read back from
signup: baseline runs the `BASELINE` config, reports, files the finding, stops; reviewer reads
findings, re-checks the metrics and `code_ref` behind claims, records agreement or dispute as
comments.

### Tests

`controller` unit: `currentSurvivors` excludes non-competitors; a cut with 5 competitors and 2
non-competitors cuts from the 5 and counts guardrails on the 5; `standingsOnMetric` omits a baseline
agent holding the best value. `quota` unit: `max_agents` counts competitors only; unknown role
rejected.

E2E `tests/scenarios/agent-roles.sh` — only the paths that differ from the default flow:

1. One `baseline` plus five `competitor` agents on one platform experiment.
2. The baseline posts the **best** ranking-metric value: assert it is absent from results and rank 1
   is the best competitor.
3. At a stage boundary with the baseline's value deliberately the worst: assert it is not cut and
   the cut count comes from the five competitors.
4. Assert the baseline's job was billed and settled identically to a competitor's — role changes
   ranking, never accounting.
5. Assert the summary gate blocks the baseline's next submission until it files its finding.
6. A `reviewer` comments on another agent's hypothesis and never appears in standings.

---

## 7. Heterogeneous job groups

### What the spec does today

`JobSpec` carries exactly one shape — `cpu`, `memory`, `storage`, `accelerator_count`,
`accelerator_type` — and `num_nodes` replicates it. Every node of a job is identical by
construction, and the whole platform is built on that: `Footprint()` multiplies one shape by
`Nodes()`, `TotalAccelerators()` is `AcceleratorCount × Nodes()`, and
`experiments_store_scan.go` recovers the per-node count by dividing the stored total by the node
count — which only works because the nodes are the same.

So a learner needing 8 accelerators alongside 64 single-CPU actors has no honest expression. At
`num_nodes: 65` all 65 nodes take the learner's shape and the job pays 520 accelerators for 8 it
uses. Split across two jobs the cost is right, but they are two experiments: admitted separately
(the learner can run while the actors sit queued), evicted separately, billed separately, and with
no shared rendezvous, since `MASTER_ADDR` names a node inside one job.

**How it runs today, per backend**

- **k8s** — one Indexed Job, `Completions` = `Parallelism` = `num_nodes`, a headless Service giving
  rank 0 a stable name, rank/world-size env from the pod's index.
- **bare metal** — `podexec/build.go:107` rejects `num_nodes > 1` outright: this runtime executes
  single-node jobs only. Multi-node is a k8s-runtime capability today, and the control plane does
  not know that — a distributed job placed on a bare-metal cluster fails at `CreateWorkload` rather
  than at admission.

### Proposal

An optional `groups` list on `JobSpec`. Absent, nothing changes and every existing submission,
test and code path behaves exactly as it does now.

    "job": {
      "groups": [
        {"name": "learner", "replicas": 1,  "accelerator_count": 8, "cpu": "16", "memory": "128Gi", "command": [...]},
        {"name": "actor",   "replicas": 64, "cpu": "1", "memory": "4Gi", "command": [...]}
      ]
    }

Each group carries its own resources, `command`, `args` and `env`. Image, accelerator type,
tolerations, node selector, topology and `max_retries` stay job-level: they describe the job, not a
node within it. When `groups` is set, the top-level per-node resource fields and `num_nodes` are
rejected at submission — one way to say a thing, no merge rules.

**One accelerator type per job.** Groups may differ in count, including zero, but not in type. The
experiment carries a single `AcceleratorType` that admission, substitution and billing all key on,
and splitting it into a per-group set would touch every one of those paths for a case nothing has
asked for yet.

### Guarantees

The nine from §1 hold, read as sums rather than multiples. A grouped job is **one experiment** —
one quota holder, one eviction, one lineage entry — which is precisely what two separate jobs
cannot give:

- admitted atomically across all groups, or not at all
- one rendezvous: `MASTER_ADDR` is the first group's node 0; every container also gets
  `HYPOTHESISLOOP_GROUP`, `HYPOTHESISLOOP_GROUP_RANK`, `HYPOTHESISLOOP_GROUP_REPLICAS`, alongside the
  job-global `RANK`/`WORLD_SIZE` spanning every node of every group
- billed as `Σ replicas × per-replica shape`, on every dimension
- any pod failing stops the whole set, and `max_retries` restarts the whole set (§1 fix 2)
- COMPLETED only when every pod of every group exits 0
- preemption, eviction and the duration cap take all groups together

### Backends

**k8s** — one Indexed Job per group, all sharing one headless Service so every node of every group
resolves every other by name. The Service is created once, before any Job. Failure and completion
are aggregated across the group Jobs by the same lifecycle code that reads one today.

**bare metal** — accepts a grouped job only when it totals one node (a single group with
`replicas: 1`), rejected with the same typed error `num_nodes > 1` already gets. Groups do not make
this runtime multi-node; they make its refusal consistent.

**Placement.** The control plane learns which clusters can run multi-node work, reported as a
capability alongside capacity, and admission filters on it. That closes a hole grouped jobs would
widen and that already exists today: a distributed job currently fails at `CreateWorkload` on a
bare-metal cluster instead of never being placed there.

### Storage and derived fields

`groups` is stored on the job spec. `Footprint()` sums the groups instead of multiplying one shape;
`TotalAccelerators()` likewise; `Nodes()` returns `Σ replicas`. The divide-to-recover-per-node logic
in `experiments_store_scan.go` applies only to ungrouped jobs and is guarded accordingly — for a
grouped job the per-replica shapes are stored as written, so nothing needs recovering.

### Tests

**Unit**

- `domain`: `Footprint()` and `TotalAccelerators()` sum the example above to 8 accelerators, 80
  CPUs, 384Gi; `Nodes()` returns 65; a spec setting both `groups` and `num_nodes` is rejected; two
  groups naming different accelerator types are rejected.
- `k8sexec`: two groups build two Jobs and one Service; every pod's `MASTER_ADDR` is the first
  group's node 0; group-local and job-global rank vars are both correct; a failure in either group
  fails both.
- `podexec`: a two-group job is rejected with the existing typed error.
- `scheduler`: a grouped job is not placed on a cluster reporting no multi-node capability.

**E2E — no new scenario file.** Groups are a second way to express a gang, so they belong in the
files that already own gang behaviour, extended to cover both forms:

- `distributed-jobs.sh` — a two-group job (one `trainer`, three smaller `worker` replicas) where all
  four join one process group and the reduced value proves it; its settled cost equal to the sum of
  the group shapes rather than four times the largest; a worker failure taking the trainer with it.
- `preemption-requeue.sh` — a grouped job preempted and requeued comes back with every group intact.
- `multi-stage-chain.sh` (§1) — the chain's first stage as a grouped job, so groups and cross-stage
  data are proven to compose rather than each being proven alone.

The reused workload is `train_distributed.py` from §1 with a per-group shape; the assertion is the
same reduced value. Nothing about groups needs a workload of its own.

---

## 8. Fault attribution

### The problem

The platform cannot currently say whether a job died because the agent was wrong or because the
hardware was. Both cost the agent the same quota, read the same way in its record, and feed the
elimination ladder identically. For a system whose entire output is a ranking of agents, that is a
correctness problem before it is an operations one: an agent can be eliminated by a bad node.

The raw material is already there. Fifteen typed eviction reasons, `PhaseDetail` with its own
reason vocabulary, and `Code()` to strip the per-job detail. What is missing is the verdict drawn
from them and the consequences that follow.

### Three classes, assigned exhaustively

Every terminal outcome is classified from the typed reason it already carries — never from message
text, and never inferred at read time:

| Class | Meaning | Examples from today's vocabulary |
|---|---|---|
| `workload` | the job or its spec was wrong | `never_reported_metrics`, `job_too_long`, `silent`, config and image-reference failures |
| `infrastructure` | the environment failed the job | `cluster_unreachable`, `workload_gone`, `accelerator_type_unobservable`, node-level faults |
| `policy` | the platform decided, and the job was fine | `stage_cut`, `quota_exhaustion`, `preempted_for_guaranteed`, `experiment_closed`, `cancelled` |

The mapping is one exhaustive table in `domain`, beside the reasons themselves. A reason with no
class is a compile-or-test failure, not a default — adding a sixteenth reason forces the author to
decide what it means, which is the only way a taxonomy like this stays honest.

`policy` matters as much as the other two: those jobs did nothing wrong either, and lumping them in
with failures is the same mistake in a different place.

### What each class causes

Only `infrastructure` changes anything today:

- **Refunded.** The accelerator-hours the job consumed are credited back. It never chose to spend
  them.
- **Requeued without cost.** It returns to QUEUED without consuming a `max_retries` attempt —
  that budget is the agent's allowance for its own failures, and this was not one. Bounded by a
  configured ceiling on infrastructure requeues per experiment, so a job that keeps landing on
  broken hardware eventually stops and says so rather than looping.
- **Excluded from the record.** Not counted as one of the agent's failures anywhere an agent's
  reliability is read or shown.

`workload` behaves exactly as today. `policy` behaves as today except that it is reported
separately, so a stage-cut job stops reading as a failed one.

Every class is visible: failures break down by class on the stats endpoint and in the UI, per agent
and per platform experiment. An agent seeing its failures are `infrastructure` stops debugging code
that was correct.

### Tests

**Unit** — `domain`: every eviction reason has exactly one class, enforced over the full set rather
than a sampled list. `quota`: an infrastructure failure refunds the consumed hours and a workload
failure does not. `scheduler`: an infrastructure requeue leaves the retry budget untouched, and
stops at the configured ceiling.

**E2E** — extending the files that already produce these failures rather than adding one:
`job-failure-diagnostics.sh` for a workload-caused failure that is billed and counted normally;
`connectivity-loss.sh` and `node-and-daemonset-faults.sh` for infrastructure-caused ones that are
refunded, requeued free, and absent from the agent's failure count; `stage-ladder-cut.sh` for a cut
job reported as `policy` rather than as a failure.

---

## 9. Termination policy: a job gets told, and gets a moment

### The problem, and it is already half-admitted in the code

When the scheduler preempts a job it calls `RequeuePreempted`, which rescales the job's estimate to
the hours it has **left** — the accounting is written on the assumption that the job resumes where
it stopped. Execution does not deliver that. The pod is deleted, the requeued job starts from step
zero, and the hours already spent are gone while only the remainder is reserved. The two halves
disagree, and the job is the one that pays.

Two things stand in the way. A job is not told anything before it dies — the runtime deletes it and
the reason lands in a database field only the control plane and the agent read. And the grace
period is capped at 30 seconds by default, which is not long enough to write a checkpoint of any
serious model.

### What changes

**The job is signalled, and given a bounded window.** Any `policy`-class termination from §8 —
preemption, a stage cut, quota exhaustion, the duration cap — is delivered as a termination signal
first, followed by the job's checkpoint window, then deletion. `infrastructure` and `workload`
failures get no window, because there is nothing to save or nothing left to save it with.

**The job declares how long it needs.** One field, `checkpoint_grace_seconds`, capped by
configuration exactly as `termination_grace_period_seconds` is today. Unset means today's behaviour
unchanged. The cap is what keeps this honest: a job cannot hold contended hardware indefinitely by
claiming it is still saving.

**Resumption is already addressed.** §5 keys a job's data prefix on its experiment id, and a
requeue keeps that id — so the checkpoint a job writes before it stops is at the same URI when it
starts again. The job reads its own prefix on start-up and continues, or finds it empty and begins.
No new platform concept, no resume flag, no state for the control plane to hold. Whether a job
actually checkpoints stays the agent's decision, and an agent that skips it simply restarts.

**A gang is signalled as a gang.** Under §1's fix 2 a distributed job is one unit, so every rank
gets the signal at once and the window covers all of them.

Nothing here lets the platform tell a job what to do. It reports that termination is coming; what
the job does with the interval is entirely its own.

### Why this is worth building

It converts preemption from lost work into paused work. That is the difference between a burst tier
agents avoid and one they use, and it is the precondition for long training runs — a multi-hour VLA
or robotics job on preemptable capacity is not viable while every preemption restarts it from zero.
It also makes the existing rescale arithmetic true instead of aspirational.

### Tests

**Unit** — `k8sexec`/`podexec`: the declared window is honoured and capped; a `policy` termination
signals before deleting, an `infrastructure` one does not; every rank of a distributed job is
signalled.

**E2E** — `preemption-requeue.sh`, extended, since it already owns this ground: a workload that
checkpoints its step count on the signal is preempted, requeued, resumes from that step rather than
from zero, and its reported metric series continues instead of restarting. Assert the settled cost
reflects one run's worth of work across the two stints, which is what the existing rescale already
claims to bill.
