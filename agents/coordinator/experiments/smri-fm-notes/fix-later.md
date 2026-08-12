# fix-later.md

## Run closed 2026-08-12T09:24Z (~22.75h into the planned 24h window, closed early by user request)

Final state: `pe-d564bb90` closed via `POST .../close`, both agent containers stopped, node
detached (`lib_detach_node k3s-tt tt-quietbox`). Final counts: 45 jobs total (23 COMPLETED, 21
EVICTED, 1 REJECTED), 16 hypotheses registered (9 open, 5 inconclusive, 2 refuted, **0
confirmed**). No infra issues at close -- clean shutdown, no jobs left orphaned RUNNING.
See the root-cause entry immediately below for why the hypothesis count and confirmation rate came
in low relative to the 24h/4-chip budget; that analysis stands as the primary takeaway for
planning the next run of this experiment.

## HIGH PRIORITY, two compounding root causes for "only 13 hypotheses / 0 confirmed in ~22h",
plus a coordinator-visibility bug that hid it until now

**Bug 1 (platform, real): `GET $SCHED_URL/experiments` silently ignores unknown query params
instead of rejecting them.** I supervised this whole run passing
`?platform_experiment_id=pe-d564bb90`, but that endpoint's only real filters are `agent` and
`status` (confirmed via `GET $SCHED_URL/openapi.json`) -- the `platform_experiment_id` param does
nothing and the endpoint just returns every job in the scheduler's *entire history* (753 jobs
across many past platform experiments), not this run's jobs. Every "COMPLETED: 277/280/294..."
figure I reported at every checkpoint all session was this global count, not `pe-d564bb90`'s real
number -- I only caught this ~22h in when the user asked for a root-cause investigation and the
per-hypothesis job breakdown didn't add up (753 jobs across 189 hypothesis_ids, most with UUID
timestamps from long before this run started). The real, correctly-filtered number (client-side
filter on the top-level `platform_experiment_id` field, not `metadata.platform_experiment_id` --
that field doesn't exist on this object, another thing that cost time to discover) is **45 jobs
total in pe-d564bb90**: 21 COMPLETED, 19 EVICTED, 4 RUNNING, 1 REJECTED, across 13 distinct
hypotheses.
**Real fix, not done:** either the endpoint should reject unknown query params (400, not silent
ignore), or it should document/support a real `platform_experiment_id` filter -- silently ignoring
a filter param that looks plausible and returning unfiltered data with no error is a sharp edge
that will mislead any caller who doesn't independently cross-check, coordinator or otherwise.
**Coordinator takeaway:** always verify a filter param actually filters (check the openapi params
list, or diff two different filter values) before trusting a count built on it, especially early
in a run before there's an obvious "this number looks too big" signal to catch it.

**Bug 2 (real root cause of low hypothesis throughput): ~53% of this run's real job-slots (24 of
45) went to baseline/control reruns (13 jobs) and min_lr reproduction across seeds (11 jobs),
leaving only 21 job-slots to screen the other 11 distinct hypotheses -- most got just 1-4 jobs
each, several only a single probe.** This is not the fleet being idle or wasting time re-running
the *same* job pointlessly -- capacity was at 4/4 essentially the entire run (confirmed by
`resource-catalog/capacity` at every checkpoint) and each job genuinely takes 2-3.5h on this
hardware under the stage-1 cap, so ~45 jobs over ~22h at 4 concurrent slots is close to the
hardware's real throughput ceiling, not a sign of waste. The actual problem is *allocation*, not
idleness: once agent-2 discovered a real ~2.65% run-to-run noise floor between two identical
control replicates (mid-run, not known at the start), properly confirming or refuting min_lr
required several matched seeds/controls, not one probe -- correct science, but it consumed the
majority of the run's compute on essentially two things (verifying the baseline itself, and
disproving one early false positive) rather than spreading probes across more of the 11 other
ideas. Compounded by the already-logged cross-agent duplication (min_lr, target-norm, flip-aug,
steps-extend each independently re-registered by both agents) and low idea diversity (only
mask_ratio=0.75 cited a specific paper finding; the rest were hyperparameter-knob variations, no
architecture/masking-strategy exploration despite `experiment.md` explicitly allowing it) -- see
those sections below for detail, they are contributing causes to the same outcome, not separate
problems.
**Why this matters for judging the run:** 0 confirmed hypotheses after ~22h is a materially worse
outcome than the "16 hypotheses tested" framing suggested at earlier checkpoints (which itself was
inflated -- the real number is 13, and even that overstates genuinely new ideas since several are
near-duplicates). The user's expectation of "many hypotheses validated" was reasonable given a
24h/4-chip budget; what actually limited it was the reproduction-cost discovery combined with
duplication and narrow idea generation, not a platform capacity problem.
**Possible real fixes, not done:** (a) fix Bug 1 so future coordinator supervision reports
accurate live numbers, catching an allocation problem like this hours earlier instead of at the
end; (b) once a noise floor this large is discovered, the system prompt or experiment.md could
more explicitly budget a fixed fraction of remaining capacity to reproduction vs new-hypothesis
screening, rather than leaving it to each agent's own judgment mid-run; (c) the dedup and idea-
diversity fixes already proposed below would free up real job-slots for more probes.

## Resolved this run: node left tainted (no-workload), every job stuck_pending-evicted for the
first ~15min of pe-d564bb90

At setup.md step 1's capacity check, I confirmed `GET .../resource-catalog/capacity` showed 4
free chips via a plain curl but never actually called `lib_attach_node` (which removes the node's
default `hypothesisloop.io/no-workload:NoSchedule` taint) — capacity being *reported* free doesn't
mean the node is *schedulable*, those are two different checks and I only did the first. Result:
every job pod both agents submitted sat in `FailedScheduling` ("untolerated taint(s)") until the
platform's own stuck_pending eviction killed it (~5min later) — 6 of the first 8 jobs across both
agents were silently evicted this way before I caught it via `kubectl get events -A | grep
FailedScheduling`. Not a platform bug: `lib_attach_node`/`lib_detach_node` exist exactly for this
and setup.md step 1.1 says to call them; I skipped the call and only did the capacity read.
Fixed live at 2026-08-11T10:54Z via `lib_attach_node k3s-tt tt-quietbox`; both then-pending pods
started Running within ~20s. Node is left attached for the remainder of this platform experiment's
run — do not `lib_detach_node` until the experiment closes.
**Takeaway for next run:** `lib_attach_node` is a mandatory call, not implied by a capacity read —
treat the two as separate steps every time, even when capacity looks fine.

## OPEN, generic platform gap: cross-agent hypothesis dedup is easy to fail on early in a run

Observed in pe-d564bb90, ~1h in: agent-smri-fm-1 and agent-smri-fm-2 each independently
registered their own near-duplicate hypothesis for the same idea, at least 4 times over —
flip augmentation (`019ff083-cd1` vs `019ff080-82f`), per-patch target normalization
(`019ff071-2ac` vs `019ff070-d9a`), extending `--steps` 6000->9000 (`019ff07e-4d2` vs
`019ff071-c63`), and raising `--min-lr` to 1e-5 (`019ff06b-db1` vs `019ff067-e7a`) — same knob,
same direction, worded differently, registered minutes apart by different agents. Per the
system prompt (`agents/experimentator/.../prompts/system_prompt.md` step 4), cross-agent dedup
is explicitly the agent's own job ("same knob and direction = the same hypothesis however
worded... if it is another agent's, register your own naming theirs") — this isn't a platform
bug, and I don't act on agents' research per `setup.md`, so I didn't touch it.

Why it happened here specifically: both agents hit the shared hypothesis pool within seconds of
each other, very early in the run (before either agent had much in the pool to read/dedup
against) — a race inherent to a fresh platform experiment's opening minutes, not a one-off. Not
severe (redundant compute on 4 probes out of a 24h budget), but worth naming as a recurring
early-run pattern rather than assuming it's a fluke, since it'll cost real quota again on a
longer/bigger fleet if it's not.

**Possible real fix, not done, not confirmed necessary yet:** if this keeps recurring across
runs, the platform could surface a lightweight near-duplicate warning at hypothesis-registration
time (e.g. same job spec diff-key already open from another agent) rather than relying purely on
each agent's own pre-submission pool read — but this needs watching over a full run before
concluding it's worth building, not decided here.

**Update, ~10.5h into pe-d564bb90:** checked the full hypothesis pool (`GET
.../hypotheses?platform_experiment_id=pe-d564bb90`) -- 15 total registrations but only ~7 are
genuinely distinct ideas (min_lr, target-norm/its ctxdenorm fix, flip-aug, steps-extend-9000,
accum-iter/batch-size, weight-decay, mask_ratio=0.75). The rest are the same near-duplicate
pattern recurring past the opening minutes, not just an early-run race: min_lr, target-norm, and
flip-aug were each independently re-registered by both agents hours apart. Roughly half the pool
is redundant rather than new territory -- real, measurable cost to hypothesis breadth this run,
not just a theoretical race condition. Reinforces that this is worth building the lightweight
dedup-warning fix for, not just watching.

## BUG-ish, generic platform gap: hypothesis pool skews toward small variations of a handful of
known knobs, not genuinely new/literature-driven ideas -- worsened by cross-agent duplication

Observed in pe-d564bb90, ~10.5h in: of 15 registered hypotheses, only ~7 are distinct ideas at
all (min_lr, target-norm, flip-aug, steps-extend-9000, accum-iter/batch-size, weight-decay,
mask_ratio=0.75), and the other ~8 are the *same* ideas re-registered independently by the other
agent (see the dedup section above). Of the 7 distinct ideas, only one (mask_ratio=0.75) cites a
specific paper finding in its own text; the rest are plausible-sounding tuning-knob variations
(LR floor, weight decay, step count, batch size, an augmentation) rather than something that
required real literature grounding to arrive at. `system_prompt.md` step 4 tells agents to
"research it" before each trial and rewards an agent who "looked up how the relevant kernels and
runtime behave," but in practice this run's actual hypothesis pool reads like standard
hyperparameter-sweep territory, not a wide net of independently-sourced ideas (e.g. architecture
changes, different masking strategies, alternative optimizers/schedules from recent MAE
literature) despite `experiment.md`'s METHOD explicitly naming "architecture/hyperparameters
(depth, width, heads, patch/masking strategy)" as fair game alongside the LR/schedule knobs
actually being tried.

Not clearly a platform bug (nothing stops an agent from proposing an architecture change), and I
don't act on agents' research per setup.md -- but the combination of duplication eating capacity
and low idea diversity means this run's phase-1 screening window (now extended to ~24h) is at
real risk of mostly re-confirming/refuting the same narrow set of knobs rather than covering
the breadth phase 1 is meant for.

**Possible real fix, not done:** the dedup-warning idea above would reclaim wasted capacity, but
doesn't by itself widen the idea pool -- if this recurs across runs, worth considering whether the
system prompt should more explicitly nudge toward architecture/masking-strategy exploration (not
just hyperparameter tuning) before rewarding "grounded in something real," since knob-tuning
trivially satisfies "grounded" (a prior trial or a general recipe fact) without requiring genuinely
new literature-sourced ideas.

**Related improvement idea (not built):** encourage the coordinator to actively watch for
duplicate hypotheses across agents, and consider introducing an eviction/expiry mechanism for
stale hypotheses in the registry, rather than leaving the pool to grow unbounded.

## HIGH PRIORITY (unaddressed, generic platform gap): no generic image distribution to bare-metal
hosts

Bare-metal cluster hosts (e.g. `tt-small`) have no way to receive a workload image other than a
manual `podman save` + `scp` + `docker load` by the coordinator — there is no pull mechanism at
all. `runtime/bare-metal/internal/podexec` only classifies a missing-image error
(`phase_detail.go`: "no such image" -> `PhaseReasonImagePullFailed`); it never attempts an actual
fetch. An image tagged `localhost/...` resolves a pull attempt against `localhost:443` on that
host, which fails immediately — same root shape as the earlier k8s containerd gap this session
already fixed (image present in podman's store != present where the job actually runs), just
never fixed for the bare-metal backend.

Hit for real 2026-08-08: `tt-small`'s bare-agent log showed `No such image:
localhost/hypothesisloop-smri-fm-workload:latest` on every reconcile attempt — not the
previously-assumed "std::bad_cast ABI mismatch" (that note was stale/unverified; the actual
blocker was this, confirmed live). Worked around once by manually `podman save` (~28GB) + `scp` to
tt-small (slow over WiFi, ~10MB/s, ~45min) + `docker load` — not a real fix, doesn't scale past
one host, and silently repeats for every future bare-metal node.

**Real fix (not done yet):** a shared container registry (e.g. `registry:2`) reachable by every
host, image pushed there once, both runtime backends (`runtime/bare-metal/internal/podexec` via
`docker/podman pull`, `runtime/k8s/internal/k8sexec` via `k3s ctr images import` or a registry
mirror) pulling from `<coordinator-ip>:5000/...` instead of assuming local presence. Generic
platform capability, not smri-fm-specific — belongs in `runtime/` and `controlplane/`, per
`important.md`.

## Non-bug pattern worth naming: agents tight-polling QUEUED/RUNNING jobs instead of the system
prompt's own "relaxed interval" guidance

Observed on both agent-smri-fm-1 and agent-smri-fm-2 in pe-d564bb90: repeated short turns
(~5-30s apart) re-checking the same still-unchanged job/capacity state, well past the point of
new information, despite system_prompt.md step 6 explicitly saying "a tight polling loop buys
nothing." Not a platform bug, and it's the agents' own session-loop behavior (out of coordinator
scope per setup.md), but it recurred across both agents in this run and agent-smri-fm-2 eventually
self-corrected by switching to a longer foreground block-wait.

**Possible real fix, not done:** the system prompt's "relaxed interval" language could be made
more concrete (e.g. a stated minimum sleep between polls of an unchanged job) rather than left
entirely to each agent's judgment, if this keeps recurring across runs.
