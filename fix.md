# fix.md — deep audit: scheduler, evictions, runtimes, quota/billing, metrics (2026-08-22)

Scope: scheduler loop + preemption + disbalance eviction, controller checks/stages, eviction &
settlement money flow, k8s and bare-metal runtimes, DB/transaction layer, metrics collection and
query math, and scaling. Evaluated against important.md. Every finding verified in code.

## 1. Eviction & lifecycle correctness — fix first

- **Eviction/cancel report failure after the terminal transition committed → disbalance cascade.**
  `scheduler/service_cancel.go:80-103`: `EvictExperiment`/`CancelExperiment` commit
  `TransitionTerminal`, then return an error if `settle()` fails. The disbalance evictor
  (`loop_disbalance.go:218-234`) reads that as "eviction failed" and skips `pendingEvictions`, so
  the next tick condemns a fresh live victim for the same shortage — triggered exactly when the
  metrics store is flaky. Fix: after a committed transition, settle best-effort (the settlement
  reconciler retries; controller's `settleAndMark` already does this) and always record success.

- **Cancel can 200 while the job keeps running.** `service_cancel.go:27-45`: a lost CAS is assumed
  "concurrently cancelled", but it also loses when the status *progressed* (QUEUED→SUBMITTED,
  SUBMITTED→RUNNING). Cancel racing admission → "cancelled" reply, job runs to completion. Fix:
  re-read and retry the transition; only already-terminal is a no-op.

- **`MarkQueued` unguarded → resurrects settled terminal jobs and double-counts.**
  `shared/db/experiments_store_lifecycle.go:56-64` (`WHERE id=$1`, no status predicate); called on
  workload-creation rollback (`loop_preempt.go:282`, `handler.go:348`). Race with cancel: the
  settled REJECTED row goes back to QUEUED and its cost counts as both live reservation
  (`AddDesiredQuotaUsage` counts QUEUED regardless of `quota_settled_at`) and settled observed —
  permanent double-count. Fix: `AND status IN ('SUBMITTED','ADMITTED')`, return rows-affected.

- **Healthy k8s jobs evicted as "workload gone" during routine drift-recreates.**
  Reconcile deletes a drifted-but-desired Job (`agentloop.go:170`); recreate is blocked while it
  terminates (`k8sexec/job_lifecycle.go:48-67`); the 3s status loop skips deleting Jobs
  (`job_lifecycle.go:130`) and pushes a complete snapshot without them; the control plane reads
  missing-from-fresh-snapshot as Gone and `checkSilence` evicts on a **single** Gone observation
  (`controller/checks.go:62-70`). One unlucky tick inside any recreate kills real work. Fix:
  report deleting/recreating jobs as `pending` (StatusLister hook exists), or require Gone across
  ≥2 consecutive snapshots.

- **A RUNNING job that never produces an observation — or whose cluster dies — is immortal.**
  `controller/checks.go:36-38,54-61`: `FirstObserved` not-observed → skip forever (stuck-pending
  only covers pre-RUNNING); `LatestJobPhase` not-found → skip with no bound. Reservation and
  accelerator held indefinitely. Fix: hard ceilings on both "can't tell" states with a distinct
  eviction reason.

- **Stuck-pending eviction can kill a healthy running pod on a telemetry gap.**
  `job_watcher_scan.go:75-79` + `job_watcher_lifecycle.go:83-100`: timeout keys on
  `status != RUNNING` and runs before the phase poll; `onRunning` refuses `MarkStarted` while
  `GetAdmittedAcceleratorType` errors/disagrees, unboundedly. Broken flavor telemetry >
  `stuckPendingTimeout` → running pod evicted `stuck_pending`. Fix: check the phase first; exempt
  phase-Running; surface persistent type-observation failure separately.

- **Stage cuts + quota-exhaustion sweeps miss ADMITTED jobs.** `stages_cut.go:108-154`,
  `reconcile.go:124-148`: RUNNING-only and QUEUED/SUBMITTED-only queries leave ADMITTED (workload
  created, reservation counted) uncovered — a cut/exhausted agent's job can still start. Fix:
  include ADMITTED (as `ReconcileClosedExperiments` already does).

- **`evictNeverStarted` is a second, non-atomic eviction path.** `job_watcher_lifecycle.go:46-61`:
  `TransitionStatus` + separate `UpdateEvictionReason` (failure only warned → reason lost). Use
  `TransitionTerminal` (one path, important.md #1/#6).

## 2. Money — quota, billing, settlement

- **Flavor-substitution state stranded after a failed claim → wrong hardware + permanent underbilling.**
  `loop_preempt.go:244-250`: tick 1 substitutes an acceptable flavor and `ReserveAdmittedFlavor`
  *persists* it; `ClaimSubmitted` then fails and the row stays QUEUED with the substitute. Tick 2
  picks the originally requested flavor; the guard `exp.AcceleratorType != exp.Job.AcceleratorType`
  is now false, so nothing writes the flavor back — the scheduler fit-checks flavor A while the
  row (which the cluster reconciles and billing rates) says flavor B, for the job's whole life.
  Fix: compare selected flavor against the **persisted** row flavor, not `Job.AcceleratorType`.

- **`Cost()` panics on retired flavors inside the scheduler loop.** `domain/accelerator.go:141-145`
  reached from `loop_preempt.go:245` and `quotaCanCoverFlavor` (`loop_tick.go:400`) with a flavor
  from an old Postgres row — one such row kills the scheduler process every tick. Fix: use
  `LookupCost` and treat !ok as an admission error.

- **Substitution re-prices the job's lifetime.** `submitJob` computes
  `newEstCost = newRate × remaining hours`, but settlement bills lifetime observed hours at the
  implied rate — the first stint, run on the old flavor, is retroactively re-priced. Fix: rate
  change effective from the substitution point, or forbid substitution once observed hours exist.

- **Preempted jobs' burned stint hours vanish from all consumption figures while QUEUED.**
  `loop_preempt.go:121-174` + `running_cost.go:88` + `AddDesiredQuotaUsage`: rescaled estimate
  covers only the remainder, nothing is settled — an agent whose 80-AccH job is preempted at hour
  60 shows ~20 reserved and can over-admit ~60 AccH until the job terminates days later.
  `checkQuotaExhaustion` and `stageProgress` undercount the same way. Fix: settle the consumed
  stint at requeue time (idempotent absolute `SetObserved`).

- **Preemption rescale is broken on bare metal (stint ≈ 0 always).**
  `observation_query.go:406-414`: `LastNotAlive` reads only `experiment_alive_heartbeat`, which
  only the k8s node-agent writes. On bare metal the grid is empty → `ObservedStintElapsedHours ≈ 0`
  → every preemption requeues with the **full original estimate**: repeated preemption
  over-reserves indefinitely (important.md #18 inverted). Fix below (§7 heartbeat) or fall back to
  `experiment_metric_value` when the heartbeat series is absent.

- **Donation headroom checked against a pre-lock snapshot (TOCTOU → donor over-commitment, then
  irreversible exhaustion evictions).** `quota/platform_experiments_donation.go:57-67,190-212` +
  `platform_experiments_store.go:451-519`: usage is read before `FulfillDonationTx` takes the
  advisory lock; a concurrent admission makes the in-tx check pass on stale usage.
  `AdmitExperimentTx` already solves this with an in-tx `observe` callback — do the same.

- **Donation cancel is an unconditional overwrite.** `donation_store.go` `UpdateDonationStatus` +
  `huma_api.go:530-545`: a fulfilled donation can be flipped to "cancelled" (transfer not
  reverted, record denies it); nonexistent ID cancels with 200. Fix:
  `WHERE id=$1 AND status='open'`, error on 0 rows.

- **Lock-ordering deadlock between opposite-direction donations.** `FulfillDonationTx` takes only
  the donor's advisory lock, then row-locks donor→recipient; A→B ∥ B→A deadlocks (surfaces as
  500). Fix: sorted advisory locks or `WHERE agent_id IN (...) ORDER BY agent_id FOR UPDATE`.

- **Stage-boundary redistribution computed from an unlocked snapshot.** `stages_cut.go:18-105`
  derives zeros/adds outside the `AdvanceStage` tx; a donation committing in between mints or
  destroys hours relative to the platform budget. Fix: re-read allocation rows `FOR UPDATE`
  inside the tx (usage pre-read is safe — it only moves up).

- **Signup/auto-start race + skip-on-error mint permanently zero-quota agents.**
  `platform_experiments_lifecycle.go:551-677`: Start reads signups, allocates, then flips status
  (non-CAS) — a signup landing in between is durably signed up with no `agent_quotas` row and no
  repair path. A transient `GetAgent` error at `:608-611` is "skipped", permanently disinheriting
  the agent; `hasTop3, _ :=` at `:613` swallows a bonus. Fix: CAS the status flip first
  (open→running blocks late signups), fail Start on infrastructure errors.

- **Stage progress priced with a different formula than billing.** `controller/observed.go:57-71`
  uses live-catalog `LookupCost` while running-cost/settlement use `RatedCost`; a mid-run rate
  change (or retired flavor → silently dropped job cost, `stages.go:110-115`) makes the boundary
  disagree with the budget agents are billed by. Fix: use the injected quota service's
  `RatedCost` path.

- **Re-settlement after samples age out zeroes a real bill.** `settlement.go:56-88`: with the
  window empty (retention < job age), `neverStarted=true` overwrites every dimension to 0 —
  refunding a job that ran. Fix: never downgrade a prior non-zero settlement to never-started.

- **Lookback horizons disagree**: quota/settlement use `now−CreatedAt` (unbounded), controller
  caps at 14d, scheduler uses its own constant — same job, different answers past 14 days. Unify.

## 3. Runtime — k8s

- **One malformed object blacks out the whole cluster (#19).** `job_lifecycle.go:135` (+ Services
  `:201`, DRA `:355`, and `agentloop.go:130-158`): any managed-labeled object missing the
  experiment-id label errors the entire list, aborting every reconcile **and** every status push
  until a human intervenes. Fix: skip-and-log the bad object, finish the pass.

- **One foreign/stale report 409s the whole status snapshot.** `clusteragentapi/handler.go:213-218`
  rejects the entire push if any report's experiment is unknown or repointed to another cluster —
  which is the normal state during a cross-cluster reschedule; the old cluster gets a status
  blackout until its Job finishes terminating. Also an N+1 `GetExperiment` per report per 3s push.
  Fix: validate per report, drop-and-log foreign ones, batch the lookup.

- **Capacity misreporting.** (a) `job_lifecycle.go:274-329`: totals count only schedulable+Ready
  nodes but requests are subtracted for pods on **all** nodes — cordoning one busy node collapses
  reported availability. (b) DRA path (`job_dra.go:58-137`) ignores node readiness entirely: a
  powered-off node's chips stay "free" forever (#12). Fix: filter both by the schedulable set.

- **Spec hash embeds live-resolved placement → inventory changes delete running jobs.**
  `job_build.go:346-354` hashes affinity/extended-resource/DRA output of `resolvePlacementFor`,
  which reads live DeviceClasses/labels; renaming a DeviceClass or a driver upgrade re-hashes and
  drift-deletes every running job of that type mid-training. Fix: hash only control-plane desired
  state, or pin resolved placement at first create.

- **`Active > 0` reported as running though Active counts Pending pods** (`job_lifecycle.go:499`);
  with the sandbox-cgroup heartbeat this makes an ImagePullBackOff job look running+alive,
  mis-routing it away from the self-healing pending path. Use `Status.Ready`/pod phase.

- **Node-agent drops payloads the server's 503 contract assumes it retries.**
  `node-agent/main.go:112-135` ignores `push()` failure; any metrics outage > gapCap punches a
  permanent billing hole and can cause false silence evictions. It also parses cpu.stat
  `usage_usec` and discards it, and never pushes from a pod-less node ("agent dead" and "no pods"
  indistinguishable). Fix: bounded retry ring with original timestamps; push unconditionally.

## 4. Runtime — bare-metal

- **NVIDIA GPUs never marked in-use → two jobs on the same GPU.** `container.go:113-129` assigns
  NVIDIA via `DeviceRequests`, but occupancy (`lifecycle.go:236-264`) reads only
  `HostConfig.Devices` — empty for NVIDIA, so every GPU always looks free and placement
  double-books device 0 while others idle. Fix: read `DeviceRequests[].DeviceIDs` too (use UUIDs).

- **Placement re-resolution moves a running job → drift-delete → falsely reported terminal.**
  `resolvePlacementFor` (`lifecycle.go:270-306`) picks first-free in probe order without
  preferring the job's current device; a neighbor finishing re-resolves a healthy job onto another
  chip, drift triggers delete, and `PollJobPhaseAndUID` (`lifecycle.go:188-196`) maps the exited
  container to Succeeded/Failed — permanently terminalizing it (k8s reports Gone; #13 divergence).
  Fix: reuse the live container's devices; treat exited-with-drift-pending as Pending.

- **One mislabeled container / one sick accelerator bricks the node (#19/#12).**
  `lifecycle.go:96-134` hard-errors the whole list on one label-less managed container (blocks
  reconcile *and* status forever); `reapTerminal` (`:251-268`) stops at the first unremovable
  container; `probeNVIDIA`/`probeTenstorrent` (`nvidia.go:33-40`, `tenstorrent.go:66-69`) fail the
  whole probe on one lost GPU or unknown PCI ID — and since capacity is probed before desired
  state (`agentloop.go:258-260`), no deletes happen either. Fix: skip-and-log per item.

- **Missing image = invisible wedge.** No `ImagePull` anywhere in `runtime/bare-metal/`; create
  fails every 2s logged locally only; no container → no phase report → `FirstObserved` never fires
  and the `ImagePullFailed` classification is unreachable. Fix: pull on create, or synthesize a
  Pending report for desired-but-uncreated jobs (matches k8s semantics).

- **Create-succeeds/start-fails wedge reported as Running.** A `created`-state container hash-matches
  ("already running", `lifecycle.go:28-34`) so start is never retried, and `created` maps to
  JobPhaseRunning (`:186-187`) — billed as running until silence eviction. Fix: retry start on
  `created`; map `created`→Pending.

- **NVML index assumed equal to `/dev/nvidiaN` minor** (`nvidia.go:42`) — diverges after driver
  reloads/hot-plug; occupancy then books against the wrong device. Use `GetMinorNumber()`.

- **Unbounded container logs on disk** (no `LogConfig`, `container.go:142-159`) and real disk usage
  not reflected in storage capacity (`capacity.go:98-112` counts requests against statfs total).
  Set log rotation; report `min(total−requests, statfs available)`.

- 64KB `bufio.Scanner` limit silently truncates long log lines (`container_logs.go:75-78`,
  `scanner.Err()` unchecked). `run_task.py:113` reads `HYPOTHESISLOOP_JOB_ID` but runtimes set
  `HYPOTHESISLOOP_EXPERIMENT_ID` → every stored record says `job_id: "local"`. Onboarding skill's
  wrapper exports `CONTROLPLANE_URL` but `bare-agent` requires `API_URL` — fails verbatim.

## 5. DB layer & transactions

- **Metrics-store network calls held inside the admission advisory lock.**
  `experiments_store_lifecycle.go:88` (`capacityAvailable`) and `combined_store.go:104`
  (`observe`) run GreptimeDB HTTP inside an open tx holding the cross-replica lock; a hung call
  stalls all admission for the cluster and can exhaust the 20-conn pool. Bound with a tight ctx
  deadline.

- **Missing indexes for the hottest queries.** No index on `experiments(cluster_name, status)` —
  `ListDesiredWorkloads` (polled continuously by every cluster-agent) and `ClaimSubmitted`'s
  in-lock read seq-scan as the table grows, inside the admission lock. Add it (partial on the
  three desired statuses); also `(agent_id, platform_experiment_id, status)` and
  `donation_requests(platform_experiment_id)`.

- **No CHECK constraints backing code assumptions.** `agent_quotas` allocations can go negative
  (stale-snapshot donation, negative `AdvanceStage` delta) → confusing "have −3.2 remaining"
  rejections; `donation_requests.status` is free text (typo = donation invisible forever); no
  `ends_at > starts_at`/budget ≥ 0 on platform_experiments. Add cheap CHECKs (fail fast).

- **Dead-but-dangerous mutators.** `UpdateExperiment`/`UpdateExperimentStatus`
  (`experiments_store.go:248-286`): zero callers, no CAS, would violate the queue-reason CHECK;
  still demanded by interfaces (`scheduler/service_types.go:27`, `controller/controller.go:21`).
  Delete both. Also vestigial: `credit_ledger` (`AppendLedgerEntry` has no non-test caller, never
  writes `platform_experiment_id`), `agents.top3_count` (never read/maintained — duplicated state,
  #3), schema enum `DRAFT`/`PROMOTED`.

- `GetDonationRequest` matches no-rows by error-string comparison instead of
  `errors.Is(err, pgx.ErrNoRows)` (`donation_store.go`).

## 6. Scheduler core — additional

- **`pendingEvictions` double-credits once the capacity report catches up.**
  `loop_disbalance.go:75-96`: pruned only by TTL, though the doc claims "or a capacity read
  catches up". Once the node reports the GPUs free (t≈15s) every tick until TTL (60s) credits them
  *again* — and `gAvail = min(desiredFree, actualFree)` already credits the EVICTED victim via the
  desired side. Phantom capacity suppresses preempt/disbalance for jobs that still need them and
  flaps admissions with `errAdmissionCapacityChanged`. Fix: drop the entry when the victim's node
  already reports the accelerators free.

- **Distinct-hosts shortage covered with cluster-scalar victims.** `preempt()`
  (`loop_preempt.go:75-98`) checks `Fits(freed, needed)` cluster-wide while the shortage from
  `preemptionShortfall` is per-node — it can evict a burst job on a node that already has free
  devices, "cover" the scalar, stand the disbalance pass down, and still not place the job; next
  tick evicts a fresh victim. Fix: attribute victims to nodes (machinery exists in
  loop_disbalance.go) and cover the per-node deficit, as `selectDisbalanceVictims` does.

- **Preemption aborts entirely on one job's failed metrics query** (`loop_preempt.go:38-45`) — one
  broken series blocks all guaranteed-tier preemption (#19). Skip/deprioritize the unrankable
  candidate. Related: a metrics *ingestion* delay ranks a 9-hour job as zero-progress and preempts
  it first — treat observed=false on a long-RUNNING job as suspect.

- **Flavor-revert path overrides pin and skips topology.** `loop_tick.go:204-208,313-317`:
  `quotaCanCoverFlavor` is always false for `PlatformExperimentID == ""` (nil quota), and the
  revert re-picks via `clusterWithBestFit` alone — a spread job can be pointed at a cluster whose
  layout can't hold it, then charged a preempt/evict cycle there. Route through the
  topology-aware candidate check restricted to the requested flavor.

- **`sortGuaranteed` comparator is not a strict weak order with mixed nil/non-nil `QueuedAt`**
  (`loop_sort.go:82-95` — cycle possible, order jitters tick to tick). Live paths always set
  `queued_at`, so impact is latent; still, order nil deterministically as newest.

- Burst jobs displaced by this tick's guaranteed pass mislabeled `capacity_unavailable` instead of
  `outranked` (`loop_tick.go:303` snapshots after the guaranteed pass). `runningLoaded = false`
  after every preempt call (`loop_tick.go:238`) forces a full re-read per blocked job even when
  preempt did nothing. `interleaveByAgent` is O(agents × max-queue) (`loop_sort.go:163-184`) —
  drop exhausted agents from the rotation.

## 7. Metrics pipeline & query layer

- **Bare-metal jobs write no alive heartbeat** — `ObservedElapsedHours`/`IsAlive`/`LastNotAlive`
  are all built on `experiment_alive_heartbeat`, written only by the k8s node-agent
  (`shared/metrics/handler.go:59`). Bare-metal jobs are billed in gapCap slivers around metric
  reports, and stint elapsed ≈ 0 breaks preemption rescale (§2). Fix: bare-metal status push
  writes `RecordObservations`, or the alive union treats `cluster_job_phase=running` /
  `experiment_metric_value` as liveness sources.

- **`LatestExperimentNode` returns the wrong node after a reschedule.**
  `experiment_node.go:621-644`: `step = maxLookback` puts every node series' last point at the
  same grid timestamp; strict `After` picks an arbitrary series — disbalance eviction can condemn
  the wrong job. (Also the first grid point reaches back 2× the lookback.) Fix: compare actual
  last-sample times (`max by (node)(timestamp(...))` or SQL `ORDER BY ts DESC LIMIT 1`).

- **Push handler accepts arbitrary/zero timestamps into the billing store**
  (`shared/metrics/handler.go`): a missing timestamp lands at year 1; one skewed clock corrupts
  elapsed/billing for every pod on the node. Reject outside a sane band around receipt time.

- **`metric_name` is unvalidated while `metric_basis` is carefully bounded**
  (`registry/metrics.go:33-55`): a job embedding a step counter in the name creates a series per
  sample, degrading the exact ranking/silence queries the basis check protects. Apply the same
  ≤64-char printable-ASCII bound.

- **Grid alignment breaks "same answer for every asker" by ±1 step** — the range-query grid is
  phased to each caller's `now` (`metricsdb.go:251`), so live accounting and settlement can
  legitimately disagree by one 30s step. Truncate `since`/`now` to an epoch-aligned grid.

- **`job_logs`/`job_phase_detail` grow without bound** (writes append per (pk, ts); no TTL) and
  `GetLatestPhaseDetailBatch` has no LIMIT — list endpoints slow linearly with PE age. Add TTLs +
  per-id latest-row query; move `CREATE TABLE IF NOT EXISTS` out of the per-write hot path.

- **`registry.GetTimeseries`: fixed 5s step from `CreatedAt`** — ~242k points × 2 series for a
  14-day job on a UI endpoint; use the capped `lookback/500` step its sibling already uses.
  Unknown experiment IDs on metrics/lineage endpoints surface as 500 not 404
  (`registry/handler.go:52-80`; logs.go got it right). `RecordMetric`'s terminal check is TOCTOU
  (`metrics.go:59-87`) — a post-terminal sample can perturb boundary rankings.

- `elapsedHours` overbills one step per alive-gap (`observation_query.go:691-722` — count
  intervals per stint).

## 8. Scaling — the platform-wide cliff

Per-job metrics range queries in hot loops. One `ObservedElapsedHours` = 4 PromQL range queries
over **14 days at 30s step ≈ 40,320 points each** (stock Prometheus rejects >11k); stint queries
add ~7 more. Multiplied by:
- every queued job per 10s tick (`completionFractions`), every preemption candidate + victim,
  one `LatestExperimentNode` per running job per triggering cluster;
- controller reconcile: the same job's grid scanned 3+× per tick (stageProgress, job-length cap,
  `checkQuotaExhaustion`→`addRunningActualCosts`), `checkSilence` up to 5 more, declared-metric
  checks per metric key; `checkQuotaExhaustion` per experiment instead of per (agent, PE);
- `running_cost.go:68` and `settlement.go:71` lookbacks are `now−CreatedAt` — **unbounded**;
- `RePrioritize` (`service_priority.go:14-50`): ~5 queries per queued job per minute incl. a
  metrics HTTP round-trip, plus O(queued × active) novelty.

k8s agent side: ~8 unpaginated cluster-wide LISTs every 2s (each capacity collector does its own
node+pod list) and per job per 3s a Job Get + three pod Lists + log streams — thousands of
apiserver requests/min at a few hundred jobs.

Fixes in leverage order: (1) one batched alive-grid range query
`max by (experiment_id)(last_over_time(...))` per tick; (2) bounded lookbacks (first-sample
watermark / capped CreatedAt, coarser step for long windows); (3) compute observed-elapsed once
per tick and thread it through; quota per (agent, PE); (4) one shared node/pod/DRA snapshot per
agent pass; (5) `RePrioritize`: batch quotas, hypothesis→count map.

## 9. Stage ranking, wiring, config

- **A stale ranking metric silently vetoes every cut.** `stages_rank.go:118-141` + `:52-81`:
  `hadData` counts samples from *any* agent, including ones cut in earlier stages, but a metric on
  which every current survivor lacks data makes all survivors tie → k shrinks to 0 → and since a
  job is cut only when below the line on **every** healthy metric, that one metric vetoes the
  whole boundary — stages advance, nobody is ever cut, silently. Fix: compute `hadData` from
  survivors only. (Note: there is deliberately no cut reversal, raising the stakes.)

- **Settlement reconciler runs N+1 times** (every control-service replica outside the
  leader-election gate, `cmd/control-service/main.go:203`, plus metrics-service `main.go:119`);
  concurrent Settles race last-writer-wins on the "final" figure. `StartExpirySweep` likewise per
  replica. Run inside the leader callback; one owner.

- **control-service has no graceful shutdown for background loops** (`main.go:99-107`): SIGTERM
  drains HTTP only; scheduler loop/JobWatcher/reconcilers run on `context.Background()` and die
  mid-admission-pass racing `pool.Close()`. One cancellable root ctx, cancel before Shutdown,
  wait for loop exit.

- **Config validation gaps** (`shared/config/load.go:40-83`): `Top3BonusFraction` unvalidated
  (negative silently shrinks a winner's quota); `MaxSubmissionsPerHour`/`Max*PerJob` accept
  negatives which read as "unlimited"; port range unchecked. Reject negatives.

- `stages.go:68` indexes `Stages[CurrentStage-1]` unguarded — one bad row (current_stage=0) panics
  the reconcile loop every tick; guard like `CurrentMaxJobHours` does.

- Reconcile aborts everything when `ListPlatformExperiments` fails (`reconcile.go:37-39`) though
  silence/exhaustion checks don't need the stage maps — continue with empty maps.

- Novelty scoring counts the job itself once persisted (`service_submit.go:186-194`,
  `service_priority.go:31`) — fresh submissions systematically outrank identical older ones;
  exclude `exp.ID`. Submission rate-limit + duplicate checks are TOCTOU outside
  `AdmitExperimentTx` (`service_submit.go:94-106,140-149`) — fold into the tx, map PK conflict
  to 409.

- Minor: `stopCutAgentJobs` ignores its `runningExps` param and triggers the scheduler for every
  ever-cut agent every tick; `checkQuotaExhaustion`/`evict` take an unused `now`;
  `splitLongLines` infinite-loops if `MaxLogLineChars ≤ 0` (guard it); `WaitForJobDeletion` is
  dead interface surface; in-RAM `pendingEvictions` lost on restart (bounded by TTL — accept or
  derive from a persisted eviction timestamp).

## 10. What checked out cleanly

- Terminal transitions are CAS-guarded (`TransitionTerminal`, `TransitionStatus`, `MarkStarted`,
  `RequeuePreempted`) — no double-transition/double-settle races (MarkQueued is the one gap, §1).
- `AdmitExperimentTx` is the model transaction: advisory lock, in-tx observed callback, `FOR
  UPDATE`, in-tx reservation sum; #18's reserved-vs-observed split is honored on both sides.
- Settlement is an idempotent absolute set with the `quota_settled_at` outbox + partial index +
  reconciler — crash-safe across the two stores; never-ran jobs settle to zero (full refund).
- Preemption rescale keeps the $/hour rate invariant (non-substitution case); AccH total-vs-per-rank
  accounting is consistent (`AcceleratorCount` total, `Job.AcceleratorCount` per rank).
- Eviction propagates purely by desired-state pull; no push/kill path anywhere (#5/#6); the
  runtimes hold no in-RAM job state and survive restarts (#4/#12).
- Ranking math (direction, ties, floor, min-survivors, whole-tie-group protection), `AdvanceStage`
  exactly-once claim, cut-agent refund vs observed usage (#18) — correct.
- No SQL/PromQL injection: identifiers via closed allowlists, values escaped; #20 (store-only
  access) holds except the documented leader-election `pool.Raw()`.
- All-or-nothing status snapshots, k8s Gone-eviction freshness gating, deterministic spec hashing
  mechanism (content problem aside), case-folded flavor handling end-to-end.
