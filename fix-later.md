# fix-later.md — smri-fm-fomo-tune-task5-pmg-round2 (pe-e11aa080)

## [RESOLVED, commit 998950a] pod-level `stuck_pending` eviction — root cause found

**Root cause**: `PollPhaseDetail` (runtime/k8s/internal/k8sexec/phase_detail.go) lists PODS
filtered by `experiment-id=X,attempt=N` to fence `scheduledNodes` to the current attempt
generation. The `attempt` label was only ever set on the parent Job's own `ObjectMeta`
(job_build.go), never on the pod template itself — confirmed live via `kubectl get pod ... -o
jsonpath='{.metadata.labels}'`, which showed no `hypothesisloop.io/attempt` key at all on a
genuinely running, progressing pod. That selector therefore matched zero pods every poll, for
every job, so `scheduledNodes` stayed permanently 0 regardless of true pod health, and
job_watcher's scale-up-deadline check evicted the job as `stuck_pending` once
`stuck_pending_timeout_seconds` (300s) passed — no matter how much real progress it was making
(job logs showed genuine subject-by-subject embedding progress right up to eviction each time).

**Live impact**: `smri11-heterofrofastacked-local` (v1, ttashift-heldout, v2, v4, v5) — 5
consecutive evictions in a row, all agent-11.

**Fix**: add the same `Attempt` label to the pod template's own `ObjectMeta.Labels`, not just the
Job's. Rebuilt/deployed as `cluster-agent:b2be3ac-drafix3`. Verified live: new pod carried
`hypothesisloop.io/attempt=2`, ran past the 5-minute deadline (18:58:14Z→19:04:06Z, ~6min) with
real progress (`fraction_complete=0.79`) and was NOT evicted — first successful run past this
deadline all session. Added `TestBuildJobPodTemplateCarriesTheAttemptLabel`.

## [historical — superseded by the fix above] pod-level eviction after admission: `stuck_pending` / `unschedulable`

**Fact**: as of 18:28Z, 4 jobs EVICTED with attempt_count=3 `eviction_reason: stuck_pending`
(`smri11-heterofrofastacked-local-v1`, `-v2`) or attempt_count=0 `eviction_reason: unschedulable`
(`smri10-block17frofa-local-v1`, `smri10-block17frofa-fcd-v1`) — distinct from the scheduler
admission-tick bug fixed twice below: these jobs got past `capacity_unavailable` admission (or
never needed to retry it) and were evicted at the k8s pod-scheduling stage instead. One denial log
line for `-v2` at 18:20:50Z (10 min after the drafix2 rollout) shows the same `cluster_unresolved`
signature recurring once more before the eviction, so admission-side flakiness may not be fully
gone either — inconclusive from one line.

**Why it matters**: `tt-operator-tt-dra-driver-kubeletplugin-z7r2k` shows 4 restarts, but the one
`lastState.terminated` k8s still retains is timestamped `2026-08-27T09:49:15-19Z` — an initial
cold-start race against the fabric-manager agent ("GetTopology: context canceled while waiting for
connections to become ready"), 11 minutes *before* the run even started at 10:00Z, self-healed
within 4s. Current plugin logs (18:23-18:29Z) show completely normal `PrepareResourceClaims`/
`Preparing claim` cycles with no errors — it is NOT actively crash-looping now. This weakens (does
not rule out) the kubelet-plugin-instability theory: `smri11-heterofrofastacked-local-v2`'s own
`stuck_pending` eviction fired at 18:26:04Z, ~16 min *after* it was confirmed RUNNING post-drafix2
(so it was admitted, started, then failed at the pod/kubelet level later — genuinely post-admission,
not a rehash of the two capacity-snapshot bugs above). Still flagging for a dedicated deep-dive
with kubelet-plugin logs and pod events at the exact 18:26:04Z eviction moment, not further
black-box API polling under a single poll-pass budget.

**Current impact**: low — agent-11 self-recovered via its own retry-resubmit loop
(`smri11-heterofrofastacked-local-v4` now RUNNING); agent-10 lost 2 jobs to `unschedulable` with
0 retry attempts (worth checking whether `unschedulable` should retry like `stuck_pending` does).
Standings unaffected: agent-10 rank 1 (0.9097), agent-11 rank 2 (0.9010), both far above 0.795
baseline.

## [RESOLVED, commit 7fb8d68] cluster_unresolved denial recurred 2026-08-27T18:00Z+ — symmetric gap in the earlier DRA fix

**Root cause found**: the earlier fix (6590dd1) only guarded "DeviceClass exists, its
ResourceSlices listing came back empty." It left the mirror case open: if the *DeviceClasses*
list call itself transiently returns empty (or omits an installed driver), `drivers` never
contains that domain at all, so the per-driver ResourceSlice loop iterates zero times for it and
`sawSliceForDriver`'s check is vacuously satisfied — same silent-zero-capacity bug, one level up,
completely unguarded by the first fix. This matches every "ruled out" item logged below: no error
from the fixed path (because a *different* path was empty), raw GreptimeDB data correct (the
producer never even tried to write real data, it silently wrote zero), a cluster-agent restart not
clearing it (transient per-listing-cycle, unrelated to process staleness).

**Fix**: cross-check the driver set actually publishing `ResourceSlice`s (an independent listing
call, already fetched) against the `DeviceClass`-derived driver set — any driver with real slices
but no matching DeviceClass this observation is an incomplete snapshot, now errors instead of
publishing false zero capacity. Added `TestLiveDRACapacitySnapshotsRejectsMissingDeviceClass`.
Rebuilt/deployed as `cluster-agent:b2be3ac-drafix2`; verified live: `smri11-heterofrofastacked-local-v2`
(stuck since ~17:59Z) admitted and RUNNING within the same poll cycle as the rollout, no disruption
to the concurrently running pod.

## [historical — superseded by the fix above] cluster_unresolved denial recurred 2026-08-27T18:00Z+

**Symptom**: `smri11-heterofrofastacked-local-v2` QUEUED continuously from ~17:59:40Z with
`not_admitted_reason: capacity_unavailable`, `cluster="" cluster_unresolved=true avail="{}"` every
tick, identical signature to the incident fixed below — but `resource-catalog/capacity` showed 3/4
blackhole free the whole time, same as before.

**Ruled out** (with direct evidence, not assumption):
- The already-fixed DRA-listing-gap path (`runtime/k8s/internal/k8sexec/job_dra.go`,
  `liveDRACapacitySnapshots`): no error logged in `kubectl -n hypothesisloop logs
  deploy/hypothesisloop-cluster-agent`; running image confirmed still `b2be3ac-drafix1`, 0
  restarts. If this were the same bug, the fixed code would have logged an explicit error instead
  of silently denying.
- Stale/raced GreptimeDB snapshot: queried `cluster_agent_heartbeat` and
  `cluster_accelerator_total_accelerators` directly via SQL at the exact tick timestamp
  (1787853790024ms) the scheduler logged the denial for — both tables agree, both include
  `tenstorrent.com/chipArch=blackhole`, at the identical timestamp. The raw data was correct at
  the instant of the bad decision.
- Node-level resource exhaustion (the other path that can set `cluster=""`, via
  `resolveClusterLocalResources` in `loop_tick.go`, independent of the DRA/accelerator-map path):
  `kubectl describe node tt-quietbox` shows only 21% memory, 25% hugepages-1Gi, 25% CPU allocated
  — ample free capacity on every dimension the job's footprint needs.
- Stale `exp.ClusterName` shortcut: the job's own API record carries no `cluster_name`, so the
  scheduler takes the normal `resolveClusterAndFootprint` path each tick, not a cached one.
- `kubectl -n hypothesisloop rollout restart deploy/hypothesisloop-cluster-agent` did NOT clear it
  (waited 15s + confirmed still denied post-restart) — rules out any client-side staleness in the
  reporter process itself.

**Not yet root-caused.** Whatever is dropping the accelerator dimension from `GetFlavorCapacity`'s
derived burst footprint for this cluster on some ticks is happening between the raw GreptimeDB data
(confirmed correct) and the Go-side `queuebackend.Backend.GetFlavorCapacity`/`minimumFootprint`
computation, or in `runClusterSnapshotQuery`'s HTTP response parsing — needs actual Go-level tracing
(e.g. a temporary raw-response dump in `runClusterSnapshotQuery`, or a unit test reproducing
`minimumFootprint`/`Sub` against a captured real snapshot) to pin down, not further black-box
querying. Flagging for the next poll pass / a dedicated deep-dive (codex or Opus, per this run's
earlier precedent) rather than guessing further under a single poll-pass budget.

**Current live impact**: `smri11-heterofrofastacked-local-v2` stuck QUEUED as of this entry.

## [RESOLVED] Burst jobs falsely denied capacity_unavailable despite free chips

**Symptom**: agent-10's job `smri10-ttaflip-stacked-v1` submitted 2026-08-27T17:21:04Z, stuck
QUEUED for 20+ min with `not_admitted_reason: capacity_unavailable: short {accelerator:...
blackhole=1,...}` while `resource-catalog/capacity` reported 3/4 blackhole chips free the whole
time. Diagnostic log added earlier this run (`loop_tick.go`'s "burst job not admitted this tick")
showed `cluster="" cluster_unresolved=true avail="{}"` every tick — the scheduler could not resolve
any target cluster at all for the job, so it treated free capacity as zero regardless of the true
count.

**Root cause**: `runtime/k8s/internal/k8sexec/job_dra.go`'s `liveDRACapacitySnapshots` built its
per-flavor accelerator capacity purely from whatever `ResourceSlice`s one `List` call happened to
return. When a transient k8s API listing gap dropped a configured driver's slices for a single
observation, the resulting empty map was published as a normal, valid capacity snapshot —
indistinguishable from "this cluster genuinely has no such hardware." The scheduler's
`reportsEveryDimension`/`clusterWithBestFit` (`controlplane/services/scheduler/loop_tick.go`) then
correctly refused to resolve a cluster for the missing flavor, which is the right behavior *given*
that input — the bug was upstream, in the snapshot producer treating a listing gap the same as an
absent flavor.

**Fix** (commit 6590dd1): a configured `DeviceClass` driver that reports zero matching
`ResourceSlice`s in one observation is now treated as an incomplete snapshot — `liveDRACapacitySnapshots`
returns an error instead of an empty map, so the reconcile skips publishing a new heartbeat/capacity
snapshot for that cycle and the last known-good snapshot stays authoritative until it goes stale on
its own. Rebuilt/pushed `hypothesisloop-cluster-agent:b2be3ac-drafix1`, rolled out via
`kubectl -n hypothesisloop set image deploy/hypothesisloop-cluster-agent ...`. Verified live:
`smri10-ttaflip-stacked-v1` was REJECTED (timed out), its resubmit `smri10-ttaflip-stacked-v2`
admitted and RUNNING within ~15s of rollout; both agents now have running jobs simultaneously
(2/4 blackhole chips in use); no further `burst job not admitted this tick` log lines since.

Root-caused jointly with an independent `codex --dangerously-bypass-approvals-and-sandbox` review
pass, which reached the same producer-side diagnosis via its own read of the same files.

## [ACCEPTED — not fixable this run] Agents run one job at a time, serially

Agents' own per-turn bash loops (`until ... done`) block on their current job before formulating
the next one; this is normal single-agent cadence, not a scheduler bug, and was repeatedly
mis-flagged as "critical: 0/4 jobs running" during live spot-checks that in every case traced back
to a job legitimately in analysis/write-up between runs. A one-sentence nudge was added to
`experiment.md` CONSTRAINTS ("Ideally keep at least one job queued...") and synced to the live
platform-experiment description; posting it as agent comments, updating the description, and even
restarting both agent containers did not change behavior mid-run (a restart resumes the in-progress
job-wait loop, it does not force a step-0 re-read of the description). Real fix belongs in
`agents/experimentator/src/hypothesisloop_agent/prompts/system_prompt.md` for future rounds — e.g.
an explicit instruction to submit a second/backup job before blocking on the first one's result —
not retroactively fixable once a round's agent containers are already running.

## [FIXED] agent-11 container lost Claude Code auth (401 OAuth token revoked)

**Symptom**: agent-11 started failing every turn with `API Error: 401 OAuth access token has been
revoked` starting 2026-08-27T17:28:34Z, retrying with exponential backoff, making zero progress.

**Root cause**: agent-11's container (unlike agent-10) had no persistent `CLAUDE_CODE_OAUTH_TOKEN`
env var baked in — it relied solely on the bind-mounted host `~/.claude/.credentials.json`. That
host file was atomically replaced at 17:26 (this coordinator session's own token refresh); a
single-file bind mount tracks the original inode, so the replace orphaned agent-11's mount while
agent-10 was unaffected (its own baked-in token takes precedence over the mount per
agents/coordinator/setup.md's own note: "the SDK always prefers the token"). Likely dates back to
an earlier this-run container restart that didn't reapply the `-e CLAUDE_CODE_OAUTH_TOKEN=...` flag.

**Fix**: recreated agent-smri-fm-fomo-tune-11 (`podman rm` + `podman run`) with the same
`CLAUDE_CODE_OAUTH_TOKEN` from `agents/coordinator/.env` that agent-10 already carries, matching
setup.md's spawn command. Verified: container re-authenticated, re-registered, and resumed
reconciling normally within 45s of restart.

**For next round**: setup.md's spawn command should treat `CLAUDE_CODE_OAUTH_TOKEN` as required for
every agent, not optional-with-mount-fallback — the fallback is fragile against exactly this kind
of host-side token rotation happening mid-run.

## [NOTE — low impact, watching] cluster_unresolved denial blips still occur post-drafix3, self-clear within ~1-2 ticks

**Fact**: 19:54:39-20:02:50Z, several `burst job not admitted` lines with `cluster_unresolved=true
avail="{}"` for `smri10-directleak-check-v1`/`-plain-v1`, on cluster-agent:b2be3ac-drafix3 (both
prior DRA fixes, 6590dd1+7fb8d68, deployed). Both jobs are RUNNING moments later — no stuck_pending
eviction resulted (unlike pre-drafix1/2, which stuck for 5-25+ min). Not a new root cause hunt this
pass (single-tick self-clearing blips, zero live impact) — just recording the residual pattern in
case it starts sticking again.
