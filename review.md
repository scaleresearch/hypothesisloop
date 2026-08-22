# Review — pre-rebuild pass

Findings from a full re-read of the front-end, core scheduling/control logic, and metrics/quota
handling against `important.md`, verified by a second reviewer (codex) which also corrected two
severity claims and added four findings of its own (N1-N4).

**Status: all decided. Fixed items are marked DONE; deferred items carry the reason.**

| # | Problem | Decision |
|---|---|---|
| D1 | Second preemption subtracts lifetime elapsed from an already-rescaled estimate; reservation collapses to the floor | DONE — rescale against the current stint only |
| D2 | Queued *burst* job could evict a running *guaranteed* job | DONE — victim tier restricted |
| D3/N3 | One bad row aborts the whole admission pass, permanently | DONE — collect + `errors.Join`, both exit paths |
| D4/D5/N4 | Disbalance evictor over-kills: after preempt, per blocked job, and after a preempt error | DONE — runs once per cluster per tick, and only when preempt neither committed nor failed. A *partial* plan now returns an error too, so it also stands down (found by codex on the second pass) |
| D6 | One metrics hiccup aborts every tick for a sort tiebreak | DONE — log and skip |
| D7 | Flavor substitution committed before the capacity claim, never reverted | DEFERRED — needs the reservation moved inside `ClaimSubmitted`'s transaction; real but narrow (requires a claim failure *and* a later re-resolve) |
| N1 | Preemption could drag a terminal job back to QUEUED | DONE — `AND status = 'RUNNING'` guard |
| N2 | 14-day lookback caps billable hours for very long jobs | DEFERRED — no job today approaches it; fixing it properly means a settled-usage checkpoint, not a bigger window |
| E1 | Quota and settlement measured observed hours on different grids | DONE — one `ObservationCadence()` for the deployment |
| E2 | Tail over-bill: counted points not intervals, and billed the gap cap past the last real sample | DONE — intervals, capped at the last real sample |
| E3 | Stage cut released a cut agent's *reserved* budget, stranding the difference | DONE — released against observed |
| E4 | One de-registered flavor took down the whole quota listing | DONE — log and skip |
| E5 | Absent metric series reads as zero usage, reopening spent budget | DEFERRED — needs a `found` signal threaded through `PopulateUsage*` and an admission fail-closed policy; wanted, but a behaviour change to admission that deserves its own pass |
| E6 | Node-agent drops heartbeats on push failure; the retry queue its handler assumes does not exist | DEFERRED — either a bounded retry buffer or delete the handler's claim; under-bills silently, does not mis-evict |
| E7 | Cut ranking and final standings duplicate a PromQL query that has already drifted (#20) | DEFERRED — the drift is real (a constraint-violating agent survives cuts then vanishes from standings); fix is one `metricsdb.BestPerAgent` both callers share |
| A1 | `crash_loop`/`agent_removed` emitted by nothing; README advertised `crash_loop` | DONE — deleted. Caveat (codex): `never_reported_metrics` covers a crash-looping job only if it has declared metrics and never emitted one; a job that reported before crash-looping relies on the runtime's retry limit producing FAILED |
| A2 | Agent balance KPIs fed by a ledger nothing writes | DEFERRED — endpoint is deliberately live-but-empty; noted, not hidden |
| B1-B5, B7-B10 | Phantom API fields, dead fallbacks, drifted TS types, eviction rendering, `PodContent`, duplicated quota math, dead code | DONE |
| B6, B11, B12 | Dashboard computes headline figures from a 1000-row fetch; duplicate SWR keys and per-row fan-out; unmemoized joins | DEFERRED — all three want an aggregate endpoint that does not exist yet; correctness of the *displayed* numbers is unaffected below 1000 jobs |
| B13 | No ESLint config at all | DEFERRED — worth doing, but adding a linter now would bury this diff in unrelated churn |
| C1 | `never-reported-metrics.sh` stated a 30s grace when the real one is ~120s, and waited too briefly to ever observe the eviction | DONE |
| C2 | Eviction reasons with no scenario | RESOLVED by A1 — the two uncovered reasons no longer exist; `workload_gone` stays untestable (the cluster-agent self-heals a deleted workload) |

Detail on each follows.

---

## A. Dead machinery / documented behaviour that does not exist

### A1. `crash_loop` and `agent_removed` are eviction reasons nothing ever emits
`controlplane/shared/domain/constants.go:87,90`

Neither constant is referenced anywhere outside its own declaration, the UI's label map
(`ui/src/lib/eviction.ts:8,11`) and — for `crash_loop` — a README bullet advertising it as a
platform feature (`README.md:10`). There is no "remove agent" operation in the codebase at all;
agents leave a platform experiment only via stage cuts, which already have their own reason.

`crash_loop` is the more serious half. The restart count is plumbed through the entire stack and
then thrown away: both executors return it (`runtime/k8s/internal/k8sexec/phase_detail.go:30`,
`runtime/bare-metal/internal/podexec/phase_detail.go:19`), the agentloop pushes it
(`runtime/shared/agentloop/agentloop.go:408`), `queuebackend` reads it back
(`controlplane/shared/queuebackend/queue_backend.go:101`) — and `job_watcher_scan.go:65` discards
it into `_`. A pod restarting forever therefore holds its accelerator and bills for it until the
silence check or `job_too_long` eventually catches it, under a reason naming the wrong fault.

**Fix:** either wire the eviction up from the already-collected restart count, or delete the
constant and the plumbing. Retaining both halves unconnected is machinery kept for nothing
(#2) plus a README documenting behaviour that does not exist (#17).

### A2. Agent credit-ledger surface is fed by a ledger nothing writes
`controlplane/shared/domain/agent.go:15-27`

`AgentBalance` is documented as always zero because nothing appends to `credit_ledger`. The agents
page renders balance, performance bonus and experience bonus from it, so three KPI columns are
permanently 0 with no indication they are structurally empty rather than genuinely zero.

**Fix:** decide one way. Either drop the endpoint and its UI columns, or note in the UI that the
ledger is inactive. Keeping a live-looking dashboard fed by a dead table is the "unknown rendered
as a real value" problem in #A2's sibling findings below.

---

## B. Front-end

### B1. The entire "Bonus Eligibility" column reads a field the API never sends
`ui/src/app/agents/page.tsx:141,269`

`(b as any).top3_count` on an `AgentBalance`, which has no such field — `top3_count` lives on
`domain.Agent`, from `GET /agents`, which this page never fetches. The "Top-3 Eligible" KPI is
therefore permanently 0 and every "+25% Top-3" chip renders inactive for every agent. An operator
reads that as fact.

**Fix:** fetch `/agents` (already wrapped as `fetchAgents`) and join on `agent_id`; drop the
`as any` so the compiler catches the next one.

### B2. Job pages render six fields that do not exist on the API's job record
`ui/src/app/jobs/page.tsx:272`, `jobs/[id]/page.tsx:204,206,207,208,289`,
`dashboard/page.tsx:297`

`final_metric`, `actual_cost_acch`, `started_at`, `completed_at`, `final_metric_value`,
`env_image`, `metric_at_eviction` — none exist on `domain.Experiment`
(`shared/domain/experiment.go:11-67`). A whole "Final Metric" table column and a "Metric at
Eviction" audit column are permanently `—`, and the Actual cost / Started / Completed rows never
render. The reader cannot distinguish "not reported yet" from "this field was never real".

Two pages already do this correctly, from the metrics store: `hypotheses/[id]/page.tsx:28`
(`JobFinalMetric`) and `platform-experiments/[id]/page.tsx:588` (`finalMetricByJobId`).

**Fix:** delete the phantom columns/rows, or source them from the metrics store the way those two
already do. Metric values come from the metrics store, never a job record (#3).

### B3. `const j = job as any` erases the type that would have caught B2
`ui/src/app/jobs/page.tsx:220`, `jobs/[id]/page.tsx:51`, `hypotheses/[id]/page.tsx:171`

**Fix:** delete the casts and use the typed `Experiment`, adding the fields the TS type is
genuinely missing (`cluster_name`, `phase_detail`, `updated_at`, `theory`, `priority_score`,
`novelty_score`, `artifacts`, `project_id`, `queued_at` — all confirmed present in Go).

### B4. `types/index.ts` is out of sync with `shared/domain` in both directions
`ui/src/types/index.ts:47-51,109-149,298`

Declares fields Go does not have (the `actual_*` set, `final_metric_value`, `started_at`,
`completed_at`, `env_image`, `MetricDataPoint.value/step/wall_time`, `AgentBalance.period`) and
omits ones it does. `MetricDataPoint`'s `metric_name`/`metric_value`/`recorded_at` are also marked
optional though Go always sends them. The file reads as a contract while being actively wrong.

**Fix:** align against `shared/domain`; delete the phantom fields and the deprecated RAM/storage
block along with the UI branches guarding on it.

### B5. `?? (p as any).value` is a fallback to a field that does not exist
`ui/src/app/jobs/[id]/page.tsx:78`, `platform-experiments/[id]/page.tsx:55`

The fallback can only ever yield `undefined`, which recharts plots as a gap. A fallback where
there is exactly one real field (#1).

**Fix:** `p.metric_value`, full stop.

### B6. Dashboard recomputes every headline figure client-side from a truncated fetch
`ui/src/app/dashboard/page.tsx:38-41,58-113`; `ui/src/app/page.tsx:20`

`fetchExperiments({ limit: 1000 })`, then totals, completion rate, crash rate, early-stop rate,
tier breakdown and eviction counts are derived in the browser. Past 1000 jobs every figure is
silently wrong with no indication, and the numbers are the UI's own arithmetic rather than the
store's (#3). It is also the heaviest request in the app and the landing page issues it for one
number.

**Fix:** expose an aggregate endpoint and read it; the landing page reads only its own counts.

### B7. Eviction reasons render three different ways, one bypassing the parser
`ui/src/app/dashboard/page.tsx:294` (and `:117`)

Uses `EVICTION_LABELS[e.eviction_reason]` directly, so any reason carrying a `"code: detail"`
payload — which the scheduler now writes — misses the map and renders raw. `:117` groups on an
already-extracted code and loses the detail. `platform-experiments/[id]:572,992` and
`jobs/[id]:54` use `evictionLabel()` correctly.

**Fix:** every render through `evictionLabel()`, every grouping through `evictionCode()`; stop
exporting the raw map.

### B8. `PodContent scrollX` silently discards `className`, `style` and `onClick`
`ui/src/components/ui/pod.tsx:53-60`

The `scrollX` branch returns a bare div, dropping `wa-pod-content` and every passed prop. 15+ call
sites use `scrollX` and all lose the panel's standard padding.

**Fix:** always render the `wa-pod-content` div and apply `overflowX` to it.

### B9. Quota-remaining is computed four times under two different definitions
`platform-experiments/[id]/page.tsx:1037-1045,835-836`, `platform-experiments/page.tsx:217-224`,
`agents/page.tsx:50-52`

Two clamp per-dimension, the scoreboard row does not clamp at all — so an over-spent agent shows
different numbers in the scoreboard and the quota table *on the same page*.

**Fix:** one `lib/quota.ts` with `quotaUsed/quotaTotal/quotaRemaining`, used everywhere; the
guaranteed/burst/remaining `<tr>` is identical in two files and becomes one component.

### B10. Duplicated helpers and dead code
- `relTime` copy-pasted at `jobs/page.tsx:20`, `hypotheses/page.tsx:27`,
  `hypotheses/[id]/page.tsx:13`; none handle the Go zero-time that `lib/format.ts:4` exists to
  catch, so an unset timestamp renders as "…y ago" nonsense. Elsewhere the same timestamps use
  raw `toLocaleString()`.
- `ui/src/components/ui/card.tsx` — zero references anywhere; also the only file still on raw
  Tailwind `slate-*` utilities.
- `lib/api.ts` — `fetchAgentBalances`, `fetchAgentLedger`, `fetchExperimentLineage`,
  `fetchResourceCatalog` have no callers.
- `platform-experiments/page.tsx:16` imports `ExperimentStatus`, never used.

**Fix:** move a zero-time-aware `relTime` into `lib/format.ts`; delete the rest.

### B11. Conflicting SWR keys and per-row fan-out
`page.tsx:20` uses key `'jobs-all'`, `dashboard/page.tsx:39` uses `['jobs-all']` for the identical
request — two cache entries, two 1000-row fetches. `platform-experiments/page.tsx:69-84` issues
three requests per card (30 requests every 10s for a 10-card page).
`hypotheses/[id]/page.tsx:29` mounts one metrics fetch per job row, each polling at 15s.

**Fix:** one key per logical resource; return the per-PE aggregates the cards need from the list
endpoint.

### B12. Unmemoized heavy joins in the render body
`platform-experiments/[id]/page.tsx:538-549,551,588-592,597-602`

Walk the full timeseries of every job on every render, while the much smaller
`CompetingAgentsChart` pivot is carefully memoized for exactly this reason. The file is 1095 lines
with eight top-level components.

**Fix:** memoize; split the page into scoreboard / competing-agents-chart / stage-ladder.

### B13. No lint config at all
There is no `.eslintrc*`, no `eslint.config.*`, no `lint` script — yet
`platform-experiments/[id]/page.tsx:256` carries an `eslint-disable-next-line
react-hooks/exhaustive-deps`. The rule that would have caught most of B1–B5 is not running.

**Fix:** add `next/core-web-vitals` + `react-hooks` and a `lint` script.

---

## C. e2e coverage

### C1. `never-reported-metrics` states a grace period four times shorter than the real one
`tests/scenarios/never-reported-metrics.sh:20`

The comment computes the grace as `silence_multiplier (3) × report_interval (5) × 2 ≈ 30s`,
ignoring `min_silence_window_seconds: 60`, which floors the window. The real grace is `2 × 60 =
120s` plus reconcile lag. The scenario likely still passes (its mute job runs 180s) but its margin
is far thinner than its own reasoning claims, and a scenario whose stated timing is wrong is one
config change from flaking.

**Fix:** correct the comment and size the wait against the actual floor.

### C2. Eviction reasons with no scenario
Covered: `silent`, `never_reported_metrics`, `quota_exhaustion`, `experiment_closed`, `cancelled`,
`job_too_long`, `stuck_pending`, `resource_disbalance`, `unschedulable`, and `stage_cut` (covered
mechanism-level by `stage-ladder-cut.sh`).

Uncovered: `crash_loop` and `agent_removed` — both are A1's dead constants, so coverage follows
whatever A1 decides. `workload_gone` remains effectively untestable: deleting a workload is
self-healed by the cluster-agent.

---

## D. Scheduler / controller

All five of D1–D5 were re-traced by hand against the source; they are not speculative.

### D1. Repeated preemption collapses a job's estimate — and therefore its bill — to ~0
`controlplane/services/scheduler/loop_preempt.go:128`

`remaining := victim.EstimatedDurationHours - elapsed[victim.ID]` mixes two different bases.
`EstimatedDurationHours` is already the *rescaled remaining* estimate after any prior preemption
(`RequeuePreempted` overwrites it, `db/experiments_store_lifecycle.go:151`), while `elapsed` is
`metricsdb.ObservedElapsedHours` over the job's **whole life**, across every stint.

**Failure:** 10h job. Runs 4h → preempted → estimate becomes 6h (rate preserved). Runs 2h more
(cumulative observed = 6h) → preempted again → `remaining = 6 − 6 = 0`, clamped to
`MinRemainingHours`, so `ratio ≈ 0` and `newCostAccH`/`newCPU`/`newRAM`/`newStorage` are all
written as ~0. The reservation vanishes from the desired-usage sum, and `Settle`'s
`rateCost = estimated/EstimatedDurationHours` is ~0 — so the whole run, including the 6h already
consumed, settles at **zero observed cost**. The agent computes for free and quota exhaustion can
never fire against it.

**Fix:** compute `remaining` against the *original* estimate and cumulative elapsed, or persist
the elapsed already accounted for at the last requeue and subtract only the current stint.

### D2. A queued *burst* job can evict a running *guaranteed* job
`loop_tick.go:275` → `loop_disbalance.go:123`

The burst admission pass calls `evictDisbalanced` for every burst job that fails to fit, and
`evictDisbalanced`'s candidate filter is only `exp.ClusterName != cluster || exp.ID == blocked.ID`
— no tier filter. `preempt()` is deliberately scoped to
`filterTierCluster(running, domain.CapacityBurst, cluster)` (`loop_tick.go:194`) for exactly this
reason.

**Failure:** a guaranteed job holding 1 accelerator + 40 cores on a 4-accelerator node is above
tolerance. A burst job queues, misses on CPU, idle accelerators exist → the guaranteed job is
selected and terminated so that a burst job might later be admitted. Tier priority inverted, live
guaranteed work destroyed.

**Fix:** pass the allowed victim tier in; a burst blocker may only ever displace burst.

### D3. One bad experiment wedges admission for the entire platform, permanently (#19)
`loop_tick.go:101,104,111,117,121,134,141,213,225,280,292`

Every one of these is a `return err` from inside a per-experiment loop over `GetPlatformExperiment`,
`IsAgentCut`, `HasUnsummarizedCompleted`, `UpdateNotAdmittedReason`.

**Failure:** one QUEUED experiment references a deleted `platform_experiment_id` → line 104 returns
`platform experiment %s not found` → `tick()` aborts before either admission pass. That row is
QUEUED forever and re-read every tick, so **no job anywhere is ever admitted again**, indefinitely.
The controller's `Reconcile` gets this right (`reconcile.go:30,110` — accumulate, `errors.Join`);
`tick()` does not.

**Fix:** mirror the controller — log, accumulate, `continue`, join at the end of the pass.

### D4. Preemption and disbalance eviction both fire for the same shortage in one iteration
`loop_tick.go:204-210`

`preempt()` plans a victim set covering the **entire** shortage, then `evictDisbalanced` is called
unconditionally against the **unchanged** `gAvail`/`nodeAvail` — neither is decremented, both being
fire-and-forget. `disbalancePremises` requires the shortage be confined to CPU/memory/storage,
which is precisely the shortage `preempt` just committed to freeing.

**Failure:** guaranteed job short 20 cores. `preempt` requeues a burst job releasing 24. Two lines
later `evictDisbalanced` re-reads the same 20-core shortage and *also* terminates a disproportionate
job. Twice the live work destroyed for one deficit.

**Fix:** have `preempt` report whether it committed a covering plan, and skip the disbalance pass
when it did.

### D5. N blocked jobs evict N victims for the same idle accelerators
`loop_tick.go:208,275`

`avail`/`nodeAvail`/`strandedNodes` are never updated after an eviction is issued, and
`evictDisbalanced` re-lists running experiments each call — so the previous victim is excluded and
it picks a *different, still-live* job.

**Failure:** node A has 2 accelerators stranded by one oversized job; five queued jobs all miss on
CPU. Iteration 1 evicts V1 (covers the shortage). Iterations 2–5 see identical availability and
evict V2…V5. Four extra running jobs destroyed to free capacity only one queued job can use.

**Fix:** the disbalance pass is a cluster-level verdict, not a per-blocked-job one — run it at most
once per tick per cluster and record the nodes a plan has already claimed.

### D6. One metrics hiccup aborts every tick, for a value that is only a sort tiebreak
`loop_preempt.go:174-194` (`completionFractions`, called at `loop_tick.go:78`)

One `ObservedElapsedHours` query per **QUEUED** experiment, `return nil, err` on the first failure,
before any admission happens. The result feeds only a sort tiebreak (`loop_sort.go:98,131`) and is
always 0 for a job that has never run. `GetTotalCapacity` twelve lines above is already correctly
"log and continue" for exactly this reason. Invariant 19.

**Fix:** skip the failing experiment at fraction 0 and log; query only jobs with a prior stint.

### D7. Flavor substitution is committed before the capacity claim and never reverted
`loop_preempt.go:205-211` — *not yet independently verified; flag for codex*

`ReserveAdmittedFlavor` commits `accelerator_type` and `estimated_cost_acch` in its own transaction;
`ClaimSubmitted` runs afterwards and may return `claimed == false`, leaving the job QUEUED with the
substituted flavor persisted. The in-memory revert at `loop_tick.go:180` never reaches the DB.

**Failure:** job requests L40, A100 acceptable. Tick 1 resolves A100, writes `accelerator_type=a100`
plus the A100 cost; the claim then fails. Tick 2 resolves back to L40, so the
`exp.AcceleratorType != exp.Job.AcceleratorType` guard is false and no reservation write occurs —
the job runs as L40 while Postgres records A100. Settlement bills at the A100 rate
(`settlement.go:114`) and stage progress is priced at it (`observed.go:65`).

**Fix:** move the flavor reservation inside `ClaimSubmitted`'s transaction, which already holds the
per-cluster lock.

### D8. Minor
- `reconcile.go:114,185` — comments say quota-exhaustion evictions get "No refund", but
  `settleAndMark` writes *observed* usage, below the reservation. The behaviour is right per #18;
  the comments are wrong.
- `loop_tick.go:279` passes `nil` shortage to `notAdmittedReasonFor`, so burst jobs never get the
  "short {…}" detail the guaranteed path provides — the diagnostic that function's own comment
  argues is essential.
- `loop_types.go:190` panics on an unwired disbalance evictor but does not validate
  `metricsDBURL`/`observedGapCap`/`observedStep`, which `preempt()` and `completionFractions` need
  on *every* tick. An unwired URL silently disables the disbalance pass while `preempt` queries an
  empty URL.

---

## E. Metrics collection / quota

The metrics reviewer independently reached D1 by a different route (via the reservation
under-count rather than the settlement rate) — two independent confirmations of the same defect.

### E1. The same job has two different "observed hours" depending on who asks
`services/quota/running_cost.go:65-75` vs `services/controller/observed.go:20-25` and
`cmd/metrics-service/main.go:116-118`

`runningCostCalcFor` derives `step` from **the platform experiment's** `ReportIntervalSeconds` and
`gapCap` as 3× it. The controller's `observedStep()` and the settler both use the **global**
`default_report_interval_seconds` and ignore the PE value. Since `ObservedElapsedHours` bills
`step × len(grid)`, the step *is* the quantum.

**Failure:** a PE with `report_interval_seconds: 600`. A job alive 12 min bills ~20 min through
`GetObservedAgentQuota` (10-min grid) but ~12 min through `settlement.Settle` (30 s grid). The
eviction check therefore fires on a number the bill will never match, and `JobCost` shows the
PE-grid figure while running and the settlement figure once settled — the same job's cost visibly
jumps at completion. `main.go:115`'s comment claims "every observed-usage query in this deployment
agrees on what 'how long did this run' means". It does not.

**Fix:** derive step/gapCap in exactly one place and hand the identical pair to every
`ObservedElapsedHours` caller.

### E2. Every job is over-billed by roughly `gapCap + step` at its tail
`shared/metricsdb/observation_query.go:69,282`

Grid points come from `last_over_time(metric[gapCap])`, which yields a point at every grid
timestamp within `gapCap` **after** the last real sample; `ObservedElapsedHours` then returns
`step × len(union)`, counting points rather than intervals over an inclusive grid.

Mid-run that forgiveness is deliberate and documented — a blip shorter than `gapCap` should still
read as alive. At the *tail* there is no later sample to bridge to, so the same mechanism invents
time that was never observed.

**Failure:** 30 s interval, 90 s gapCap. A job that heartbeats once and dies produces points at
t, t+30, t+60, t+90 → billed 2 min for one instant of life. Every normally-finishing job is billed
~90 s past its last heartbeat plus one extra step for the inclusive endpoint. `Settle` writes this
as the final bill, so the whole platform is systematically over-charged by a fixed ~2 min/job, and
`checkQuotaExhaustion` and `StageProgress` inherit the inflation.

**Fix:** count intervals (`step × (len(union) − 1)`) and drop grid points more than one `step` past
the newest real sample.

### E3. A stage cut strands the gap between a running job's estimate and its real cost
`services/controller/stages_cut.go:25-30,169`

`release += guaranteedOf(q) − usedOf(q)`, where `used` was filled by `PopulateUsage` (settled
observed) **plus** `AddDesiredQuotaUsage` (reservations, including each RUNNING job's full
estimate), with no observed-cost correction.

**Failure:** a cut agent has a job estimated at 100 AccH that has consumed 10. At the boundary
`used` reads 100, so only `guaranteed − 100` returns to survivors; moments later the job is evicted
and settled at the real 10. The 90 AccH difference is released to nobody and the cut agent's row is
zeroed — budget permanently lost, once per cut agent with in-flight work.

**Fix:** correct running costs to observed before computing `release`, or compute the release after
the cut agent's jobs have settled.

### E4. One job on a de-registered flavor takes down the whole quota listing (#19)
`services/quota/running_cost.go:100-107,130-133`, `platform_experiments_quota.go:100`

`costOf` errors on `no registered rate for accelerator type`, `correctRunningCosts` propagates it
out of the per-job loop, and `ListQuotas` returns it. One job admitted on a flavor later removed
from `rate_by_name` takes down every agent's quota, the dashboard and the donation headroom check
for the entire platform experiment.

**Fix:** log-and-skip that experiment, leaving its admission estimate in place exactly as the
existing `!rc.found` branch already does, and finish the pass.

### E5. An absent metric series is indistinguishable from zero usage, and zero means "spend freely"
`shared/metricsdb/usage.go:117-124,139-142`

`PopulateUsage`/`PopulateUsageOne` only write `Used*` when a sample comes back; a successful query
returning an empty vector leaves them at zero, and no caller can tell "nothing consumed" from "the
store answered with nothing". `SettledCostForJob` in the same package *does* return an explicit
`ok`.

**Failure:** GreptimeDB restored from backup, a `kind`/`tier` label spelling drift, or the series
aged past retention — `AdmitExperiment` then evaluates every agent against `used = 0` and admits
the full budget again on top of what was already spent.

**Fix:** return which buckets were actually answered (or an overall `found`) and make admission fail
closed rather than read empty as zero.

### E6. Node-agent drops heartbeats on push failure; the retry queue the handler relies on does not exist
`runtime/k8s/cmd/node-agent/main.go:112-114,118-135` vs `shared/metrics/handler.go:60-65`

`push()` returns a bool that is discarded — no buffer, no retry, no backlog. The handler returns 503
on a GreptimeDB write failure with the comment "Non-2xx tells node-agent's retry queue to keep this
payload and retry later". There is no such queue.

**Failure:** a 10-minute GreptimeDB outage with 20 jobs running produces no heartbeat samples ever
for those 10 minutes; the gaps exceed `gapCap`, so every affected job is permanently under-billed by
10 min, silently. (Eviction is incidentally safe: the cluster-agent's phase push fails too, so
`checkSilence` reads "can't tell".)

**Fix:** a bounded in-process retry buffer keyed by the *collection* timestamp (`WriteGaugeAt`
already preserves it), re-pushed until acked — or delete the comment and document the loss.

### E7. Cut ranking and final standings duplicate a PromQL query that has already drifted (#20)
`services/controller/stages_rank.go:143` and `services/quota/platform_experiments_results.go:190`

Both build near-identical PromQL against `experiment_metric_value` from inside a service package —
an ad-hoc metrics-store query outside `metricsdb`. Both comments promise that a mid-run cut and the
final results can never rank the same field differently; that is already false. The results query
groups by `job_id` and its caller filters out constraint-violating agents
(`platform_experiments_results.go:105-137`); the cut query does neither. **An agent whose run
violates a declared constraint survives every cut on rank, then vanishes from the final standings.**

**Fix:** one exported `metricsdb.BestPerAgent(...)` returning per-agent best plus the producing
`job_id` and the non-raw set, with both callers applying the same constraint filter on top.

### E8. Minor
- `platform_experiments_quota.go:34` (`GetQuota`) corrects only the **accelerator** dimension for
  running jobs; CPU-core-hours keep the full admission estimate, so two dimensions of the same
  displayed quota sit on different bases.
- `platform_experiments_results.go:126` re-runs `ListSignups` inside the per-metric loop, once per
  constraint metric, for a value that cannot change within the call.
- `ObservedLookback` (`usage.go:87`) sizes every `last_over_time` window to the PE's full lifetime,
  making each admission check a multi-week range scan on the hot path for a long-running PE.


---

## Verification

- `gofmt`, `go build ./...`, `go vet`, `go test -count=1` — 14 packages, all pass
- `tsc --noEmit` and `next build` — clean
- `bash -n` on all 27 scenarios, `tests/lib/*`, `tests/run.sh` — clean
- New tests: 4 in `scheduler/loop_tick_resilience_test.go`, 3 in `metricsdb/observed_elapsed_test.go`.
  The scheduler ones were checked against a deliberately reverted fix to confirm they fail without it.
- Two codex passes: the first verified findings and killed one (D3 — already fixed before it ran),
  corrected D1's and E2's severity, and added N1-N4. The second reviewed the fixes and found one
  real hole (partial preemption plan), now closed.

Not run: the e2e suite. No scenario has been executed against these changes.
