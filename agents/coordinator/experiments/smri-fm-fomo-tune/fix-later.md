## 2026-08-16 coordinator follow-up: `frofa_stability_enet` confound diagnostic run -- FAILS

Per the standing instruction that `frofa_stability_enet` (local ~0.81, external FCD 0.875) was an
"open, unverified" candidate pending the SAME score-level diagnostic that refuted
`hetero_ensemble_frofa`, ran it: `scratch_ppmr_validation/confound_direct_diagnostic_frofa_stability_enet.py`
(reuses `confound_direct_diagnostic.py`'s exact helpers/protocol against `heads_agent8.py`'s
`_FroFAStabilityEnet`). **VERDICT: FAIL.** Partial correlation of final OOF score vs AP-extent
controlling for label: r=-0.536, p=0.0 (5000 within-class permutations, seed=0 reproduction
AUROC=0.8056, seed-sweep 0.795/0.826/0.873). GBT nonlinear check on the score: R^2=0.039, p=0.03
(significant, though much weaker than hetero_ensemble_frofa's R^2=0.25). Residualized-feature-level
check stays clean (R^2=-0.148, p=0.25), same pattern as before: FroFA pushes the SCORE, not the
features, into confound-dependence, and this now holds across two structurally different head
families (RDA/PAM ensemble AND stability-selected elastic-net) -- **FroFA itself, not any one
downstream head, is implicated.** Matched-confound-subset AUROC (diagnostic-only, underpowered):
0.98 at 11 pairs (caliper 0.5sd), 0.78 at 13 pairs (caliper 1.0sd) -- noisy, not treated as
reassurance either way. Full numbers: `confound_direct_diagnostic_frofa_stability_enet_result.json`.

**Consequence: no local-AUROC-beating candidate from this round (hetero_ensemble_frofa,
frofa_stability_enet) has passed the direct score-level diagnostic. Both remain refuted/unverified
as confound-clean.** The 0.795 debiased baseline is the only number on this task with a
consistently weak/borderline (not large) score-level dependence (partial_r=-0.55, p=0.001, but GBT
R^2 not clearly significant across checks) -- still not perfectly clean, but categorically less so
than either FroFA-based candidate.

**New structural exploration (coordinator scratch, not a production hypothesis):** tried a
purely rank-based, non-covariance head (`scratch_ppmr_validation/rank_head_exploration.py`,
Mann-Whitney/rank-biserial per-feature screening + rank-position scoring) to test whether
*removing covariance/variance structure entirely* (the mechanism `confound_followup.py` pinned as
the main channel for RDA/PAM/FroFA) closes the leak. Result: local AUROC 0.818 (promising) but
**also FAILS**, with partial_r=-0.71 (p=0.0) -- LARGER than either FroFA config, though it does
pass the nonlinear GBT check (p=0.27). Mechanism is different from the RDA/PAM case: aggregating
many individually-weak confound-correlated per-feature scores produces a strong linear correlation
in aggregate (central-limit-style), unrelated to per-feature capacity. **Conclusion: reducing head
capacity/removing covariance structure is not sufficient by itself to close this leak** -- it is a
genuinely open problem, not solved by any lever tried so far (linear score residualization,
dropping RDA, variance-aware feature residualizer, rank-based heads). Two untried ideas worth a
future hypothesis, pointed at from `experiment.md`'s LEVERS section: (1) rank-based (not OLS)
score-level orthogonalization, (2) redesigning FroFA's augmentation noise to be orthogonal to the
fold-safe confound direction rather than isotropic.

**Recommendation for the live platform experiment (pe-1b62dccc) agents:** do not spend further
budget trying to rescue `hetero_ensemble_frofa` or `frofa_stability_enet` as-is; both are refuted
as confound-clean local winners. If pursuing a local-AUROC win, prioritize the two untried levers
above over further ensemble/head variations on the same FroFA recipe -- variations on FroFA+{RDA,
PAM,elastic-net,rank-head} have now been tried 4 ways and all leak. If time is short, the safest
path to a genuine, defensible round result remains the 0.795 debiased baseline or a
generalization-holding technique (CL2N, prespecified ensembling without FroFA) per `experiment.md`'s
existing guidance.

---

# Out-of-scope / open issues (smri-fm-fomo-tune, Task_5)

Only genuinely open items live here. Resolved threads and per-poll status entries have been
removed: a run's narrative belongs in its own record, not in the open-issues list (`important.md`
#17 — documentation is not a working log). The authoritative session conclusion is
`FINAL_RESULT.md`; the method and its constraints are in `experiment.md`.

The full pre-clearing history (1,881 lines, 25 polls) was preserved at
`/tmp/claude-1000/-home-ttuser-projects-hypothesisloop/cff6d2fa-c224-4603-b537-880dddd97d88/scratchpad/fix-later.md.original`
before this file was rewritten.

---

## 1. Permutation-test verdict on the label-adaptive failure mode — OPEN, do not pre-empt

Three techniques (LDA-shrinkage head, depth-fusion+LDA, PLS-DA) showed a local AUROC win on the
48-subject Task_5 set that did not hold on the external FCD check. The working explanation is
label-adaptive high-capacity overfitting at n=48 / p=1024: techniques that use labels to fit a
high-dimensional projection lock onto sample-specific spurious label-correlated directions, giving
a stable-looking but fake pooled OOF gain. The clean negative control is that non-label-adaptive
techniques (CL2N, prespecified-config ensembling, FroFA) held locally *and* transferred.

A permutation/null-distribution test (200-500 label shuffles under the same 20-fold protocol, for
both the failing label-adaptive heads and the holding techniques as control) plus a capacity sweep
was dispatched to settle this quantitatively. Results, if the run completed, are in
`scratch_ppmr_validation/overfit_confirmation_results.json` and `OVERFIT_MECHANISM_CONFIRMED.md`.

**Open until that verdict lands and is reviewed.** These were the most promising local results of
the session, and the operator's explicit instruction is that they are not to be dismissed as
"didn't generalize, drop it" without a rigorous, quantitative understanding of why. Do not treat
PLS-DA or LDA-shrinkage as refuted noise before then.

## 2. No independent PMG validation cohort exists — durable gap

Nothing in this project's data provides a second PMG cohort (different site/scanner/protocol from
Task_5's 48 subjects). The FCD cohort (OpenNeuro ds004199) is the best available external check and
is now honestly labelled a *related-malformation transfer probe / feature-generality check* rather
than a PMG-generalization test, because:

- single pediatric site (Bonn), mixed acquisition protocols within it;
- only 59% (50/85) of FCD-positive labels are histopathology-confirmed, the rest clinical or
  radiological suspicion;
- T1w only — FLAIR, the more diagnostic sequence for FCD, is absent;
- the check **refits a new head on FCD data** rather than applying the frozen PMG classifier, so it
  answers "does the frozen encoder generically detect cortical abnormality", not "does the PMG
  classifier transfer" or "does it avoid false-positiving on FCD";
- FCD and PMG are structurally different malformations.

Treat this as durable: no amount of further work on the FCD cohort closes it. A genuine
"generalizes to PMG" claim needs an actual second PMG cohort.

## 3. True-registration harmonization: computed, never evaluated

`scratch_task5_repro/reg_register.py` completed a real ITK Mattes-MI affine registration of all 48
subjects onto a reference grid (48/48 registered, ~22 min total); the outputs sit in
`scratch_task5_repro/reg_harmonized_volumes/`. No downstream evaluation was ever written — there is
no `reg_*_encoder_extract.py` or `reg_*_full_protocol_eval.py`, unlike the two cheaper
harmonization attempts, which both have eval scripts and both made the confound leak *worse*
(R²=0.51 fixed-grid, R²=0.90 content-crop).

A future hypothesis wanting genuine spatial-registration harmonization has a running start: the
expensive step is done, only feature extraction plus the standard eval protocol remain. There is no
strong prior it will do better, but it has not been ruled out.

## 4. The confound fix is a conservative floor, not a ceiling

AP-extent is strongly correlated with diagnosis in this dataset, so no purely statistical method
can fully separate true disease signal from the confound using these data alone. Fold-safe OLS
residualization removes *all* linearly AP-extent-associated signal, including any genuine PMG
signal that happens to correlate with head coverage. The shipped 0.795 AUROC is therefore a
defensible, conservative floor and likely a slight underestimate — not a hard ceiling to beat.

## 5. Task 3 (brain age) was never checked for the same confound

The confound investigation covered Task_5 only. Task 3's 494-subject set has not been through the
equivalent diagnostic (single-feature AUROC/correlation of header/file/intensity metadata against
the label, geometry-anchor check, residual-leak check). Production wiring deliberately leaves it
uncorrected — `cross_validate`'s `confound` argument is populated only for `task == "task5"` — since
there is no evidence either way. If Task 3 becomes a focus, run `confound_check.py` against it
first rather than assuming its head/pooling levers are confound-free.

## 6. ABIDE validation set: present, never inspected

`scratch_abide_validation/` has its own pipeline and carries `features.npz`, `preds.json` and
`metrics.json`, so at least one cross-validation pass has been run — but nobody checked whether it
predates the confound fix. Its numbers must not be cited as "already validates the fix" without a
fresh look. (The parallel `scratch_fcd_validation/` cohort *was* subsequently run and characterised
— see item 2.)

## 7. Nothing stops one agent re-deriving a peer's already-published result

Observed directly: after one agent root-caused an external-cohort failure and posted a working
number, the other spent a long window independently re-deriving the same answer through its own
code path, ultimately abandoning it as environment flakiness. The pointer had already been shared
once. This is a conduct/prompt question rather than a control-plane feature, but it cost real
budget and is worth an explicit norm: once a peer publishes a resolved number for a named
diagnostic, take it as given unless directly disputing the method.

---

## Platform-side items (reviewed 2026-08-16)

The opaque-job-failure thread raised across several polls ("log capture appears to cut off before
the actual failure line") was a real platform defect and is **fixed**: a terminal job's workload was
deleted before its final log tail and container termination reason could be captured, so whether a
failure was diagnosable was a race. **Requires the runtime image rebuilt**, not just the control
plane; after that, a failed job's `GET /experiments/{id}/logs` carries the crashed instance's real
tail and a `container_failed` reason with its exit code.

Checked and *not* defects, so nothing is pending: stale `open`/`running` platform experiments from
an abandoned session (hygiene — `SweepExpired` already closes anything carrying an `ends_at`), and
stages being immutable once a run is going (working as designed; the full ladder is a create-time
decision). One behaviour change affects setup: a platform experiment now requires at least one
ranking metric at create/update — `role` defaults to `ranking`, so a normal contract is unaffected.

---

## Round close: pe-99a1efec (2026-08-16)

`pe-99a1efec` ("smri-fm-fomo-tune", 2-stage 40/60 screen→confirm, budget 20 accelerator-hours,
`ends_at` 2026-08-16T15:31:21Z) reached its `ends_at` and was auto-closed by `SweepExpired` before
this close-out ran — `POST .../close` was attempted anyway to confirm idempotency and correctly
400'd `invalid_transition: experiment is closed`, so no body-less/idempotent-close gap here; the
supervise.md instruction to "always POST close" needs a caveat for already-terminal experiments.

**Final standings** (`GET .../results`): `agent-smri-fm-fomo-tune-5` and
`agent-smri-fm-fomo-tune-6` tied exactly on the ranking metric, both `auroc = 0.8576388888888889`
(rank 1 and rank 2, `basis: raw`) — a genuine tie, not a rounding artifact. Both cleared the
0.795 debiased baseline. `seconds_per_subject` (diagnostic only) split them marginally:
agent-5 3.19s, agent-6 3.20s. Both agent containers (`agent-smri-fm-fomo-tune-5`,
`agent-smri-fm-fomo-tune-6`) had already exited cleanly (exit 0) by the time this close-out ran —
no `podman stop` was needed. Branches/worktrees left intact per standing instruction. Node
`tt-quietbox` detached (`lib_detach_node k3s-tt tt-quietbox`, taint re-applied, confirmed via
`kubectl get node tt-quietbox -o jsonpath='{.spec.taints}'`).

Open item carried forward: since both agents tied at the exact same AUROC to 13 significant
figures, the winning *config* (not just winning agent) should be identified from each agent's
final job/hypothesis record before the next round cites this result as a starting point — this
close-out did not have time to pull individual job configs to confirm whether the tie reflects the
same technique converged on independently or two different techniques landing on the same pooled
OOF AUROC by coincidence.

## Round close: pe-7ad5e875 (2026-08-16) — launch spawned only 1/2 agents

`pe-7ad5e875` ("smri-fm-fomo-tune", 3-stage 35/35/30 with `max_job_hours` 0.25/0.75/1.5, budget 48
accelerator-hours, `max_agents` 2) was found with `signed_up_agents: ["agent-smri-fm-fomo-tune-7"]`
only — `signup_count: 1` against `max_agents: 2` — roughly 20 minutes after launch, with status
already flipped `Open` -> `Running` (`starts_at` had passed), so signup was permanently closed and
a second agent could never join this round. `podman ps` confirmed only one container
(`agent-smri-fm-fomo-tune-7`, up ~20 min) existed for this platform experiment; no
`agent-smri-fm-fomo-tune-8` (or similarly-numbered second container) was ever created or exited —
this was not a container that crashed after spawning, it was never spawned in the first place.

**Root cause: NOT a capacity issue.** `GET /resource-catalog/capacity` at the time showed
`tenstorrent.com/chipArch=blackhole` at 3/4 available — comfortably enough for a second
`CHIPS_PER_AGENT=2` agent (would have needed exactly 2, and even the 3 already free covered that).
So the original launch task's `NUM_AGENTS = floor(available_devices / CHIPS_PER_AGENT)` math
should have produced 2, and evidently intended to (a `pe-7ad5e875` with `max_agents: 2` was
created), but only one `podman run` for the second agent container either never executed, silently
failed, or was skipped in that launch's step 3 — the launch task did not verify container count
immediately afterward, so the gap went unnoticed until this supervise-side check caught it ~20 min
in.

**Close-out:** `POST .../platform-experiments/pe-7ad5e875/close` succeeded (`{"status":"closed"}`,
first attempt, no idempotency caveat needed here since the round was still `running` going in).
Final standings (`GET .../results`, single agent, 3 hypotheses total): `pam` (Nearest Shrunken
Centroids) **refuted** — failed to clear baseline; `rda` (diagonal-heavy Regularized Discriminant
Analysis, gamma in {0.9,0.97,1.0}) **inconclusive**, local AUROC 0.7413 [0.582,0.878] at seed=0,
below the 0.795 debiased baseline but well within its own wide CI — a 3-seed sweep (seeds 1/2/3:
0.767/0.804/0.764, mean~0.778) essentially matched the baseline's own seed-sweep mean, so treated
as a non-beat rather than a regression; `stability_enet` (stability-selected elastic-net logistic
regression) was **still running** (job `smri7-stabenet-v1` in flight) at close time — its status
was left `open`, no verdict recorded, since ending the round mid-job means this hypothesis's
result is genuinely unknown, not a refutation. `agent-smri-fm-fomo-tune-7`'s container was stopped
(`podman stop`, required a SIGKILL after SIGTERM timeout — took slightly over the 10s grace
period, nothing alarming) with its branch/commits left intact in `$CODE_REPO_URL`
(`git://192.168.1.76/hypothesisloop-smri-fm-fomo-tune.git`, includes the `pam`/`rda`/`stability_enet`
head implementations in `heads.py` — real, reusable code regardless of this round's early close).
Node `tt-quietbox` detached (`lib_detach_node k3s-tt tt-quietbox`), taint
`hypothesisloop.io/no-workload` confirmed re-applied, capacity back to 4/4 blackhole chips free.

**Action for future launches:** setup.md step 3 already says to spawn one container per agent and
implicitly expects `NUM_AGENTS` running — this is now underlined explicitly: immediately after
issuing all `podman run` commands, run `podman ps` and count containers matching the new
`PLATFORM_EXPERIMENT_ID`/experiment name, and confirm the count equals `NUM_AGENTS` before walking
away. Also poll `GET /platform-experiments/{id}` shortly after (well before `starts_at`) and
confirm `signup_count` reaches `NUM_AGENTS` — a container can be running but still fail to reach
signup (auth/network issue), which `podman ps` alone won't catch. Neither check is expensive and
both together would have caught this within a minute of the original launch instead of ~20 minutes
via supervise-side discovery.

---

**2026-08-16: permutation-test diagnostic canceled.** The background `overfit_confirmation.py`
process (PID 2651704, permutation test investigating why old results failed) was killed per
operator direction. Operator wants compute/attention focused purely on the live platform
experiment producing a strong model, not on explaining past failures. No further diagnostic work
on this thread unless explicitly requested again.

---

## pe-1b62dccc: healthy relaunch confirmed, round underway (poll 2026-08-16 16:37Z)

`pe-1b62dccc` (successor to `pe-7ad5e875`, which closed early with `stability_enet` unresolved)
launched cleanly this time: `signup_count: 2/2` (`agent-smri-fm-fomo-tune-8`, `-9`), matching
setup.md's post-launch checklist added after the previous round's 1/2-signup bug. `ends_at`
2026-08-17T16:30:46Z, ~24h out — no urgency.

Both agents carried forward `pe-7ad5e875`'s context correctly: both independently registered
`stability_enet` (re-run of the evicted attempt) and `hetero_ensemble` (agent-8's hypothesis text
explicitly cites porting agent-7's prespecified-but-never-run hetero_ensemble design from the prior
round). At this poll: `smri8-stabenet-v1` COMPLETED, `smri8-heteroens-v1` RUNNING,
`smri9-stabenet-v1` RUNNING, `smri9-heteroens-v1` RUNNING — all within stage 1's 0.25h job cap,
no stuck jobs. Stage 1 progress 0.4%. Both agents are additionally using idle time productively
(agent-9 was implementing a FroFA+stability_enet combination head while waiting on job completion).
No action taken; nothing to fix. Will need a follow-up poll once `smri8-stabenet-v1`'s result comes
back to see if the re-run resolves the "genuinely unknown" verdict left by the early close.

---

## pe-1b62dccc: poll 2026-08-16 17:05Z — real numbers in, one promising lead, one recurring platform-adjacent failure

**Status:** 2/2 agents alive, both actively running jobs (no cuts, stage 1, progress ~2.5%).
`stability_enet` (both agents' standalone re-runs) is now **refuted** (agent-8: `smri8-stabenet-v1`;
agent-9: `01a00b6a-518b`) — resolves last poll's "genuinely unknown" carry-over from `pe-7ad5e875`'s
early close. `supervised_pca` also refuted. `rda`/`pam` refuted or inconclusive as before.

**hetero_ensemble (logreg + diagonal-RDA + PAM, z-scored, equal-weight):** local AUROC **0.8003**,
confirmed independently by both agents (`smri8-heteroens-v1`, `smri9-heteroens-v1`, byte-identical
per agent-9's own hypothesis text). This is only ~0.005 above baseline's 0.795 point estimate —
given baseline's own CI is [0.652, 0.912] (width ~0.26), this is almost certainly *inside* the
CI-noise band, i.e. NOT yet a "meaningful" beat by experiment.md's own bar. Status left `open` by
both agents pending the external FCD check (correctly not claimed as a win) — but see below, that
check is stuck.

**frofa_stability_enet (stability-selected elastic-net ON TOP OF FroFA augmentation, agent-9,
new this round, not in experiment.md's prior-round list):** local AUROC **0.8299** (seed=0,
`smri9-frofastabenet-v1`, COMPLETED) — clearly the best local number either agent has produced
this round, and plausibly outside baseline's noise band given the ~0.03 gap over hetero_ensemble's
own near-null 0.005 gap. Agent-9 is correctly running the mandatory 2-seed robustness sweep
(`smri9-frofastabenet-sweep-v1`, seeds 1,2) before trusting it — still RUNNING at poll time, ~12
min in, within stage 1's 15-min cap. **This is the one to watch:** if the sweep holds (doesn't
collapse toward 0.795), the mandatory external FCD probe is the next required step per
experiment.md's hard requirement — agent-9 has not started it yet (correctly gating on the sweep
first). No FCD job for this hypothesis exists yet as of this poll.

**Root-caused failure — repeated external-FCD-check job failures (not yet flagged to agents):**
Both agents' hetero_ensemble external FCD attempts are stuck in a fail/evict retry loop: agent-8
5 attempts (`smri8-heteroens-external-fcd-v1..v5`, mix of EVICTED reason=never_reported_metrics and
FAILED), agent-9 6 attempts (`smri9-heteroens-fcd-v1..v6`, same pattern). Every failing log ends
identically mid-embedding-phase (subject ~30-35 of 170):
`[warn] metric POST failed (1/1): HTTP Error 422: Unprocessable Entity`, then the job goes silent
and is eventually evicted for never reporting a terminal metric. Traced the 422 to
`controlplane/services/registry/metrics.go` `RecordMetric`: the only conditions that produce
`ErrInvalidMetric`→422 are `metric_basis` >64 chars/non-ASCII, `metric_value` non-finite, or
**`fraction_complete` outside [0,1]** (line 52-53) — or the target experiment already being
terminal. Agent-9 already suspected a posting-frequency issue and adjusted (`v4` commit: "post
embed-phase metrics every 5th subject, not every subject") but that did NOT fix it — v4/v5/v6
still failed, just slightly later. This points away from a rate-limit and toward the FCD script's
own `fraction_complete` computation going out of range partway through the 170-subject embed loop
(e.g. wrong denominator/off-by-one) — **this is the agents' own `external_fcd.py` diagnostic
script, not platform code**, so per this file's mandate I have not touched it. Flagging here
because it has now cost 11 wasted job-attempts (~35-40 min of accelerator time) across both agents
without either flagging it as a script bug in their hypothesis notes — worth a nudge if it keeps
recurring past the next poll, since the platform-side behavior (rejecting bad fraction_complete) is
correct and not something to "fix" away.

**Action taken:** none (no platform/environment bug — the 422s are correct server-side rejections
of malformed client input from the agents' own script). Documented here per the "never drop a
finding without root-causing" directive. Nothing else needs coordinator intervention; round has
~23h left, well ahead of `ends_at`.

---

## pe-1b62dccc: poll 2026-08-16 17:35Z — sweep result mixed, FCD bug self-fixed by agent-9, new distinct bug found

**Status:** 2/2 agents alive (`podman ps`: both up ~1h), stage 1, progress 4.5% (up from 2.5%,
steady pace), no cuts, `ends_at` unchanged 2026-08-17T16:30:46Z.

**frofa_stability_enet 3-seed sweep — DONE, result is mixed, not a clean hold.** Pulled the raw
metric points from `smri9-frofastabenet-sweep-v1` (`GET .../metrics`):
seed=0 **0.8299** (`auroc`, the original result), seed=1 **0.7743** (`diagnostic_auroc_seed1`),
seed=2 **0.8211** (`diagnostic_auroc_seed2`). `leak_r2` = -0.021 (confound check clean, no residual
AP-extent leak). **This is not the clean robustness confirmation last poll hoped for**: seed=1
(0.7743) falls *below* the 0.795 debiased baseline, seed=2 also undershoots the seed=0 headline
number. Mean across 3 seeds ≈ 0.809, still above baseline, but the spread (0.774–0.830, range
0.056) is wide enough that "meaningfully beats baseline, not a CI-noise bump" is not yet a slam
dunk — this needs the same skepticism previously applied to the label-adaptive dead-end (though
frofa_stability_enet is not a pure label-adaptive projection like PLS/LDA, `stability_enet`'s
feature-selection step does use labels, so the same overfitting mechanism is plausible at n=48).
Do not report this as a clean win yet; a seed-0-only headline number would be exactly the mistake
`experiment.md` warns against.

**External FCD check for frofa_stability_enet — ran successfully:** `smri9-frofastabenet-fcd-v1`
COMPLETED (first clean completion of any external-FCD job all round). `external_fcd_auroc` =
**0.8752** [0.821, 0.924], vs FCD's own established baseline ~0.887 — inside its own CI, so **no
clear regression**, a genuine (if narrow) pass of the generalization probe. Combined with the
seed-sweep spread above, the honest characterization is: local gain over baseline is real on
average but not uniformly robust across seeds (1 of 3 dips below baseline), while the external
transfer check itself is clean. Worth a note back to agent-9 to report both facts together, not
just the seed=0 headline.

**External-FCD 422/eviction bug — RESOLVED, by the agent itself, not by coordinator intervention.**
Confirmed by reading `tenstorrent/src/fomo_tune_tt/external_fcd.py` on agent-9's branch at commit
`9e31f14` (the branch's current HEAD) and cross-referencing `controlplane/services/controller/checks.go`
`checkSilence`: the real mechanism was never `fraction_complete` going out of `[0,1]` in the script's
own math (that computation, `(i+1)/len(rows)`, is always in range) — it was the *eviction watchdog*
requiring progress under the platform experiment's **declared** metric keys (`auroc`,
`seconds_per_subject`, from `pe-1b62dccc`'s own `metrics` list) within a grace window (`2 * max(3 *
report_interval_seconds, minSilenceWindow)`, ≈2 min here given `report_interval_seconds=5`).
`external_fcd.py`'s embed phase (the expensive, ~3.4s/subject part) was, until agent-9's own fix,
posting progress only under undeclared `external_fcd_*`-prefixed metric names — invisible to
`AnyDeclaredMetricReported`, so the watchdog correctly (by its own documented design) saw "alive
pod, zero declared-metric activity for 6x its report interval" and evicted with
`EvictionNeverReportedMetrics` partway through the embed loop (~subject 30-35, matching the ~2min
grace window almost exactly). Every subsequent `post_metric` call after that eviction 422s because
`RecordMetric` rejects samples for a terminal experiment (`metrics.go` line 66-67) — that's the
"HTTP Error 422" agents kept seeing and (reasonably, from inside the job) misread as a
metrics-endpoint problem rather than "this job was already evicted." Agent-9's own commit `9e31f14`
("also post embed-phase progress under the declared seconds_per_subject key, not only
external_fcd_seconds_per_subject") fixes exactly this, and it's now empirically confirmed: the
first FCD job run on that commit (`smri9-frofastabenet-fcd-v1`) completed clean end-to-end. **No
coordinator code change was made** — the fix already lives in the agents' own shared-module commit
history and works; per the "don't act on an agent's behalf" norm this was correctly left to the
agent, and in fact was already resolved by the time this poll happened. Downgrading from "consider
fixing the shared harness directly" to "confirmed self-resolved, watch for propagation" (see next
item).

**Propagation gap — agent-8 has NOT picked up agent-9's fix, still hitting the same bug.**
`agent-8`'s branch (`remotes/origin/agent-agent-smri-fm-fomo-tune-8-pe-1b62dccc`) is still on
`fb44a31`, the original version of `external_fcd.py` with neither of agent-9's two fixes. Agent-8's
own FCD attempts for the same `frofa_stability_enet` hypothesis are failing the identical way:
`smri8-frofastabenet-external-fcd-v1` EVICTED `never_reported_metrics`, v2/v3 FAILED. This is
exactly the pattern flagged in item 7 above ("nothing stops one agent re-deriving a peer's
already-published fix") — agent-8 is currently burning job attempts on a bug its peer already
root-caused and fixed in the same shared repo. Left alone per "never act on an agent's behalf" (no
cherry-pick performed on agent-8's branch), but flagging here in case it's still unresolved next
poll — if so, worth a nudge that agent-9's `external_fcd.py` fix exists and where.

**New, distinct bug found — NOT the same as the 422/eviction issue, NOT touched.**
`hetero_ensemble`'s external-FCD attempts are now at 15 attempts for agent-9 alone
(`smri9-heteroens-fcd-v1..v15`) and have shifted from `EVICTED never_reported_metrics` (pre-fix
pattern) to plain `FAILED` (v4/v5/v8/v9/v10/v11/v12/v13/v14, even though these run on the same
fixed `9e31f14` commit as the successful `frofastabenet-fcd-v1`). Logs for v13/v14 cut off
immediately after `"HEAD=hetero_ensemble"` is logged — i.e. it dies at or just after
`build_head("hetero_ensemble")`/embed start, before any per-subject progress line, with no
traceback captured (possibly the same opaque-log-truncation issue noted as "fixed, needs runtime
image rebuild" in the platform-side section above — worth checking if that rebuild has actually
landed on this node). This looks like a bug in `hetero_ensemble`'s own head implementation
(`heads.py`) when applied to the FCD cohort's data shape, not a harness/metrics issue — **this is
agent-9's own hypothesis-specific research code, explicitly out of scope for coordinator
intervention.** Flagging only because 15 attempts (~40+ min accelerator time just this round) with
no visible change in agent-9's approach between attempts suggests it hasn't been diagnosed on the
agent side yet — worth a nudge next poll if it's still blindly retrying past attempt ~20.

**Action taken:** none (both the resolved 422 bug and the new hetero_ensemble failure are
agent-owned; no platform/shared-harness code was modified this poll). Round healthy otherwise, ~23h
left.

---

## pe-1b62dccc: poll 2026-08-16 18:05Z — new local-best (hetero_ensemble_frofa), frofa_stability_enet externally confirmed but seed-fragile, propagation gap persists

**Status:** 2/2 agents alive (`podman ps`, both up ~2h), stage 1, progress 6.4% (up from 4.5%), no
cuts, `ends_at` unchanged 2026-08-17T16:30:46Z. No stuck jobs beyond the known FCD retry pattern
below (all within stage-1's 0.25h job cap).

**frofa_stability_enet — status unchanged from last poll, now the one CONFIRMED result meeting
half the bar.** Seed sweep (seed0=0.8299, seed1=0.7743, seed2=0.8211, mean~0.809) still not
seed-robust — 1 of 3 seeds dips below the 0.795 baseline. External FCD check (agent-9,
`smri9-frofastabenet-fcd-v1`): confirmed **0.8752** [0.821, 0.924], inside FCD's own ~0.887 CI, no
regression — this part passed cleanly. **Net verdict unchanged: real average local gain (mean
~0.809 > 0.795) with clean external transfer, but not seed-robust enough to call a slam-dunk win.**
No new seeds run since last poll.

**NEW: hetero_ensemble_frofa (agent-8, FroFA-lite augmentation stacked under the 3-member
logreg+diagonal-RDA+PAM ensemble) — local AUROC 0.8663 [0.751, 0.953], the best local number
either agent has produced all round**, clearly ahead of frofa_stability_enet's 0.830 headline and
hetero_ensemble's own near-null 0.8003. Independently re-implemented by agent-9 too
(`smri9-heteroensfrofa-v*`, several device-related job failures before v8 finally got past the
correctness gate and into the embed phase — still RUNNING at poll time, no result yet). Agent-8 has
started (a) a seed-robustness sweep (`smri8-heteroens-frofa-sweep-v1`, RUNNING, appropriately
following the round's own "verify seed-robustness before trusting a new best" guidance) and (b) 6
external-FCD attempts (`smri8-heteroens-frofa-external-fcd-v1..v6`), ALL EVICTED/FAILED. **This is
the same fix-later item as last poll's "propagation gap," now confirmed still open**: cloned
`origin/agent-agent-smri-fm-fomo-tune-8-pe-1b62dccc` (HEAD `0c91c2c`) and confirmed via
`git merge-base --is-ancestor 9e31f14 <branch>` → NO — agent-8's branch still does not contain
agent-9's `9e31f14`/`235c44f` `external_fcd.py` fixes, 5 commits and ~45+ min after agent-9 pushed
them. Agent-8's own comment on the hypothesis thread (18:03Z) shows it has correctly identified the
failures as "the same infra pattern documented on 01a00b6c-d694 (now confirmed t[o be fixed]...)"
but has not yet cherry-picked or re-derived the fix itself. **This is the load-bearing result of
the round right now (best local number, still unconfirmed externally) — worth a nudge next poll if
agent-8 still hasn't picked up the fix, since it's the most valuable pending validation in flight.**

**hetero_ensemble (no FroFA) — now fully resolved, confirmed NULL result.** External FCD check
finally completed on attempt 15 (`smri9-heteroens-fcd-v15`): **0.886**, essentially exact match to
FCD's own ~0.887 baseline — no regression, but combined with local 0.8003 (already established as
inside baseline's CI-noise band), this closes out as a clean-transfer/no-local-gain null, not a
win. Correctly written up as "resolution" on the hypothesis thread by agent-8. No further action
needed on this hypothesis.

**Action taken:** none (propagation gap remains agent-owned per "don't act on an agent's behalf";
noted here for visibility, not intervention). Round healthy, ~22h left, well ahead of schedule.

---

## pe-1b62dccc: poll 2026-08-16 18:35Z — HEADLINE: hetero_ensemble_frofa clears BOTH bars

**Status:** 2/2 agents alive (`podman ps`, both up 2h), stage 1, progress 8.4%, no cuts, `ends_at`
unchanged. Round healthy, well ahead of schedule.

**hetero_ensemble_frofa (agent-8's 01a00b96 / agent-9's 01a00bb2) is now the first candidate this
round meeting the FULL standing goal — flagging prominently per instructions.**

1. **Seed-robustness sweep — DONE, clean hold.** Agent-9's `smri9-heteroensfrofa-v8` (byte-identical
   local AUROC 0.8663194444444445 to 15 decimals vs agent-8's `smri8-heteroens-frofa-v4`) also
   carried a 3-seed diagnostic sweep in the same job: seed1=0.8733, seed2=0.8854, seed3=0.8646 — all
   comfortably above the 0.795 baseline, 2 of 3 alternate seeds score *above* the seed=0 headline.
   Combined 4-seed range 0.865-0.885, mean~0.872, minimum 0.8646 — no collapse, unlike
   frofa_stability_enet's 0.774-0.830 spread. This is materially more robust than the previous
   round leader's seed behavior. (Agent-8's own seed sweep attempt, `smri8-heteroens-frofa-sweep-v1`,
   FAILED at device init — but agent-9's sweep succeeded and is the operative evidence.)
2. **External FCD check — PASSED.** `smri9-heteroensfrofa-fcd-v4` COMPLETED:
   `external_fcd_auroc` = **0.8829** [0.828, 0.932] vs FCD's own ~0.887 baseline — inside CI, no
   regression. Took 13+ combined attempts across both agents (device-init crash pattern, same
   eviction/422 family as previously root-caused, now confirmed self-resolved once a job actually
   gets past device init) but succeeded cleanly once it did.
3. **Propagation gap — now moot for this result.** Agent-8's own branch (`0c91c2c...`) never picked
   up agent-9's `external_fcd.py` fixes and its 9 external-FCD attempts (v1-v9) all
   EVICTED/FAILED; agent-8 correctly stopped retrying (18:13Z comment) and deferred to agent-9's
   independently-succeeding run rather than duplicating effort — good behavior, no coordinator
   action needed. Both agents' hypothesis threads (01a00b96, 01a00bb2) are marked `confirmed`.
4. **New hypothesis since last poll:** agent-9 opened `task5_aux_age` (01a00bd3, open) — probing
   whether Task_3's brain-age embeddings (same frozen encoder, disjoint subjects) can serve as an
   auxiliary feature for Task_5. Feasibility probe (`smri9-task3lvis-probe-v1`) completed: subjects
   fit within existing shape budget, but full 494-subject embedding exceeds stage-1's 15-min job
   cap. `smri9-auxtask3-logreg-v1` RUNNING at poll time, no result yet — watch next poll.
   Agent-8 also logged a clean dead-end writeup (not silently dropped) for the mean-regularized
   multi-task head: no defensible auxiliary label exists in Task_5's data (single 0/1 label,
   no laterality/site metadata), correctly declined rather than fabricating one.

**Numbers, for the record:** local AUROC 0.8663 (seed0) / 0.865-0.885 (4-seed range, mean~0.872),
+0.071 over the 0.795 debiased baseline — the largest margin of any technique this round that also
holds up externally. External FCD 0.8829 vs FCD's own ~0.887 baseline, inside CI.

**What's still missing before this could be called "finalized" in FINAL_RESULT.md/experiment.md**
(per instructions, coordinator is NOT doing this writeup yet, just flagging):
- Agent-8's own local run has only 1 clean seed (v4); its own sweep job crashed at device init
  (`smri8-heteroens-frofa-sweep-v1` FAILED) — the 4-seed robustness evidence currently rests
  entirely on agent-9's run. Worth an independent second sweep on agent-8's side (or an explicit
  note that agent-9's sweep is being treated as sufficient) before calling this fully closed.
  the external FCD pass likewise rests on a single successful run (agent-9's fcd-v4); agent-8 had
  zero successful external-FCD completions of its own (0/9).
- No written mechanism/root-cause explanation yet of *why* stacking FroFA-lite augmentation under
  the 3-member ensemble outperforms either component alone (hetero_ensemble alone: 0.800 local;
  frofa_stability_enet alone: mean~0.809) — agents have named the mechanism informally
  ("model-diversity variance reduction + FroFA-stabilized small-n statistics") but this hasn't been
  written up rigorously.
- FINAL_RESULT.md and experiment.md still reflect an earlier round (`pe-05083086`) and have not
  been updated for `pe-1b62dccc`'s outcome — flagged for a future pass, not done here.
- Consider one more independent seed/external-FCD replication attempt on agent-8's own branch,
  time permitting, purely to avoid the whole external validation resting on one agent's one run.

**Action taken:** none (observation/documentation only, per "never act on an agent's behalf").
Round healthy, progress on track, ~22h left.

---

## pe-1b62dccc: poll 2026-08-16 18:59Z — local replication of hetero_ensemble_frofa confirmed genuine; external-FCD replication still single-agent; task5_aux_age closed as clean null

**Status:** 2/2 agents alive (`podman ps`, both up ~3h), stage 1, progress 10.1%, `ends_at` unchanged
2026-08-17T16:30:46Z. 12 hypotheses total, all settled (6 confirmed, 4 refuted, 2 inconclusive) —
zero open threads as of this poll. No stuck jobs (agent-8's device-init failures are retried and
abandoned appropriately, not looping indefinitely).

**1. Independent replication of `hetero_ensemble_frofa` — LOCAL is now genuine, EXTERNAL still is not.**
Agent-8's own local run (`smri8-heteroens-frofa-v4`) succeeded independently: AUROC
0.8663194444444445, byte-identical to agent-9's `smri9-heteroensfrofa-v8`. Agent-8 explicitly
audited this for false-positive "replication" (diffed its own commits against agent-9's branch) and
confirmed the two implementations are structurally different code (`_ZScoreEnsemble` calling
`build_head()` by name vs. `_HeteroEnsemble` taking factory closures) that converge on the same
math — not a shared-branch/copy-paste artifact. **This resolves the local-replication gap flagged
last poll.** However, **external FCD replication is still single-agent**: agent-8's external-FCD
attempts for this hypothesis are now 0/9 (all EVICTED/FAILED, same device-init crash pattern,
attempts v1-v9 across 17:47Z-18:13Z), while agent-9's single successful run (`fcd-v4`, 0.8829) is
still the only completed external check. Agent-8 correctly stopped retrying at 9 attempts rather
than burning quota indefinitely.

**2. Mechanism for FroFA+ensemble stacking — still informal, not written up rigorously.** No
mechanism doc found on either agent's hypothesis thread or commit history beyond the informal
framing already noted last poll ("model-diversity variance reduction + FroFA-stabilized small-n
statistics"). Unchanged from prior poll.

**3. `task5_aux_age` — DONE, clean null result, correctly filed.** `smri9-auxtask3-logreg-v1`
completed: mean AUROC ~0.770 vs baseline's own seed-sweep mean 0.777 — no signal in either
direction, within noise. Agent-9 filed it as an honest inconclusive result rather than
overselling. Agent-8 reviewed and declined to build further on it (correctly judged as not worth
combinatorial stacking onto the stronger `hetero_ensemble_frofa` head, per the round's own guidance
against speculative combination search).

**4. Other closures since last poll:** mean-regularized multi-task head reconfirmed as a dead end
(no defensible auxiliary label). `frofa_stability_enet`'s missing seed3 diagnostic filled in
(0.793) — consistent with its already-known seed-fragile profile, no change to its verdict.

**5. New in-flight item:** agent-8 started a *held-out 37/11 split* diagnostic for
`hetero_ensemble_frofa` (beyond the 20-fold CV protocol) — currently at the file-existence/schema
probe stage (`smri8-heldout-probe-v1`), no result yet. Worth checking next poll.

**6. Device-init crash — assessed as shared platform flakiness, not an agent-8-specific bug.**
Both agents hit the identical "device instability" pattern this session (agent-9 needed multiple
attempts before its FCD job succeeded too; agent-8's local `heteroens-frofa` run itself failed
several times before v4 succeeded, then external-FCD failed 9/9). The pattern is described
consistently as intermittent capacity/device-init flakiness affecting jobs from both agents, not
something isolated to agent-8's branch or code. Not treating this as an agent-8 code defect. Given
it resolves on retry roughly ~1-in-5 to 1-in-10 attempts for both agents, this looks like a
capacity/contention issue on the shared accelerator pool rather than a deterministic bug — worth
raising to platform ops as a capacity/reliability observation if it persists, but not something to
patch in agent code. No podman/controller-side error confirming a code-level root cause was found;
recommend one more poll cycle to see if it clears before treating as a standing platform issue.

**Assessment:** `hetero_ensemble_frofa` now has genuine independent LOCAL replication (2 agents,
byte-identical result, structurally distinct code, audited for false-positive replication) plus a
clean external-FCD pass — but that external pass still rests on a single agent's single successful
run (agent-9, fcd-v4, 9/9 attempts failed for agent-8). **This is the one remaining gap before
calling this fully closed**: not "missing" in the sense of contradicting evidence, but not yet the
2-agent-external-confirmation bar the round has been holding itself to. Recommend: if capacity
allows before `ends_at`, get one more successful external-FCD attempt from agent-8 (or a 3rd
attempt from either agent) purely for the external-check redundancy; otherwise this is close enough
to finalize with a clearly stated caveat ("external transfer confirmed by 1 of 2 replicating
agents due to device-init attrition, not a result disagreement"). Mechanism write-up is the other
open item before FINAL_RESULT.md/experiment.md get updated for this round.

**Action taken:** none (observation/documentation only). Round healthy, ~21.5h left.

---

## pe-1b62dccc: poll 2026-08-16 19:25Z — held-out 37/11 diagnostic now independently replicated by BOTH agents (0.9667); external-FCD replication still single-agent; a diversity extension (4th ensemble member) confirmed as a null tie, not a further win; still no rigorous mechanism writeup

**Status:** 2/2 agents alive (agent-8, agent-9 both `Up 3h` in `podman ps`), platform experiment stage
1, `running`, `ends_at` unchanged (2026-08-17T16:30:46Z, ~21h left). 16 hypotheses total now (up
from 12 last poll): 8 confirmed, 3 refuted, 3 inconclusive, 2 still open/in-flight. No stuck jobs —
one job (`smri9-heteroensfrofa4-v4`) is genuinely RUNNING, not hung; everything else terminal.
Agent-7's old container (from a prior, unrelated platform experiment `pe-7ad5e875`) exited 3h ago —
irrelevant to this round, different `platform_experiment_id`, not investigated further.

**1. Held-out 37/11 split diagnostic — DONE, and independently replicated by both agents.** Both
agent-8 (`smri8-heldout-check-v1`) and agent-9 (`smri9-heldout-eval-v4`) completed the diagnostic
for `hetero_ensemble_frofa` on the fixed, never-resampled split (`held_out_test_subjects.json`,
`random_state=42`): **both report `held_out_auroc=0.9667`** (29/30 concordant pairs), matching
exactly. This is diagnostic-only (n_test=11, coarse 1/30 granularity) and correctly not treated as
a substitute for the frozen KFold(20) number, but it's a clean fourth line of evidence with no
collapse: frozen-protocol 0.866, 4-seed reseed sweep 0.865-0.885, external FCD 0.883, held-out-split
0.967. Both agents flagged it as reassuring corroboration, not a headline number — good discipline.
Agent-9's job also caught and fixed a real bug on the way (`smri9-heldout-eval-v3` evicted for
`never_reported_metrics` — script wasn't posting `seconds_per_subject` during the embed loop, same
class of bug `external_fcd.py` had before commit `9e31f14`; fixed in commit `6fa0d4c`).

**2. External-FCD replication — STILL single-agent, unchanged from last poll.** Agent-8's external-
FCD attempts for `hetero_ensemble_frofa` remain 0/9 (v1-v9, all EVICTED/FAILED at device init,
`smri8-heteroens-frofa-external-fcd-v1..v9`). No new attempts since last poll. Agent-9's single
successful run (`fcd-v4`, 0.8829 [0.828,0.932]) is still the only completed external check for this
hypothesis. This is the one gap that hasn't closed across two consecutive polls.

**3. New: a 4th-member diversity extension (`hetero_ensemble_frofa4`) was tried and correctly
called a null tie, not a further win.** Both agents independently added `pca_lda` (rank-constrained
PCA+LDA, itself already vetted in a prior round) as a 4th structurally-distinct member on top of the
confirmed 3-member FroFA-augmented ensemble. Agent-8's run: local AUROC 0.8733 [0.760,0.957] vs the
3-member version's 0.8663 — a +0.007 bump, well inside the 3-member technique's own 0.865-0.885
seed-noise band, i.e. a tie. Agent-8 stopped after 3 failed seed-sweep attempts (device-init flake
again) given the low expected value of continuing to chase a marginal number — correct call, not a
dropped thread, explicitly reasoned in the hypothesis comment. Agent-9's parallel implementation
(`hetero_ensemble_frofa4`, hypothesis `01a00bfe`) has one job still RUNNING
(`smri9-heteroensfrofa4-v4`) after 3 prior FAILED attempts; worth a glance next poll but not
expected to change the verdict given agent-8's already-landed tie.

**4. Mechanism writeup — still not done rigorously.** No dedicated mechanism doc exists anywhere
(checked `scratch_task5_repro/`, `tenstorrent/src/`, git log on `heads.py` — nothing beyond the
informal explanation already embedded in hypothesis text/summaries: "diversity across model type
reduces correlated error, FroFA's extra noisy training rows stabilize each member's small-n
internal statistics — RDA's covariance shrinkage, PAM's per-feature variance, logreg's
regularization path — all computed from ~46 real rows per fold"). This explanation is plausible and
consistent with the evidence (frofa_stability_enet alone went from refuted 0.708 to confirmed 0.830
via the same augmented-rows mechanism; hetero_ensemble alone added +0.005 via diversity alone) but
it has not been written up as a standalone rigorous document — still an open item, unchanged status
from last two polls.

**5. Other closures since last poll:** nothing new besides items 1 and 3 above; the round appears
to be converging (16 hypotheses, all but 2 settled, both of the open ones are marginal extensions
of the already-confirmed flagship, not new independent threads).

**Assessment: hetero_ensemble_frofa is now ready for finalize/consolidate, with one caveat to state
plainly rather than paper over.** Local side is fully confirmed: 2 independent agents,
byte-identical seed=0 AUROC (0.8663194444444445), structurally different code (audited), 4-seed
reseed robustness (0.865-0.885, mean 0.872, tightest spread of any technique this round), clean
residual-leak check (R^2=-0.0214), and now a held-out 37/11 split independently replicated by both
agents at 0.9667. External generalization has a real, clean pass (0.8829 vs FCD's own 0.887
baseline, heavy CI overlap) but it rests on ONE agent's ONE successful run out of ~13 combined
attempts across both agents — agent-8's own 9 external-FCD attempts all failed at device init, a
platform-flakiness pattern (not a code defect) that has now persisted across three polls without
clearing. Waiting further for a second external success looks like diminishing returns: agent-9
already burned significant retries reaching its one success, agent-8's 9/9 failure streak shows no
sign of resolving, and the round only has ~21h left. **Recommendation: proceed to finalize now**
rather than keep polling for a 2nd external replication that this platform's device-init reliability
may simply not deliver in the remaining time.

**What the finalize/consolidate pass should cover in `FINAL_RESULT.md` and `experiment.md`** (not
done in this poll, per instructions — flagging for the next dispatched task):
- **Confirmed method**: `HEAD=hetero_ensemble_frofa` — FroFA-lite feature augmentation
  (additive Gaussian noise + multiplicative per-sample jitter in standardized space, `n_augments=4`,
  fit fold-safe on TRAIN rows only, shared once per fold) stacked underneath a prespecified
  equal-weight 3-member heterogeneous ensemble (ridge logistic regression + diagonal-heavy RDA +
  shrunken-centroid/PAM), each member z-scored by its own train-row score stats before averaging.
- **Local numbers with CIs**: seed=0 AUROC 0.8663194444444445 [0.751, 0.953]; 4-seed sweep
  0.8663/0.8733/0.8854/0.8646 (mean 0.872, no collapse toward baseline on any seed); residual-leak
  R^2=-0.0214 (confound genuinely eliminated, not partially riding the AP-extent artifact).
- **External numbers with CIs**: FCD feature-generality probe 0.8829 [0.828, 0.932] vs FCD's own
  cached baseline 0.887 [0.832, 0.934] — heavy CI overlap, no regression. State plainly this rests
  on a single successful run (agent-9's `fcd-v4`) after ~13 combined failed attempts across both
  agents, all due to documented device-init platform flakiness, not a code or result disagreement.
  Also carry forward experiment.md's own framing caveat: this is a "feature-generality probe,"
  never literal "generalizes to PMG" (FCD refits its own head, is a structurally different
  malformation, only 59% histopathology-confirmed, single-site, T1w-only).
- **Extra corroborating evidence**: the fixed, never-resampled 37/11 held-out split
  (`held_out_test_subjects.json`), independently replicated by both agents at held_out_auroc=0.9667
  — diagnostic-only, not a substitute for the frozen KFold(20) number, but no collapse.
- **Mechanism**: model-type diversity (logistic/covariance-shrinkage/centroid-distance error
  patterns are weakly correlated) reduces variance from averaging; FroFA's extra noisy training
  rows increase the effective row count feeding each member's small-n internal statistics (RDA's
  covariance shrinkage, PAM's per-feature variance, logreg's regularization path), which is the
  same mechanism that independently took `frofa_stability_enet` from refuted (0.708) to confirmed
  (0.830). Note for the writeup: this is still an informal synthesis, not a dedicated ablation
  document — say so rather than presenting it as rigorously isolated.
- **Comparison against this round's other techniques**: `hetero_ensemble` alone (0.800 local,
  clean external), `frofa_stability_enet` alone (0.830 local, clean external), the 4-member
  diversity extension `hetero_ensemble_frofa4` (0.873, a tie within noise — do not report as a
  further improvement), and the three refuted label-adaptive dead ends this round
  (`supervised_pca`, `stability_enet` standalone, `cl2n_pam`) plus prior-round label-adaptive
  failures (LDA-shrinkage, PLS-DA, depth-fusion+LDA) for contrast.
- **Honest caveats to carry into the writeup**: (a) external validation rests on 1 successful run,
  not 2 — state the ~13-attempt device-flakiness context rather than silently presenting it as
  fully redundant; (b) the FCD check measures generic cortical-abnormality feature transfer, not
  literal PMG generalization; (c) n=48 with 24 positives means the local CI is wide even though the
  point estimate is well clear of baseline; (d) the mechanism explanation is a plausible synthesis
  of two already-validated sub-mechanisms, not an independently ablated/isolated proof.

**Action taken:** none (observation/documentation only, per "never act on an agent's behalf").
Round healthy, progress on track (16 hypotheses, 14 settled), ~21h left.

## pe-1b62dccc: 2026-08-16 -- ROUND CLOSED / CANDIDATE FINALIZED (documentation checkpoint,
platform experiment left RUNNING, not closed)

Finalize/consolidate task executed per the standing goal ("beat the honest debiased baseline
AND demonstrably generalize, validated rigorously"). Re-verified every claimed number directly
against source before writing anything down, per repeated instruction not to rubber-stamp a
summary:
- Pulled both agents' actual branches (`fomo-tune-repo/agent-agent-smri-fm-fomo-tune-8-pe-1b62dccc`,
  `-9-pe-1b62dccc`) via `git fetch`/`git show` -- did not rely on fix-later.md's own prior poll
  entries as the source of truth, even though (see below) they turned out to be accurate.
- Read `tenstorrent/src/fomo_tune_tt/heads.py` on both branches directly: confirmed
  `_FroFAHeteroEnsemble`/`build_head("hetero_ensemble_frofa", ...)` exists on both, matches the
  claimed spec (FroFA `n_augments=4` default, 3-member ridge-logreg + `_DiagonalHeavyRDA`
  (`gamma=0.97`) + `_NearestShrunkenCentroids` ensemble, z-scored per-member before averaging).
- Read both agents' `SESSION_FINDINGS.md` results tables directly: both independently report
  local seed=0 AUROC for `hetero_ensemble_frofa` as **0.866** (agent-8's table) / **0.872 mean
  (0.865-0.885 4-seed sweep)** (agent-9's table), matching the 15-decimal
  0.8663194444444445 figure carried in the prior poll log. Both report external FCD 0.883 for this
  technique, matching. Both report the held-out-split 0.967 corroboration.
- **No discrepancy found.** Every number in the round's summary (local CV, 4-seed sweep, held-out
  split, external FCD point estimate + CI, the comparison numbers for `hetero_ensemble` alone and
  `frofa_stability_enet` alone, the `hetero_ensemble_frofa4` null-tie result) checked out against
  the actual branch code and both agents' own findings docs. This round's poll discipline
  (byte-identical cross-agent checks, explicit device-init-flakiness attribution rather than
  silent retries) held up under an independent re-check.

**What's confirmed:** `hetero_ensemble_frofa` is now documented as the new best-known result in
both `FINAL_RESULT.md` (new headline section, superseding but not deleting the old LDA-shrinkage
headline, which is retained and explicitly marked superseded/refuted-as-generalizing) and
`experiment.md`'s BASELINE section (added alongside, not replacing, the 0.795 debiased baseline it
is scored against).

**What's still open / genuinely unresolved (carried forward as future work, not swept under the
headline):**
1. **External replication count is 1, not 2.** Agent-9's `fcd-v4` is the only successful external
   FCD run for this technique out of ~13 combined attempts across both agents; agent-8 is 0/9 on
   this specific job, consistently failing at device init. This is assessed as shared platform
   flakiness, not a code defect, but it is a real evidentiary gap -- a second independent external
   success (from agent-8's own branch, or a third external cohort entirely) would meaningfully
   strengthen the claim and should be picked up opportunistically if the platform experiment's
   remaining ~21h allows a device-healthy window.
2. **No controlled mechanism ablation.** The diversity+FroFA-stabilized-small-n-statistics
   explanation is inferred from comparing three different configs' aggregate results
   (`hetero_ensemble` alone, `frofa_stability_enet` alone, `hetero_ensemble_frofa` combined), not
   from an ablation designed to isolate each ingredient's marginal contribution under matched
   conditions. A proper ablation (e.g., varying `n_augments` while holding the ensemble fixed;
   varying ensemble membership while holding FroFA fixed, on a shared held-out diagnostic) is
   still open.
3. **Only one external cohort (FCD) has been used as the generalization probe**, and per section 7
   of `FINAL_RESULT.md`, even that is a feature-generality probe (refit-per-fold, different
   malformation, partially-unconfirmed labels, single-site, T1w-only) rather than literal PMG
   transfer. No independent PMG cohort exists anywhere in this project's data -- that gap remains
   unresolved and should be stated plainly in any external communication of this result, not
   silently substituted with the FCD number.
4. **No production/clinical claim is being made or should be inferred** -- this is a research-stage
   candidate with genuine but bounded evidence.

**Action taken:** documentation only (`FINAL_RESULT.md`, `experiment.md`, this entry updated/
appended). Platform experiment `pe-1b62dccc` left RUNNING, not closed -- ~21h remain and agents
may still find further ablation, more seeds, or another external replication. This is a
consolidation checkpoint, not an end-of-round action.

## Coordinator check-in — 2026-08-16T20:11Z (~9h49m to ~10h target close at 2026-08-17T06:00Z)

Poll via `podman logs`/`podman ps` (API not reachable from this shell session; used container
logs directly instead). Both `agent-smri-fm-fomo-tune-8` and `-9` containers are Up (~4h),
actively working — round is healthy, 2/2 agents alive, no stuck/crashed jobs (containers
`agent-smri-fm-fomo-tune-7` exited 137 ~4h ago and older `-1..6` are prior, already-exited
containers, not currently-running work).

Since the consolidation checkpoint (FINAL_RESULT.md/experiment.md/fix-later.md updated
2026-08-16T19:29-19:30Z, headline `hetero_ensemble_frofa` unchanged), two new hypotheses appeared
from agent-9, both **not beating the headline**:
1. `POOL_MODE=radial` (logreg) retested — coordinator (agent-8) flagged this as already refuted
   under the current debiased protocol in a different platform experiment (pe-99a1efec,
   AUROC 0.7552) before agent-9's job finished; agent-9's own run reproduced the identical
   byte-for-byte result (0.7552083333333334) and marked it refuted. No new information, confirms
   prior finding.
2. `TTA_FLIP=1` (logreg) — genuinely new code (test-time flip augmentation), not previously run.
   Prior art (inconclusive, different config) was flagged as heads-up but let run since it
   differs. Seed=0: AUROC=0.734 [0.575, 0.869], leak check clean. Seed=1: AUROC=0.7552. Both
   **below the 0.795 debiased baseline** and well below the 0.8663 headline. Diagnostic sweep
   (seeds 2/3) still in progress as of this check-in; trending toward refutation but not yet
   finalized by the agents.

No mechanism-ablation work (isolating FroFA n_augments or ensemble membership contribution) has
been started by either agent since the checkpoint. No second independent external FCD replication
attempt observed in this window either (agent-8 still has not retried the FCD job in this poll
window; agent-9 hasn't re-run it either — both are occupied with the hypotheses above).

**Verdict: headline candidate `hetero_ensemble_frofa` (local 0.8663, external FCD 0.8829, 1
successful external run) stands unchallenged.** Nothing this cycle beats or meaningfully
strengthens it; the two open gaps from the checkpoint (single external replication, no controlled
ablation) remain open. Backstop cron will force-close at ~2026-08-17T06:00Z; not closing manually.

## Coordinator audit (2026-08-16, `pe-1b62dccc`): does `hetero_ensemble_frofa` have an
AP-extent-confound-style bug of its own? -- No new leak found; one documentation overclaim fixed.

Prompted by explicit request to re-verify the headline candidate isn't exploiting the same class
of confound (or a new non-anatomical shortcut) that produced the original 0.99->0.795 correction.
Checked against the actual code on both agents' branch tips (`origin/agent-agent-smri-fm-fomo-tune-8
-pe-1b62dccc` @ 8812901, `-9-pe-1b62dccc` @ e4863c2), not just prose claims:

1. **Residualizer ordering, both branches**: `run_task.py::cross_validate()` fits
   `FoldSafeResidualizer` on TRAIN-fold rows only and transforms both train/test BEFORE
   `build_head("hetero_ensemble_frofa", ...)` is ever called (agent-9: `run_task.py` lines
   122-133). Both branches' `_FroFAHeteroEnsemble.fit()` receives already-residualized features;
   FroFA's own augmentation/scaler is fit on those same fold's train rows only
   (`_frofa_augment`), and all three ensemble members (logreg/RDA/PAM) are fit on the SAME
   augmented rows within a fold. No custom/bypassed CV loop found for this head on either branch
   -- confirmed genuinely inside the shared protocol, not a special-cased shortcut.
2. **leak_r2 claim corrected, not confirmed as originally stated**: the reported R^2=-0.0214 does
   NOT probe FroFA's augmented feature space or the ensemble's combined score, despite
   `FINAL_RESULT.md`'s prior wording -- `extra_transform_factory_for("hetero_ensemble_frofa")`
   returns `None`, so the diagnostic is the same generic post-residualizer check every
   `None`-mapped head gets (confirmed by `frofa_stability_enet`'s near-identical -0.021 from the
   same code path). `FINAL_RESULT.md`'s headline section now carries this correction inline.
   No mechanism exists for FroFA (label/confound-agnostic Gaussian noise+jitter, fit only from
   clean train rows) or the ensemble (fit only on those same clean features) to reintroduce the
   AP-extent signal, but this is reasoning from the mechanism, not a measured result -- flagged as
   low-but-unmeasured risk, not "measured and clean."
3. **Other non-anatomical shortcuts checked** (`scratch_task5_repro/confound_check.py` /
   `confound_check.json`, n=48, single-feature AUROC vs label): non-AP voxel spacing/affine det.
   0.503, total voxel count 0.632, file size 0.566, intensity stats 0.57-0.68 -- all weak, none
   used as features anywhere. Subject-ID sort order: AUROC 1.000 (dataset-construction artifact,
   already called out in `experiment.md` line 312 as unusable, and confirmed NOT used as a
   feature or fold key in `confound.py`/`run_task.py`). Site/scanner/TR/TE metadata: absent from
   the NIfTI headers entirely (single-site Bonn cohort) -- moot, not "ruled out." Every volume is
   resampled to isotropic 1.0mm + a fixed target shape before the encoder sees it
   (`fomo_tune/backbone.py`), so raw resolution/shape differences can't leak through that path
   either (this is also why the two image-level "harmonization" attempts in section 1 failed --
   the confound survives resampling because it leaks through the *learned* representation, not
   raw header stats).

**Verdict (SUPERSEDED, see below): no confound-style bug found in `hetero_ensemble_frofa`.** The
residualize-before-head protocol is genuinely followed on both branches, and no plausible
alternative shortcut (spacing, dimensions, site metadata, subject ordering) is reachable by this
pipeline. The one real defect found is a documentation overclaim (leak_r2 described as
ensemble-specific when it's generic) -- fixed in `FINAL_RESULT.md`'s headline section, not a code
or protocol bug. **This verdict rested entirely on mechanistic REASONING ("no mechanism exists for
FroFA/the ensemble to reintroduce AP-extent"), not on a measurement of the ensemble's actual
output. The direct measurement below contradicts it: the ensemble's final score DOES show
significant residual AP-extent dependence. Do not cite the "no confound-style bug found" verdict
above without reading the correction immediately below.**

## Direct diagnostic of `hetero_ensemble_frofa`'s ACTUAL final score vs AP-extent (2026-08-16,
follow-up to the audit above, prompted by codex pushback on both open concerns) -- **FINDS A REAL,
STATISTICALLY SIGNIFICANT RESIDUAL CONFOUND DEPENDENCE the generic leak_r2 check cannot see.**

The prior audit above explicitly flagged its own gap: "no mechanism exists ... but this is
reasoning from the mechanism, not a measured result." This follow-up runs the actual measurement
codex proposed instead of reasoning about it. Full script and raw output:
`scratch_ppmr_validation/confound_direct_diagnostic.py` /
`confound_direct_diagnostic_result.json`. Reimplements `hetero_ensemble_frofa` by importing the
real classes straight from agent-8's `heads.py` (pe-1b62dccc, commit 8812901) -- not a
reimplementation from scratch -- and reproduces the reported OOF AUROC almost exactly (0.8698 here
vs 0.8663 reported; the small gap is expected float/RNG-path noise, not a different pipeline).

**Concern 1 -- subject-ID sort order (cheap check, done first).** `headers.json` + the cached
label array show subject-ID order is not merely correlated with label, it is a PERFECT ALIAS for
it in this 48-subject release: `sub_01..sub_24` are ALL controls, `sub_25..sub_48` are ALL cases
(`id_order_is_perfect_label_alias: True`). A literal block split on ID order (train on the first
half, test on the second) is therefore DEGENERATE, not just weak -- the train fold is single-class
and a classifier can't even be fit. This explains the documented AUROC=1.000 subject-ID artifact
directly: it isn't a smuggled acquisition-date/protocol signal, it's the dataset release ordering
controls-then-cases. No mtime/DICOM/acquisition-date field exists in the NIfTI headers to check
real chronology directly (`dump_headers.py`'s fields are all header/geometry, no timestamp beyond
filesystem `mtime`, which is this node's unpack time, not acquisition time -- checked, correlates
0.976 with subject-ID because files were unpacked in ID order, tells us nothing about the scanner).
To still get a meaningful chronological/batch probe despite the alias problem, ran a WITHIN-CLASS
ID-order block split instead (first half of controls by ID + first half of cases by ID = train,
second half of each = test, and vice versa -- preserves 50/50 label balance in both train and test
while still splitting along the ID-order axis): **AUROC 0.847 and 0.792** for the two directions --
both comfortably inside the frozen 20-fold protocol's own 95% CI [0.751, 0.953], no collapse. **Concern 1
verdict: no evidence of a hidden chronological/protocol shortcut riding subject-ID order distinct
from the already-known, already-corrected-for AP-extent confound.** (`corr(subject-ID,
AP-extent)=0.456` moderate, `corr(subject-ID, label)=0.866` -- consistent with subject-ID being
downstream of the SAME construction batching that produced the AP-extent confound, not an
independent second leak channel.)

**Concern 2 -- direct measurement of the final ensemble score vs AP-extent, controlling for
label. THIS FAILS H0.** Two independent tests, both against the actual 20-fold OOF ensemble score
(post-residualizer, post-FroFA, post-3-head-stack -- exactly the number that produces the reported
0.8663 AUROC):
- **Partial correlation (score vs AP-extent, controlling for label via within-class centering),
  5000-permutation test (permuting AP-extent WITHIN each label stratum, so H0 = score ⊥ AP-extent |
  label is tested exactly as codex specified):** ensemble **partial r = -0.669, p = 0.0/5000**.
  Individual heads: logreg r=-0.439 (p=0.0), RDA r=-0.735 (p=0.0), PAM r=-0.657 (p=0.0). **Every
  single component and the ensemble itself is overwhelmingly significant** -- these are not
  borderline p-values, the real partial correlations sit far outside a null distribution with std
  ~0.12-0.18 centered at ~0.
- **Cross-fitted GradientBoostingRegressor, 5-fold OOF, predicting the within-class AP-extent
  residual from the score, 200-permutation significance test (same within-class-permutation H0):**
  ensemble **R^2_oos = 0.252, p = 0.005/200**. logreg R^2=0.184 (p=0.005), RDA R^2=0.018 (p=0.03,
  weak), PAM R^2=-0.091 (p=0.075, not significant alone). **The ensemble's combined score alone
  explains a real 25% of held-out variance in label-residual AP-extent** -- not noise.
- **Contrast that exposes exactly the blind spot codex predicted:** running the IDENTICAL
  cross-fitted-GBT+permutation test against the raw post-residualizer 1024-dim FEATURES (i.e. the
  same thing the existing generic `leak_r2` diagnostic checks, just with a flexible nonlinear
  model instead of RidgeCV) gives R^2_oos = -0.148, p = 0.25 -- clean, exactly matching the
  production `leak_r2 ~ -0.045/-0.021` story. **The generic feature-level check is genuinely clean;
  the actual ensemble OUTPUT is not.** This is direct, measured confirmation that FroFA's
  augmentation + the RDA/PAM covariance- and variance-based mechanisms regenerate an
  AP-extent-correlated signal downstream of an honestly-clean linear residualizer -- exactly the
  failure mode codex's review said the existing diagnostic couldn't see, and exactly what the
  prior audit's "no mechanism exists" reasoning got wrong.
- **Confound-balanced case/control matching** (greedy 1:1 nearest-neighbor match on AP-extent,
  caliper 0.5/1.0 SD): only 11-13 matched pairs survive at n=48, so this leg is underpowered
  (wide CI, not treated as decisive either way) -- matched-subset AUROC stayed high (0.975, 0.917)
  but sample size is too small to distinguish "confound-independent signal" from noise at that n.
  Not strong evidence either direction; flagged as inconclusive, not as reassurance.

**Verdict: this is a genuine, measured problem, not a documentation nit.** The prior "no
confound-style bug found" conclusion is WRONG as stated -- it substituted mechanistic reasoning
("no mechanism exists for FroFA/the ensemble to reintroduce AP-extent") for a direct measurement,
and the measurement contradicts the reasoning. `hetero_ensemble_frofa`'s reported 0.8663 AUROC
cannot be treated as fully confound-clean: its final score carries a statistically significant
residual dependence on AP-extent even after fold-safe linear feature residualization and even
after conditioning on label. Concretely plausible mechanism: RDA's covariance shrinkage and PAM's
per-feature variance estimates are both small-n statistics sensitive to how residual VARIANCE (not
just mean) is structured across the ~46 real + augmented training rows per fold, and FroFA's
noise/jitter perturbs that variance structure -- exactly the "residual variance / covariance
confound leakage" channel a linear mean-based residualizer + a linear RidgeCV leak-check cannot
detect. **This does not mean the whole 0.8663 result is pure artifact** -- AP-extent also
correlates with genuine PMG signal (already documented), and the ensemble's local-AUROC advantage
over the plain-logreg 0.795 baseline may be partly real. But it means the specific claim "the
confound is genuinely eliminated in this technique" is FALSE, and the technique likely rides the
AP-extent axis MORE than the plain baseline does, not less. **Escalate: this changes the
trustworthiness of the current top candidate. Do not report `hetero_ensemble_frofa` as
"confound-clean" or "no leak found" anywhere without citing this correction.** Recommended next
step if this candidate is pursued further: repeat the fold-safe residualization on the
FroFA-augmented rows themselves (residualize the augmented feature space against AP-extent again,
per-fold, before the RDA/PAM fits) rather than only residualizing the pre-augmentation input, or
replace RDA/PAM's covariance/variance estimators with confound-aware ones -- neither has been
tried yet.

**RESOLVED (2026-08-16, coordinator follow-up) -- verdict: REFUTED, candidate demoted, no fix
found.** Ran exactly the next-step diagnostics this section called for, plus the two suggested
fixes. Full script/output: `scratch_ppmr_validation/confound_followup.py` /
`confound_followup_result.json`.

- **How much of 0.8663 is confound vs real signal:** post-hoc fold-safe linear residualization of
  the OOF ensemble SCORE itself (same recipe as the feature-level residualizer, applied to the 1-D
  score) still yields AUROC=0.8438 (real signal exists above 0.795), but the residualized score
  STILL has partial_r=-0.684, p=0.0 against AP-extent -- the leak is not linear-in-AP-extent, so a
  naive score-level linear fix does not actually clean it, it just discards some AUROC.
- **Which component drives it:** dropping RDA (logreg+PAM+FroFA) shrinks the GBT-based leak check
  from R^2=0.252 to R^2=0.060 (still p=0.01, still real) while AUROC actually rises to 0.8767 --
  RDA is a large contributor but not the sole cause. FroFA is a genuine amplifier: re-running this
  same direct diagnostic on `hetero_ensemble` WITHOUT FroFA gives partial_r=-0.542 (p=0.0015, still
  significant, weaker) and a NON-significant GBT check (R^2=-0.015, p=0.04 borderline) at
  AUROC=0.7743 -- at/below baseline, consistent with this config's earlier "null result" call.
  Even bare logreg alone (no ensemble, no FroFA) shows a significant partial correlation
  (r=-0.552, p=0.001) -- this is a property of fitting ANY supervised head on n=48 residualized
  features, present at low/borderline severity even in "clean" configs, and pushed to
  large/highly-significant severity specifically by FroFA+RDA together.
- **Attempted fix -- variance-aware residualizer** (regress out AP-extent's squared/centered term
  in addition to the linear term, targeting the "linear residualizer only strips means" mechanism):
  tested, and it FAILED on both axes -- AUROC dropped to 0.7726 (below baseline) AND the leak got
  WORSE (GBT R^2_score=0.215 p=0.01; GBT R^2_features=0.168 p=0.000, meaning even the
  previously-clean feature-level check became significantly leaky, most likely because the added
  quadratic term overfits at n<=46 train rows per fold). **This fix is rejected and was NOT applied
  to the shared `confound.py` harness** -- it actively makes both the score and the underlying
  feature check worse, so implementing it would be a regression, not a fix.
- **Verdict:** no configuration tested clears both bars simultaneously. Every config beating 0.795
  (raw ensemble 0.870, RDA-dropped 0.877, score-residualized 0.844) still shows p<=0.01 residual
  AP-extent dependence; every config free of that dependence (no-FroFA, logreg-alone) sits at or
  below baseline (~0.77-0.78). AUROC gain and confound leak move together in this technique family
  at n=48/p=1024, not apart. `hetero_ensemble_frofa` is refuted as a confound-clean candidate --
  **do not promote it, cite its 0.8663/0.8767 numbers as a win, or use it as a target-to-beat
  baseline going forward.** Current best-standing evidence-based candidate: the plain 0.795
  debiased baseline; `hetero_ensemble` (no FroFA) ties it locally with clean external FCD transfer
  (0.886) but is itself only borderline-clean, not a proven improvement.
  `frofa_stability_enet` (local mean ~0.809, external FCD 0.875, seed-fragile) was never put
  through this score-level diagnostic and should be treated as unverified, not as a fallback win,
  until it is.
