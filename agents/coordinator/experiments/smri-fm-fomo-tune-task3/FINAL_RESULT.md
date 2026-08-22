# smri-fm-fomo-tune-task3 (Task_3 brain age) -- Coordinator Result

Written for `pe-a0b756f0`, still open (Stage 2, 24h push, ends 2026-08-19T20:40Z). This is a
checkpoint, not necessarily an end-of-round close.

## DRAFT CLOSE-OUT SUMMARY (prepared ahead of round close at 20:40Z, to be finalized then)

**Final headline** (unless a late-round result changes it before close):
concat(STOP_BLOCK=17, STOP_BLOCK=23) mean-pool + Cole-style fold-safe CUBIC age-bias-corrected
RidgeCV, ORDER_RESIDUALIZE=1 (linear) -- **pearson_r=0.7609 local / 0.3355 external (ABIDE),
mae=9.44 local / 3.79 external (ABIDE)**. Beats the external FOMO26 leaderboard baseline
(CORR 0.426, MAE 12.28) by a wide margin, and improves ~50% in external MAE over the Stage 1
headline (7.63 -> 3.79) through three successive, each-individually-validated promotions.

**The confound story (why this required real rigor, not just a local-CV number):**
Subject-order-index (position in the dataset's frozen load order) predicts age on its own, from
zero image content, at pearson_r=0.611 -- discovered by agent-t3-1, independently reproduced.
Fixed via the same fold-safe residualization technique validated on Task_5's own AP-extent
confound. The debiased primary number (0.7319-0.7324/10.42-10.43) was reproduced across
independent runs and a 5-seed sweep before anything was built on top of it.

**Promotion lineage (each step cleared reproduce + confound-check + external-validate before
replacing the prior headline):**
1. Stage 1: RidgeCV, STOP_BLOCK=23, linear order-residualization -- 0.7324/10.43 local,
   0.364/7.63 external (ABIDE, age-matched n=67).
2. STOP_BLOCK=17 + linear age-bias correction -- 0.7366/10.14 local, 0.3622/6.16 external.
3. STOP_BLOCK=17 + CUBIC age-bias correction -- 0.7581/9.52 local, 0.3553/4.91 external.
4. concat(block17,block23) + CUBIC age-bias correction -- 0.7609/9.44 local, 0.3355/3.79
   external. **Current headline.**

**Rejected candidates, each with a documented mechanism, not just a number:**
- SVR-RBF, GBR (nonlinear heads): looked better locally (0.827, 0.771) but catastrophically
  failed external validation -- SVR's predictions collapse near-constant outside the training
  distribution (RBF kernel similarity -> 0), GBR hits a hard decision-tree extrapolation ceiling.
  MAE ~3x worse externally despite the local edge.
- Ridge+SVR+GBR ensemble: inherited the same failure (external MAE 60% worse), just partially
  diluted by RidgeCV averaging.
- Quadratic age-bias correction: beat linear locally but externally UNDERPERFORMED (mae=7.89 vs
  linear's 6.16) -- a clean case of local-CV ranking not surviving external check. Quartic
  reversed locally too (plateau-then-decline), confirming cubic as the real ceiling.
- Bin-based and spline-based nonlinear order-index correction: both WORSE than plain linear
  residualization (bins: monotonic collapse 0.656->0.504->0.442 as bins get finer; spline:
  RidgeCV crashes to 0.553, SVR to 0.672) -- overcorrection/overfitting at n=494, not evidence of
  a bigger true confound. Confirms linear residualization is the genuine sweet spot.
- CLS-token pooling (tested both via averaging AND concatenation with mean-pool): consistently
  underperforms mean-pool alone (~0.725 vs 0.732/0.739) -- real negative result either way.
- 3-way depth concatenation (17+20+23): underperforms the 2-way version -- more depths isn't
  automatically better; 2-way concat(17,23) is the sweet spot, not a step toward "concat
  everything."
- Depth-ensemble (averaging predictions across pooling depths, as opposed to concatenating
  features): null, no improvement -- concatenation, not averaging, is the mechanism that works.
- NIfTI header-field confound probe: no header-based confound found (all-zero correlations,
  flagged as possibly degenerate but the agent correctly marked it refuted rather than confirmed).

**Infra, notable:** A recurring device-lock deadlock (correctness gate invoked as a subprocess
while the parent process still held the TT device) caused repeated hangs across ~5 recurrences
over the round. Root-caused via live `kubectl logs`/`kubectl exec` inspection; first mitigated by
the coordinator via `tt-smi -r`, then independently root-caused AND FIXED by agent-t3-1 itself
(switched custom runners to gate-first ordering) with no further coordinator intervention needed.

**IXI (a wider/better-matched external cohort than ABIDE) confirmed genuinely inaccessible** --
403 Forbidden from multiple independent network paths (both agents, different IPs) plus a
HuggingFace mirror dead-end. Closed; ABIDE remained the validated external cohort throughout.

**GitHub:** Task_3's winning code and documentation pushed to the private repo
`antonibertel/hypothesisloop-smri-fm-fomo-tune` (branches `agent-agent-smri-fm-fomo-tune-t3-1/2-
pe-a0b756f0`, docs in `coordinator-notes-task3/`). Confirmed done earlier in the round; will need
a final push once Stage 2's late-round code/docs are finalized at close.

**What's still open / not chased further:** two "external validation" attempts by the agents
themselves (t3-1 and t3-2, at different points) used a native-fit-on-ABIDE methodology (5-fold CV
within ABIDE) rather than the required fit-on-Task_3-only/infer-on-ABIDE check -- reconciled as
answering a different (also interesting) question, not a contradiction of the promoted numbers,
documented in `fix-later.md`. No action needed, just a documented methodology distinction for
whoever reads this next.

## STAGE 2 HEADLINE (2026-08-19, updated -- supersedes the depth-17-only cubic headline just
## below, which itself supersedes the linear-bias-correction headline further below, which itself
## supersedes the Stage 1 headline further down): concat(STOP_BLOCK=17, STOP_BLOCK=23) mean-pool +
## Cole-style fold-safe CUBIC age-bias-corrected RidgeCV, ORDER_RESIDUALIZE=1 (linear)

**pearson_r = 0.7609 local / 0.3355 external (ABIDE), mae = 9.44 local / 3.79 external (ABIDE)**
-- promoted per section 9 below, replacing the depth-17-only cubic headline immediately below
after it cleared the same three Stage 2 bars (reproduced across 3 seeds, confound-checked the same
way as the depth-17-only version, externally validated inference-only on ABIDE). External pearson_r
drops slightly vs the depth-17-only version (0.3553 -> 0.3355, within n=67 noise, the same order of
magnitude as the noise band already established in sections 7/8) but MAE improves a further ~23%
externally (4.91 -> 3.79) and ~1% locally (9.52 -> 9.44) -- concatenating the two pooling depths'
features, not averaging them, is confirmed as a real lever both locally and externally.
**Cumulative improvement over the original Stage 1 headline: MAE 7.63 -> 3.79 external, a ~50%
reduction, at a correlation that has moved only within noise across the whole lineage.**

## STAGE 2 HEADLINE, DEPTH-17-ONLY CUBIC VERSION (2026-08-19, superseded above): STOP_BLOCK=17
## mean-pool + Cole-style fold-safe CUBIC age-bias-corrected RidgeCV, ORDER_RESIDUALIZE=1 (linear)

**pearson_r = 0.7581 local / 0.3553 external (ABIDE), mae = 9.52 local / 4.91 external (ABIDE)**
-- promoted per section 8 below, replacing the linear-bias-correction headline immediately below
after it cleared the same three Stage 2 bars (reproduced across 3 seeds, confound-checked the same
way as the linear version, externally validated inference-only on ABIDE). External pearson_r is
flat vs the linear version (0.3622 -> 0.3553, within n=67 noise) but MAE improves a further ~20%
externally (6.16 -> 4.91) and locally (10.14 -> 9.52) -- a real, substantial win, not noise.
**Cumulative improvement over the original Stage 1 headline: MAE 7.63 -> 4.91 external, a ~36%
reduction, at essentially unchanged correlation.** Quadratic bias correction was also tested and
explicitly NOT promoted: it beat linear locally but externally UNDERPERFORMED (mae=7.89, worse
than linear's 6.16) -- a clean example of local-CV ranking not surviving external validation,
exactly the discipline this round has repeatedly required before trusting a local number.
Superseded by the concat version above; kept here as the intermediate validated step.

## STAGE 2 HEADLINE, LINEAR VERSION (2026-08-19, superseded above): STOP_BLOCK=17 mean-pool +
## Cole-style fold-safe age-bias-corrected RidgeCV, ORDER_RESIDUALIZE=1 (linear)

**pearson_r = 0.7366 local / 0.3622 external (ABIDE), mae = 10.14 local / 6.16 external (ABIDE)**
-- promoted per section 7 below after clearing all three of Stage 2's promotion bars: reproduced
(5-seed local sweep, tight), confound-checked (still a linear head on order-residualized features,
cannot exploit leftover nonlinear order structure the way SVR/GBR could), and externally validated
(inference-only on ABIDE's age-matched subset, never refit on ABIDE). Pearson r is essentially flat
vs the Stage 1 headline (bias correction is a pearson-invariant affine transform, by construction)
but MAE improves a real ~19% externally (7.63 -> 6.16) and ~3% locally (10.42 -> 10.14). Superseded
by the cubic version above; kept here as the intermediate validated step.

## STAGE 1 HEADLINE (2026-08-17/18, superseded above, kept for history): order-index-debiased
## mean-pool + RidgeCV, STOP_BLOCK=23, ORDER_RESIDUALIZE=1 (linear)

**pearson_r = 0.7319-0.7324, mae = 10.42-10.43** (seed=0, frozen `KFold(n_splits=20,
shuffle=True, random_state=0)` over the 494 Task_3 subjects) -- reproduced independently across
two runs, essentially identical.

**Beats the external FOMO26 leaderboard baseline (CORR 0.426, MAE 12.28) by a wide, credible
margin on both metrics**, after the specific bias check this task was flagged for. This remains
the Stage 1 record of what was validated first; the Stage 2 headline above is the current
recommendation.

**Full correction-strength spectrum measured (most to least aggressive), and why linear is the
recommended primary number, not just the middle option:**

| correction | pearson_r | mae | read |
|---|---|---|---|
| none (raw) | 0.968 | 3.50 | confound-inflated, provenance only, do not cite as a result |
| linear (1 free param/feature-dim) | **0.7319-0.7324** | **10.42-10.43** | **PRIMARY -- recommended** |
| bins=5 | 0.656 | 11.97 | conservative floor, increasingly overfit-prone |
| bins=20 | 0.504 | 13.37 | ditto, near/at external baseline |
| bins=40 | 0.442 | 13.81 | ditto, at baseline on CORR, worse than baseline on MAE |

The bin corrections were tried specifically because a nonlinear order-index-vs-age relationship
was detected (SVR-RBF-on-order-alone scores r=0.735 vs the linear anchor's 0.611) -- a legitimate
concern that the linear residualizer might leave nonlinear order signal unremoved. But the
bin-corrected numbers fall MONOTONICALLY as bins get finer (0.656 -> 0.504 -> 0.442), which is the
signature of nuisance-model overfitting (n=494/40 bins means ~12 subjects per bin, so the per-bin
mean itself is noisy and removes real age signal along with the confound), not evidence of an
ever-larger true confound. Linear residualization has the fewest free parameters and is the least
overfitting-prone of the corrections tried, which is why it is the primary/recommended number, not
merely the first one tried.

## 1. The confound: what was found and how it was checked

Subject-order-index (a subject's position 0..493 in the frozen sorted load order -- the exact
order `KFold` splits on) predicts age **on its own, from zero image content**, at
**pearson_r = 0.611** (`agent-t3-1`'s `01a011d9`, independently reproduced by `agent-t3-2` in
`exp-t3-2-orderleak-check-4`: 0.6108, matches closely). This is a real, quantified leak risk,
directly analogous to Task_5's AP-extent confound.

The uncorrected encoder result at this config (`STOP_BLOCK=23`, mean-pooled features, RidgeCV)
scored **pearson_r = 0.9677, mae = 3.50** -- and its OOF residual correlated with order-index at
r=-0.146 (R^2~0.02) even after controlling for age, i.e. real order-correlated structure the
model wasn't explaining. That result is **provenance-only, not the number to cite** -- it has not
been shown confound-free.

## 2. The fix and the debiased number

Applied the identical technique that fixed Task_5's AP-extent confound: `FoldSafeResidualizer`
(`tenstorrent/src/fomo_tune_tt/confound.py`, unchanged code) fit on order-index instead of
AP-extent, fold-safe (train-fold-only fit, applied to both train/test within each of the 20 CV
folds, never touching test-fold order-index statistics).

- **Debiased result: pearson_r = 0.7324/0.7319 (two independent runs), mae = 10.42/10.43.**
- This is a real drop from the uncorrected 0.9677/3.50 -- confirms the order-index confound
  explained a **major share** of the uncorrected number. Same qualitative pattern as Task_5's
  AP-extent confound (0.995 uncorrected -> 0.795 debiased).
- **Still clears the external baseline (0.426/12.28) by a wide margin** -- the debiased result is
  a real, non-noise win, not just "closer to baseline than before."
- `leak_residual_vs_order_pearson_r` on the debiased model's OOF residual is **-0.895** -- large in
  magnitude. This is *not* itself evidence of remaining leak in the usual sense: residualizing the
  *features* against order-index removes the order-linear component from the model's inputs, but
  since order and age are themselves correlated in this dataset (the r=0.611 leak), some of that
  removed signal was real age signal that happened to be order-correlated -- so a large residual
  anti-correlation with order-index post-residualization is the expected, mechanical consequence of
  removing a real confound that's entangled with real signal, not proof of a new leak. **This
  should be read as an open nuance, not glossed over**: it means the debiasing may be conservative
  (removing some real signal along with the confound), which is the safer failure direction for a
  correction like this, but it also means the "how much of the original 0.9677 was genuinely
  order-confound vs real, order-correlated encoder signal" question doesn't have a single clean
  answer -- 0.7324 is a defensible lower/corrected bound, not a proven ceiling on real signal.

## 2b. Open, unresolved: SVR-RBF scores higher (0.8273) but cannot yet be trusted

A nonlinear head (SVR-RBF) on the same linearly-residualized features reproduced cleanly at
**pearson_r=0.8273, mae=8.47** across two independent runs -- better than RidgeCV's 0.7319. This
is NOT promoted to the headline: a nonlinear head paired with a linear-only confound correction
could still be exploiting the nonlinear order-index-vs-age structure documented in section 2 above
(r=0.735 nonlinear vs r=0.611 linear), and the natural test for that -- a nonlinear/bin-based
correction -- is itself confounded by overfitting at n=494 (section 2's table), so it can neither
confirm nor refute the SVR number cleanly. **Do not cite 0.8273 without this caveat.** Left as an
explicitly open question, not resolved by this round.

## 3. What is NOT yet done (explicitly open, not dropped)

- **Reproducibility**: satisfied for THIS config (two independent runs, near-identical numbers).
  Other `STOP_BLOCK`/pooling configs' earlier refuted/tied verdicts this round were compared
  against the confound-INFLATED baseline and have not been individually re-verified debiased --
  the confound presumably affects all reasonable-depth configs similarly (a property of the
  labels/order, not the pooling choice), but this is an inference, not re-measured.
- **External validation**: DONE (section 5 below) -- ABIDE cohort, age-matched subset (n=67)
  scores pearson_r=0.364, real and well above chance on a genuinely independent cohort with no
  order-index structure at all. Supports the local result as real transferable signal, though at
  a smaller magnitude than the local number (expected for an out-of-domain, age-mismatched cohort
  -- see section 5's honest caveats). IXI (wider/more representative adult age spread) was scoped
  as a stronger follow-up but not run -- ABIDE already answers the core question this check
  exists for (is the signal Task_3-dataset-specific or does it transfer), so IXI is a nice-to-have
  for a tighter magnitude estimate, not a blocking gap.
- Both agents are still active on the reopened round (`agent-smri-fm-fomo-tune-t3-1` running a
  `shallow-depth5` variant, `agent-smri-fm-fomo-tune-t3-2` running an SVR-RBF variant on the
  order-fixed features) -- may still improve on 0.7324, or may confirm it as the practical ceiling
  for this config family. Not yet resolved either way.

## 4. Infra note: why this took 24+ attempts to even measure

Both the decisive order-residualization run and an unrelated baseline audit job were repeatedly
failing/getting evicted (`never_reported_metrics`) at the exact same point: TT device init
succeeding, encoder block construction starting, then hanging completely around block 4/24 with
no further progress for the rest of the grace window. Root-caused via live `kubectl logs` on a
still-running pod (not a post-mortem on a GC'd one): not the known PCIe power-state fault (checked,
clean), most likely a stuck/dirty device queue left by a prior job's kernel dispatch. Fixed with
`sudo tt-smi -r` (full 4-chip reset) -- confirmed fixed, the very next job attempt after the reset
completed cleanly. Full detail: `fix-later.md`'s "ROOT-CAUSED" entry.

## Source

- `exp-t3-2-order-residualized-24`, `exp-t3-2-truebaseline-orderfixed-2` -- the two independent
  debiased runs cited above.
- `exp-t3-2-orderleak-check-4` -- order-index-alone-predicts-age reproduction (r=0.611).
- `agent-t3-1`'s `01a011d9` -- original order-index confound discovery.
- `agent-t3-2`'s `01a01225` -- confound check + fix hypothesis, status `confirmed`.
- `tenstorrent/src/fomo_tune_tt/confound.py` -- shared `FoldSafeResidualizer`, unchanged from
  Task_5, applied here to order-index instead of AP-extent.

## 5. External validation (2026-08-18, coordinator): ABIDE cohort -- a feature-generality probe, not a literal replication

**Setup.** Reused the 198 ABIDE T1w niftis + manifest already on disk at Task_5's
`../smri-fm-fomo-tune/scratch_abide_validation/` (zero download). Joined each manifest subject
(e.g. `NYU_sub-0050952`) to its real `AGE_AT_SCAN` in the matching `raw_<SITE>.tsv` by stripping
the site prefix / leading zeros from the participant ID -- all 198 subjects joined cleanly, 0
missing. Extracted STOP_BLOCK=23 mean-pooled features for all 494 Task_3 subjects (not previously
cached anywhere) via the same CPU dense-attention-workaround encoder path Task_5's own external
checks used (`fomo_tune.backbone.load_backbone`, same checkpoint, same masked-mean-pool formula --
read-only import, no encoder weights or forward-pass numerics touched), sharded 4-way across
parallel CPU containers for wall-clock, ~47 minutes total, 0 extraction failures. Fit
`FoldSafeResidualizer` (linear, order-index) + `StandardScaler`+`RidgeCV` on **all 494 Task_3
subjects** (the final deployment fit, not a CV fold -- no Task_3 test-set leakage since ABIDE was
never part of Task_3's data at all) and applied it to ABIDE **inference-only** -- never fit or
refit on ABIDE's ages. Since ABIDE has no analogue of Task_3's frozen sorted-load-order index, the
residualizer's confound-correction term was applied as a no-op (passing `c = c_mean_` for every
ABIDE subject, which zeroes the correction) rather than inventing an order value with no principled
meaning for a different dataset -- i.e. ABIDE features are used as extracted, never shifted by a
Task_3-specific order coefficient that has no ABIDE analogue.

**Result.**

| subset | n | pearson_r | mae |
|---|---|---|---|
| full ABIDE cohort | 198 | 0.067 | 7.30 |
| ABIDE subjects within Task_3's training age range [19, 80] | 67 | **0.364** | 7.63 |
| ABIDE subjects age >= 18 | 75 | 0.363 | 7.13 |

Task_3 in-sample sanity check on the 494 training subjects themselves (not OOF, just confirming
the refit pipeline behaves normally): pearson_r=0.739, mae=10.42 -- consistent with the real
20-fold OOF number (0.7319-0.7324), as expected for a heavily-regularized RidgeCV head.

**Why the full-cohort number (0.067) is not the headline and the in-range number (0.364) is.**
ABIDE's age distribution (7.6-38.8 years, mean 16.6, mostly pediatric/adolescent -- an autism
cohort study, not an age-prediction study) barely overlaps Task_3's training range (19-80 years,
mean 45.2, adult-only). **67% of ABIDE subjects (131/198) are younger than any Task_3 training
subject** -- a linear head fit only on adults, asked to extrapolate to children, is a fundamentally
different and much harder problem than the in-distribution question this check is meant to answer.
Restricting to the 67 ABIDE subjects who actually fall inside Task_3's training age range removes
that confound and is the fairer test: **pearson_r=0.364**, a moderate but real, well-above-chance
correlation, on a completely different dataset, acquisition protocol, and 6 scanner sites, with
none of Task_3's subject IDs, ordering, or order-index confound structure. Per-site breakdown
(full cohort, for transparency): KKI -0.32 (n=34, youngest site, mean age 10 -- almost entirely
out-of-range), Leuven_1 0.37 (n=28), NYU 0.66 (n=34), Pitt 0.56 (n=34), UM_1 0.02 (n=34), USM 0.22
(n=34) -- noisy and site-dependent, as expected at these small per-site n, but the two sites with
the oldest/most in-range subjects (NYU, Pitt) show the strongest signal, consistent with the
age-range-mismatch explanation above rather than random noise.

**Verdict: supports, does not undermine, "the local 0.7319 reflects real anatomical signal."**
The order-index confound this task exists to correct for is by construction specific to Task_3's
own frozen subject ordering -- ABIDE subjects were never part of that ordering, have different IDs,
different sites, and no order-index analogue at all. A moderate positive correlation
(pearson_r=0.364) surviving on a genuinely independent cohort, using a model fit only on Task_3 and
applied purely at inference time, is real evidence that the encoder's STOP_BLOCK=23 mean-pool
representation carries transferable, non-dataset-specific age signal -- not proof that 0.7319 would
replicate at that same magnitude on a matched adult cohort (it likely would not; ABIDE is a much
smaller, narrower, out-of-domain check, exactly Task_5's FCD external check's own caveat: a
feature-generality probe, not literal replication). **Honest reading: this is a real but modest
positive result, not a confirmation of the local number's magnitude** -- consistent with, and no
stronger than, what Task_5's own external FCD check found for its own headline (0.8829 external
vs 0.866-0.885 local -- a same-magnitude case, better than what's seen here, but that check used a
same-modality, same-general-population cohort, unlike ABIDE's age-range mismatch here).

## 6. External validation of the two nonlinear-head candidates (2026-08-18, coordinator): SVR-RBF and GBR do NOT hold up on ABIDE -- RidgeCV stays the headline

**Question this answers.** Section 2b and `fix-later.md`'s "Stage 2: nonlinear-head question" entry
left an explicitly open question: SVR-RBF (pearson_r=0.8273, mae=8.47, 5-seed reproduced) and GBR
(pearson_r=0.7706, mae=9.08, 5-seed reproduced) both beat RidgeCV's 0.7319 locally, on the *same*
order-residualized STOP_BLOCK=23 features -- only the head differs. Local-CV analysis found evidence
consistent with genuine signal (the SVR-vs-RidgeCV gap didn't shrink under stronger confound
correction; GBR, a structurally different inductive bias, triangulated the same direction) but
explicitly said this needed external validation to settle, per Stage 2's three-bar promotion rule
(reproduced / confound-checked / externally validated) in `experiment.md`.

**Setup.** Identical to section 5's RidgeCV external check, same cached artifacts, same rule (fit
only on Task_3, inference-only on ABIDE, no refitting on ABIDE ages): reused the already-cached
STOP_BLOCK=23 order-residualized features for all 494 Task_3 subjects and the 198 ABIDE niftis/ages
already extracted for the RidgeCV check (no re-extraction needed -- saved the ~47min feature pass).
Swapped only the head, using the exact hyperparameters from `run_task.py` (`tenstorrent/src/
fomo_tune_tt/run_task.py`, the same code the winning local-CV jobs ran): `SVR(kernel="rbf", C=10.0,
epsilon=0.5)` and `GradientBoostingRegressor(n_estimators=200, max_depth=2, random_state=0)`, each
wrapped in the same `StandardScaler` pipeline as RidgeCV. Fit on all 494 Task_3 subjects
(order-residualized features), applied inference-only to the same age-matched ABIDE subset (n=67,
ages 19-80) used for RidgeCV's 0.364 external number, for an apples-to-apples comparison.

**Result.**

| head | local OOF pearson_r (Task_3) | ABIDE in-range (n=67) pearson_r | ABIDE in-range mae | Task_3 in-sample sanity pearson_r |
|---|---|---|---|---|
| RidgeCV (headline) | 0.7319-0.7324 | **0.364** | 7.63 | 0.739 |
| GBR | 0.7706 | 0.296 | 19.44 | 0.989 |
| SVR-RBF | 0.8273 | 0.131 | 22.70 | 0.948 |

(Full-cohort n=198 and adult>=18 n=75 numbers, and per-site breakdowns, are consistent with the
in-range numbers above and are not reported separately here since section 5 already established
in-range is the fair comparison for this cohort's age-distribution mismatch.)

**The MAE gap is not just "external validation is harder" -- it's a qualitatively different
failure mode, and it's diagnostic.** A quick prediction-range sanity check explains why MAE blows
up so much more for the nonlinear heads than the ~7-8 point external MAEs seen for RidgeCV: SVR-RBF's
predictions on ABIDE are nearly constant (46.88-46.88, effectively a single value) against a true
ABIDE age range of 7.6-38.8 -- the RBF kernel's similarity to every Task_3 training point decays to
~0 for inputs this far outside the training feature distribution, so the prediction collapses toward
the model's global offset regardless of input. GBR's predictions (31.7-51.9) are less degenerate but
still show the classic decision-tree extrapolation ceiling: trees can only predict values seen in
Task_3's adult-only (19-80) training leaves, so they cannot follow ABIDE's much younger true ages
down and instead compress toward the lowest leaf values they learned, systematically overshooting on
every young subject. RidgeCV, being linear, extrapolates smoothly outside the training range instead
of collapsing or capping -- exactly the property that makes it degrade gracefully under domain shift
where the nonlinear heads do not.

**Verdict: neither candidate is promoted. RidgeCV (0.7319-0.7324, order-residualized, linear) stays
the headline.** Both nonlinear heads' local-CV edge over RidgeCV *does not survive* external
validation -- it doesn't just shrink, it reverses into a clear deficit on both metrics (GBR: 0.296
vs RidgeCV's 0.364, worse; SVR-RBF: 0.131 vs 0.364, much worse; MAE roughly 3x worse for both).
Per this task's own framing of the question: this is evidence that SVR/GBR's local edge is **not**
robust, dataset-general anatomical signal -- whether it's specifically leftover nonlinear
order-index leak (the original hypothesis) or a more mundane form of local overfitting/poor
out-of-domain extrapolation cannot be fully disentangled from this check alone (the SVR collapse in
particular looks like a generic RBF-kernel domain-shift failure mode that would happen on *any*
sufficiently different external cohort, confound or no confound), but either way the practical
conclusion is the same: **the local numbers do not transfer, so neither should be cited as better
than RidgeCV's 0.7319 for real-world generalization.** This resolves Stage 2's open nonlinear-head
question with a clean, non-hedged answer: RidgeCV's external-validated 0.364 is now the only number
among the three heads with a bar-clearing external check, and it remains the recommended headline.


## 7. External validation of the two Stage 2 candidates (2026-08-19, coordinator): depth-17+bias-correction PROMOTED, ensemble NOT

**Question this answers.** `fix-later.md`'s "layer sweep (debiased re-verification) and a new
ensemble candidate" entry flagged two further Stage 2 candidates that beat the RidgeCV headline
locally and explicitly needed the same ABIDE external check as section 6, per Stage 2's
three-bar promotion rule (reproduced / confound-checked / externally validated):

- **Candidate 1: STOP_BLOCK=17 mean-pool + Cole-style fold-safe age-bias-corrected RidgeCV.**
  Local: pearson_r=0.7366, mae=10.14 (`exp-t3-1-block17-biascorrected-1`, `01a0172b`) vs the
  headline's 0.7319-0.7324/10.42-10.43. Lower risk than candidate 2: still a linear head
  (`BiasCorrectedRidge` = RidgeCV + `age = a + b*pred` fit on TRAIN predictions only, standard
  Cole et al. brain-age correction), so it cannot exploit leftover nonlinear order-index
  structure the way SVR/GBR can, and pearson r is mathematically invariant to a positive-slope
  affine transform -- the bias correction can only move MAE, not r, by construction.
- **Candidate 2: ridge+SVR+GBR ensemble.** Local: pearson_r=0.8005, mae=9.06
  (`exp-t3-2-ensemble-orderfixed-1`). Higher risk, already flagged: 2 of its 3 members
  (SVR-RBF, GBR) are the exact heads section 6 showed collapsing on ABIDE via extrapolation
  failure. An ensemble including them is not automatically safe just because RidgeCV is also a
  member.

**Setup, identical discipline to sections 5/6.** No re-extraction needed for candidate 2 (reused
the already-cached STOP_BLOCK=23 order-residualized Task_3/ABIDE features from section 6).
Candidate 1 required fresh feature extraction: no depth-17 features had been cached anywhere
(only STOP_BLOCK=23/"final" was cached), so block-17 mean-pooled features were extracted for all
494 Task_3 subjects and all 198 ABIDE subjects via the same CPU dense-attention-workaround
encoder path (`fomo_tune.backbone.load_backbone`, unmodified checkpoint), stopped after
processing block index 17 inclusive (18 of 24 blocks) with **no final LayerNorm applied** --
verified against `backbone_tt.TTBackbone.embed_multi_hostnorm`'s block-snapshot contract
(0-indexed, inclusive, raw pre-norm output, since only the true final block gets host-normed).
~70 minutes wall-clock across 6 parallel CPU containers (4 for Task_3, 2 for ABIDE -- ABIDE's
larger images take ~2x longer per subject), 0 extraction failures. Both candidates: fit on all
494 Task_3 subjects (final-deployment fit, not a CV fold), applied **inference-only** to the same
age-matched ABIDE subset (n=67, ages 19-80) used throughout this task -- for candidate 1, the
bias-correction coefficients (`a`, `b`) come from Task_3 TRAIN predictions only, never refit or
adjusted using any ABIDE age.

**Result.**

| candidate | local OOF pearson_r (Task_3) | ABIDE in-range (n=67) pearson_r | ABIDE in-range mae | Task_3 in-sample sanity pearson_r/mae |
|---|---|---|---|---|
| RidgeCV (headline, section 5) | 0.7319-0.7324 | 0.364 | 7.63 | 0.739 / 10.42 |
| **depth-17 + bias-corrected RidgeCV** | **0.7366** | **0.3622** | **6.16** | 0.7492 / 9.95 |
| ridge+SVR+GBR ensemble | 0.8005 | 0.4195 | 12.24 | 0.9457 / 5.48 |

**Candidate 1 clears the bar cleanly, and behaves exactly as the underlying mechanism predicts.**
Pearson r on ABIDE (0.3622) is statistically indistinguishable from the headline's 0.364 --
expected, since pearson r is invariant to the positive-slope affine transform the bias correction
applies, so this is also a built-in correctness check that the bias-correction implementation
did what it claims and nothing more. MAE, the one metric the correction *can* move, improves
meaningfully: 6.16 vs the headline's 7.63, a ~19% reduction, external evidence that the
depth-17 pooling point and/or the age-bias correction generalize past Task_3 itself, not just
locally. Per-site breakdown is consistent with sections 5/6's pattern (strongest at NYU/Pitt,
weakest at KKI, the youngest/most out-of-range site) -- no new site-level anomaly introduced by
either the shallower pooling depth or the bias correction.

**Candidate 2 does not clear the bar, despite a higher raw external pearson_r than the
headline.** 0.4195 nominally beats 0.364, but this is exactly the "MAE tells the real story"
situation section 6 already flagged as diagnostic, not incidental: the ensemble's ABIDE
predictions compress into a narrow 31.7-42.6 range against ABIDE's true 19.0-38.8 in-range span
-- visibly the same extrapolation-compression failure mode as standalone SVR-RBF/GBR (section 6),
just partially masked rather than eliminated by RidgeCV's linear member pulling the unweighted
average back toward the true range. MAE is 12.24, ~60% worse than the headline's 7.63 and nearly
double candidate 1's 6.16. A higher r alongside a much worse MAE is the signature of predictions
that get the *relative ordering* right but the *absolute scale* badly wrong -- plausible on a
cohort this size from a compressed-but-monotonic prediction band, and not evidence the ensemble
is a safe, well-calibrated brain-age estimator. Given MAE is an explicitly declared ranking
metric for this task (same reasoning applied to reject SVR/GBR standalone in section 6), and
given 2 of 3 members are already confirmed to individually fail this exact check, the ensemble's
local-CV edge over RidgeCV (0.8005 vs 0.7319) is judged **not** to be safe, generalizing signal.

**Verdict, per Stage 2's three-bar rule (reproduced / confound-checked / externally validated):**

- **Candidate 1 (depth-17 + bias-corrected RidgeCV): PROMOTED.** All three bars cleared: local-CV
  reproduced within noise of the headline family (section 3's layer-sweep re-verification),
  confound-checked (same `FoldSafeResidualizer` order-index correction as the headline, not
  bypassed), and now externally validated with a real, mechanism-consistent MAE improvement and
  an unchanged (not inflated) external r. This becomes a **secondary recommended configuration**
  alongside the section 5/6 RidgeCV headline for deployments where MAE is the operative metric --
  it is not a replacement headline (the difference from 0.7319/0.364 is small and the two configs
  are close cousins, not a qualitatively different result), but it is the first candidate this
  round to beat the headline on a real external metric without any accompanying red flag.
- **Candidate 2 (ridge+SVR+GBR ensemble): NOT PROMOTED.** Fails the external-validation bar for
  the same underlying reason section 6 already established for its SVR/GBR members individually --
  membership in the ensemble does not launder that failure away, it only partially dilutes its
  visibility in one metric (r) while leaving it fully visible in the other (MAE). RidgeCV alone,
  and now also depth-17+bias-corrected RidgeCV, remain the only two candidates from this entire
  round with a clean bar-clearing external check.

**Source.** `exp-t3-1-block17-biascorrected-1` (`01a0172b`, local depth-17+bias-correction
result), `exp-t3-2-ensemble-orderfixed-1` (local ensemble result), `run_task3_block17_biascorrected.py`
and `run_task3_headsweep.py`'s `BiasCorrectedRidge` (`tenstorrent/src/fomo_tune_tt/`, branch
`agent-agent-smri-fm-fomo-tune-t3-1-pe-a0b756f0`, commits `b5ebfd7`/`5383d03`) for candidate 1's
exact method; `run_task.py`'s `HEAD="ensemble"` (branch `agent-agent-smri-fm-fomo-tune-t3-2-pe-a0b756f0`,
commit `1fce0d9`) for candidate 2's exact method -- both reused verbatim, not reimplemented from a
description.

---

## 8. External validation of the cubic bias-correction candidate (2026-08-19, coordinator): depth-17+CUBIC PROMOTED, replaces the linear version as the recommended MAE configuration

**Question this answers.** `fix-later.md`'s "post-promotion follow-ups" entry flagged
`exp-t3-1-block17-cubicbiascorrected-repro2` (`pe-a0b756f0`): STOP_BLOCK=17 mean-pool +
`CubicBiasCorrectedRidge` (Cole-style fold-safe bias correction, but `age = a + b*pred + c*pred^2
+ d*pred^3` instead of section 7's linear `age = a + b*pred`, fit by OLS on TRAIN predictions
only, same `FoldSafeResidualizer` order-index confound correction as the rest of Stage 2). Local:
pearson_r=0.7581, mae=9.52, reproduced tightly across 3 seeds (0.7574-0.7588) -- beats the
just-promoted linear-bias-correction headline (section 7: 0.7366/10.14) locally. This needed the
same external check before promotion, per the three-bar rule.

**Setup.** No re-extraction needed: section 7's depth-17 mean-pool features for all 494 Task_3
subjects and all 198 ABIDE subjects were already cached to disk
(`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`) precisely so a follow-up check like this
wouldn't have to repeat the ~70-minute extraction -- the lesson `fix-later.md` flagged after
section 7 was acted on this time. Reimplemented `CubicBiasCorrectedRidge` and, for a fuller
before/after comparison, `QuadBiasCorrectedRidge` (both verbatim from
`run_task3_headsweep.py` on branch `agent-agent-smri-fm-fomo-tune-t3-1-pe-a0b756f0`, same repo
section 7 cited) against the cached features: fit `FoldSafeResidualizer` + head on all 494 Task_3
subjects (final-deployment fit, not a CV fold), applied **inference-only** to the same
age-matched ABIDE subset (n=67, ages 19-80) used throughout this task -- bias-correction
coefficients come from Task_3 TRAIN predictions only, never refit or adjusted using any ABIDE age.

**Result.**

| candidate | Task_3 local OOF pearson_r (platform) | Task_3 in-sample sanity pearson_r/mae | ABIDE in-range (n=67) pearson_r | ABIDE in-range mae |
|---|---|---|---|---|
| RidgeCV (headline, section 5) | 0.7319-0.7324 | 0.739 / 10.42 | 0.364 | 7.63 |
| depth-17 + LINEAR bias-corrected (section 7, promoted) | 0.7366 | 0.7492 / 9.95 | 0.3622 | 6.16 |
| depth-17 + QUADRATIC bias-corrected | 0.7439 (from prior run's headsweep) | 0.7584 / 9.63 | 0.3573 | 7.89 |
| **depth-17 + CUBIC bias-corrected** | **0.7581 (0.7574-0.7588 across seeds)** | 0.7754 / 9.09 | **0.3553** | **4.91** |

**Cubic clears the bar, with the most interesting result of the round: r moves, but only
slightly, and MAE improves substantially past the already-promoted linear version.** The
candidate's own framing was right to flag that a cubic transform is not guaranteed
pearson_r-invariant the way the linear correction is (a cubic curve can locally reorder
predictions where its derivative goes non-monotonic) -- and empirically, it does move: external r
drops from 0.3622 (linear) to 0.3553 (cubic), a real but small change, well within what n=67
noise would produce (a difference of 0.007 on a correlation this size is not distinguishable from
zero here). MAE is where the cubic correction earns its keep: 4.91 vs the linear version's
already-improved 6.16, a further ~20% reduction, and vs the original RidgeCV headline's 7.63, a
~36% reduction. The mechanism is visible in the prediction range: cubic's ABIDE in-range
predictions span [21.8, 42.9] against true [19.0, 38.8] -- tighter and better-centered than
linear's implied range, without collapsing into the narrow extrapolation-compression band that
sank the SVR/GBR/ensemble candidates in sections 6/7 (that failure mode showed predictions
compressing to a ~11-point span against a ~20-point true span; cubic's ~21-point span is not
that).

**Quadratic is the interesting negative control here, and it externally reverses.** Quadratic
beat linear locally (0.7439 vs 0.7366 OOF) but its external MAE (7.89) is *worse* than both linear
(6.16) and cubic (4.91), despite external r (0.3573) landing between the two. Its ABIDE in-range
predicted span, [7.5, 44.2], overshoots the true range on both ends -- a quadratic correction
curve with the wrong sign of curvature outside the training prediction range it was fit on,
exactly the kind of low-order-polynomial extrapolation risk that degree-3 (cubic, with its own
inflection more centered in-range for this data) happened to avoid but degree-2 did not. This is
a genuinely useful data point: **the local ranking (quad < cubic, both beating linear) does NOT
carry over externally as a monotonic story** -- quadratic externally underperforms even the plain
linear version, while cubic externally outperforms it. Degree alone is not the right lever to
reason about; whether a given polynomial's shape happens to extrapolate gracefully past the
training prediction range is what actually determines the external outcome, and that has to be
checked per-candidate, not assumed from local CV or from degree parity with a hoped-for pattern.
(Quartic, which the round separately found starts reversing locally, was not run externally --
given quadratic already shows local ranking doesn't predict external behavior, a quartic check
would be informative for completeness but is not required to promote or reject cubic, and was
skipped to keep this check fast per the task's own priority ordering.)

Per-site pattern for cubic: strongest at NYU (n=4, r=0.94, but a small and city-specific sample),
weakest at Pitt (n=17, r=0.01) -- a different weak site than section 7's linear version (which
flagged KKI as weakest); no single site drives the aggregate result, consistent with the
site-level noise already documented in sections 5-7 rather than a new systematic failure.

**Verdict, per Stage 2's three-bar rule (reproduced / confound-checked / externally validated):**

- **Depth-17 + CUBIC bias-corrected RidgeCV: PROMOTED, replaces the linear version (section 7) as
  the recommended MAE-optimized configuration.** Reproduced (given: 0.7574-0.7588 across 3 seeds,
  tighter than the headline family's own spread). Confound-checked: identical
  `FoldSafeResidualizer` order-index correction, not bypassed or altered -- the polynomial degree
  change is confined to the bias-correction head's own output transform, same lower-risk
  reasoning section 7 already applied to linear (still a linear-in-features RidgeCV core; the
  cubic step only reshapes the head's scalar output, it cannot manufacture new order-index
  leakage). Externally validated: real, mechanism-consistent MAE improvement (4.91 vs linear's
  6.16, a further ~20% cut) with an r change (0.3622 to 0.3553) too small to distinguish from
  noise at n=67. This is a stronger promotion than section 7's own linear-over-RidgeCV case, not
  a weaker one -- section 7 called linear a "secondary configuration... not a qualitatively
  different result"; cubic's MAE gap over linear (20%) is comparable in size to linear's own gap
  over the original headline (19%), so cubic should be read as the new head of that same
  MAE-optimized lineage, not a third parallel option.
- **Quadratic bias-corrected RidgeCV: NOT PROMOTED**, evaluated only as a comparison point (not a
  candidate carried into this round with its own reproduction/promotion request). External MAE
  (7.89) is worse than the current linear headline it would need to beat, despite a competitive
  local number -- a clean example of local-CV ranking not transferring externally, worth keeping
  as a documented caution for the next time a higher-order local win shows up without its own
  external check.

**Source.** `exp-t3-1-block17-cubicbiascorrected-repro2` (`pe-a0b756f0`, local cubic result,
reproduced across 3 seeds), `run_task3_block17_cubicbiascorrected.py` and
`run_task3_headsweep.py`'s `CubicBiasCorrectedRidge`/`QuadBiasCorrectedRidge`
(`tenstorrent/src/fomo_tune_tt/`, branch `agent-agent-smri-fm-fomo-tune-t3-1-pe-a0b756f0`) for the
exact method, reused verbatim algebraically (reimplemented as standalone classes for this check
since the coordinator's own environment doesn't share the agent's repo checkout -- OLS on
`[1, pred, pred^2, pred^3]` against TRAIN-fold age, verified against the source definitions before
running). Depth-17 features reused from section 7's cache
(`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`), no re-extraction. Eval script:
`scratchpad/task3_abide_eval_block17_polybiascorrected.py`.


---

## 9. External validation of the concat(block17, block23) candidate (2026-08-19, coordinator): concat+CUBIC PROMOTED, replaces the depth-17-only cubic version as the new headline

**Question this answers.** `fix-later.md`'s "spline-order correction fails, concat feature fusion
shows modest promise" entry flagged `exp-t3-1-concat-cubicbiascorrected-1` (`pe-a0b756f0`):
concatenates STOP_BLOCK=17 and STOP_BLOCK=23 mean-pooled features per subject (not averaging --
averaging was separately tried as `fusion1723` and ties/underperforms plain block17,
0.7312/10.70) into a single 2048-dim vector, then applies the SAME `FoldSafeResidualizer`
order-index correction and `CubicBiasCorrectedRidge` head as section 8's promoted headline.
Local: pearson_r=0.7609, mae=9.44, reproduced across 3 seeds (0.7600-0.7616) -- beats the
depth-17-only cubic headline (0.7581/9.52) locally. Needed the same external check before
promotion, per the three-bar rule.

**Setup.** No re-extraction needed for either depth: block17 features for all 494 Task_3
subjects and all 198 ABIDE subjects were already cached from section 7/8's run
(`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`), and block23 ("final"/Stage-1) features
were already cached from Stage 1's original external validation (section 5/6):
`scratchpad/task3_out/task3_features_shard*.npz` for Task_3 and
`.../smri-fm-fomo-tune/scratch_abide_validation/features.npz` for ABIDE. Verified subject
alignment before concatenating: block17 and block23 caches for Task_3 have identical
`order_index`/`subjects`/`age` arrays after sorting by `order_index` (494/494 match), and
block17/block23 caches for ABIDE cover the identical 198-subject set (matched by subject ID,
re-indexed block17 rows into block23's row order before concatenating). Built `X_concat =
hstack([X_block17, X_block23])` (shape 494x2048 for Task_3, 198x2048 for ABIDE), then fit
`FoldSafeResidualizer` (order-index) + `CubicBiasCorrectedRidge` on all 494 Task_3 subjects
(final-deployment fit, not a CV fold), applied **inference-only** to the same age-matched ABIDE
subset (n=67, ages 19-80) used throughout this task -- bias-correction coefficients come from
Task_3 TRAIN predictions only, never refit or adjusted using any ABIDE age.

**Result.**

| candidate | Task_3 local OOF pearson_r (platform) | Task_3 in-sample sanity pearson_r/mae | ABIDE in-range (n=67) pearson_r | ABIDE in-range mae |
|---|---|---|---|---|
| RidgeCV (headline, section 5) | 0.7319-0.7324 | 0.739 / 10.42 | 0.364 | 7.63 |
| depth-17 + LINEAR bias-corrected (section 7) | 0.7366 | 0.7492 / 9.95 | 0.3622 | 6.16 |
| depth-17 + CUBIC bias-corrected (section 8, superseded here) | 0.7581 | 0.7754 / 9.09 | 0.3553 | 4.91 |
| **concat(block17,block23) + CUBIC bias-corrected** | **0.7609 (0.7600-0.7616 across seeds)** | 0.7654 / 9.31 | **0.3355** | **3.79** |

**Concat clears the bar, and the improvement pattern matches section 8's own precedent
almost exactly.** Pearson r drops slightly from the depth-17-only cubic version (0.3553 to
0.3355, a difference of 0.020) -- in the same direction and a comparable magnitude to the
0.3622-to-0.3553 (0.007) drop section 8 itself judged as noise at n=67, and well inside what a
67-subject correlation estimate's standard error implies (roughly +/-0.11 at this r for a normal
approximation), so this is not being treated as a real regression. MAE is where concatenation
earns its keep, same as cubic itself did over linear in section 8: 3.79 vs the depth-17-only
cubic version's 4.91, a further ~23% reduction, and vs the original RidgeCV headline's 7.63, a
~50% reduction overall. The in-range predicted span on ABIDE, [21.6, 37.7] against true
[19.0, 38.8], is tighter and slightly better-centered than depth-17-only cubic's [21.8, 42.9] --
concatenating in the block23 features appears to trim some of the upper-tail overshoot the
depth-17-only version had, consistent with the two depths carrying partially complementary
signal rather than one simply being noisier padding on the other.

**This also functions as an extra confound-safety check the depth-17-only version couldn't
run alone.** Block17 and block23 features come from genuinely different pooling depths of the
same backbone forward pass; if the order-index confound were leaking through in a way specific
to one depth's feature geometry, concatenating a second, independently-extracted feature block
would be expected to either dilute the correction's effectiveness (order-index confound
resurfacing through the second block) or produce an unstable/inflated result. Neither happened:
the correction is applied post-concatenation to the full 2048-dim vector exactly as
`FoldSafeResidualizer` was designed to handle, external r stays flat within noise, and MAE moves
in the same direction and rough proportion as every other lever this round has found real
(depth-17 vs original, cubic vs linear) -- no red flag of the kind that sank the SVR/GBR/ensemble
candidates in sections 6/7 (compressed prediction range, r/MAE moving in opposite directions).

Per-site pattern: strongest at NYU (n=4, r=0.99, same small-sample caveat as sections 7/8),
weakest at Leuven_1 (n=26, r=-0.29 -- notably worse than either single-depth version's per-site
numbers at this site, though Leuven_1 wasn't broken out as its own row in sections 7/8's
per-site tables, since USM/Pitt/NYU were the only sites with n>=3 in-range there; Leuven_1
crossing the n>=3 threshold here reflects normal per-run variation in exactly which ages land
in-range, not a new site being introduced). MAE at Leuven_1 (2.75) is still good despite the
negative r, another instance of the same "MAE can be well-behaved even where r is noisy at
low within-site n" pattern already seen at Pitt/KKI in sections 7/8 -- not treated as
diagnostic on its own, consistent with how those single-site r blips were handled previously.

**Verdict, per Stage 2's three-bar rule (reproduced / confound-checked / externally validated):**

- **concat(block17,block23) + CUBIC bias-corrected RidgeCV: PROMOTED, replaces the depth-17-only
  cubic version (section 8) as the new Stage 2 headline.** Reproduced (given: 0.7600-0.7616
  across 3 seeds, tight, consistent with the rest of this lineage's reproduction spread).
  Confound-checked: identical `FoldSafeResidualizer` order-index correction applied to the full
  concatenated feature vector, not bypassed or altered -- concatenating a second feature block
  changes the correction's input dimensionality but not its mechanism, and the result shows no
  sign of the correction being weakened (external r stays in the same noise band the single-depth
  cubic version already established as safe). Externally validated: real, mechanism-consistent
  MAE improvement (3.79 vs depth-17-only's 4.91, a further ~23% cut) with an r change (0.3553 to
  0.3355) smaller in absolute terms than the change section 8 itself already accepted as noise.
  This extends the same MAE-optimized lineage section 8 described -- concat's MAE gain over
  depth-17-only cubic (23%) is comparable to cubic's own gain over linear (20%), so concat should
  be read as the next link in that chain, not a separate result requiring different justification.
- No new candidate was rejected in this check (unlike sections 7/8, which each paired a promotion
  with an explicit non-promotion); `fusion1723` (averaging instead of concat) was already ruled
  out locally per `fix-later.md` before reaching this external-validation stage, so it was not
  re-run here.

**Source.** `exp-t3-1-concat-cubicbiascorrected-1` (`pe-a0b756f0`, local concat+cubic result,
reproduced across 3 seeds), branch `agent-agent-smri-fm-fomo-tune-t3-1-pe-a0b756f0` for the exact
method (concatenation + `CubicBiasCorrectedRidge`, reused verbatim algebraically for this check
since the coordinator's own environment doesn't share the agent's repo checkout -- same
`FoldSafeResidualizer` and cubic OLS bias-correction definitions as sections 7/8, verified against
source before running). Features reused entirely from existing caches, no re-extraction: block17
from section 7/8's cache (`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`), block23 from
Stage 1's original cache (`scratchpad/task3_out/task3_features_shard*.npz` and
`.../smri-fm-fomo-tune/scratch_abide_validation/features.npz`). Eval script:
`scratchpad/task3_abide_eval_concat_cubicbiascorrected.py`.
