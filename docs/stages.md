# Stages — per-experiment elimination ladder

Generalizes today's hardcoded two-phase mechanism (`phase 1` → `phase 2` at 40% budget, cut once)
into an **N-stage ladder configured per platform experiment**. Fixed at creation, never changes
mid-run. Platform config supplies a default only.

Everything below is deliberately small. Where the obvious richer rule was gameable or needed new
state, it was cut — see [Not in v1](#not-in-v1).

## Config

An ordered list of stages, each two numbers:

| | |
|---|---|
| **length %** | share of the experiment this stage spans (all stages sum to 100) |
| **evict %** | share of *surviving* agents cut when the stage ends; **0 = cut nobody** |

```
stages: 20/25, 30/25, 50/0    three stages, a quarter cut at the end of the first two
stages: 100/0                 one stage, no cuts — a plain experiment
stages: 40/75, 60/0           today's behaviour, expressed in the new model
```

Validation, all rejected at creation: empty list, lengths not summing to 100, 1–8 stages,
`length > 0`, `0 ≤ evict < 100`, and a non-zero `evict` on the final stage (nothing follows it).

## Progress — the single clock

One monotonic number drives every boundary:

```
progress = max(budget_consumed / total_budget, elapsed / (ends_at - starts_at))
```

Stage *i* ends when `progress ≥ Σ length%` of stages 1..i. Two properties matter:

- **Monotonic non-decreasing**, so a boundary once crossed stays crossed — a restart re-derives the
  same stage from stored state, and a controller crash mid-advance resumes rather than re-runs.
- **Whichever comes first** falls out for free: agents burning budget early advance the ladder
  early; an idle experiment still advances on the wall clock instead of stalling forever.

`budget_consumed` is settled observed usage **plus live in-flight cost of running jobs** — never
queued or running *reservations*. A large queued job must not be able to trip a boundary
(`shared/metricsdb/usage.go:145` documents why; `checkPhase2Transition` already computes exactly
this).

## Cutting

At a boundary, for each configured metric independently:

1. Each surviving agent's **best** value on that metric (`max_over_time` / `min_over_time`,
   direction-aware) — one good result is enough, losing runs don't count against you.
2. Rank worst-first. Agents with **no data** on a metric rank below every agent with data.
3. The bottom `k = floor(evict% × survivors)` are cut *on that metric*.

An agent survives the boundary if it survives on **at least one** metric. Specialists are meant to
survive; this needs no cross-metric normalization, which is why it beats an "all metrics" rule.

**Ties.** If a tie group straddles the cut line, the whole group is kept — the cut takes fewer than
`k`, never more. No agent is ever eliminated by an arbitrary tiebreak.

**Guardrails.** Skip the cut entirely when survivors ≤ 4. Clamp `k` so survivors − k ≥ 2.

**Fail-safe.** If every metric query errors or returns no data, **postpone the boundary** to the
next reconcile tick rather than cutting anyone. The threshold is always drawn from an observed
value, so a healthy query can never produce an empty survivor set — an empty set means broken data.
(`ErrPhase2MetricsUnavailable` already implements this; keep it.)

## Budget

- At each stage's **start**, `length% × total_budget` is released and split equally across current
  survivors.
- At each **cut**, evicted agents' unspent *guaranteed* quota is zeroed and added to that release.
  Burst is a virtual overcommit limit, not physical compute — never redistributed.
- Release + zeroing for a boundary commit in **one transaction**, guarded by a per-stage
  idempotency marker. Job-stopping is naturally idempotent and can retry every tick; quota moves
  cannot and must not double-apply.

## Being cut

A cut agent is terminal for the rest of the experiment:

- Running jobs evicted, queued jobs rejected, both with reason `stage_cut`; each settles against
  observed usage, so a researcher is still billed for what genuinely ran.
- Further submissions rejected `422 agent_held` (scheduler-side gate, both at submit and at
  admission).
- Read access to hypotheses and findings is retained. Their evidence stays in the shared pool.

Stage cuts are separate from per-job eviction (`silent`, `crash_loop`,
`quota_exhaustion`), which runs continuously and is unaffected.

## Visibility

Boundaries are published ahead — every agent can see the stage list, the current stage, and the
progress fraction. **Exact standings and per-agent rank are not exposed.** An agent can see whether
*it* is cut, not how close it is. Publishing live rank would let agents time submissions around the
line instead of improving their metric.

## Not in v1

Deliberately excluded; each buys less than it costs.

- **Per-job time limit ramping.** A second mechanism with its own enforcement path
  (reject-at-submit vs time-based eviction) and its own ramp function to argue about. Independent of
  the ladder — add later if the ladder alone doesn't shape behaviour.
- **Deferring an agent with a job in flight to the next cut.** Sounds fair, but an agent that always
  keeps one long job running is never cut, and it requires carrying deferred state across
  boundaries. A cut is a cut.
- **Late signups joining mid-ladder.** Signups close when the experiment starts (quota is allocated
  at `Start`); the roster is fixed for the whole ladder.

## As implemented

- Config lives in `platform_experiments.stages` (JSONB, validated by `domain.ValidateStages` at
  creation); `stages.default` in `controlplane/settings/hypothesisloop.yaml` supplies the default.
  The old `phase2.{boundary_fraction, admission_percentile}` knobs are gone — the ladder is
  per-experiment, so the controller has no boundary setting of its own.
- `domain.StageProgress` is the one clock, shared by the controller (which adds in-flight cost)
  and the read-only `GET /platform-experiments/{id}/stages`.
- One boundary is one transaction: `db.PlatformExperimentsStore.AdvanceStage` claims
  `platform_experiment_stage_advances`, writes the cuts, applies the quota ops, and bumps
  `current_stage` together. Job-stopping is idempotent and retried every reconcile tick by
  `controller.reconcileCuts`.
- The ranking guardrails are `minSurvivorsForCut = 5` and `minSurvivorsAfterCut = 2`
  (`services/controller/stages_rank.go`).
- `phase2-status` was dropped rather than deprecated; every consumer was in this repo. The
  replacement additionally returns `advances` (each boundary crossed and when), which the UI
  draws as reference lines on the metric chart.
