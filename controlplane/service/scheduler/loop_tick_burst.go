package scheduler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

// burstPass admits the queued burst jobs in what the guaranteed pass left: fairness-weighted
// (least quota used first), then completion proximity, shortest job first. A burst job that does
// not fit waits; nothing is preempted for it.
//
// No cluster-wide "is there any capacity at all" shortcut here: a footprint's dimensions are
// different units (millicores, bytes, device counts), so summing them into one number answers no
// question anyone asked — RAM bytes dominate by orders of magnitude and the sum is positive
// whatever the accelerator situation is. Fit is decided per job, per dimension, by domain.Fits,
// which writes the same capacity_unavailable reason.
func (l *Loop) burstPass(ctx context.Context, t *tickState, queued []*domain.Experiment) {
	burst := filterTier(queued, domain.CapacityBurst)
	// Quota usage for fairness ordering, read after the guaranteed pass has claimed its own.
	burst, quotas := l.fetchQuotaMap(ctx, burst, &t.errs)
	sortBurst(burst, quotas, t.completion)
	// Interleave by agent so one agent's queue depth can't claim every unit of burst capacity that
	// frees up this tick ahead of another agent with fewer jobs waiting — see interleaveByAgent's
	// doc comment.
	burst = interleaveByAgent(burst)
	for _, exp := range burst {
		l.admitBurst(ctx, t, exp, quotas[quotaKey(exp.AgentID, exp.PlatformExperimentID)])
	}
}

func (l *Loop) admitBurst(ctx context.Context, t *tickState, exp *domain.Experiment, quota *domain.AgentQuota) {
	c := t.clusters
	persistedAcceleratorType := exp.AcceleratorType
	placed, ok := l.placeQueued(ctx, "burst", exp, t.bAvail, quota, c, &t.errs)
	if !ok {
		return
	}
	cluster, fp := placed.cluster, placed.fp
	fits := domain.Fits(t.bAvail[cluster], fp) && placement.TopologyFits(c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], exp)
	if !fits {
		l.burstWait(ctx, t, exp, placed)
		return
	}
	l.admit(ctx, t, "burst", exp, cluster, persistedAcceleratorType, fp, t.bAvail)
}

// burstWait is the burst job that does not fit where it was placed: it may run the disbalance
// eviction, and writes why it waits.
func (l *Loop) burstWait(ctx context.Context, t *tickState, exp *domain.Experiment, placed queuedPlacement) {
	c := t.clusters
	cluster, fp, misfit := placed.cluster, placed.fp, placed.misfit
	// Same shortage vector the guaranteed pass computes, for the same reason: "short {cpu: 12}"
	// tells an operator which dimension to fix, where a bare capacity_unavailable tells them only
	// that something, somewhere, did not fit.
	shortage := placement.PreemptionShortfall(t.bAvail[cluster], c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], exp, fp)
	// The guaranteed pass logs every skip ("guaranteed job needs preemption"); the burst pass had
	// no equivalent, so a burst job stuck QUEUED for many ticks left no trail at all — the actual
	// bAvail[cluster]/nodeAvail[cluster] values a tick computed could only be reconstructed after
	// the fact from Postgres/metrics archaeology, not read off the logs. cluster=="" (every
	// candidate accelerator type/cluster combination failed to resolve) is called out explicitly because
	// bAvail[""] is always the nil map, so the shortage vector below is the job's whole footprint
	// regardless of real capacity — worth distinguishing from a genuine per-dimension shortfall at
	// a glance.
	l.logger.Info("burst job not admitted this tick",
		zap.String("exp", exp.ID), zap.String("cluster", cluster), zap.Bool("cluster_unresolved", cluster == ""),
		zap.String("avail", footprintStr(t.bAvail[cluster])), zap.String("need", footprintStr(fp)),
		zap.String("shortage", footprintStr(shortage)))
	fitAtTickStart := domain.Fits(t.bAvailInitial[cluster], fp) &&
		placement.TopologyFits(c.nodeAvailAtTickStart[cluster], t.nodeResourcesAtTickStart[cluster], c.nodeLabels[cluster], exp)
	// A job that fit when the tick began lost its room to a higher rank and waits; nothing running
	// is evicted for it.
	if !fitAtTickStart && !t.disbalanceRan[cluster] {
		t.disbalanceRan[cluster] = true
		if err := l.evictDisbalanced(ctx, exp, cluster, t.bAvail[cluster], t.totalCapacity[cluster], c.nodeResourcesTotal[cluster], c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], fp); err != nil {
			l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
			t.errs = append(t.errs, fmt.Errorf("experiment %s: disbalance eviction on %s: %w", exp.ID, cluster, err))
		}
	}
	reason := misfit
	if reason == "" {
		reason = c.whenIdle.neverFitsReason(exp)
	}
	if reason == "" {
		reason = slotWaitReason(exp, cluster, c.nodeResourcesTotal[cluster], t.nodeResourcesAtTickStart[cluster], c.nodeLabels[cluster])
	}
	if reason == "" {
		reason = noNodeOfArchitectureReason(exp, cluster, c.nodeLabels[cluster])
	}
	if reason == "" {
		claimed, err := c.resolveCache.loadClaimed(ctx)
		if err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("list the jobs that hold capacity: %w", err))
			return
		}
		scalarFits := domain.Fits(t.bAvail[cluster], fp)
		freed := !scalarFits && !placement.HasSlots(c.nodeLabels[cluster]) && domain.Fits(t.desiredFree[cluster], fp)
		reason = notAdmittedReasonFor(fitAtTickStart, scalarFits, freed, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
		l.holdRoom(exp, cluster, fp, c, t.held, t.bAvail)
	}
	if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
		l.skipExperiment(&t.errs, exp, fmt.Errorf("mark not admitted: %w", err))
	}
}
