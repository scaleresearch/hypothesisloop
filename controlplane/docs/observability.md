# Observability & Audience Separation

**Scope:** what's persisted where, phase/status read APIs, and who can see what.
Code: `controlplane/shared/metrics`, `controlplane/services/registry`, `controlplane/services/controller`.

Metric data never enters Postgres. ML timeseries live in the metrics backend (Prometheus-compatible remote-write, backed by GreptimeDB — `controlplane/services/registry/remotewrite.go`); utilization and silence state live in-memory. The control plane is the sole gateway — agents and `node-agent` push to it, never directly to the metrics backend.

### ML metric timeseries

Agents push via `POST /experiments/{id}/metrics`; each push is stored labelled with `job_id`, `platform_experiment_id`, `agent_id`, `metric_name`, queryable on demand (`GET /experiments/{id}/metrics`). At the Phase 2 boundary the controller queries the metrics backend directly for each agent's best (direction-aware) value over the experiment lifetime — decoupled from in-memory state, correct across restarts (see `scheduling.md`).

### GPU/CPU utilization per pod

`node-agent` (DaemonSet, `cluster/cmd/node-agent/main.go`) pushes per-pod CPU utilization samples every ~2s to the metrics service's `/internal/node-metrics` endpoint. Held in an in-memory rolling window, never written to Postgres. Used for preemption victim selection and stall detection where available; on setups without utilization data, preemption falls back to elapsed-time-only victim selection.

### Metric silence detection

Each reconcile tick, the controller syncs the last-seen metric timestamp per running job from the metrics backend, compares the gap to `3 × report_interval_seconds`, and evicts silently-failing jobs (`silent`, see `jobs-and-metrics.md`). Syncing from the backend (not local state) keeps this correct across service restarts and multi-instance deployments.

### Phase status

`GET /platform-experiments/{id}/phase2-status` returns current phase, when Phase 2 triggered (if applicable), and which agents are active vs. held — the authoritative view for UI and agents.

### Audience separation

- **Agents:** own quota balance, own job status and metrics, experiment list, sign-up status, phase2-status.
- **Operators:** all of the above, plus per-agent quotas, eviction audit log, cluster utilization, and experiment lifecycle endpoints.

See `tests/workload/spec.md` for the concrete agent-facing endpoint contract.
