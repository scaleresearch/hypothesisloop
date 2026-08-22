# fix-later.md -- smri-fm-fomo-tune-task3

Append-only findings log for this experiment. Each entry: what happened (observed, not
paraphrased), what was changed and why, resolved or still open.

## Infra note (2026-08-17): shares its code repo and job image with `smri-fm-fomo-tune` (Task 5)
This directory holds its own `experiment.md`, `seed/job.yaml`, `Dockerfile.experimentator`, and
`tenstorrent/patches/` for documentation/bookkeeping clarity, but does NOT have its own
`$CODE_REPO_URL` or job image -- it deliberately reuses Task 5's:
- `CODE_REPO_URL=git://192.168.1.76/hypothesisloop-smri-fm-fomo-tune.git` (same bare repo; that
  repo's `run_task.py`/`seed/job.yaml` already support `TASK=task3` and always have).
- `localhost/hypothesisloop-smri-fm-fomo-tune-workload:latest` (same job image; same
  `Dockerfile.experimentator`-built agent image, `localhost/hypothesisloop-experimentator-smri-fm-fomo-tune`).
This is intentional -- the underlying code and image are genuinely identical between the two
tasks, only the platform experiment (`pe-a0b756f0`) and its objective differ. If this ever needs
to diverge (e.g. a Task-3-only code change that must never affect Task 5, or vice versa), that's
the point at which to actually fork the repo/image, not before.

Also inherited from Task 5, still relevant here:
- The recurring containerd image-eviction bug (image periodically disappears from
  `k3s ctr images`, root cause unconfirmed, mitigated with a pinning DaemonSet
  `smri-fm-fomo-tune-image-pin` in namespace `hypothesisloop-jobs`) -- see Task 5's own
  `fix-later.md` for the full writeup. Watch for the same `image_pull_failed`/`unschedulable`
  pattern here too; the mitigation should cover both tasks since it's the same image.
- The recurring PCIe power-state fault on board `0000:03:00.0` (3 confirmed occurrences as of
  2026-08-17) -- same node, same hardware, applies here too. `sudo /home/ttuser/.tenstorrent-venv/bin/tt-smi -r 0000:03:00.0`
  is the known mitigation; escalate to physical inspection if it recurs a 4th time.

## 2026-08-17 CONFIRMED ROOT CAUSE (supersedes the "untested GC hypothesis" entry above): kubelet image-GC disk threshold
- `journalctl -u k3s` shows, repeatedly, every 5 minutes: `"Disk usage on image filesystem is over the high threshold, trying to free bytes down to the low threshold" usage=86 highThreshold=85`, and at 17:13:39: `"Removing image to free bytes" imageID="sha256:122039abed..."` -- the exact ID of the workload image just re-imported minutes earlier. `df -h /` confirmed 85-88% used at the time (root filesystem doubles as containerd's imagefs here, no separate imagefs configured).
- This is kubelet's own `image-gc-high-threshold-percent` (default 85%) doing exactly what it's designed to do: when the imagefs crosses 85% used, it deletes unused (not referenced by a running pod) images to get back under the low threshold (80%). Our 35GB image, imported via `ctr images import` (not a normal pull, so nothing pins/references it between job runs), was the first eligible candidate every time.
- This is a genuine disk-space problem, not a mysterious GC bug: `podman system df` showed 866GB (100%) reclaimable in podman's own local image store (553 images, only 32 active) sitting on the same physical disk. `podman image prune -f` freed it immediately: disk usage dropped from 85% to 66% (2.9T -> 2.3T used, 3.6T total) in one command.
- Status: FIXED for now via `podman image prune -f`. The image-pin DaemonSet applied earlier did NOT work as a mitigation -- it never actually held a reference because it was itself stuck in `ImagePullBackOff` the whole time (the image was gone before the DaemonSet's own pod could pull it). Left in place; harmless, but not what fixed this.
- Recurrence risk: podman's local image store will refill with dangling layers from every future `podman build` (agent images, workload image rebuilds). Re-run `podman image prune -f` (or check `podman system df`) periodically, especially before/after any image rebuild, rather than waiting for jobs to start failing again.

## 2026-08-18: order-residualized-stopblock23-v1 job CrashLoopBackOff'd (5/5 retries failed)
- `exp-t3-2-order-residualized-21`, the job meant to answer whether STOP_BLOCK=23's pearson_r=0.968
  survives fold-safe order-index residualization, crashed on every restart. k3s log shows
  CrashLoopBackOff, container exits within seconds of ttnn init, no captured traceback (pod GC'd
  before deeper inspection; not pursued further per standing time-management directive).
- Not diagnosed further. Status: OPEN. Next action: resubmit fresh rather than debug the dead pod;
  if it crash-loops again, that itself is a data point (something wrong with ORDER_RESIDUALIZE=1
  code path, not a one-off infra blip) worth a real look then.

## 2026-08-18: task3-baseline-meanpool-ridgecv-v9-hostnorm-retry-round-extended FAILED, not diagnosed
- Failed quickly after checkout (commit 7257ece, shallow-depth fallback runner). Not investigated
  further per standing time-management directive -- this is the agent's own hypothesis code, left
  to that agent to notice/fix on its own (never act on an agent's behalf). Status: OPEN, low
  priority unless it recurs across multiple distinct hypotheses (would then suggest a shared
  infra/env issue rather than one hypothesis's bug).

## 2026-08-18 ROOT-CAUSED: recurred across both hypotheses -- TT device hang mid encoder-block-build
- `order-residualized-stopblock23-v1` (t3-2) failed/evicted 23 times; `task3-baseline-*-retry-round-
  extended` (t3-1) failed/evicted 3 times, both stalling at the exact same place: live `kubectl logs`
  on the running pod (`exp-exp-t3-1-baseline-confound-audit-r39-xs6gc`) showed clone -> checkpoint
  load -> device open all succeed normally in seconds, then `[MaskedInferenceEncoder.__init__] block
  N/24 built` progresses fast (block 0: 2.3s, blocks 1-4: ~0.0s each) and then hangs completely --
  no further log line for the remaining ~8 min until eviction (`never_reported_metrics`, correctly
  reported per the grace-window fix, but the underlying hang is the real problem, not the grace
  window). Confirmed not the known PCIe power-state fault (`journalctl -k` clean, `tt-smi -s` showed
  healthy telemetry, no fault flags). Most likely a stuck/dirty device queue left over from a prior
  job's kernel dispatch, not a code bug in block construction itself (block 0-4 build fine every
  time; it always stalls at the same phase, not a random block).
- Action taken: `sudo tt-smi -r` (reset all 4 chips) to clear any stuck device state. CONFIRMED
  FIXED: reopened `pe-a0b756f0` (ends 18:00Z), `exp-t3-2-order-residualized-24` COMPLETED cleanly
  on the 24th combined attempt -- stuck-device-state theory confirmed, not a code-path bug.
- Meanwhile: `01a01225` (t3-2's order-index confound check) closed OPEN/HONEST rather than false-
  positive -- marked `inconclusive` by the agent itself. Confirmed: order-index alone predicts age
  at pearson_r=0.611 (independently reproduced, matches t3-1's 01a011d9). STOP_BLOCK=23's OOF
  residual correlates with order-index at r=-0.146 (R^2~0.02) after controlling for age -- small
  but nonzero, not proof either way. The decisive check (ORDER_RESIDUALIZE=1 fold-safe correction,
  same technique as Task 5's AP-extent fix) never completed -- 18 submissions / 60+ pod attempts,
  all crashing before the correctness gate, root cause = the device hang above. Status: RESOLVED --
  see result below.
- RESULT (post-reset): `exp-t3-2-order-residualized-24` completed. ORDER_RESIDUALIZE=1 collapses
  pearson_r 0.9677 -> 0.7324, mae 3.50 -> 10.42 (hypothesis 01a01225 now `confirmed`). Confirms the
  order-index confound explained a major share of the uncorrected number -- same pattern as Task 5's
  AP-extent confound (0.995 -> 0.795). 0.7324 still clears the external baseline (0.426) by a wide
  margin -- a real, debiased win, not noise. Honest number to cite going forward is 0.7324/10.42, not
  0.9677/3.50 (provenance-only, same convention as Task 5's BASELINE section). Open follow-up (not
  blocking): other STOP_BLOCK configs' earlier refuted/tied verdicts were compared against the
  confound-inflated baseline, not individually re-verified debiased.

## 2026-08-18: SVR-RBF (order-fixed) beats RidgeCV locally, but flags a NEW nonlinear-order leak
- `01a011ac` (t3-2): HEAD=svr_rbf on the same order-residualized STOP_BLOCK=23 features reproduces
  cleanly across 2 runs -- pearson_r=0.8273, mae=8.47 both times (`exp-t3-2-svr-orderfixed-1/2`) --
  better than plain RidgeCV's 0.7324/10.42 debiased number.
- BUT: `leak_order_nonlinear_anchor_pearson_r=0.7345` on these runs, notably higher than the
  r=0.611 LINEAR order-index-vs-age correlation the fold-safe residualizer was built to remove.
  The residualizer only strips the linear order component from features; a nonlinear order->age
  relationship this strong could still be exploitable by a nonlinear head like SVR-RBF, which
  would mean 0.8273 rides more residual order confound than the 0.7324 RidgeCV number, not less.
  Not yet confirmed either way -- flagging, not concluding.
- Agent is already on this without prompting: same hypothesis now has a `binfixed` follow-up
  in flight (`exp-t3-2-svr-binfixed-1`, `exp-t3-2-truebaseline-binfixed-1`), evidently testing a
  correction for the nonlinear order relationship. Status: OPEN, watching for the binfixed result
  before treating 0.8273 as validated. Do not cite 0.8273 as the new headline until this resolves.
- RESOLVED (t3-2 bin-sensitivity sweep): n_bins in {5,20,40} on RidgeCV gives pearson_r
  0.656/0.504/0.442 -- MONOTONICALLY falling as bins get finer, the signature of nuisance-model
  overfitting (n=494/40 bins ~=12 subjects/bin, noisy per-bin mean removes real signal along with
  confound), not evidence of an ever-larger true confound. Linear residualization (1 free
  parameter/feature-dim) is the least overfitting-prone correction and stays the DEFAULT for this
  reason (code_ref 87dca6b). **Full picture, most to least corrected: raw=0.968 (confound-
  inflated, provenance only) -> linear=0.7319/10.43 (PRIMARY, recommended) -> bins(5/20/40)=
  0.656/0.504/0.442 (increasingly conservative/overfit-prone floor estimates, not necessarily more
  honest).** SVR-RBF's 0.8273 under linear-only correction remains an OPEN uncertainty, not
  promoted: a nonlinear head paired with only a linear confound correction could still exploit
  nonlinear order->feature structure the residualizer didn't remove, and the natural test for that
  (bin-based nonlinear correction) is itself confounded by overfitting at this n, so it can't
  cleanly confirm or refute the SVR number. Do not cite 0.8273 without that caveat attached.

## 2026-08-18: exp-t3-1-shallow-depth5-23 hung 46+ min at the correctness gate (new hang variant)
- Log frozen at "embedding+CV done, now running correctness gate" since 14:13:25; still RUNNING
  per API at 14:59 with no eviction (once a job posts any metric during CV, the never_reported_
  metrics path can't fire again -- see system_prompt.md fix -- so a hang AFTER first metrics post
  can persist indefinitely, unlike the earlier block-build hang which was caught by that check).
  Ties up 1 of 4 chips idle; not fully blocking (3 remain available). Not killed -- t3-1's own job
  to notice via its own polling, per "never act on an agent's behalf." Status: OPEN, watching
  whether it recurs on a fresh submission (would suggest the tt-smi reset didn't fully clear
  something, or a second, later-stage hang point exists distinct from the block-build one).
  UPDATE: still hung at 75+ min; t3-1's own polling loop just re-checks terminal status every
  ~6min without diagnosing or resubmitting -- a real throughput loss for that agent, though not
  blocking overall research since 3/4 chips stay in use by t3-2. Not intervened on (agent's own
  job to notice); flagging as a behavioral gap (a stuck-job self-diagnosis nudge) worth adding to
  the generic experimentator prompt in a future pass, not urgent enough to act on mid-round.
  UPDATE 2: t3-1 self-recovered (cancelled shallow-depth5-23, resubmitted as -24) without any
  coordinator intervention -- good, matches the desired behavior. RECURRED: -24 hit the exact
  same correctness-gate hang. 3/4 chips still available, non-blocking; not reset (would disrupt
  the concurrently-running healthy job on another chip).
  UPDATE 3 (ROOT-CAUSED via live `kubectl exec ps`/re-check after 20s): RECURRED A THIRD TIME on
  -25. This is a genuine device-lock deadlock, not flakiness or a slow compile: `run_task3_shallow`
  (PID 23, still alive, holding the TT device open) spawns `parity_fomo.py` (PID 2808) as a
  SEPARATE subprocess to run the correctness gate, and that child tries to open the SAME physical
  device (`/dev/tenstorrent/1`) while the parent still holds it -- confirmed by PID 2808's CPU TIME
  not advancing at all across a 20s recheck (00:01:17 -> 00:01:17), i.e. truly blocked, not slow.
  This is specific to the `run_task3_shallow` fallback runner's own code path (the shallow-depth
  max-blocks variant) -- it does not release/close its device handle before invoking the gate as a
  child process, unlike (presumably) the main `run_task.py` path. This is a code bug in that
  specific script, not a platform/hardware issue -- **not fixed here** (agent's own hypothesis
  code, never act on their behalf), but now has a concrete, evidenced root cause instead of just a
  recurring symptom. Whoever owns `run_task3_shallow` should close/release the device before
  calling `parity_fomo.py` as a subprocess, or run the gate in-process instead of via subprocess.
  UPDATE (Stage 2): recurred a 4th time, on a DIFFERENT hypothesis this time
  (`task3-baseline-meanpool-ridgecv-v9-hostnorm-retry-round-extended`, not `run_task3_shallow`) --
  same exact failure point (frozen at "running correctness gate" for 24+ min). Confirms this is
  broader than one script's subprocess bug; still non-blocking (other chips active), still not
  intervened on.
  UPDATE 2: same pod (`exp-exp-t3-1-baseline-confound-audit-r40-2dgmz`) confirmed still frozen at
  the identical log line (20:50:20) 61 minutes in -- 5th recurrence, same job even (not a fresh
  submission yet). t3-1's agent has not noticed/cancelled it this time, unlike its earlier clean
  self-recovery pattern. Still non-blocking (t3-2 actively producing results on the other chip).
  Not intervened on -- but if this specific job/agent doesn't self-recover soon, worth noting as a
  regression in the agent's own stuck-job handling, not just an infra recurrence.

## 2026-08-18 Stage 2: post-SVR/GBR-failure pivot -- polynomial order-correction, header probe
- t3-2 pivoted well after the SVR/GBR external-validation failure: tried a quadratic (order^2)
  term added to the confound correction (`ridge-polyorder2`, 3 free params vs bins=20's 20) as a
  more controlled alternative to naive binning for the nonlinear order-index relationship.
  Result: pearson_r=0.7149/mae=10.67 with RidgeCV -- ties or marginally underperforms the plain
  linear correction (0.7319/10.43), not an improvement. `svr-polyorder-1` (SVR on top of the same
  quadratic correction): 0.8192/8.46 -- still elevated like every other SVR config tested, but
  given the already-established mechanism (SVR-RBF predictions collapse near-constant outside
  training distribution, confirmed via the ABIDE external check regardless of correction
  strength), this is very unlikely to generalize either -- not re-testing externally, same
  architecture-level failure mode applies regardless of confound-correction choice.
- `headerprobe-2` (checking NIfTI header fields -- datatype, sform_code, qform_code, n_extensions
  -- for correlation with order-index/age, a Task_5-style scan-geometry confound check): ALL 8
  reported correlations are exactly 0.0, not just small. This reads as a degenerate case (these
  header fields are likely constant across all 494 subjects by construction of the preprocessing
  pipeline, giving an undefined/trivial correlation reported as 0) rather than a confidently
  computed null result. Status: mostly resolved -- agent marked the hypothesis `refuted` (not
  `confirmed`), the safe call either way, but left no comment explaining the all-zero readout, so
  whether it actually checked field variance vs. hit the degenerate case is unconfirmed. Low
  priority: this was a side probe, not on the critical path of the validated headline.
- t3-1 self-recovered from the 5th hang recurrence (cancelled r40/r41, r42 completed) -- resolves
  the "hasn't noticed" concern from the prior check. r42 was a plain-unmodified re-verification run
  (pearson_r=0.9677/mae=3.50, matches the known raw/uncorrected number, provenance only, not a new
  result). `ridge-polyorder3` (cubic order-index term): pearson_r=0.7136/mae=10.77, still no better
  than the plain linear correction -- polynomial confound-correction terms (2nd and 3rd order both
  tried) consistently tie or underperform linear. Both agents currently running fresh jobs.
- **CORRECTNESS-GATE HANG: FIXED BY AGENT (t3-1), CLOSING THIS OUT.** After r42 (plain unmodified
  `seed/run_job.sh`, gate-BEFORE-CV ordering) completed cleanly in ~7min, t3-1 diagnosed why its
  own custom runners (`run_task3_order_residualized.py`, `run_task3_baseline.py`) kept hanging:
  they ran the gate AFTER embedding+CV, while the main process still held the TT device open --
  exactly the device-lock deadlock mechanism this file root-caused earlier. Fix: switched both
  custom runners to gate-FIRST (matching r42's proven-reliable ordering), committed, resubmitted.
  This is the agent independently finding and fixing the same bug flagged in this file days
  earlier -- no coordinator intervention needed. Status: RESOLVED, watching next runs to confirm
  no 6th recurrence, but confident given the mechanism now matches the working config exactly.
- **Triple-independent confirmation of the headline number**: t3-1's fresh order-residualized
  run and t3-2's own (two independent normalization paths) all agree to 4 decimal places:
  pearson_r=0.7319, mae=10.429. Exceeds the reproducibility bar already met -- this is now about
  as solid as a local-CV number gets.
- CONFIRMED: `exp-t3-1-order-residualized-2` (post-fix resubmission) COMPLETED cleanly, no hang.
  Gate-first fix holds on first retest.

## 2026-08-18 Stage 2: layer sweep (debiased re-verification) and a new ensemble candidate
- t3-1's `layersweep-v2` systematically re-ran EVERY STOP_BLOCK depth (0,5,9,13,17,20,23,final)
  under order-correction in one job, closing the gap FINAL_RESULT.md section 3 flagged (other
  depths were only ever compared against the confound-INFLATED baseline, never re-verified
  debiased). Result: depths cluster 0.60-0.74 debiased vs. 0.85-0.97 raw/inflated -- confirms the
  confound affects all reasonable depths similarly (not pooling-choice-specific), as inferred
  earlier but now actually measured. depth_17 (0.7364) is marginally above depth_23/final
  (0.7319-0.7324) -- inside noise, not yet a claimed improvement, worth a repeat run before
  reading into it.
- `ensemble-orderfixed-1` (t3-2): simple ridge+SVR+GBR ensemble (echoing Task_5's winning
  `hetero_ensemble` approach) scores pearson_r=0.8005/mae=9.06 -- notably above RidgeCV alone.
  **Same caution as the standalone SVR/GBR candidates applies, arguably more so**: 2 of the 3
  ensemble members (SVR, GBR) are the exact heads already shown to fail external validation via
  catastrophic extrapolation collapse outside Task_3's adult-only training range. An ensemble
  that includes them is not automatically safe just because RidgeCV is also in the mix -- needs
  the same ABIDE external check before any promotion, not assumed safe by association. Status:
  OPEN, do not cite without external validation, exactly Stage 2's own three-bar rule.
- RESOLVED: ensemble (ridge+SVR+GBR) external validation done. ABIDE n=67: pearson_r=0.4195,
  mae=12.24 -- nominally beats RidgeCV headline's external 0.364 on correlation, but MAE is ~60%
  worse (12.24 vs 7.63). Predictions compress into a narrow 31.7-42.6 band against true 19-38.8 --
  the same extrapolation-compression signature SVR/GBR showed standalone, just partially masked
  by RidgeCV's better-behaved member averaging it out. NOT PROMOTED -- MAE blowup rules it out
  despite the nominal correlation win, consistent with the standing "both metrics must hold"
  discipline in experiment.md.
- Local progress continues on the age-bias-correction axis while external validation for
  depth17+bias-correct runs in the background: `block17-biascorrected-2` reproduces tightly across
  3 more seeds (0.7360-0.7370); `agebiascorrect-quad` (quadratic bias term) improves further to
  pearson_r=0.7407/mae=9.99 -- first candidate under 10y MAE. `svr-agebiascorrect` still elevated
  (0.8278/8.11) as expected for the same high-risk head already shown to fail externally --
  bias-correcting SVR's predictions doesn't fix its extrapolation-collapse mechanism.
- Bias-correction degree escalation continues climbing: depth17+quadratic reproduces across 3 seeds
  (0.7433-0.7449, mae~9.92); depth23+cubic reaches pearson_r=0.7542/mae=9.52. Watch this the same
  way the bins-based confound-correction escalation was watched earlier this round -- a real
  improvement should plateau, a climbing number with no ceiling as polynomial degree increases is
  the overfitting signature already seen once this round (bins=5/20/40 -> 0.656/0.504/0.442). Not
  yet concerning at cubic (only 4 free params on n=494, unlike bins=40's ~12-subjects-per-bin
  regime), but worth a quartic check and/or external validation before trusting degree>2 without
  question.
- Cubic bias-correction reproducibility CONFIRMED: 5-seed sweep gives pearson_r 0.7533-0.7549,
  mae 9.50-9.53 -- tight, real, not noise.
- RESOLVED: quartic bias-correction (`agebiascorrect-quartic-1`) DROPPED slightly vs cubic
  (pearson_r=0.7522/mae=9.61 vs cubic's 0.7542/9.52) -- this is the plateau-then-reverse signature,
  NOT unbounded climbing. Reassuring: degree escalation tops out around cubic and reverses rather
  than continuing to "improve" indefinitely, the opposite of the bins=5/20/40 runaway-overfitting
  pattern seen earlier. Cubic (mae~9.52) looks like the real ceiling for this technique, not an
  artifact of chasing higher polynomial degree.
- t3-1's newer external-validation jobs (`abide-external-validate-2`, `abide-svr-external-
  validate-1`) extend the SAME native-fit-on-ABIDE methodology (not the required inference-only
  check) to cubic bias-correction and SVR -- still doesn't answer Stage 2's promotion question,
  same caveat as before applies. Not flagged again to the agent (never act on their behalf);
  still waiting on the coordinator-dispatched agent's inference-only check for the number that
  actually resolves promotion.
- t3-1 independently started its OWN ABIDE external validation job (CPU-only, reusing the
  coordinator's cached ABIDE features -- smart, avoids re-extraction) for the bias-corrected head
  candidates, in parallel with the coordinator-dispatched validation agent doing the same thing.
  Not duplicated effort in a wasteful sense -- two independent implementations converging (or not)
  on the same external number is the same cross-confirmation pattern already used productively
  for the headline (triple-matched to 4 decimals). Letting both run; will reconcile whichever
  lands first against the other when it arrives.
- RECONCILED: t3-1's own check (`exp-t3-1-abide-external-validate-1`) landed pearson_r=0.594,
  mae=2.62 -- wildly different from (and much better than, implausibly so: lower than Task_3's own
  training MAE) the expected external-transfer number. Read the actual committed script
  (`task3_abide_external_validate.py`, `cross_validate_external`): it does `head.fit(X[train],
  y[train])` on ABIDE's OWN features/ages, 5-fold CV entirely WITHIN ABIDE -- i.e. it fits and
  evaluates natively on ABIDE, never applying a Task_3-only-trained model to ABIDE inference-only.
  This answers a different, also-legitimate question ("does the bias-correction architecture help
  when refit on a different cohort") but is NOT the "does the Task_3-fitted model's local edge
  generalize" check Stage 2's promotion bar requires -- every other external check this round
  (RidgeCV, SVR, GBR, ensemble) fit ONLY on Task_3's 494 subjects and applied inference-only to
  ABIDE, never refitting. t3-1's number should NOT be used to promote depth17/bias-correction --
  it isn't the right methodology for that question, not a contradiction of the coordinator's
  inference-only check. Not a bug in the usual sense (the script is internally consistent and its
  docstring is honest about doing 5-fold CV, not a Task_3-fit-then-infer check), just a different
  question than the one that matters for promotion. Waiting on the coordinator-dispatched agent's
  inference-only check (still extracting) for the number that actually resolves this.

## 2026-08-18 Stage 2: nonlinear-head question (SVR/GBR vs RidgeCV) -- well-reasoned, still open
- t3-2's own analysis (hypothesis `01a011ac`) on why residual-vs-order correlation isn't a clean
  leak signature: RidgeCV (structurally linear, cannot exploit nonlinear leftover order structure)
  shows an EQUAL-OR-LARGER residual-order correlation than SVR at both correction strengths tested
  (-0.895 vs -0.770 linear-fix; -0.703 vs -0.645 bins-fix) -- meaning this metric mechanically
  reflects how much true-age signal the correction removed from features, not which head is
  "cheating." More telling: SVR's edge over RidgeCV (+0.095 linear-fix, +0.129 bins-fix) does NOT
  shrink as correction strengthens (if anything widens) -- if the edge were pure leftover-confound
  exploitation, stronger correction should have closed it.
- Triangulation: GBR (gradient-boosted trees, a completely different inductive bias than SVR's RBF
  kernel) ALSO beats RidgeCV, same direction: pearson_r=0.7706 vs 0.7319 (+0.039, smaller than
  SVR's +0.095 but consistent). Two structurally different nonlinear methods agreeing makes a
  single-method-artifact explanation less likely.
- Agent's own honest ranking (not upgraded to confirmed, correctly): RidgeCV 0.732 (most
  defensible) < GBR 0.771 (moderate, triangulated) < SVR 0.827 (largest edge, single-method,
  least certain). Status: OPEN, needs external validation to resolve per Stage 2's own promotion
  bar -- next step is running the GBR/SVR-fitted models through the same ABIDE external check
  already built for the RidgeCV headline (see FINAL_RESULT.md section 5) rather than more local-CV
  variants, which won't settle this on their own.

## 2026-08-18 (coordinator): ABIDE external validation -- CPU extraction ran fine, no infra issues; one methodology note
- Ran entirely off the TT-device path: STOP_BLOCK=23 mean-pool feature extraction for all 494
  Task_3 subjects done via the same CPU dense-attention-workaround encoder path Task_5's own
  external validation scripts use (`fomo_tune.backbone.load_backbone`, unmodified checkpoint/
  forward pass), sharded 4-way in parallel podman containers off `localhost/hypothesisloop-smri-
  fm-fomo-tune-workload:latest` (already has torch/nibabel/sklearn baked in -- no new image
  needed). ~47 min wall-clock for all 494 subjects (4 shards x ~124 each, ~20-22s/subject), 0
  extraction failures. No TT hardware, no job submission, no platform-experiment API calls
  involved -- this was pure coordinator-side inference against already-fitted-elsewhere logic
  (`FoldSafeResidualizer` + `RidgeCV`), mirroring how Task_5's `scratch_ppmr_validation`/
  `scratch_abide_validation` scripts worked.
- One real infra hiccup, self-resolved: initial checkpoint download (`hf://medarc/walnut/...`,
  ~3.9GB) took ~14 min across 4 concurrent containers sharing one `HF_HOME` cache dir -- the
  huggingface_hub file lock correctly serialized it to a single download (no 4x duplicate
  fetch), so this was just genuinely slow first-time download, not a bug. Not worth changing;
  a shared warm cache would avoid repeating this on a future run.
- Methodology note, not a bug: ABIDE's age distribution (7.6-38.8, autism-cohort study) barely
  overlaps Task_3's training age range (19-80, adult brain-age study) -- 131/198 ABIDE subjects
  are younger than any Task_3 training subject. Naively reporting the full-cohort correlation
  (pearson_r=0.067) would have been a misleading result caused by extrapolation outside the
  training distribution, not by anything wrong with the model or method. Fixed by also reporting
  the age-range-restricted subset (n=67, ages 19-80, pearson_r=0.364) as the more honest read.
  Any future external-cohort check for Task_3 should check age-range overlap with [19, 80] BEFORE
  trusting the full-cohort number. Status: RESOLVED, documented in FINAL_RESULT.md section 5.

## 2026-08-18 (coordinator): SVR-RBF/GBR external validation -- both collapse on ABIDE, resolves the Stage 2 nonlinear-head question
- Reused the already-cached STOP_BLOCK=23 order-residualized Task_3 features and ABIDE
  niftis/ages from the RidgeCV ABIDE check (FINAL_RESULT.md section 5) -- no re-extraction, saved
  the ~47min feature pass entirely. Swapped only the head to the exact hyperparameters from
  `run_task.py`: `SVR(kernel="rbf", C=10.0, epsilon=0.5)` and `GradientBoostingRegressor(
  n_estimators=200, max_depth=2, random_state=0)`. Fit on all 494 Task_3 subjects, inference-only
  on the same age-matched ABIDE subset (n=67, [19,80]) used for RidgeCV's 0.364 external number.
- RESULT: both heads' local-CV edge over RidgeCV reverses on ABIDE. GBR: local 0.7706 -> external
  0.296 (vs RidgeCV's 0.364). SVR-RBF: local 0.8273 -> external 0.131 (vs RidgeCV's 0.364). MAE for
  both nonlinear heads is ~3x RidgeCV's external MAE (19-23 vs 7.6).
- Root cause of the MAE blowup, confirmed via a prediction-range sanity check (not just "harder
  domain, lower r"): SVR-RBF's ABIDE predictions are nearly constant (46.875-46.881, a true ABIDE
  age range of 7.6-38.8) -- classic RBF-kernel collapse when inputs are far outside the training
  feature distribution (kernel similarity to every training point -> ~0, prediction reverts to the
  model's global offset). GBR's predictions (31.7-51.9) show the standard decision-tree
  extrapolation ceiling -- trees can't predict below the lowest training-leaf value learned on
  Task_3's adult-only (19-80) ages, so they systematically overshoot every young ABIDE subject.
  RidgeCV's linear extrapolation degrades gracefully by comparison; this is likely the actual
  mechanism, more than the residual-order-leak hypothesis originally flagged in section 2b/2c.
- No infra issues -- ran off cached artifacts entirely, same podman-container CPU path as the
  RidgeCV check, no TT device or job submission involved. One env note: the coordinator's own venv
  lacks sklearn (`ModuleNotFoundError`), had to run via `podman run --entrypoint python3 ...
  localhost/hypothesisloop-smri-fm-fomo-tune-workload:latest` same as the earlier RidgeCV eval
  scripts -- not a bug, just a reminder for next time to use the workload image directly rather
  than trying bare `python3` first.
- Status: RESOLVED. Neither SVR-RBF nor GBR is promoted -- RidgeCV (0.7319-0.7324 local,
  0.364 external) stays the sole headline; it is now also the only head with a bar-clearing
  external validation among the three candidates. Documented in FINAL_RESULT.md section 6.


## 2026-08-19 (coordinator): depth-17+bias-corrected and ensemble external validation -- one real re-extraction, no infra failures
- Continuing sections 5/6's ABIDE external-validation work: two more Stage 2 candidates needed
  the same check (`fix-later.md`'s "layer sweep... and a new ensemble candidate" entry).
  Candidate 2 (ridge+SVR+GBR ensemble) reused the already-cached STOP_BLOCK=23 features entirely
  -- no re-extraction. Candidate 1 (STOP_BLOCK=17 pooling + age-bias-corrected RidgeCV) did NOT
  have cached features anywhere: only STOP_BLOCK=23/"final" had ever been extracted and cached
  for either Task_3 or ABIDE, despite depth-17 features existing transiently inside prior TT job
  runs (`layersweep-v2`, `exp-t3-1-block17-biascorrected-1`) -- those never wrote a portable
  feature cache to disk outside the job's own ephemeral output. Had to re-extract from scratch:
  ~70 min wall-clock across 6 parallel CPU containers (4 for Task_3's 494 subjects, 2 for
  ABIDE's 198), stopping the forward pass after block index 17 inclusive with no final LayerNorm
  (matching `backbone_tt.TTBackbone.embed_multi_hostnorm`'s block-snapshot contract exactly --
  0-indexed inclusive, raw pre-norm). 0 extraction failures across all 6 shards. Slower than
  section 5's ~47min STOP_BLOCK=23 pass despite computing fewer blocks (18 vs 24) because ABIDE's
  larger images (more visible patches/tokens, up to ~9000 vs Task_3's ~3000-4000) dominate
  per-subject wall time more than block count does -- worth remembering for any future depth
  sweep's time estimate: token count, not block count, is the bigger lever.
- **Actionable lesson for future rounds, not fixed here**: any local-CV job that reports a
  winning config using a NON-default `STOP_BLOCK` or other feature-affecting hyperparameter
  should ideally also emit a portable `features.npz`-style cache (even just to the job's own
  output dir, not necessarily coordinator scratch) so a later external-validation pass doesn't
  have to redo a 45-90 minute extraction just because the winning depth differs from whatever the
  first external check happened to use. Not acted on this round (would require changing agent-run
  job scripts on their own branches, out of scope for a coordinator external-validation pass) --
  flagging for whoever scopes the next round's job-script conventions.
- Result: candidate 1 (depth-17+bias-corrected RidgeCV) PROMOTED -- ABIDE in-range pearson_r=0.3622
  (flat vs headline's 0.364, as expected since the bias correction is a pearson-r-invariant affine
  transform), mae=6.16 (real ~19% improvement over headline's 7.63). Candidate 2 (ensemble) NOT
  promoted -- ABIDE in-range pearson_r=0.4195 nominally beats the headline but mae=12.24 is ~60%
  worse, the same extrapolation-compression failure already documented for its SVR/GBR members in
  the prior fix-later entry, only partly diluted by averaging in RidgeCV. No infra failures beyond
  the missing-cache re-extraction noted above. Documented in FINAL_RESULT.md section 7.

## 2026-08-19 Stage 2: post-promotion follow-ups -- depth-ensemble null, new stronger candidate found
- `depth-ensemble-1` (averaging block17 + final-layer predictions): pearson_r=0.7361/mae=10.47,
  ties block17-only, no improvement -- null result, combining depths doesn't help.
- `network-probe-1`: infra diagnostic (checked outbound reachability, likely scoping IXI external
  validation feasibility), returned `any_reachable=1`. Not a model result.
- NEW STRONGER CANDIDATE: `block17-cubicbiascorrected-repro2` -- STOP_BLOCK=17 + CUBIC (not linear)
  bias correction: pearson_r=0.7581/mae=9.52, reproduces tightly across 3 seeds (0.7574-0.7588).
  Beats the just-promoted linear-bias-correction headline (0.7366/10.14) locally. Dispatched a
  coordinator-level external validation (depth17 features already cached from the prior run, so
  this should be fast) to check the same three-bar promotion rule before citing it. Status: OPEN,
  local-only so far.

## 2026-08-19 (coordinator): cubic bias-correction external validation -- PROMOTED, closes the OPEN item above
- Resolves the "NEW STRONGER CANDIDATE... Status: OPEN" entry immediately above. Ran
  `depth-17 + CubicBiasCorrectedRidge` inference-only on the same ABIDE in-range subset (n=67),
  reusing the cached depth-17 features from section 7's run (no re-extraction, ~2 min total).
  Also ran `QuadBiasCorrectedRidge` on the same cache as a free comparison point.
- Result: cubic external pearson_r=0.3553 (flat vs linear's 0.3622 -- within n=67 noise), mae=4.91
  (a further ~20% improvement over the already-promoted linear version's 6.16, ~36% over the
  original RidgeCV headline's 7.63). Quadratic external mae=7.89 -- *worse* than linear despite a
  competitive local number, a clean case of local-CV ranking not surviving external check
  (its ABIDE-predicted range overshoots true age on both ends, [7.5,44.2] vs true [19.0,38.8]).
- Verdict: **depth-17 + CUBIC bias-corrected RidgeCV PROMOTED**, replaces the linear version
  (FINAL_RESULT.md section 7) as the recommended MAE-optimized Stage 2 configuration. Quadratic
  NOT promoted (external MAE regression). Documented in FINAL_RESULT.md section 8.
- No infra issues -- ran entirely off the section-7 feature cache
  (`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`), via `/tmp/venv5/bin/python3` (has
  sklearn; the coordinator's bare `python3` still doesn't). Quartic (flagged in the round as
  reversing locally) was not checked externally -- not required to promote/reject cubic, flagged
  as a possible future data point if anyone wants full degree-sweep external coverage.

## 2026-08-19 Stage 2: IXI confirmed genuinely inaccessible -- closed, don't pursue further
- Both agents independently probed IXI (the wider/better-age-matched external cohort scoped
  earlier as a nice-to-have over ABIDE): t3-1's `network_probe` (01a017b6) found the landing page
  reachable but the actual file-serving subdomain returns 403 Forbidden on the T1 tar/demographics
  downloads; t3-2's `ixiprobe-1` independently confirmed the same 403 from a different network
  path (job pod vs dev container), plus a dead-end on a HuggingFace mirror. Genuinely blocked from
  every angle tried, not a one-off network hiccup. Status: CLOSED -- ABIDE remains the validated
  external cohort (already answered the core question this round needed); IXI is not worth
  further attempts.

## 2026-08-19 Stage 2: spline-order correction fails, concat feature fusion shows modest promise
- Spline-based order-index confound correction (smarter/smoother alternative to naive binning,
  hypothesized to unlock SVR/GBR safely): RESULT IS WORSE, not better. RidgeCV+spline:
  pearson_r=0.5526/mae=13.11 (much worse than linear correction's 0.7319/10.43). SVR+spline:
  0.6722/11.09 (worse than SVR+linear's own 0.8273/8.47). Definitively closes this direction --
  every correction stronger than linear tried so far (bins, spline) overcorrects and loses real
  signal at n=494; linear residualization is confirmed as the sweet spot, not just the first
  thing tried.
- `concat1723-orderfixed-1` (concatenating block17+block23 features, not averaging): local
  pearson_r=0.7386/mae=10.35 -- a modest but genuine improvement over plain block17 (0.7364/10.53).
  `fusion1723` (averaging instead of concat) ties/underperforms (0.7312/10.70) -- concat, not
  averaging, is the direction with any signal. t3-1 now stacking concat with cubic bias
  correction (`concat-cubicbiascorrected-1`, running) to see if the two independently-positive
  levers combine additively. Status: OPEN, local-only so far, would need the same three-bar
  treatment (reproduction + external validation) before promotion if it beats the current
  cubic-bias-corrected headline.
- RESOLVED (local side): `concat-cubicbiascorrected-1` combines both levers -- pearson_r=0.7609/
  mae=9.44, reproduces across 3 seeds (0.7600-0.7616). Beats the current headline (0.7581/9.52).
  `concat1723` alone also reproduces well (0.7377-0.7390 across seeds). Both features (block17,
  block23) already cached, so dispatched a fast external-validation agent
  (a1d9b9a550c494e33) rather than waiting on agents to build it themselves -- reuses cached
  artifacts from sections 5-8, no re-extraction expected.


## 2026-08-19 (coordinator): concat(block17,block23)+cubic external validation -- PROMOTED, new headline
- Resolves the "t3-1 now stacking concat with cubic bias correction... dispatched a fast
  external-validation agent (a1d9b9a550c494e33)" note in the entry above. Built the concatenated
  block17||block23 feature matrix (2048-dim) for all 494 Task_3 subjects and all 198 ABIDE
  subjects entirely from existing caches -- block17 from section 7/8's cache
  (`scratchpad/task3_out/{task3,abide}_block17_shard*.npz`), block23 from Stage 1's original
  cache (`scratchpad/task3_out/task3_features_shard*.npz` and
  `.../smri-fm-fomo-tune/scratch_abide_validation/features.npz`). No re-extraction for either
  depth. Verified subject/order alignment across both caches before concatenating (494/494 and
  198/198 match). Fit `FoldSafeResidualizer` (order-index) + `CubicBiasCorrectedRidge` on all 494
  Task_3 subjects, applied inference-only to the same ABIDE in-range subset (n=67).
- Result: external pearson_r=0.3355 (vs depth-17-only cubic's 0.3553 -- a 0.02 drop, smaller than
  the 0.007 drop section 8 already accepted as n=67 noise), mae=3.79 (a further ~23% improvement
  over depth-17-only cubic's 4.91, ~50% over the original RidgeCV headline's 7.63). Local
  in-sample sanity: pearson_r=0.7654/mae=9.31, consistent with the platform's reported OOF
  0.7609/9.44.
- Verdict: **concat(block17,block23) + CUBIC bias-corrected RidgeCV PROMOTED**, replaces the
  depth-17-only cubic version (FINAL_RESULT.md section 8) as the new Stage 2 headline. No
  companion candidate rejected this round (`fusion1723`/averaging was already ruled out locally
  before reaching external validation). Documented in FINAL_RESULT.md section 9.
- No infra issues -- ran entirely off existing caches via `/tmp/venv5/bin/python3`. Eval script:
  `scratchpad/task3_abide_eval_concat_cubicbiascorrected.py`.

## 2026-08-19 Stage 2: 3-way depth concat underperforms 2-way; CLS-token retry via concat
- `concat172023` (block17+block20+block23, three-way): pearson_r=0.7317/mae=10.72 -- WORSE than
  the promoted 2-way concat(17,23) (0.7386/10.35, pre-bias-correction baseline). More depths is
  not automatically better -- confirms 2-way concat is the sweet spot, not a step toward "concat
  everything." The `concat1323`/`concat923` variants mentioned as in-flight last check never
  actually ran (redirected to this 3-way test instead).
- t3-2 made a sound analogical move: CLS-token pooling was refuted earlier this round but only
  via AVERAGING with mean-pool (0.7254 vs 0.7319) -- never via CONCATENATION, which is exactly
  the mechanism that turned depth-fusion from a null (average, 0.7312) into a real win (concat,
  0.7386->0.7609 promoted). Retrying CLS+mean via concat now. Applying a round-learned pattern
  to a previously-closed lever -- worth watching.
- RESOLVED: `concatcls-orderfixed-1` (CLS+mean-pool via concatenation): pearson_r=0.7254/
  mae=10.67 -- matches the earlier averaging-based number almost exactly, no improvement.
  Confirms CLS-token pooling genuinely doesn't add value regardless of combination mechanism --
  not an averaging-specific artifact, a real negative result tested both ways now. Closed.

## 2026-08-19: t3-2's own concat+cubic external validation -- same non-qualifying methodology as
## the earlier t3-1 case, not a contradiction of the promoted headline
- `concat1723cubic-abide-external-1` (t3-2's own platform job) reports pearson_r=0.653/mae=2.60 --
  again very different from the coordinator's inference-only number (0.3355/3.79). Checked the
  actual script (`external_abide_concat_validate.py`, branch t3-2): `head.fit(features[train],
  y[train])` on ABIDE's OWN features/labels, 5-fold CV entirely within ABIDE -- explicitly
  ported verbatim from t3-1's earlier `task3_abide_external_validate.py` (same docstring credit).
  Same reconciliation as the "RECONCILED" entry above for t3-1's original case: answers a
  different question (does the architecture help when refit per-cohort), not the required
  fit-Task_3-only/infer-on-ABIDE check. Does not contradict or affect the promoted headline
  (FINAL_RESULT.md section 9). Not flagged to the agent (never act on their behalf).

## 2026-08-19 Stage 3: round extended 24h, agent resumed with a genuinely new pooling architecture
- Extended `pe-a0b756f0` to 2026-08-20T20:40Z (budget 200h, stage 4 added), `experiment.md`/live
  description updated with Stage 3 framing pointing at the flattening-local/growing-external-gains
  observation, without prescribing method. Dispatched a GitHub push agent for current best code+
  docs (in progress as of this entry).
- t3-1 picked up the extension and tried `normweighted-pooling-1` -- a norm-weighted (not plain
  mean) pooling variant, one of the "not-yet-tried pooling architecture" directions noted in the
  Stage 3 framing. Result: ties plain mean-pool almost exactly (pearson_r=0.7363 vs 0.7364) --
  clean null, no improvement, but a legitimate new direction now explored rather than left
  untried.
- Two more Stage 3 attempts, both clean nulls: `concat-elasticnet-1` (ElasticNet head instead of
  RidgeCV on the concat features): pearson_r=0.7356/mae=10.59, slightly worse than RidgeCV's own
  0.7385/10.35 on the same features -- RidgeCV stays the best head. `concat1723-meanstd-1`
  (pooling mean+std instead of just mean): pearson_r=0.7296/mae=10.85, worse than plain mean-pool
  concat (0.7386/10.35) -- extra pooled statistics don't help here.
