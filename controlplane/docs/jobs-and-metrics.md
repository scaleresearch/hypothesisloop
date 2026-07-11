# Jobs & Metrics

**Scope:** job submission contract, lifecycle, and the per-job eviction guards.
Code: `controlplane/services/scheduler` (submission/admission), `controlplane/services/controller` (eviction guards), `controlplane/services/dedup`, `controlplane/shared/metrics`.

---

## Job submission

Every job references a `platform_experiment_id`; jobs cannot be submitted outside a `running` experiment. Agent specifies: GPU type, GPU count (≥ 1), estimated duration, `hypothesis`, `theory`, and `capacity_tier` (`guaranteed`/`burst`).

- **Theory (required):** the specific prediction being tested — more concrete than `hypothesis`. Missing `theory` → `400 malformed`.
- **Summary gate:** after a COMPLETED job, the agent must `POST /experiments/{id}/summary` before submitting new jobs in the same platform experiment. FAILED/EVICTED jobs are excluded (infra failures shouldn't be penalized). Violating this returns `403 summary_required`. Re-enforced every scheduler tick, not just at submission time, so a batch of simultaneously-queued jobs can't bypass it as runs complete — the gate result is cached per (agent, experiment) within a tick.
- **Rate limit:** `max_submissions_per_hour` (default 100) per agent per platform experiment. Violating → `429 rate_limited`.
- **Resource caps:** `max_gpu_count_per_job` / `max_cpu_cores_per_job` / `max_ram_gb_per_job` / `max_storage_gb_per_job` from `openresearch.yaml`, validated in `scheduler/admission.go:ValidateExperiment` ahead of any quota debit.
- **Dedup / novelty score:** `controlplane/services/dedup` computes a `novelty_score` (0–1) stored on the job for observability and priority-weighting. Current implementation is a stub returning `1.0` always; it does not gate admission in v1. Planned upgrade path: TF-IDF cosine similarity against active hypotheses, then embedding-based similarity.
- Estimated cost = `estimated_duration_hours × gpu_count × gpu_type_t4h_rate`; rejected if it exceeds the agent's remaining tier balance.

On success: job is written `QUEUED` and estimated cost is debited from the ledger immediately (reservation) — no cluster interaction yet.

### Job status flow

```
QUEUED → [SUBMITTED*] → ADMITTED → RUNNING → COMPLETED
    ↓                                  ↓
REJECTED                        EVICTED / FAILED
```

`SUBMITTED` is internal: the scheduler has asked `cluster-agent` (via desired-state) to create the Job but it hasn't reported back as running yet. Not surfaced in agent-facing reads.

This is deliberately a single-bit gate ("has the cluster-agent confirmed RUNNING"), not a mirrored parallel state machine — Volcano's dual PodGroup/Job state machines stay safe the same way, by minimizing coupling to one boolean rather than synchronizing two independently-owned state machines. Don't "improve" this into a richer parallel state machine.

### Reproducibility & lineage

Every job stores immutable pointers at submission: docker image hash, `data_ref`, `config_hash` — sufficient to reproduce the run. A job may carry an optional `parent_id`; `GET /experiments/{id}/lineage` walks the chain oldest-first.

---

## Metrics

Jobs push ML metrics (loss, accuracy, etc.) via the control plane API; the control plane is the sole gateway (agents and node-agent never talk to the metrics backend directly). Metrics are defined at the Platform Experiment level, not per-job — every job is expected to emit its parent experiment's declared metrics, and may emit additional ones (all queryable).

**Storage split (unchanged from original design):**

| Data | Postgres? |
|---|---|
| Job metadata, status, timestamps | Yes |
| Quota balances, credit ledger | Yes |
| ML metric timeseries | No — pushed to the metrics backend (Prometheus-compatible remote-write / GreptimeDB), queried on demand |
| CPU/GPU utilization (node-agent) | No — in-memory rolling window only |
| Eviction decisions (reason, timestamp) | Yes, as fields on the job/experiment record |

`POST /experiments/{id}/metrics` to push, `GET /experiments/{id}/metrics` to query. See `tests/workload/spec.md` for the full agent-facing request/response contract.

---

## Eviction guards (platform-enforced, no per-job config)

Implemented in `controlplane/services/controller/controller.go`.

| Guard | Condition | Reason | Location |
|---|---|---|---|
| Silence | no metric push for ≥ `3 × report_interval_seconds` | `silent` | `checkSilence` (controller.go:576) |
| Crash-loop | k8s Job hits `BackoffLimit` (config `scheduler.job_backoff_limit`, default 3) and is marked `Failed` natively | `FAILED` (job watcher path, not a controller guard) | job watcher |
| Overrun | wall-clock elapsed > `1.5 × estimated_duration_hours`, **unless** agent still has unconsumed quota in the job's tier | `overrun` | `checkOverrun` (controller.go:643) |
| Metric decline | no direction-aware improvement on any tracked metric for > `metric_decline_fraction × estimated_duration_hours` (default 30%); needs ≥ 2 observations | `metric_decline` | `checkMetricDecline` (controller.go:520) |
| Quota exhaustion | actual T4h consumed reaches 99% of the tier quota | `quota_exhaustion` | `checkQuotaExhaustion` (controller.go:397) |

All guards refund unused reserved T4h **except** `quota_exhaustion`, where the budget is genuinely spent. All eviction decisions are logged with reason against the job record in Postgres; metric values themselves are never written to Postgres. See `scheduling.md` for how eviction interacts with the admission loop and preemption.
