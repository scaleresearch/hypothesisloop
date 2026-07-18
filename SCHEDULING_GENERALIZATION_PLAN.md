# Plan: Generalized multi-dimensional admission & fair-share scheduling (v2)

Goal: a job can request CPU only, one configured accelerator only, or a mix. CPU and any
catalogued accelerator get hours-based fair share (guaranteed/burst, like GPU today). RAM,
ephemeral storage, and any uncatalogued extended resource get a hard physical-fit check only — no
hours, no fairness, no preemption machinery for them. Today only GPU is generalized at the billing
layer; RAM/storage are *already* modeled as hours-budgeted dimensions end-to-end, which must be
explicitly migrated away, not silently reinterpreted; and the admission loop assumes one scalar
resource per job with no cross-dimension fit check and a live-capacity race that can over-admit.

This version incorporates a critique of v1 (`SCHEDULING_GENERALIZATION_PLAN_ANALYSIS.md`). Changes
from v1 and why are called out inline as **(v1→v2)** notes.

## Model

Three separable concerns, not one mechanism:

1. **Physical fit** — instantaneous CPU, accelerator, memory, and ephemeral-storage requests must
   all fit on one cluster.
2. **Budget eligibility** — hours-based budgets (CPU, catalogued accelerators only) gate
   submission/use over time.
3. **Runtime enforcement** — Kubernetes requests/limits + native scheduling handle node-level
   placement and pressure; nothing new needed here (see Class B below).

A resource can participate in (1) alone, or in both (1) and (2). CPU/accelerators: both. RAM/
storage/uncatalogued extended resources: (1) only.

## Class A — Hours-based fair-share (CPU + any catalogued accelerator)

1. **Rename `domain.GPUType` → `AcceleratorType`** (`controlplane/shared/domain/gpu.go`). The rate
   registry itself is already generic (open string type, config-driven), but **(v1→v2)**: the
   rename is not cosmetic — `GPUType`/`GPUCount` also appear in the job DSL, DB columns, JSON
   payloads, config (`gpu_types`), cluster-agent status, labels/env vars, and affinity/toleration
   logic. Treat this as a real migration: enumerate every reference before touching it, and don't
   ship it before steps 2-6 land (see Sequencing) — a bare rename without the model change below
   just spreads a second misleadingly-singular type through the codebase.
2. **Introduce a minimal accelerator resource descriptor** (not full substitution-billing —
   **(v1→v2, scoped down)**: substitution rules between accelerator types weren't asked for and
   add speculative complexity). Minimum fields: stable accelerator type ID, Kubernetes
   extended-resource name, flavor/capacity key. A job may request zero or one accelerator type
   (matches today's shape) plus CPU.
3. **Canonical internal resource primitives** — **(v1→v2, new)**: introduce
   `ResourceKey{Kind, Flavor}` and `Footprint map[ResourceKey]int64` in canonical units
   (millicores for CPU, not whole-core rounding; bytes for memory/storage; integer count for
   accelerators). One `fits(capacity, footprint) bool` predicate used everywhere: occupied-footprint
   subtraction, cluster selection, guaranteed/burst admission, and preemption planning. This
   replaces the v1 idea of `admissionUnit()` returning "a few scalar entries" checked independently
   — a mixed job must fit **all** its dimensions on the **same** cluster, so the predicate has to be
   evaluated jointly, not per-dimension. **(v1→v2, correction)**: `domain.JobSpec.NumNodes`
   (`jobspec.go:36-51`) already exists — `BuildJob` compiles any job with `NumNodes > 1` into a k8s
   Indexed Job (`parallelism == completions`) with per-rank env vars and a headless Service for
   rendezvous (`job_build.go:36-259`, `job_lifecycle.go:16-55`). The per-job `Footprint` computed
   for admission must therefore be `per-node-footprint × NumNodes` on every dimension, not
   per-pod — this was missing from v1 and would have under-counted every distributed job's true
   resource need.
4. **`tick()`/`preempt()`** (`loop_tick.go`, `loop_preempt.go`) — rewritten around the `fits()`
   predicate: a candidate cluster is only viable if every requested dimension fits; cluster
   selection among viable candidates uses a stated deterministic policy (e.g. least post-placement
   dominant share) with a stable tie-break, not an ad hoc "most available."
5. **Preemption plans the whole shortage vector before mutating anything** — **(v1→v2)**: compute
   the shortage vector for a candidate cluster, choose one burst-victim set whose aggregate
   footprint covers every shortage, verify the post-preemption vector fits, and only then evict.
   A deterministic greedy heuristic is fine as long as its objective is stated explicitly (e.g.
   fewest victims, then least lost observed progress, then stable ID).
6. **Fix `fetchQuotaMap`'s key** — **(v1→v2, bug fix independent of everything else)**: it's
   currently keyed by `AgentID` alone despite quota being tracked per agent/platform-experiment
   pair, so jobs from different platform experiments can read/overwrite the wrong ratio. Key by
   `(AgentID, PlatformExperimentID)`.
7. **Define and implement one fairness aggregation** — **(v1→v2)**: dominant utilization,
   `max(used_dimension / guaranteed_dimension)` over the dimensions the job actually requests and
   that are tracked (budget = 0/untracked dimensions are excluded, not treated as zero
   utilization). Apply the same treatment to `computePriority`'s cost-efficiency term (currently
   sums raw T4h/CPU-core-hours/RAM/storage-hours, which isn't dimensionally valid) and to the
   `GPUHours()` sort tiebreak (currently always zero for CPU-only jobs). Confirm whether
   `PriorityScore` is actually consumed by `sortGuaranteed`/`sortBurst` — if not, wire it in or drop
   the dead computation.
8. **Schema** — keep the existing parallel-column pattern for CPU/accelerator hours dimensions
   (matches `agent_quotas`/`platform_experiments` today); no shape change needed there.

## Class B — Hard-cap resources (RAM, storage, any uncatalogued extended resource)

No hours, no guaranteed/burst quota, no preemption fairness — must fit at admission, Kubernetes
enforces after that.

1. **Explicit removal migration for the existing RAM/storage hours model** — **(v1→v2, this was
   the biggest gap in v1)**. The current code already treats RAM/storage as hours-budgeted:
   `domain.ResourceRAMGBHours`/`ResourceStorageGBHours`, `BudgetRAMGBHours`/`BudgetStorageGBHours`
   on platform experiments, guaranteed/burst columns on `agent_quotas`, submission-time
   estimate+debit, preemption rescaling, settlement, and phase-2 redistribution. This plan removes
   or deprecates all of it for RAM/storage specifically (CPU/accelerators keep their hours model).
   Decide and document: what happens to existing platform experiments with a non-zero RAM/storage
   budget already set, and to historical usage metrics for those dimensions, during rollout.
2. **Admission check** — extend cluster-agent's existing capacity piggyback
   (`cluster/cmd/cluster-agent/reconcile.go:fetchDesiredState`) to also report live free RAM and
   ephemeral-storage, computed from k8s's own accounting (`node.status.allocatable` minus current
   pod `resources.requests`, counted only against the node a pod is actually assigned to — Pending/
   unassigned pods need a separate pending-reservation, not a blanket cluster-wide subtraction).
   **(v1→v2, correctness fix)**: also fix the CPU/GPU version of this same collector, since it has
   the identical race described in the durable-reservation item below.
3. **No new enforcement code** — **(v1→v2, correction)**: `workload.BuildJob` already sets equal
   requests/limits for CPU, memory, ephemeral storage, GPU, and `ExtraResources` when present. Drop
   the "cluster-agent sets requests/limits" step from v1 entirely; add tests verifying this existing
   behavior instead, plus a decision on what happens when a request is genuinely unset (see
   "explicit requests" below — the answer should be "reject at submission," not "silently
   default").
4. **Accepted limitation, stated explicitly**: ephemeral-storage enforcement in Kubernetes is
   eviction-based and depends on kubelet accounting support, not a hard cgroup ceiling the way
   memory is; `emptyDir`/`/dev/shm` usage can blur into memory accounting. This plan does not
   attempt to fix that — it's a Kubernetes-level property, not something to reimplement — but the
   plan should say so rather than imply RAM and storage are equally hard-enforced.
5. **No new request columns** — **(v1→v2, correction)**: `experiments.job_spec` already stores the
   canonical k8s quantity strings for CPU/memory/storage; that is the source of truth for the
   admission footprint. Do not add a second scalar representation that can drift from it.

## Cross-cutting correctness fixes (apply to both classes)

1. **Require explicit resource requests at submission** — **(v1→v2)**: `JobSpec.CPU`/`Memory`/
   `Storage` may be empty today and get filled in later from a per-cluster `JobDefaults` ConfigMap
   inside the cluster, after the control plane has already picked a cluster and computed a budget
   estimate. This makes the footprint genuinely unknowable at admission time and different clusters
   can silently disagree on defaults. Fix: reject submissions missing a required resource request
   rather than resolving cluster-side defaults pre-admission (matches `important.md`'s "no
   fallbacks — one path or error" over publishing/reconciling per-cluster defaults).
2. **Durable pending-capacity reservations** — **(v1→v2, replaces a real bug in v1's design)**: the
   scheduler currently trusts that live-reported capacity already reflects SUBMITTED/ADMITTED jobs
   and doesn't subtract them again — that's false in the window between `MarkSubmitted` and the
   cluster-agent actually creating the pod, so a second tick can double-admit into the same
   capacity. Fix by keeping the reservation durable in Postgres (not an in-process cache — consistent
   with `important.md` rule 4) and reconciling it against observed cluster state once the pod
   exists, rather than only trusting a point-in-time capacity number.
3. **Fail closed on stale/missing capacity** — any required dimension that's missing or stale in a
   capacity report should exclude that cluster from admission this tick, not silently treat it as
   zero or unlimited.

## Explicitly out of scope / accepted limitations

- **Multi-node/gang-scheduling mechanism itself is NOT out of scope — it's already built and not
  part of this plan's problem.** `JobSpec.NumNodes` + `BuildJob`'s Indexed Job shape already gives
  multi-node distributed jobs today, and per `cluster/docs/execution-layer.md:51,64` the team
  already evaluated and rejected JobSet/Volcano/PodGroup in favor of k8s 1.36's native gang
  scheduling: any Indexed Job with `parallelism == completions` auto-gets atomic all-or-nothing
  admission via an in-tree `Workload`/`PodGroup`, once the cluster has the `GenericWorkload`/
  `WorkloadWithJob`/`GangScheduling` feature gates on (already enabled in local dev). No JobSet
  migration is needed. What *is* in scope for this plan (see Class A step 3 above): making sure the
  control-plane's own admission footprint accounts for `NumNodes` correctly, since that's a
  genuine gap. What remains an accepted limitation: proving *per-node* placement feasibility
  (e.g. whether N specific nodes with the right topology are actually free, not just whether the
  cluster's aggregate capacity sums to enough) — the codebase already documents this as an
  approximation for GPU capacity, and extending the same acceptance to CPU/RAM/storage is
  consistent rather than building a bin-packing/topology solver. Stuck-Pending recovery remains
  the safety net for the rare case aggregate capacity says yes but no valid node placement exists.
- Cross-accelerator-type substitution/billing rules — not requested; the minimal descriptor in
  Class A step 2 deliberately excludes this.
- Control-service leader election/failover — out of scope; not part of this system's design.

## Sequencing

1. Specify the Class B removal migration precisely (list every RAM/storage-hours field/table/path
   to remove or deprecate) and the minimal accelerator descriptor for Class A.
2. Add characterization tests first: mixed CPU+accelerator jobs, fractional CPU, unset-request
   rejection, multi-cluster fit, same-tick and cross-tick reservation, stale capacity, multi-
   shortage preemption.
3. Introduce canonical `ResourceKey`/`Footprint`/`fits()` primitives; wire into occupied-footprint
   subtraction, cluster selection, admission, and preemption planning uniformly.
4. Fix durable pending-capacity reservations (Postgres-backed) before adding new live dimensions —
   this is a correctness prerequisite, not an enhancement.
5. Require explicit resource requests at submission (remove the cluster-side-default footprint
   ambiguity).
6. Implement multi-dimensional cluster selection/admission using `fits()` with canonical units.
7. Implement vector preemption (plan full victim set, verify, then execute).
8. Extend the capacity piggyback to RAM/storage (reusing the existing transport), applying the
   same per-node/assigned-pod accounting fix to CPU/GPU's existing collector.
9. Fix `fetchQuotaMap` key, define and implement the dominant-utilization fairness aggregator, wire
   it into `computePriority` and the sort tiebreaks.
10. Only then do the `AcceleratorType` public rename (DB/config/JSON/labels), since it should follow
    the model rather than precede it.
11. Update observability: expose per-dimension shortage/reason, selected cluster, capacity snapshot
    age, active reservations, and preemption victim rationale.

## Minimum acceptance tests

**(v1→v2, correction)**: extend the existing `tests/e2e-flow.sh` and `tests/advanced-e2e.sh` with
new scenarios rather than standing up new test files/harnesses — these already drive real
submission→admission→execution flows against the running stack, so the new coverage below should
be added as scenarios inside them wherever they already exercise the equivalent single-dimension
GPU case today:

- A mixed CPU+accelerator job is rejected if either dimension is unavailable and admitted only to a
  cluster where both fit.
- RAM/storage are physically checked but never debited as hours budgets; old RAM/storage quota
  fields and flows are removed/deprecated consistently, with no dangling references.
- Two scheduler ticks before pod creation cannot reserve the same capacity twice.
- Fractional CPU requests retain millicore precision through admission and job creation.
- Submissions with an unset required resource request are rejected, not silently defaulted.
- Stale/missing capacity in any required dimension excludes that cluster for the tick.
- Multi-dimensional preemption selects a sufficient victim set before evicting anyone, and does not
  evict if the guaranteed job still can't fit afterward.
- Existing memory/storage/GPU requests and limits remain present in generated Jobs (regression test
  on `workload.BuildJob`, not new code).
- A `NumNodes > 1` job's admission footprint is `per-node × NumNodes`, and it is admitted only where
  the cluster's aggregate capacity covers the full multi-node total, not just one node's share.
- Fairness tests cover multiple platform experiments, untracked dimensions, CPU-only jobs,
  accelerator-only jobs, and mixed jobs.
- Schema migrations are tested against the current schema, not only a fresh database.
