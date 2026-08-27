package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// tick runs one full admission pass. Single-threaded — no concurrent ticks.
func (l *Loop) tick(ctx context.Context) error {
	if !l.ticking.CompareAndSwap(false, true) {
		panic("scheduler: tick() re-entered concurrently — capacity accounting is not safe under concurrency, see Loop.ticking")
	}
	defer l.ticking.Store(false)

	start := time.Now()
	defer func() { obsmetrics.AdmissionTickDuration.Observe(time.Since(start).Seconds()) }()

	// Per-experiment failures collected across every pass below and joined at the end (#19). A
	// tick reads state that is shared by every queued job but acts on each one individually, so
	// only a failure to read that shared state is grounds for abandoning the pass.
	var tickErrs []error

	// The disbalance evictor is a cluster-level verdict about stranded accelerators — at most one
	// per cluster per tick, shared by both admission passes. See its call sites for why.
	disbalanceRan := map[string]bool{}

	// autoscaler.md's backlog signal (secondary path, for clusters with no native autoscaler to
	// react to a Pending pod). Published once at the end of this tick, whatever else happens.
	backlog := newBacklogAggregator()

	// Amortizes the running-experiment node-attribution lookups tick-time "max" resolution needs
	// (see loop_resolve.go) across every queued job this tick considers, in both passes.
	resolveCache := newResolutionCache(l)

	// Reprioritize all queued jobs after every tick, regardless of outcome — deferred so every
	// exit path (including early returns below) runs it, keeping queue order fresh even when
	// nothing was queued for burst.
	defer func() {
		if err := l.reprioritizer.RePrioritize(ctx); err != nil {
			l.logger.Warn("reprioritize after tick", zap.Error(err))
		}
	}()

	// 1. Get available physical capacity as a domain.Footprint per cluster — a pooled total
	// would hide which cluster has room, admitting against a combined number while the actual
	// target cluster is full.
	gAvail, bAvail, err := l.workload.GetFlavorCapacity(ctx)
	if err != nil {
		return err
	}
	nodeAvail, err := l.workload.GetAcceleratorCapacityByNode(ctx)
	if err != nil {
		return fmt.Errorf("per-node accelerator capacity: %w", err)
	}
	nodeResources, err := l.workload.GetNodeResourceCapacity(ctx)
	if err != nil {
		return fmt.Errorf("per-node resource capacity: %w", err)
	}
	nodeLabels, err := l.workload.GetNodeLabels(ctx)
	if err != nil {
		return fmt.Errorf("node labels: %w", err)
	}
	// Which clusters can run a job that spans more than one node. A capability each cluster
	// reports about itself, filtered on here so a distributed job is never placed on a runtime
	// that executes single-node jobs only — that used to be discovered at workload creation,
	// after the job had already been admitted and had already claimed a reservation.
	multiNodeCapable, err := l.workload.GetMultiNodeCapability(ctx)
	if err != nil {
		return fmt.Errorf("multi-node capability: %w", err)
	}
	// Speculative-submit inputs (autoscaler.md), read only when a deployment has opted in via
	// WithSpeculation — a LoopWorkloadClient that has not implemented these two methods (every
	// pre-autoscaler test fake, and any deployment that never calls WithSpeculation) must not be
	// asked to.
	var autoscalerEnabled map[string]bool
	var clusterIDs map[string]string
	if l.triedClusterTTL > 0 {
		autoscalerEnabled, err = l.workload.GetAutoscalerCapability(ctx)
		if err != nil {
			return fmt.Errorf("autoscaler capability: %w", err)
		}
		clusterIDs, err = l.workload.GetClusterIDs(ctx)
		if err != nil {
			return fmt.Errorf("cluster ids: %w", err)
		}
	}
	// gAvail only has entries for clusters GetFlavorCapacity's heartbeat read found connected
	// (see queuebackend.Backend.GetFlavorCapacity) — reusing its key set here is the same
	// "unreachable cluster is never a speculative candidate" rule the doc calls out as a
	// pre-existing bug fixed in step 0, applied to the speculative path too.
	connectedClusters := make(map[string]bool, len(gAvail))
	for cluster := range gAvail {
		connectedClusters[cluster] = true
	}
	// speculativeFootprintByCluster approximates each cluster's outstanding speculative
	// accelerator footprint as its whole SUBMITTED footprint — a job's live-fit-vs-speculative
	// distinction lives in the metrics store (scheduled_nodes), which the loop does not read per
	// job. Counting every SUBMITTED accelerator here makes clusterSpeculativeCap strictly more
	// conservative than the design's exact definition, never less — the safe direction for a cap.
	speculativeFootprintByCluster := map[string]int{}
	if l.triedClusterTTL > 0 {
		submitted, err := l.store.ListSubmittedExperiments(ctx)
		if err != nil {
			return fmt.Errorf("list submitted for speculative footprint: %w", err)
		}
		for _, s := range submitted {
			if autoscalerEnabled[s.ClusterName] {
				speculativeFootprintByCluster[s.ClusterName] += s.AcceleratorCount
			}
		}
	}
	// Installed (not free) capacity — evidence for one decision, the disbalance eviction, and an
	// input to nothing else in this tick. Unlike the three reads above it, admission never
	// consults it.
	//
	// So a failure here is logged and the pass continues with no totals, rather than aborting the
	// tick. That is not a fallback: "no totals for a cluster" is already a defined state with one
	// defined answer — evictDisbalanced refuses to condemn anything it cannot measure a share
	// against. An unreadable read lands in that same state. Failing the whole tick instead would
	// let a telemetry gap in the evidence for one destructive decision stop every ordinary
	// admission on the platform, which is the opposite of what the evidence is for.
	totalCapacity, err := l.workload.GetTotalCapacity(ctx)
	if err != nil {
		l.logger.Warn("total capacity unavailable; disbalance eviction cannot judge this tick", zap.Error(err))
		totalCapacity = nil
	}
	// Per-node installed CPU/memory/storage — the stable denominator evictDisbalanced's node-local
	// fair-share math needs, since nodeResources above is free capacity and moves every tick. Same
	// degrade-gracefully treatment as totalCapacity immediately above: absent for a cluster (or
	// entirely, on a read failure) just means that cluster's disbalance judgment abstains this
	// tick, never that the whole tick fails.
	nodeResourcesTotal, err := l.workload.GetNodeTotalCapacity(ctx)
	if err != nil {
		l.logger.Warn("per-node total capacity unavailable; disbalance eviction cannot judge this tick", zap.Error(err))
		nodeResourcesTotal = nil
	}

	// Credit back capacity from disbalance victims a prior tick already evicted but that this
	// fresh read still can't see as free (the node/agent hasn't reported it back yet) — otherwise
	// this tick re-derives the same shortage and evicts a fresh set of victims to free capacity
	// that is already being freed. See loop_disbalance.go's applyPendingEvictions.
	l.applyPendingEvictions(start, gAvail, bAvail, nodeAvail)

	// The capacity this tick started with, before any admission below claims a slice of it. It
	// is what separates "you lost this to jobs ahead of you, wait" from "this cluster cannot
	// host your request, shrink it" — a distinction a submitter cannot make from the outside
	// and cannot act on if it is wrong. Both passes classify against the same snapshot.
	nodeAvailAtTickStart := cloneNodeAvailByCluster(nodeAvail)
	nodeResourcesAtTickStart := cloneNodeAvailByCluster(nodeResources)

	// 2. GetFlavorCapacity already subtracts the complete desired footprint (SUBMITTED/
	// ADMITTED/RUNNING). No second reservation or live-usage subtraction here — either would
	// double-count. MarkSubmitted is itself the durable capacity claim.

	// 3. Get all QUEUED experiments.
	queued, err := l.store.ListQueuedExperiments(ctx)
	if err != nil {
		return err
	}
	completion, err := l.completionFractions(ctx, queued)
	if err != nil {
		return err
	}

	// 3a. Enforce the summary gate: skip agents with COMPLETED experiments missing a summary.
	// The gate also runs at submission time, but a batch submitted before any run completes is
	// already QUEUED, so re-check here to pause the rest until summaries are written.
	// 3b. Enforce the current stage's max_job_hours: a job queued while an earlier, looser stage
	// was running must not slip through after the ladder tightened. Held, not rejected — a later
	// stage may lift the cap, and then it admits normally.
	// One unreadable row must not decide the fate of every other queued job. A QUEUED experiment
	// pointing at a deleted platform experiment is re-read on every tick forever, so aborting the
	// pass here would wedge admission for the entire platform indefinitely — the exact shape
	// important.md #19 exists to forbid. Per-experiment failures are logged, collected, and the
	// experiment is dropped from this tick's consideration; the pass finishes, and the joined
	// error surfaces at the end so the failure is still loud.
	summaryBlocked := map[string]bool{} // key: agentID+"/"+platformExpID
	maxJobHours := map[string]float64{} // key: platformExpID; 0 = unlimited
	filtered := queued[:0]
	for _, exp := range queued {
		if exp.PlatformExperimentID == "" {
			filtered = append(filtered, exp)
			continue
		}
		limit, seen := maxJobHours[exp.PlatformExperimentID]
		if !seen {
			pe, err := l.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
			if err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("stage job length: %w", err))
				continue
			}
			if pe == nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("stage job length: platform experiment %s not found", exp.PlatformExperimentID))
				continue
			}
			limit = pe.CurrentMaxJobHours()
			maxJobHours[exp.PlatformExperimentID] = limit
		}
		if limit > 0 && exp.EstimatedDurationHours > limit {
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedJobTooLong); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark job too long: %w", err))
			}
			continue
		}
		cut, err := l.store.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID)
		if err != nil {
			l.skipExperiment(&tickErrs, exp, fmt.Errorf("stage cut: %w", err))
			continue
		}
		if cut {
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedStageCut); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark stage cut: %w", err))
			}
			continue
		}
		key := exp.AgentID + "/" + exp.PlatformExperimentID
		if blocked, seen := summaryBlocked[key]; seen {
			if !blocked {
				filtered = append(filtered, exp)
			}
			continue
		}
		blocked, err := l.store.HasUnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil {
			l.skipExperiment(&tickErrs, exp, fmt.Errorf("summary gate: %w", err))
			continue
		}
		summaryBlocked[key] = blocked
		if !blocked {
			filtered = append(filtered, exp)
		} else {
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedSummaryGate); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark summary gate: %w", err))
			}
		}
	}
	queued = filtered

	// 4. Guaranteed pass: FIFO by age bucket, quota-ratio tiebreak within a bucket, then
	// completion proximity DESC, then shortest job first.
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	guaranteedQuotas, err := l.fetchQuotaMap(ctx, guaranteed)
	if err != nil {
		return err
	}
	sortGuaranteed(guaranteed, guaranteedQuotas, completion, l.guaranteedFairnessWindow)
	// Both snapshots are taken here, before either pass claims anything. Taking the burst one
	// after the guaranteed pass told a burst job displaced by a guaranteed one that the cluster
	// had no room for it, when what happened is that a higher tier outranked it.
	gAvailInitial := cloneAvail(gAvail)
	bAvailInitial := cloneAvail(bAvail)

	// Preemption candidates are read once per tick rather than once per job that fails to fit —
	// a saturated cluster with a deep guaranteed queue otherwise issues one full scan per queued
	// job, every tick. It is re-read after a preempt, because preempt requeues its victims and
	// the next candidate must not consider them running.
	var running []*domain.Experiment
	runningLoaded := false

	// Snapshot pre-tick availability so a skip can be classified: capacity_unavailable (no
	// capacity existed before this tick) vs outranked (capacity existed, but other guaranteed
	// jobs earlier in sort order already claimed it).
	for _, exp := range guaranteed {
		// A job already assigned a cluster (pinned by a prior attempt in submitJob) stays
		// pinned — just recompute its footprint under that flavor; otherwise pick a (cluster,
		// flavor) pair among the requested type and any AcceptableAcceleratorTypes where the
		// whole footprint fits — see resolveClusterAndFootprint/clusterWithBestFit.
		// What the row says right now, captured before the placement search below mutates
		// exp.AcceleratorType to communicate its choice. submitJob needs both to know whether it
		// has a new flavor to persist.
		persistedFlavor := exp.AcceleratorType
		candidates, narrowed := eligibleClusters(gAvail, multiNodeCapable, exp)
		if len(candidates) == 0 && narrowed {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedNoMultiNodeCluster); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark no multi-node cluster: %w", err))
			}
			continue
		}
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(candidates, nodeAvail, nodeResources, nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(guaranteedQuotas[quotaKey(exp.AgentID, exp.PlatformExperimentID)], exp) {
			// Reverting to the requested flavor re-places the job, layout included: picking on
			// scalar fit alone could point a spread job at a cluster whose nodes cannot hold it,
			// and charge it a failed placement there.
			exp.AcceleratorType = exp.Job.AcceleratorType
			fp = exp.Footprint()
			if placed, ok := placeAtFlavor(candidates, nodeAvail, nodeResources, nodeLabels, exp); ok {
				cluster = placed
			} else {
				cluster = clusterWithBestFit(candidates, fp)
			}
		}
		// Resolve any "max" CPU/memory/storage sentinel (and validate any explicit number) against
		// THIS cluster's own fair share, now that cluster and accelerator flavor are finally
		// settled for this attempt — see loop_resolve.go. A cluster this job's own resources
		// cannot fit is folded into the ordinary not-admitted path below by simply un-picking the
		// cluster, exactly like a scalar or topology miss: preemption cannot fix a job whose own
		// request is oversized for every node, so there is nothing else to try here.
		if cluster != "" {
			resolvedJob, fitsCluster, rerr := l.resolveClusterLocalResources(ctx, resolveCache, exp, cluster, nodeResourcesTotal[cluster], nodeAvail[cluster], nodeLabels[cluster])
			if rerr != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("resolve cluster-local resources: %w", rerr))
				continue
			}
			if !fitsCluster {
				cluster = ""
			} else {
				exp.ResolvedJob = resolvedJob
				fp = exp.Footprint()
			}
		}
		// Which of the two fit checks failed decides whether preemption can help at all, so they
		// are evaluated separately rather than or-ed into one condition.
		scalarFits := domain.Fits(gAvail[cluster], fp)
		if !scalarFits || !topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp) {
			// Speculative submit (autoscaler.md): before spending a guaranteed job's preemption
			// power, try every autoscaler-enabled cluster this job hasn't already failed over
			// from. A candidate has no live capacity by definition (that's why we're here) — the
			// SUBMITTED row itself is the scale-up request the native autoscaler reacts to.
			// Live-fit always wins over speculating (this branch only runs on live no-fit), and
			// speculating anywhere always wins over preempting a burst job (this runs first).
			candidates, cerr := l.speculativeCandidates(ctx, resolveCache, exp, autoscalerEnabled, connectedClusters, clusterIDs, multiNodeCapable, nodeAvail, nodeResourcesTotal, nodeLabels, speculativeFootprintByCluster, gAvail)
			if cerr != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("speculative candidates: %w", cerr))
				continue
			}
			// waitingForScaleUp: this cluster's own desired-free already went negative in this
			// job's accelerator dimension (a SUBMITTED row is already outstanding here, possibly
			// this same job a moment ago). Preempting a burst job to cover that shortage would be
			// a wrong eviction for a shortage the incoming node is already going to fill; the
			// deadline in job_watcher_scan.go bounds the wait (autoscaler.md's skip-preemption rule).
			waitingForScaleUp := autoscalerEnabled[cluster] && negativeInDimension(gAvail[cluster], exp.AcceleratorType)
			if len(candidates) > 0 {
				specCluster := candidates[0]
				// The "max" CPU/memory/storage resolution above ran against whichever cluster
				// looked live-fit-eligible before this job fell through to speculation -- a
				// different cluster than specCluster, or none at all if every candidate failed
				// that pre-check. Re-resolve against the actual speculative target now that
				// it's chosen: a stale resolution would either persist a "max" share sized for
				// the wrong cluster's nodes, or (if never resolved at all) leave the literal
				// "max" sentinel in the submitted spec -- codex review caught this as a real
				// hazard.
				if specCluster != cluster {
					resolvedJob, fitsSpecCluster, rerr := l.resolveClusterLocalResources(ctx, resolveCache, exp, specCluster, nodeResourcesTotal[specCluster], nodeAvail[specCluster], nodeLabels[specCluster])
					if rerr != nil {
						l.skipExperiment(&tickErrs, exp, fmt.Errorf("resolve cluster-local resources for speculative target: %w", rerr))
						continue
					}
					if !fitsSpecCluster {
						// This job's own request (post-"max"-resolution) cannot fit even the
						// speculative target's largest node -- speculativeCandidates' own
						// fitsLargestNode check used the unresolved request, so this can still
						// happen. Nothing else to try this tick.
						obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
						if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedCapacityUnavailable); err != nil {
							l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
						}
						continue
					}
					exp.ResolvedJob = resolvedJob
				}
				if err := l.submitJobTo(ctx, exp, specCluster, clusterIDs[specCluster], persistedFlavor, true); err != nil {
					l.logger.Error("speculative submit", zap.String("exp", exp.ID), zap.String("cluster", specCluster), zap.Error(err))
					obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
					reason := notAdmittedReasonForSubmitError(err)
					if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
						l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
					}
					continue
				}
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "submitted").Inc()
				obsmetrics.SpeculativeSubmitsTotal.Inc()
				speculativeFootprintByCluster[specCluster] += exp.AcceleratorCount
				// The claimed footprint is not subtracted from gAvail/bAvail/nodeAvail here: a
				// speculative claim has no live node to subtract from, and GetFlavorCapacity
				// already carries desired-free negative for this cluster starting next tick via
				// SumDesiredFootprintByCluster. Nothing else in this tick reads gAvail[specCluster]
				// again for a decision this job's SUBMITTED row should have already influenced.
				continue
			}
			// Try to preempt that cluster's own burst jobs to make room; scoped to this cluster
			// since freeing a burst job elsewhere wouldn't help here.
			if !runningLoaded {
				running, err = l.store.ListRunningExperiments(ctx)
				if err != nil {
					// Without the running set there is no tier-safe preemption plan to make, and
					// no basis for the disbalance pass either — both reason about what is running.
					// Skip this job rather than abandon the tick, and terminate nothing on state
					// we could not read.
					l.skipExperiment(&tickErrs, exp, fmt.Errorf("list running for preemption: %w", err))
					continue
				}
				runningLoaded = true
			}
			burstRunning := filterTierCluster(running, domain.CapacityBurst, cluster)
			shortage := preemptionShortfall(gAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
			l.logger.Info("guaranteed job needs preemption",
				zap.String("exp", exp.ID), zap.String("cluster", cluster),
				zap.String("avail", footprintStr(gAvail[cluster])), zap.String("need", footprintStr(fp)),
				zap.String("shortage", footprintStr(shortage)),
				zap.Int("burst_candidates", len(burstRunning)))
			// preempt() only requeues victims — it never waits for their Jobs to disappear, so
			// this tick can't know the accelerator is really free yet. exp stays QUEUED; a
			// later tick's fresh capacity read admits it once the resource is genuinely gone.
			// Preemption picks victims to cover a cluster-wide scalar shortage. When the cluster
			// already has the capacity in total and only the layout does not work -- distinct
			// hosts, labels, per-node distribution -- covering that scalar proves nothing: the
			// victims are chosen without knowing which node each one holds, so evicting them can
			// free devices on a node the job could already use and leave it just as unplaceable.
			// It would then do the same again next tick, destroying burst work every time.
			//
			// Targeting the right node needs per-job node attribution, which today is one
			// metrics-store query per candidate per tick -- the exact pattern the query-volume
			// work exists to remove, so it lands with the batched per-tick node read, not before
			// it. Until then this refuses to guess: no plan is better than a destructive one.
			// The disbalance pass below is node-aware and still runs.
			var committed bool
			var err error
			switch {
			case scalarFits:
				l.logger.Info("skipping preemption: cluster has the capacity, the layout does not fit",
					zap.String("exp", exp.ID), zap.String("cluster", cluster))
			case waitingForScaleUp:
				l.logger.Info("skipping preemption: cluster is autoscaler-enabled and already waiting for scale-up",
					zap.String("exp", exp.ID), zap.String("cluster", cluster))
			default:
				committed, err = l.preempt(ctx, shortage, burstRunning, exp)
				if err != nil {
					l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
				}
			}
			// Only a preempt that actually requeued victims makes the running set stale. Marking
			// it stale unconditionally cost a full re-read for every blocked job behind it on a
			// saturated cluster, where preempt most often finds nothing to take.
			if committed {
				runningLoaded = false
			}
			// Three reasons this tick's disbalance pass may not run for this job, all of them
			// "the evidence for terminating live work is not there":
			//   - preempt already committed a plan covering the whole shortage: the capacity is
			//     on its way back and the deficit is no longer outstanding (availability is not
			//     decremented here, so re-reading it would double-count the same shortage).
			//   - preempt errored: whatever it could not read or commit, we do not get to answer
			//     a failed tier-safe remedy with a more destructive one.
			//   - the pass already ran for this cluster this tick: it is a cluster-level verdict
			//     about stranded accelerators, not a per-queued-job one. Running it once per
			//     blocked job against unchanged availability evicts a fresh victim each time.
			if err == nil && !committed && !disbalanceRan[cluster] {
				disbalanceRan[cluster] = true
				if err := l.evictDisbalanced(ctx, exp, cluster, gAvail[cluster], totalCapacity[cluster], nodeResourcesTotal[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			backlog.record(start, cluster, "guaranteed", exp, shortage)
			reason := ""
			switch {
			case waitingForScaleUp:
				reason = domain.NotAdmittedWaitingForScaleUp
			case allSpeculativeCandidatesTried(exp, autoscalerEnabled, connectedClusters, clusterIDs, l.triedClusterTTL):
				reason = domain.NotAdmittedNoScalableCapacity
			default:
				fitAtTickStart := domain.Fits(gAvailInitial[cluster], fp) &&
					topologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
				reason = notAdmittedReasonFor(fitAtTickStart, fp, shortage)
			}
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Error("submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		subtractFootprint(gAvail[cluster], fp)
		// bAvail is the same shared pool's other view — without this, the burst pass later in
		// this tick could still see this unit as free and double-book it.
		subtractFootprint(bAvail[cluster], fp)
		reservePlacement(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "admitted").Inc()
	}

	// 5. Burst pass: fairness-weighted (least quota used first), then completion proximity, shortest job first.
	burst := filterTier(queued, domain.CapacityBurst)
	if len(burst) == 0 {
		return errors.Join(tickErrs...)
	}

	// No cluster-wide "is there any capacity at all" shortcut here: a footprint's dimensions are
	// different units (millicores, bytes, device counts), so summing them into one number answers
	// no question anyone asked — RAM bytes dominate by orders of magnitude and the sum is
	// positive whatever the accelerator situation is. Fit is decided per job, per dimension, by
	// domain.Fits in the loop below, which writes the same capacity_unavailable reason.

	// Fetch quota usage for fairness ordering.
	quotaMap, err := l.fetchQuotaMap(ctx, burst)
	if err != nil {
		return err
	}
	sortBurst(burst, quotaMap, completion)
	// Interleave by agent so one agent's queue depth can't claim every unit of burst capacity
	// that frees up this tick ahead of another agent with fewer jobs waiting — see
	// interleaveByAgent's doc comment.
	burst = interleaveByAgent(burst)

	for _, exp := range burst {
		persistedFlavor := exp.AcceleratorType
		candidates, narrowed := eligibleClusters(bAvail, multiNodeCapable, exp)
		if len(candidates) == 0 && narrowed {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedNoMultiNodeCluster); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark no multi-node cluster: %w", err))
			}
			continue
		}
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(candidates, nodeAvail, nodeResources, nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)], exp) {
			// Reverting to the requested flavor re-places the job, layout included: picking on
			// scalar fit alone could point a spread job at a cluster whose nodes cannot hold it,
			// and charge it a failed placement there.
			exp.AcceleratorType = exp.Job.AcceleratorType
			fp = exp.Footprint()
			if placed, ok := placeAtFlavor(candidates, nodeAvail, nodeResources, nodeLabels, exp); ok {
				cluster = placed
			} else {
				cluster = clusterWithBestFit(candidates, fp)
			}
		}
		// See the guaranteed pass above for why this runs here, after cluster/flavor settle and
		// before the fit check.
		if cluster != "" {
			resolvedJob, fitsCluster, rerr := l.resolveClusterLocalResources(ctx, resolveCache, exp, cluster, nodeResourcesTotal[cluster], nodeAvail[cluster], nodeLabels[cluster])
			if rerr != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("resolve cluster-local resources: %w", rerr))
				continue
			}
			if !fitsCluster {
				cluster = ""
			} else {
				exp.ResolvedJob = resolvedJob
				fp = exp.Footprint()
			}
		}
		if !domain.Fits(bAvail[cluster], fp) || !topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp) {
			// Same shortage vector the guaranteed pass computes, for the same reason: "short
			// {cpu: 12}" tells an operator which dimension to fix, where a bare
			// capacity_unavailable tells them only that something, somewhere, did not fit.
			shortage := preemptionShortfall(bAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
			if !disbalanceRan[cluster] {
				disbalanceRan[cluster] = true
				if err := l.evictDisbalanced(ctx, exp, cluster, bAvail[cluster], totalCapacity[cluster], nodeResourcesTotal[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			backlog.record(start, cluster, "burst", exp, shortage)
			fitAtTickStart := domain.Fits(bAvailInitial[cluster], fp) &&
				topologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(fitAtTickStart, fp, shortage)); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Error("submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		subtractFootprint(bAvail[cluster], fp)
		reservePlacement(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
	}

	backlog.publish(speculativeFootprintByCluster)

	return errors.Join(tickErrs...)
}

// skipExperiment drops one experiment from this tick and records why, so a pass over many
// survives any one of them failing (#19). The log line is per-experiment because the joined
// error is read as a tick-level summary and would otherwise bury which job was at fault.
func (l *Loop) skipExperiment(errs *[]error, exp *domain.Experiment, err error) {
	l.logger.Warn("skipping experiment this tick",
		zap.String("exp", exp.ID),
		zap.String("platform_experiment", exp.PlatformExperimentID),
		zap.Error(err))
	*errs = append(*errs, fmt.Errorf("experiment %s: %w", exp.ID, err))
}

// notAdmittedReasonFor names why a queued job was skipped. fitAtTickStart is the whole
// discriminator: outranked means the job *would* have been admitted against the capacity this
// tick started with and lost it to jobs earlier in sort order, so waiting is the right response.
// It used to be inferred from "some dimension of the footprint shrank this tick", which is true
// of every skipped job on any busy cluster — a request oversized for the hardware was told it
// had been outranked, and waiting for the queue to drain would never admit it.
func notAdmittedReasonFor(fitAtTickStart bool, footprint domain.Footprint, shortage domain.Footprint) string {
	if fitAtTickStart {
		return domain.NotAdmittedOutranked
	}
	// The shortfall vector is otherwise only visible in the scheduler's own logs (see the
	// "guaranteed job needs preemption" log a few lines above this call site) -- a submitter has
	// no other way to tell "your request is oversized for this cluster" from "the cluster is
	// just busy right now", and no way to self-diagnose which resource to shrink.
	if len(shortage) > 0 {
		return domain.NotAdmittedCapacityUnavailable + ": short " + footprintStr(shortage)
	}
	return domain.NotAdmittedCapacityUnavailable
}

// eligibleClusters narrows the clusters a job may be placed on to the ones that can actually run
// it. Today that is one capability — multi-node execution — and a single-node job sees every
// cluster, exactly as before.
//
// The returned map shares its Footprint values with avail rather than copying them, so the
// in-place reservations admission makes against the returned view are the same numbers every
// later job in this tick reads. A copy here would let a job admitted through the narrowed view
// be double-booked by one placed through the full one.
//
// narrowed says whether this filter is what emptied the result, and it is the only thing that
// makes an empty result diagnosable. A single-node job is returned avail untouched, so an empty
// set for one means no cluster reported capacity at all — a heartbeat gap, not a missing
// capability. Reporting that as "no multi-node cluster" told a plain one-node job it had been
// refused for spanning hosts it never asked to span.
func eligibleClusters(avail map[string]domain.Footprint, multiNodeCapable map[string]bool, exp *domain.Experiment) (clusters map[string]domain.Footprint, narrowed bool) {
	if exp.Job.Nodes() <= 1 {
		return avail, false
	}
	out := make(map[string]domain.Footprint, len(avail))
	for cluster, footprint := range avail {
		if multiNodeCapable[cluster] {
			out[cluster] = footprint
		}
	}
	return out, true
}

func cloneAvail(avail map[string]domain.Footprint) map[string]domain.Footprint {
	out := make(map[string]domain.Footprint, len(avail))
	for cluster, fp := range avail {
		copy := make(domain.Footprint, len(fp))
		for key, value := range fp {
			copy[key] = value
		}
		out[cluster] = copy
	}
	return out
}

// quotaCanCoverFlavor is an early, non-authoritative filter for a more expensive acceptable
// flavor; ReserveAdmittedFlavor's transaction remains the final concurrency-safe check.
func quotaCanCoverFlavor(quota *domain.AgentQuota, exp *domain.Experiment) bool {
	if quota == nil || exp.AcceleratorCount <= 0 {
		return false
	}
	rate, ok := exp.AcceleratorType.LookupCost()
	if !ok {
		return false
	}
	newCost := rate * float64(exp.AcceleratorCount) * exp.EstimatedDurationHours
	used, limit := quota.UsedBurstAccH, quota.BurstAcceleratorHours
	if exp.CapacityTier == domain.CapacityGuaranteed {
		used, limit = quota.UsedGuaranteedAccH, quota.GuaranteedAcceleratorHours
	}
	return used-exp.EstimatedCostAccH+newCost <= limit
}

// footprintStr renders a Footprint for logging — domain.Footprint's struct-keyed map can't be
// marshalled by zap.Any (JSON keys must be strings).
func footprintStr(fp domain.Footprint) string {
	parts := make([]string, 0, len(fp))
	for k, v := range fp {
		parts = append(parts, fmt.Sprintf("%s:%s=%d", k.Kind, k.Flavor, v))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

// subtractFootprint subtracts fp from avail in place, clamped at zero per dimension. No-op if
// avail is nil (cluster not present in the capacity map).
func subtractFootprint(avail domain.Footprint, fp domain.Footprint) {
	if avail == nil {
		return
	}
	for k, v := range fp {
		avail[k] -= v
		if avail[k] < 0 {
			avail[k] = 0
		}
	}
}

// shortfall returns, for every dimension footprint requests, how much more avail would need to
// have to fit it (need - have, floored at 0 per dimension) — the vector preempt() must cover.
func shortfall(avail domain.Footprint, footprint domain.Footprint) domain.Footprint {
	out := domain.NewFootprint()
	for k, need := range footprint {
		have := avail[k]
		if have < need {
			out.Add(k, need-have)
		}
	}
	return out
}

// foldLookup is map[key] with a case-insensitive fallback (see domain.AcceleratorType.MatchesLabels).
func foldLookup(m map[string]int64, key string) int64 {
	if v, ok := m[key]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return 0
}

// foldMatchingKey returns the key in m matching want case-insensitively, or want itself if
// none matches (a subsequent m[want] lookup then correctly misses/reads zero).
func foldMatchingKey(m map[string]int64, want string) string {
	if _, ok := m[want]; ok {
		return want
	}
	for k := range m {
		if strings.EqualFold(k, want) {
			return k
		}
	}
	return want
}

// preemptionShortfall keeps accelerator availability in the placement domain declared by the
// job. Cluster-level extended-resource totals combine devices from nodes with different labels,
// so they cannot answer whether (for example) A100 capacity is available when L40 capacity is
// idle. CPU, memory, storage, and other resources retain their cluster-level shortfall.
func preemptionShortfall(clusterAvail domain.Footprint, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment, footprint domain.Footprint) domain.Footprint {
	out := shortfall(clusterAvail, footprint)
	// A job runs on one node, so a deficit can be entirely per-node: every dimension can be
	// plentiful cluster-wide while no single node has the combination. Reported as an empty
	// shortage, that job looked like it needed nothing — which left preemption with no vector to
	// cover and the disbalance evictor with no deficit to explain, exactly when a neighbour's
	// disproportionate request was the reason nothing fit.
	addNodeResourceShortfall(out, byNode, nodeResources, labelsByNode, exp)
	if exp.Job.TotalAccelerators() <= 0 {
		return out
	}
	key := strings.ToLower(string(exp.AcceleratorType))
	capacities := make([]int64, 0, len(byNode))
	for node, capacity := range byNode {
		if labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			capacities = append(capacities, foldLookup(capacity, key))
		}
	}
	sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })

	// The job's nodes, hungriest first, matched against the roomiest hosts. For an ungrouped job
	// every node wants the same count and the order is immaterial; for a grouped one, pairing the
	// learner with the emptiest host is the assignment that leaves the smallest real shortage.
	perRank := make([]int64, 0, exp.Job.Nodes())
	for _, shape := range exp.NodeShapes() {
		if shape.AcceleratorCount > 0 {
			perRank = append(perRank, shape.AcceleratorCount)
		}
	}
	sort.Slice(perRank, func(i, j int) bool { return perRank[i] > perRank[j] })
	var missing int64
	if requiresDistinctHosts(exp) {
		for rank, need := range perRank {
			available := int64(0)
			if rank < len(capacities) {
				available = capacities[rank]
			}
			if available < need {
				missing += need - available
			}
		}
	} else {
		for _, need := range perRank {
			if len(capacities) == 0 {
				missing += need
				continue
			}
			sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })
			if capacities[0] < need {
				missing += need - capacities[0]
				capacities[0] = 0
			} else {
				capacities[0] -= need
			}
		}
	}
	acceleratorKey := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: key}
	if missing > out[acceleratorKey] {
		out[acceleratorKey] = missing
	}
	return out
}

// candidateAcceleratorTypes returns the flavors admission should try for exp, in preference
// order: exp.AcceleratorType first, then any distinct AcceptableAcceleratorTypes.
// AcceleratorCount is already flavor-independent, so trying an alternate only changes which
// accelerator key the footprint is keyed under.
func candidateAcceleratorTypes(exp *domain.Experiment) []domain.AcceleratorType {
	if exp.Job.AcceleratorType == "" {
		return nil
	}
	// A job that has already executed re-admits onto the flavor it actually ran on, and nothing
	// else. Settlement bills lifetime observed hours at the rate the row carries, so moving a
	// requeued job to a different flavor silently re-prices the stint it already ran at the new
	// flavor's rate. Keyed on the durable preemption marker rather than on observed hours: metrics
	// are delayed, so "has it consumed anything yet" can still read zero for a job that has run.
	if exp.EvictionReason == string(domain.EvictionPreemptedForGuaranteed) {
		return []domain.AcceleratorType{exp.AcceleratorType}
	}
	types := make([]domain.AcceleratorType, 0, 1+len(exp.Job.AcceptableAcceleratorTypes))
	types = append(types, exp.Job.AcceleratorType)
	// The requested type is allowed to also appear in AcceptableAcceleratorTypes (see the
	// admission check in admission.go), so collapse that overlap here rather than retrying the
	// same flavor twice — "distinct" above is this dedup, not an assumption about the input.
	seen := map[domain.AcceleratorType]bool{exp.Job.AcceleratorType: true}
	for _, t := range exp.Job.AcceptableAcceleratorTypes {
		if seen[t] {
			continue
		}
		seen[t] = true
		types = append(types, t)
	}
	return types
}

// resolveClusterAndFootprint picks a concrete (cluster, flavor) pair for an unpinned job, trying
// every candidate flavor (requested type first) and returning the first that fits outright on
// some cluster — see candidateAcceleratorTypes. This lets a job whose requested flavor is
// saturated land on a free AcceptableAcceleratorTypes alternative instead of sitting QUEUED with
// idle capacity elsewhere. If nothing fits outright, falls back to the requested flavor's own
// best-fit cluster so the caller's preemption path still runs.
// TotalAccelerators, not the top-level per-node count: a grouped job states its counts on its
// groups and leaves the top-level field empty, so reading it literally said every heterogeneous
// job wanted no accelerators at all. Such a job then took the CPU-only shortcut below — no flavor
// substitution and, worse, no placeAtFlavor, so it was placed on scalar fit alone and could be
// handed a cluster whose nodes had not one free device of the hardware it asked for.
func resolveClusterAndFootprint(avail map[string]domain.Footprint, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, domain.Footprint) {
	if exp.Job.TotalAccelerators() <= 0 {
		// Still judged on node layout: a cluster whose aggregate is large enough but whose nodes
		// are individually too small for one rank would otherwise be picked on the scalar alone,
		// fail topologyFits in the tick, and go to preemption while another cluster could host it.
		fp := exp.Footprint()
		if cluster, ok := placeAtFlavor(avail, nodeAvail, nodeResources, nodeLabels, exp); ok {
			return cluster, fp
		}
		return clusterWithBestFit(avail, fp), fp
	}

	requested := exp.AcceleratorType
	var fallbackCluster string
	var fallbackFP domain.Footprint
	for i, t := range candidateAcceleratorTypes(exp) {
		exp.AcceleratorType = t
		fp := exp.Footprint()
		if i == 0 {
			fallbackCluster, fallbackFP = clusterWithBestFit(avail, fp), fp
		}
		if cluster, ok := placeAtFlavor(avail, nodeAvail, nodeResources, nodeLabels, exp); ok {
			return cluster, fp
		}
	}
	exp.AcceleratorType = requested
	return fallbackCluster, fallbackFP
}

// placeAtFlavor names the cluster that can host exp at the accelerator type it currently carries,
// judged on footprint and node layout alike. ok=false means no cluster can — the caller decides
// what to do with a job that does not fit anywhere, rather than being handed a cluster that only
// looks big enough in the scalar.
func placeAtFlavor(avail map[string]domain.Footprint, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, bool) {
	fp := exp.Footprint()
	clusters := make([]string, 0, len(avail))
	for name := range avail {
		clusters = append(clusters, name)
	}
	sort.Strings(clusters)
	for _, candidate := range clusters {
		if domain.Fits(avail[candidate], fp) && topologyFits(nodeAvail[candidate], nodeResources[candidate], nodeLabels[candidate], exp) {
			return candidate, true
		}
	}
	return "", false
}

func requiresDistinctHosts(exp *domain.Experiment) bool {
	// TotalAccelerators rather than the top-level per-node count: a grouped job states its counts
	// per group and leaves the top-level field empty, which read literally would say every
	// heterogeneous job is happy to stack all its ranks on one host.
	if exp.Job.Nodes() <= 1 || exp.Job.TotalAccelerators() <= 0 {
		return false
	}
	return exp.Job.Topology == nil || exp.Job.Topology.SpreadAcrossHosts == nil || *exp.Job.Topology.SpreadAcrossHosts
}

// topologyFits proves that every rank of a hard spread-across-hosts accelerator job has a
// distinct currently-schedulable node with enough free devices of the selected flavor.
func topologyFits(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	return reservePlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labelsByNode, exp)
}

func labelsMatch(actual, required map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func desiredPlacementFits(byNode map[string]map[string]int64, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, desired []*domain.Experiment, candidate *domain.Experiment) bool {
	// Planned jointly, not one job after another: the runtime is free to put an already-desired
	// job on whichever node it likes, so replaying it onto one fixed node and then asking whether
	// the candidate fits around that guess rejected candidates the cluster could host.
	jobs := make([]*domain.Experiment, 0, len(desired)+1)
	jobs = append(jobs, desired...)
	jobs = append(jobs, candidate)
	_, ok := planPlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labelsByNode, jobs)
	return ok
}

func cloneNodeCapacity(byNode map[string]map[string]int64) map[string]map[string]int64 {
	cloned := make(map[string]map[string]int64, len(byNode))
	for node, capacity := range byNode {
		cloned[node] = make(map[string]int64, len(capacity))
		for key, count := range capacity {
			cloned[node][key] = count
		}
	}
	return cloned
}

// reservePlacement walks exp's ranks over the candidate nodes and subtracts what each rank takes,
// returning false when no node can host one.
//
// Every dimension is checked per node, not just accelerators. A job runs on one node and must fit
// that node's free CPU/memory/storage as well — checking those against a cluster-wide total
// admitted jobs no node could run: they were placed in desired state, held a reservation, and sat
// unschedulable until the stuck-pending deadline. It also silently disabled the disbalance
// evictor, which only ever runs when admission fails and so never saw the jobs whose neighbours'
// disproportionate requests were the reason nothing fit.
//
// This is resource-vector feasibility, not proof that the runtime will schedule the job: taints,
// volumes and affinity are the runtime's own concerns and are not modelled here.
// It walks domain.JobSpec.NodeShapes() — one entry per node, which for a heterogeneous (grouped)
// job are genuinely different shapes. An averaged shape would be the one thing that must not be
// used here: no node ever holds the average of a learner and 64 actors, so a job proven to fit
// against it fits nowhere.
//
// The walk is a search, not a single greedy pass: ranks are tried hardest-first, each on the
// tightest node that fits, and a rank that finds no host sends the search back to move an earlier
// one. A plain first-fit in declared order let a small rank take the only node a later large rank
// could use and reported a job with a valid assignment as not fitting. The input maps are
// modified only when the whole job is placed — a false answer leaves them exactly as they were.
func reservePlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	plan, ok := planPlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labelsByNode, []*domain.Experiment{exp})
	if !ok {
		return false
	}
	for _, p := range plan {
		if p.shape.AcceleratorCount > 0 {
			byNode[p.node][p.key] -= p.shape.AcceleratorCount
		}
		subtractNodeResources(nodeResources[p.node], p.shape.Resources)
	}
	return true
}

// rankAssignment is one rank of one job pinned to a node by planPlacement. key is the exact
// accelerator key under which that node reported the job's flavor (see foldMatchingKey).
type rankAssignment struct {
	job   int
	shape domain.NodeShape
	node  string
	key   string
}

// placementSearchBudget bounds the number of node trials one planPlacement call may make before
// it gives up. Hardest-first ordering with best-fit node choice and equivalent-node pruning places
// realistic jobs without backtracking at all; the budget is a guard against an adversarial layout
// exhausting the tick, and running out of it is reported as "does not fit" — a conservative
// answer, the same one the greedy walk gave whenever it guessed wrong.
var placementSearchBudget = 50000

// planPlacement finds nodes for every rank of every job in jobs against the given free capacity,
// consuming capacity in the maps as it goes (callers pass clones). Distinct-host rules apply per
// job; ranks of different jobs may share a node. Returns the assignment and whether all ranks
// were placed. On false the maps are left in an unspecified state.
func planPlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, jobs []*domain.Experiment) ([]rankAssignment, bool) {
	type rank struct {
		job      int
		shape    domain.NodeShape
		key      string
		distinct bool
		nodes    []string // qualifying candidate set for this rank
	}
	var ranks []rank
	for i, exp := range jobs {
		key := string(exp.AcceleratorType)
		// Two candidate sets: a node that reported accelerators, and any node the cluster
		// reported resources for. A node needing no accelerator must not be confined to
		// accelerator nodes.
		acceleratorNodes := qualifyingNodes(byNode, labelsByNode, exp)
		plainNodes := qualifyingNodes(nodeResources, labelsByNode, exp)
		distinct := requiresDistinctHosts(exp)
		for _, shape := range exp.NodeShapes() {
			nodes := plainNodes
			if shape.AcceleratorCount > 0 {
				nodes = acceleratorNodes
			}
			ranks = append(ranks, rank{job: i, shape: shape, key: key, distinct: distinct, nodes: nodes})
		}
	}
	// Hardest rank first: most accelerators, then the largest fungible request. The rank with the
	// fewest possible hosts is placed while the most hosts are still free.
	sort.SliceStable(ranks, func(a, b int) bool {
		if ranks[a].shape.AcceleratorCount != ranks[b].shape.AcceleratorCount {
			return ranks[a].shape.AcceleratorCount > ranks[b].shape.AcceleratorCount
		}
		return shapeResourceTotal(ranks[a].shape) > shapeResourceTotal(ranks[b].shape)
	})

	used := make([]map[string]bool, len(jobs))
	for i := range used {
		used[i] = make(map[string]bool)
	}
	plan := make([]rankAssignment, len(ranks))
	budget := placementSearchBudget

	// nodeFits reports whether node can host r right now and the accelerator key it would draw on.
	nodeFits := func(r rank, node string) (string, bool) {
		if r.distinct && used[r.job][node] {
			return "", false
		}
		nodeKey := ""
		if r.shape.AcceleratorCount > 0 {
			// byNode retains each device's own raw driver-reported casing (see
			// domain.AcceleratorType.MatchesLabels re: casing) — find the matching key
			// case-insensitively rather than assuming it equals key verbatim.
			nodeKey = foldMatchingKey(byNode[node], r.key)
			if byNode[node][nodeKey] < r.shape.AcceleratorCount {
				return "", false
			}
		}
		if !nodeHasRoom(nodeResources[node], r.shape.Resources) {
			return "", false
		}
		return nodeKey, true
	}
	// leftover is what node would have left after hosting r, summed into one number per family —
	// the best-fit sort key only. Summing across dimensions is fine for deciding which node to
	// *try* first (a rough "how much room is left" ranking); it is not fine for deciding two nodes
	// are the same node, which is what equivalentState below is for.
	leftover := func(r rank, node, nodeKey string) (int64, int64) {
		var accelerators, resources int64
		if r.shape.AcceleratorCount > 0 {
			accelerators = byNode[node][nodeKey] - r.shape.AcceleratorCount
		}
		for k, v := range nodeResources[node] {
			resources += v - r.shape.Resources[k]
		}
		return accelerators, resources
	}
	// equivalentState is the exact post-placement state of a node, dimension by dimension. It is
	// the key the interchangeability pruning below is allowed to use.
	//
	// The scalar sum from leftover must never be used for this. Memory and storage are both
	// byte-valued and of the same magnitude, so a node with 2Gi of memory and no disk and a node
	// with no memory and 2Gi of disk produce the identical sum — the pruning then discarded one of
	// them as a duplicate of the other and lost every assignment that needed the discarded one,
	// reporting a placeable job as not fitting.
	equivalentState := func(r rank, node, nodeKey string) string {
		dimensions := make([]string, 0, len(nodeResources[node]))
		for k := range nodeResources[node] {
			dimensions = append(dimensions, k)
		}
		sort.Strings(dimensions)
		var b strings.Builder
		if r.shape.AcceleratorCount > 0 {
			fmt.Fprintf(&b, "a=%d;", byNode[node][nodeKey]-r.shape.AcceleratorCount)
		}
		for _, k := range dimensions {
			fmt.Fprintf(&b, "%s=%d;", k, nodeResources[node][k]-r.shape.Resources[k])
		}
		return b.String()
	}

	var place func(i int) bool
	place = func(i int) bool {
		if i == len(ranks) {
			return true
		}
		r := ranks[i]
		type candidate struct {
			node, key string
			accLeft   int64
			resLeft   int64
			state     string
		}
		candidates := make([]candidate, 0, len(r.nodes))
		for _, node := range r.nodes {
			nodeKey, ok := nodeFits(r, node)
			if !ok {
				continue
			}
			accLeft, resLeft := leftover(r, node, nodeKey)
			candidates = append(candidates, candidate{node: node, key: nodeKey, accLeft: accLeft, resLeft: resLeft, state: equivalentState(r, node, nodeKey)})
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			if candidates[a].accLeft != candidates[b].accLeft {
				return candidates[a].accLeft < candidates[b].accLeft
			}
			return candidates[a].resLeft < candidates[b].resLeft
		})
		// Two nodes that would be left in the same state are interchangeable for this rank, and
		// for every rank after it. Retrying the same failed subtree from an equivalent node is
		// what turns a homogeneous cluster into an exponential search.
		tried := make(map[string]bool)
		for _, c := range candidates {
			if tried[c.state] {
				continue
			}
			tried[c.state] = true
			if budget <= 0 {
				return false
			}
			budget--
			if r.shape.AcceleratorCount > 0 {
				byNode[c.node][c.key] -= r.shape.AcceleratorCount
			}
			subtractNodeResources(nodeResources[c.node], r.shape.Resources)
			used[r.job][c.node] = true
			plan[i] = rankAssignment{job: r.job, shape: r.shape, node: c.node, key: c.key}
			if place(i + 1) {
				return true
			}
			used[r.job][c.node] = false
			addNodeResources(nodeResources[c.node], r.shape.Resources)
			if r.shape.AcceleratorCount > 0 {
				byNode[c.node][c.key] += r.shape.AcceleratorCount
			}
			if budget <= 0 {
				return false
			}
		}
		return false
	}
	if !place(0) {
		return nil, false
	}
	return plan, true
}

func shapeResourceTotal(shape domain.NodeShape) int64 {
	var total int64
	for _, amount := range shape.Resources {
		total += amount
	}
	return total
}

func addNodeResources(available, used map[string]int64) {
	for key, amount := range used {
		available[key] += amount
	}
}

// qualifyingNodes is the sorted set of nodes from capacity that carry exp's required node labels.
func qualifyingNodes(capacity map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) []string {
	nodes := make([]string, 0, len(capacity))
	for node := range capacity {
		if labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	return nodes
}

// hardestNodeShape is the single node of exp that is hardest to place — most accelerators first,
// then the largest fungible request. Diagnostics that describe a job by one shape (the shortfall
// vector, and the preemption plan built from it) use this one, because covering the hardest node
// is what actually unblocks the job. Every shape is identical for an ungrouped job, so this is
// that job's per-node shape exactly as before.
func hardestNodeShape(exp *domain.Experiment) domain.NodeShape {
	var hardest domain.NodeShape
	var hardestTotal int64
	for _, shape := range exp.NodeShapes() {
		var total int64
		for _, amount := range shape.Resources {
			total += amount
		}
		if hardest.Resources == nil || shape.AcceleratorCount > hardest.AcceleratorCount ||
			(shape.AcceleratorCount == hardest.AcceleratorCount && total > hardestTotal) {
			hardest, hardestTotal = shape, total
		}
	}
	return hardest
}

func nodeHasRoom(available, needed map[string]int64) bool {
	if len(needed) == 0 {
		return true
	}
	// A node the cluster did not report resources for cannot be proven to have room for a job
	// that needs some. Reporting them is required (see clusteragentapi), so an absent entry is a
	// malformed report, not a default.
	if available == nil {
		return false
	}
	for key, amount := range needed {
		if available[key] < amount {
			return false
		}
	}
	return true
}

func subtractNodeResources(available, used map[string]int64) {
	for key, amount := range used {
		available[key] -= amount
	}
}

// addNodeResourceShortfall folds in what the fungible dimensions still lack once the job's ranks
// are laid out over the qualifying nodes. It walks the ranks hardest-first, consuming each node it
// places one on, and reports the *least* deficient node for the first rank that finds no host —
// least deficient because covering that one node lets that rank land, which is what has to be
// freed somewhere rather than everywhere.
//
// The walk has to consume capacity as it goes, because a job's ranks compete with each other. Only
// asking whether some node could host one rank called a job satisfied whenever a single node had
// room, even when the job needed several such nodes and the cluster had one: two ranks wanting 2Gi
// of disk apiece on a cluster holding 4Gi spread as 1+1+2 passes the cluster-level check, cannot
// be placed, and was reported as needing nothing at all — leaving preemption with no vector to
// cover and the disbalance evictor with no deficit to explain, so the job sat queued forever while
// the scheduler said it wanted nothing.
func addNodeResourceShortfall(out domain.Footprint, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) {
	shapes := exp.NodeShapes()
	if len(shapes) == 0 {
		return
	}
	// Hardest first, mirroring planPlacement: the rank with the fewest possible hosts is laid down
	// while the most nodes are still free, so a deficit reported here is one the real search would
	// also have hit rather than an artefact of an unlucky order.
	sort.SliceStable(shapes, func(a, b int) bool {
		if shapes[a].AcceleratorCount != shapes[b].AcceleratorCount {
			return shapes[a].AcceleratorCount > shapes[b].AcceleratorCount
		}
		return shapeResourceTotal(shapes[a]) > shapeResourceTotal(shapes[b])
	})

	// Clones: this is a diagnostic pass over the tick's live capacity maps and must not disturb
	// them for the admission decisions that follow.
	accelerators := cloneNodeCapacity(byNode)
	resources := cloneNodeCapacity(nodeResources)
	key := strings.ToLower(string(exp.AcceleratorType))
	distinct := requiresDistinctHosts(exp)
	occupied := map[string]bool{}

	nodes := make([]string, 0, len(resources))
	for node := range resources {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes) // deterministic reporting across ticks

	for _, shape := range shapes {
		var bestDeficit map[string]int64
		var bestTotal int64
		placed := ""

		for _, node := range nodes {
			if !labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
				continue
			}
			if distinct && occupied[node] {
				continue
			}
			// Only nodes that could host this rank's accelerators are candidates; where the
			// accelerators themselves are missing, the per-node accelerator shortfall reported by
			// preemptionShortfall already says so.
			if shape.AcceleratorCount > 0 && foldLookup(accelerators[node], key) < shape.AcceleratorCount {
				continue
			}
			deficit := map[string]int64{}
			var total int64
			for resource, need := range shape.Resources {
				if short := need - resources[node][resource]; short > 0 {
					deficit[resource] = short
					total += short
				}
			}
			if total == 0 {
				placed = node
				break
			}
			if bestDeficit == nil || total < bestTotal {
				bestDeficit, bestTotal = deficit, total
			}
		}

		if placed != "" {
			if shape.AcceleratorCount > 0 {
				nodeKey := foldMatchingKey(accelerators[placed], key)
				accelerators[placed][nodeKey] -= shape.AcceleratorCount
			}
			subtractNodeResources(resources[placed], shape.Resources)
			occupied[placed] = true
			continue
		}

		// This rank has nowhere to go. Report what the closest node lacks and stop: freeing that
		// much lets this rank land, and a later tick re-measures whatever remains.
		for resource, short := range bestDeficit {
			kind, ok := nodeResourceKind(resource)
			if !ok {
				continue
			}
			if k := (domain.ResourceKey{Kind: kind}); out[k] < short {
				out[k] = short
			}
		}
		return
	}
}

func nodeResourceKind(resource string) (domain.ResourceKind, bool) {
	switch resource {
	case domain.NodeResourceCPUMillicores:
		return domain.ResourceKindCPU, true
	case domain.NodeResourceMemoryBytes:
		return domain.ResourceKindMemory, true
	case domain.NodeResourceStorageBytes:
		return domain.ResourceKindStorage, true
	default:
		return "", false
	}
}

// clusterWithBestFit picks a target cluster for footprint among every configured cluster in
// avail (iterated in stable, sorted-by-name order for determinism):
//  1. the first cluster where footprint already Fits — a job that fits outright always beats
//     one that would need preemption; cluster name order is the tiebreak among those
//  2. otherwise, among the clusters that could ever host the request, the one with the smallest
//     total shortage (sum across dimensions of max(0, need-have)) — the best candidate for
//     preempt() to free room on
//
// "Could ever host it" is the load-bearing half of step 2, and it is not the same question as
// step 1's. A cluster that has never reported a dimension the job asks for — an accelerator
// flavor it does not own — has a shortage equal to the whole request, which ties with a cluster
// that owns exactly that hardware and is merely full of preemptible burst work. The tie used to
// go to whichever name sorted first, and when it went to the cluster without the hardware the
// guaranteed job was handed a preemption target where no victim could ever free what it needs:
// preemption is scoped to the chosen cluster, found nothing to take there, and the job stayed
// QUEUED tick after tick while the real hardware sat under evictable burst jobs.
//
// Returning "" when no cluster reports the request is the honest answer and needs no special
// case at the call sites: the absent footprint fails every fit check, so the job is marked
// capacity_unavailable, which is exactly what "no cluster on this platform has this hardware"
// means.
func clusterWithBestFit(avail map[string]domain.Footprint, footprint domain.Footprint) string {
	names := make([]string, 0, len(avail))
	for c := range avail {
		names = append(names, c)
	}
	sort.Strings(names)

	for _, c := range names {
		if domain.Fits(avail[c], footprint) {
			return c
		}
	}

	best := ""
	var bestShortage int64 = -1
	for _, c := range names {
		if !reportsEveryDimension(avail[c], footprint) {
			continue
		}
		var shortage int64
		for _, v := range shortfall(avail[c], footprint) {
			shortage += v
		}
		if bestShortage < 0 || shortage < bestShortage {
			bestShortage = shortage
			best = c
		}
	}
	return best
}

// reportsEveryDimension reports whether capacity has an entry for every dimension footprint asks
// for — present, not necessarily sufficient. Zero free of a flavor the cluster owns is a cluster
// preemption can unblock; no entry at all is a cluster that does not have the hardware, and no
// eviction there will ever produce it.
func reportsEveryDimension(capacity domain.Footprint, footprint domain.Footprint) bool {
	for key, need := range footprint {
		if need <= 0 {
			continue
		}
		if _, reported := capacity[key]; !reported {
			return false
		}
	}
	return true
}

// cloneNodeAvailByCluster deep-copies a cluster -> node -> resource capacity map, so a snapshot
// survives the in-place reservations admission makes as it walks the queue.
func cloneNodeAvailByCluster(byCluster map[string]map[string]map[string]int64) map[string]map[string]map[string]int64 {
	cloned := make(map[string]map[string]map[string]int64, len(byCluster))
	for cluster, byNode := range byCluster {
		cloned[cluster] = cloneNodeCapacity(byNode)
	}
	return cloned
}
