package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// preemptionState is what the guaranteed pass reads once and keeps across the jobs it cannot fit.
type preemptionState struct {
	// Preemption candidates are read once per tick rather than once per job that fails to fit —
	// a saturated cluster with a deep guaranteed queue otherwise issues one full scan per queued
	// job, every tick. It is re-read after a preempt, because preempt requeues its victims and
	// the next candidate must not consider them running.
	running []*domain.Experiment
	loaded  bool
	// Per cluster of slots, the slots being left that no waiting job has been promised yet.
	vacating map[string]map[string]map[string]int64
}

// guaranteedPass admits the queued guaranteed jobs: FIFO by age bucket, quota-ratio tiebreak
// within a bucket, then completion proximity DESC, then shortest job first. A job that does not
// fit is bet on a cluster that scales up, or makes room by preemption, or waits.
func (l *Loop) guaranteedPass(ctx context.Context, t *tickState, queued []*domain.Experiment) {
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	guaranteed, quotas := l.fetchQuotaMap(ctx, guaranteed, &t.errs)
	sortGuaranteed(guaranteed, quotas, t.completion, l.guaranteedFairnessWindow)
	// A backlog one agent queued at once is older than every job the others queue after it, and
	// by age alone it took each core that freed until it drained.
	guaranteed = interleaveByAgent(guaranteed)
	p := &preemptionState{vacating: map[string]map[string]map[string]int64{}}
	for _, exp := range guaranteed {
		l.admitGuaranteed(ctx, t, p, exp, quotas[quotaKey(exp.AgentID, exp.PlatformExperimentID)])
	}
}

func (l *Loop) admitGuaranteed(ctx context.Context, t *tickState, p *preemptionState, exp *domain.Experiment, quota *domain.AgentQuota) {
	c := t.clusters
	// What the row says right now, captured before the placement search mutates
	// exp.AcceleratorType to communicate its choice. submitJob needs both to know whether it has a
	// new flavor to persist.
	persistedFlavor := exp.AcceleratorType
	placed, ok := l.placeQueued(ctx, "guaranteed", exp, t.gAvail, quota, c, &t.errs)
	if !ok {
		return
	}
	cluster, fp, betFlavor, misfit := placed.cluster, placed.fp, placed.betFlavor, placed.misfit
	// Which of the two fit checks failed decides whether preemption can help at all, so they are
	// evaluated separately rather than or-ed into one condition.
	scalarFits := domain.Fits(t.gAvail[cluster], fp)
	liveFits := scalarFits && placement.TopologyFits(c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], exp)
	if exp.EnvironmentImage != "" && (misfit != "" || !liveFits) {
		l.waitForPush(ctx, "guaranteed", t.environments, exp, &t.errs)
		return
	}
	// Speculative submit (autoscaler.md): before spending a guaranteed job's preemption power, try
	// every autoscaler-enabled cluster this job hasn't already failed over from. A candidate has
	// no live capacity by definition — the SUBMITTED row itself is the scale-up request the native
	// autoscaler reacts to. Within one priority live fit wins over speculating, and speculating
	// anywhere wins over preempting a burst job. Across priorities the higher cluster wins: a job
	// that fits live on a lower cluster first asks a scaling cluster its org ranked above it, and
	// comes back to the lower one when that bet times out (job_watcher's scale-up deadline) and
	// the cluster is excluded for it.
	var bets []string
	var provenFitByCluster map[string]bool
	if !liveFits || (exp.EnvironmentImage == "" && scalesAbove(cluster, exp, t.autoscalerEnabled, t.connected, c.registered)) {
		chosenFlavor := exp.AcceleratorType
		if liveFits {
			// The flavor the lower cluster fit at is that cluster's. The bet is for the one the job
			// is queued at, as it is when nothing fits.
			exp.AcceleratorType = betFlavor
		}
		var err error
		bets, provenFitByCluster, err = l.speculativeCandidates(ctx, c.resolveCache, exp, t.autoscalerEnabled, t.connected, c.clusterIDs, c.multiNode, c.registered, c.nodeAvailAtTickStart, c.nodeResourcesTotal, c.nodeLabels, t.speculativeFootprint, t.gAvail)
		if err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("speculative candidates: %w", err))
			return
		}
		if liveFits {
			if bets = above(bets, cluster, c.registered); len(bets) == 0 {
				exp.AcceleratorType = chosenFlavor
			}
		}
	}
	switch {
	case len(bets) > 0:
		l.submitSpeculative(ctx, t, exp, cluster, bets[0], persistedFlavor, provenFitByCluster[bets[0]])
	case !liveFits:
		l.makeRoom(ctx, t, p, exp, placed, scalarFits)
	default:
		// bAvail is the same shared pool's other view — without claiming from it too, the burst
		// pass later in this tick could still see this unit as free and double-book it.
		l.admit(ctx, t, "guaranteed", exp, cluster, persistedFlavor, fp, t.gAvail, t.bAvail, t.desiredFree)
	}
}

// submitSpeculative submits exp to specCluster, a cluster that scales up and has no room for it
// yet, where cluster is where the job was placed live.
func (l *Loop) submitSpeculative(ctx context.Context, t *tickState, exp *domain.Experiment, cluster, specCluster string, persistedFlavor domain.AcceleratorType, provenFit bool) {
	c := t.clusters
	// The resolution in placeQueued ran against whichever cluster looked live-fit-eligible before
	// this job fell through to speculation -- a different cluster than specCluster, or none at
	// all if every candidate failed that pre-check. Re-resolve against the actual speculative
	// target now that it's chosen: a stale resolution would persist a slot size of the wrong
	// cluster.
	if specCluster != cluster {
		resolvedJob, specMisfit, err := l.resolveClusterLocalResources(ctx, c.resolveCache, exp, specCluster, c.nodeResourcesTotal[specCluster], c.nodeAvailAtTickStart[specCluster], c.nodeResources[specCluster], c.nodeLabels[specCluster])
		if err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("resolve cluster-local resources for speculative target: %w", err))
			return
		}
		if specMisfit != "" {
			// This job's own request is above its share on the speculative target or no slot there
			// takes it. Nothing else to try this tick.
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			if err := l.markNotAdmitted(ctx, exp, specMisfit); err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			return
		}
		exp.ResolvedJob = resolvedJob
	}
	if err := l.submitJobTo(ctx, exp, specCluster, c.clusterIDs[specCluster], persistedFlavor, true, provenFit); err != nil {
		l.logger.Log(submitErrorLevel(err), "speculative submit", zap.String("exp", exp.ID), zap.String("cluster", specCluster), zap.Error(err))
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
		reason := notAdmittedReasonForSubmitError(err)
		if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("mark not admitted: %w", err))
		}
		return
	}
	obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "submitted").Inc()
	obsmetrics.SpeculativeSubmitsTotal.Inc()
	t.speculativeFootprint[specCluster] += exp.AcceleratorCount
	// The claimed footprint is not subtracted from gAvail/bAvail/nodeAvail here: a speculative
	// claim has no live node to subtract from, and GetFlavorCapacity already carries desired-free
	// negative for this cluster starting next tick via SumDesiredFootprintByCluster. Nothing else
	// in this tick reads gAvail[specCluster] again for a decision this job's SUBMITTED row should
	// have already influenced.
}

// makeRoom is the guaranteed job that does not fit live on the cluster it was placed on and has
// no bet to make: it preempts that cluster's own burst jobs, or skips preempting for capacity
// already on its way, or runs the disbalance eviction, and writes why it waits.
func (l *Loop) makeRoom(ctx context.Context, t *tickState, p *preemptionState, exp *domain.Experiment, placed queuedPlacement, scalarFits bool) {
	c := t.clusters
	cluster, fp, misfit := placed.cluster, placed.fp, placed.misfit
	// waitingForScaleUp: this cluster's own desired-free already went negative in this job's
	// accelerator dimension (a SUBMITTED row is already outstanding here, possibly this same job a
	// moment ago). Preempting a burst job to cover that shortage would be a wrong eviction for a
	// shortage the incoming node is already going to fill; the deadline in job_watcher_scan.go
	// bounds the wait (autoscaler.md's skip-preemption rule).
	waitingForScaleUp := t.autoscalerEnabled[cluster] && negativeInDimension(t.gAvail[cluster], exp.AcceleratorType)
	// Preempting is scoped to this cluster since freeing a burst job elsewhere wouldn't help here.
	if !p.loaded {
		running, err := l.store.ListRunningExperiments(ctx)
		if err != nil {
			// Without the running set there is no tier-safe preemption plan to make, and no basis
			// for the disbalance pass either — both reason about what is running. Skip this job
			// rather than abandon the tick, and terminate nothing on state we could not read.
			l.skipExperiment(&t.errs, exp, fmt.Errorf("list running for preemption: %w", err))
			return
		}
		p.running, p.loaded = running, true
	}
	burstRunning := filterTierCluster(p.running, domain.CapacityBurst, cluster)
	shortage := placement.PreemptionShortfall(t.gAvail[cluster], c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], exp, fp)
	// With no burst job to preempt this is a full cluster and a job waiting its turn, every tick
	// until capacity frees: the row's not_admitted_reason says so once.
	level := zapcore.DebugLevel
	if len(burstRunning) > 0 {
		level = zapcore.InfoLevel
	}
	l.logger.Log(level, "guaranteed job needs preemption",
		zap.String("exp", exp.ID), zap.String("cluster", cluster),
		zap.String("avail", footprintStr(t.gAvail[cluster])), zap.String("need", footprintStr(fp)),
		zap.String("shortage", footprintStr(shortage)),
		zap.Int("burst_candidates", len(burstRunning)))
	// The job fits once the platform jobs already told to stop are gone: earlier requeues freed its
	// capacity, and taking more victims while their pods terminate takes one per tick.
	//
	// On a cluster of slots both questions are asked per slot, of the resolver that sized the job:
	// the total is covered by a free slot of another shape, which never takes it.
	slots := placement.HasSlots(c.nodeLabels[cluster])
	totalCovers := scalarFits && !slots
	promised := !slots && !scalarFits && domain.Fits(t.desiredFree[cluster], fp)
	leaving := ""
	if slots {
		if p.vacating[cluster] == nil {
			claimed, err := c.resolveCache.loadClaimed(ctx)
			if err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("list claimed for vacating slots: %w", err))
				return
			}
			p.vacating[cluster] = placement.VacatingSlots(cluster, c.nodeResourcesTotal[cluster], t.nodeResourcesAtTickStart[cluster], c.nodeLabels[cluster], claimed)
		}
		if node, free, _ := placement.SlotFor(exp, cluster, c.nodeResourcesTotal[cluster], p.vacating[cluster], c.nodeLabels[cluster]); free {
			leaving, promised = c.nodeLabels[cluster][node][domain.NodeSlotLabel], true
			delete(p.vacating[cluster], node)
		}
	}
	var committed bool
	var err error
	switch {
	case totalCovers:
		// A preemption here would repeat every tick while its victim's pod terminates: the total
		// already covers the job, so nothing below reads the capacity as promised.
		l.logger.Info("skipping preemption: cluster has the capacity, the layout does not fit",
			zap.String("exp", exp.ID), zap.String("cluster", cluster))
	case waitingForScaleUp:
		l.logger.Info("skipping preemption: cluster is autoscaler-enabled and already waiting for scale-up",
			zap.String("exp", exp.ID), zap.String("cluster", cluster))
	case promised:
		l.logger.Info("skipping preemption: the capacity is already being freed",
			zap.String("exp", exp.ID), zap.String("cluster", cluster))
	default:
		committed, err = l.preempt(ctx, cluster, t.gAvail[cluster], c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeResourcesTotal[cluster], c.nodeLabels[cluster], burstRunning, exp)
		if err != nil {
			l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
			t.errs = append(t.errs, fmt.Errorf("experiment %s: preempt on %s: %w", exp.ID, cluster, err))
		}
	}
	// A guaranteed job whose capacity is being freed holds what is free against the burst pass, or
	// burst refills the room and the job never fits. On a cluster of slots the hold is the slot
	// itself, taken until its instance leaves: the total is the room of other slots, which never
	// take this job.
	if !slots && (promised || committed) {
		placement.SubtractFootprint(t.bAvail[cluster], fp)
	}
	if promised {
		placement.SubtractFootprint(t.desiredFree[cluster], fp)
	}
	// Only a preempt that actually requeued victims makes the running set stale. Marking it stale
	// unconditionally cost a full re-read for every blocked job behind it on a saturated cluster,
	// where preempt most often finds nothing to take.
	if committed {
		p.loaded = false
	}
	// Three reasons this tick's disbalance pass may not run for this job, all of them "the
	// evidence for terminating live work is not there":
	//   - preempt already committed a plan covering the whole shortage: the capacity is on its way
	//     back and the deficit is no longer outstanding (availability is not decremented here, so
	//     re-reading it would double-count the same shortage).
	//   - preempt errored: whatever it could not read or commit, we do not get to answer a failed
	//     tier-safe remedy with a more destructive one.
	//   - the pass already ran for this cluster this tick: it is a cluster-level verdict about
	//     stranded accelerators, not a per-queued-job one. Running it once per blocked job against
	//     unchanged availability evicts a fresh victim each time.
	if err == nil && !committed && !promised && !t.disbalanceRan[cluster] {
		t.disbalanceRan[cluster] = true
		if err := l.evictDisbalanced(ctx, exp, cluster, t.gAvail[cluster], t.totalCapacity[cluster], c.nodeResourcesTotal[cluster], c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], fp); err != nil {
			l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
			t.errs = append(t.errs, fmt.Errorf("experiment %s: disbalance eviction on %s: %w", exp.ID, cluster, err))
		}
	}
	obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
	t.backlog.record(c.start, cluster, "guaranteed", exp, shortage)
	noOffer := noOfferReason(exp, cluster, c.nodeResourcesTotal[cluster], t.nodeResourcesAtTickStart[cluster], c.nodeLabels[cluster])
	neverFits := c.whenIdle.neverFitsReason(exp)
	reason := ""
	switch {
	case leaving != "":
		reason = waitingForSlotReason(cluster, leaving)
	case waitingForScaleUp:
		reason = waitingForScaleUpReason(cluster)
	case allSpeculativeCandidatesTried(exp, t.autoscalerEnabled, t.connected, c.clusterIDs, c.registered, l.triedClusterTTL):
		reason = noScalableCapacityReason()
	case misfit != "":
		reason = misfit
	case neverFits != "":
		reason = neverFits
	case noOffer != "":
		reason = noOffer
	default:
		fitAtTickStart := domain.Fits(t.gAvailInitial[cluster], fp) &&
			placement.TopologyFits(c.nodeAvailAtTickStart[cluster], t.nodeResourcesAtTickStart[cluster], c.nodeLabels[cluster], exp)
		claimed, err := c.resolveCache.loadClaimed(ctx)
		if err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("list the jobs that hold capacity: %w", err))
			return
		}
		reason = notAdmittedReasonFor(fitAtTickStart, totalCovers, promised, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
		l.holdRoom(exp, cluster, fp, c, t.held, t.gAvail, t.bAvail)
	}
	if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
		l.skipExperiment(&t.errs, exp, fmt.Errorf("mark not admitted: %w", err))
	}
}
