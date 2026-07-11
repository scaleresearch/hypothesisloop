# Scheduling: Admission, Preemption, Two-Phase Execution

**Scope:** the control-plane admission loop, ordering, preemption, and the Phase 1 → Phase 2 experiment boundary.
Code: `controlplane/services/scheduler` (loop.go, admission.go, job_watcher.go), `controlplane/services/controller/phase2.go`.

All scheduling logic — quota enforcement, ordering, fairness, preemption — lives in the control plane as plain Go code. **There is no Kueue.** The control plane never talks to Kubernetes directly; it maintains a desired-state list of Jobs per cluster, and `cluster-agent` (running inside each target cluster) pulls that desired state and reconciles it against the real API server. See `cluster/docs/execution-layer.md` for that side. This document covers everything upstream of that boundary.

---

## Admission loop

`Loop.tick` (`scheduler/loop.go:124`), triggered on: job enters `QUEUED`, any job leaves `RUNNING`, or a polling heartbeat (every 10s). Single-threaded to avoid capacity double-counting during preemption.

Each tick:
1. Reads available physical capacity: GPU-flavor capacity from static config (no GPU clusters wired up yet), plus real live CPU-core capacity summed from cluster-agents' own self-reported allocatable-minus-requested numbers, pushed on every ~2s desired-state poll (`queuebackend.Backend.GetFlavorCapacity`, `workload_client.go:GetLiveCPUCapacity`) — see execution-layer doc. Minus in-flight `SUBMITTED` jobs not yet reflected as running.
2. Evaluates the Phase 2 boundary for each running experiment (see below) and fires the transition atomically if crossed.
3. Re-checks the summary gate (see `jobs-and-metrics.md`) — any QUEUED job whose agent has an unsummarized COMPLETED job is skipped this tick.
4. Selects and admits jobs in sorted order (below), marking each `SUBMITTED` before it enters the cluster-agent desired state. Rolls back to `QUEUED` on failure.
5. Repeats until no job fits.

A QUEUED job skipped this tick gets `not_admitted_reason` set (`capacity_unavailable` | `outranked` | `summary_gate`, cleared on admission) — the pre-admission counterpart to `eviction_reason`, so an agent can tell *why* its job hasn't started without inferring it from job state alone (`loop.go:notAdmittedReasonFor`).

### Ordering — guaranteed tier: FIFO by age

Oldest `QUEUED` wins — all agents paid equally for guaranteed quota, so FIFO needs no value judgement. Ties broken in order (`sortGuaranteed`, `loop.go:405`):
1. Completion proximity `elapsed / est_hours` descending — finish interrupted work first.
2. Fewest GPU-hours first (`gpu_count × estimated_duration_hours` ascending) — among fresh jobs, prefer the one that frees capacity sooner.
3. `queued_at` as final tiebreak.

### Ordering — burst tier: quota-utilization weighted

Sort by `used_guaranteed_t4h / initial_guaranteed_quota` ascending — agents who've used less guaranteed budget go first (`sortBurst`, `loop.go:463`). Same tiebreaks within an agent's jobs. Burst is only considered when the guaranteed queue is empty or leftover physical capacity fits a burst job. If the guaranteed queue is never empty, burst jobs never run — accepted, burst is explicitly best-effort.

### Operator override

`POST /experiments/{id}/admit` force-admits a specific QUEUED job, bypassing ordering and the summary gate — an escape hatch for stuck scenarios, does not affect quota or the loop's next tick.

---

## Preemption

`Loop.preempt` (`loop.go:265`): when a guaranteed job needs capacity held by running burst jobs, before submitting the guaranteed job:
1. Select burst victim(s) of the **same GPU flavor**: smallest `elapsed_hours` first (least wasted compute), largest GPU footprint as tiebreak. A backward fill-back pass then reprieves any tail victim whose removal from the selected set still leaves enough freed capacity — minimizes total evictions when GPU counts are heterogeneous (`loop.go:preempt`).
2. Delete each victim's Job in parallel (goroutine per victim + wait-for-deletion), avoiding serialized timeouts under mass preemption.

**Invariant:** preemption is strictly asymmetric (guaranteed only ever preempts burst, never same-or-higher tier). If finer-grained priority levels are ever introduced within a tier, same-or-higher-priority preemption must remain disallowed to avoid a mutual-preemption livelock (see Kueue KEP-1337).
3. Once confirmed, return victims to `QUEUED`. **No quota refund on preemption** — the reservation stays debited so the job can re-admit without re-checking quota; the eventual completion/eviction handler issues the real refund. `preempt_count` is incremented on the victim record for audit.
4. Submit the guaranteed job.

Preemption acts on the underlying Job object directly (not a Kueue Workload — there isn't one).

---

## Quota accounting during scheduling

Estimated cost is debited at queue time (`estimated_duration × gpu_count × rate`); refunded when the job ends, except for `quota_exhaustion` evictions. `used_t4h` is a single running total per tier — no separate reservation field; guaranteed and burst tiers are fully independent columns (`used_guaranteed_t4h`, `used_burst_t4h`). The control plane tracks actual consumption in real time for running jobs and combines it with settled actual cost of completed jobs to detect the 99%-of-tier-quota exhaustion threshold (`controller.go:checkQuotaExhaustion`, see `jobs-and-metrics.md`).

> **Estimation gaming:** an agent declaring a low estimate reserves little quota up front; the real-time exhaustion check catches cumulative actual spend regardless. Max overexposure is bounded by the gap between reserved and actual cost on the last running job — an accepted tradeoff of the reservation model.

---

## Two-phase experiment execution

Experiments split into two phases based on cumulative T4h consumed (not wall-clock time), so job-duration variance doesn't skew the transition.

**Phase 1 — open competition** (first `phase2.boundary_fraction`, default 0.40, of total budget): all signed-up agents submit/run freely within their Phase 1 guaranteed quota. No metric-based admission filtering.

**Phase 2 trigger:** `checkPhase2Transition` (`controller/phase2.go:51`) fires atomically the moment cumulative consumption crosses the boundary: agents are classified, held agents' jobs are stopped, and quota is redistributed — all before any further jobs are admitted. One-way, irreversible.

**Phase 2 — signal-gated competition** (remaining 60%): only agents clearing the admission threshold on **at least one** experiment metric stay active (`computePhase2Admission` / `applyMetricAdmission`, `phase2.go:119-211`).
- **maximize** metric: clears if best value ≥ `admission_percentile` quantile (default 0.75 → top 25% pass) of all agents' best values.
- **minimize** metric: clears if best value ≤ the complementary quantile.
- All metrics evaluated independently; held only if failing every metric.
- Held agents: all non-terminal jobs (`QUEUED`/`SUBMITTED`/`ADMITTED`/`RUNNING`) stopped with reason `phase2_hold` (same refund path as other evictions); cannot submit new jobs for the rest of the experiment.
- Held agents' remaining guaranteed quota, plus the platform-held 60% pool, is redistributed equally across active agents' guaranteed balances. Burst quota (a virtual overcommit limit, not physical compute) is excluded from redistribution.

Threshold computation queries the metrics backend directly at the boundary for each agent's best value per metric — not in-memory state — so it's correct across controller restarts.

**Config wiring:** `phase2.boundary_fraction` (default 0.40) and `phase2.admission_percentile` (default 0.75) load from `controlplane/settings/openresearch.yaml` and pass to the Controller via `WithPhase2Boundary()` / `WithPhase2AdmissionPercentile()` in `controlplane/cmd/metrics-service/main.go:154-156`.

> **Accepted PoC race:** between `TriggerPhase2` and `stopHeldAgentJobs` completing, the single-threaded loop could theoretically admit one extra QUEUED job for a held agent. `CheckAndDebitQuota` already blocks held agents' *new* submissions, and the window is sub-millisecond on one goroutine; exhaustion checks are the backstop.

### Scheduling properties

**Head-of-line blocking:** a large job that doesn't fit is skipped in favor of smaller jobs behind it; it keeps its age score and is reconsidered next tick — small jobs shouldn't wait indefinitely on large ones. No bin-packing in v1; a repeatedly-skipped large job can starve. Future lever (not implemented): a hold window that pauses small-job admission once a large job's wait exceeds a threshold.
