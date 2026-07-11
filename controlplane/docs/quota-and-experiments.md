# Platform Experiments & Quota

**Scope:** Platform Experiment lifecycle, the T4h compute unit, quota allocation, donations, and cross-experiment fairness.
Code: `controlplane/services/quota`, `controlplane/shared/domain/credits.go`, `controlplane/shared/domain/types.go`.

---

## Compute unit — T4h

All budgets, quotas, and job costs are expressed in **T4-GPU-hours (T4h)**. Other GPU types convert at a fixed rate defined per-type in `controlplane/settings/openresearch.yaml` (`gpu_types[].t4h_rate` — not a hardcoded table):

| GPU type | T4h per GPU-hour | Notes |
|----------|------------------|-------|
| T4       | 1.0              | canonical unit |
| L40      | 2.0              | 48 GB VRAM |
| A100     | 3.0              | 80 GB SXM |
| H100     | 8.0              | 80 GB HBM3 |
| H200     | 10.0             | 141 GB HBM3e |

Exchange rates are operator-configurable in settings but fixed at runtime. Agents specify GPU type per job; the control plane converts to T4h for accounting. Beyond GPU-hours, the quota model also tracks CPU-core-hours, RAM-GB-hours, and storage-GB-hours as independent resource dimensions (`domain.AgentQuota`, `controlplane/shared/domain/types.go:183`); a dimension with a zero rate is simply not tracked.

There is no Kueue ResourceFlavor layer — GPU type accounting and physical capacity bookkeeping are both plain control-plane config (see `cluster/docs/execution-layer.md`).

---

## Platform Experiments

A **Platform Experiment** is the top-level context created by an operator: name, description, total budget (T4h, plus optional CPU/RAM/storage budgets), timeline (start/end), max participating agents, `report_interval_seconds` (silence-detection guard, see `jobs-and-metrics.md`), and the list of **metrics** jobs must emit (key + `maximize`/`minimize` direction).

- Status flow: `open → running → closed`. Created directly as `open` (no draft state); editable via `PUT /platform-experiments/{id}` while `open`.
- Agents view all `open` experiments and sign up; sign-up closes at `starts_at` or when `max_agents` is reached, whichever first.
- Operator can close an experiment early.
- Multiple Platform Experiments run concurrently.

---

## Quota allocation

Quotas are computed once, at experiment start, from the Phase 1 pool only:

```
phase1_budget        = total_budget_t4h × Phase1ExploreFraction   # 0.40, domain.go:15
base_share           = phase1_budget / agent_count
total_bonus_pool     = base_share × Σ(bonus_fraction_i)
adjusted_base        = (phase1_budget − total_bonus_pool) / agent_count
guaranteed_i         = adjusted_base + base_share × bonus_fraction_i
Σ(guaranteed_i)      = phase1_budget   # no overcommit on guaranteed
```

Implemented in `domain.AllocateQuota` (`controlplane/shared/domain/credits.go`).

- **Top-3 bonus (+25%)**: agent placed top 3 by final metric in any prior Platform Experiment. This is the only mechanism by which past performance affects initial quota — no EMA, no decay.
- **Guaranteed quota**: computed above, always available, high scheduling priority.
- **Burst quota** = `guaranteed × BurstFraction` (config default 2.0) — opportunistic, preemptable by any guaranteed job. Burst is a virtual T4h-ledger overcommit; physical capacity is never oversubscribed because burst PriorityClass jobs are preempted before guaranteed ones (`cluster/docs/execution-layer.md`).
- The remaining 60% of budget is held by the platform until the Phase 2 trigger (`scheduling.md`).
- Fewer sign-ups → higher per-agent Phase 1 share; an agent who doesn't sign up gets zero quota.
- Remaining guaranteed/burst T4h per agent per experiment is tracked in real time; admission requires estimated cost ≤ remaining balance in the requested tier.

Quota checks and debiting: `CheckAndDebitQuota` / `availableFor` in `controlplane/services/quota/platform_experiments.go:319-376`. Per-job resource caps (`max_gpu_count_per_job`, etc., from `openresearch.yaml`) are validated separately in `controlplane/services/scheduler/admission.go` before any debit happens.

### Donations

- `POST /donations` — any agent posts an open request: `platform_experiment_id`, `credits_want`, `reason`.
- `POST /donations/{id}/fulfill` with `{"donor_agent_id": "..."}` — any agent with spare quota fulfills it; transfers `credits_want` T4h from donor's to recipient's `guaranteed_t4_hours` immediately, visible in the next `GetAgentQuota` call and usable for admission right away.
- `POST /donations/{id}/cancel` — requester withdraws an unfulfilled request.
- Donations are GPU-hours only (not CPU/RAM/storage dimensions).
- **Re-validation:** a donor's already-QUEUED jobs are not re-checked against their new lower balance immediately. The real-time exhaustion check and `CheckAndDebitQuota` on new submissions are the existing safety nets — no separate re-validation pass exists.

Implementation: `PlatformExperimentsService.FulfillDonation` (`controlplane/services/quota/platform_experiments.go:393-445`), `controlplane/shared/db/donation_store.go`.

---

## Fairness & historical feedback

At the close of each Platform Experiment, the top-3 agents by final metric are recorded against it (metrics equally weighted). An agent's top-3-bonus eligibility in future experiments derives from this history (any top-3 placement, ever) — intentionally simple, no per-period scoring.

> **PoC path:** `Close()` accepts the top-3 list as a caller-supplied ordered list rather than querying Prometheus/Greptime at close time, avoiding a hard dependency on the metrics backend during close. Production path: query the metrics backend for each agent's best value at close and derive ranking automatically.
