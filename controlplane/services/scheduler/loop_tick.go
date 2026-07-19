package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// tick runs one full admission pass. Single-threaded — no concurrent ticks.
func (l *Loop) tick(ctx context.Context) error {
	if !l.ticking.CompareAndSwap(false, true) {
		panic("scheduler: tick() re-entered concurrently — capacity accounting is not safe under concurrency, see Loop.ticking")
	}
	defer l.ticking.Store(false)

	start := time.Now()
	defer func() { obsmetrics.AdmissionTickDuration.Observe(time.Since(start).Seconds()) }()

	// Reprioritize all queued jobs after every tick, regardless of which pass admitted or
	// skipped anything — deferred so every exit path (including the guaranteed-only and
	// no-burst-capacity early returns below) actually runs it, not just the path that reaches
	// the bottom of the function. Queue order needs to stay fresh (novelty shifts as jobs
	// start/finish, age increases, etc.) even on a tick where nothing was queued for burst.
	if l.reprioritizer != nil {
		defer func() {
			if err := l.reprioritizer.RePrioritize(ctx); err != nil {
				l.logger.Warn("reprioritize after tick", zap.Error(err))
			}
		}()
	}

	// 1. Get available physical capacity as a canonical domain.Footprint per cluster — a pooled
	// cluster-less total would hide which specific cluster has room, so a job could get admitted
	// against a combined number while the one cluster it actually lands on is full.
	gAvail, bAvail, err := l.workload.GetFlavorCapacity(ctx)
	if err != nil {
		return err
	}

	// 2. RUNNING jobs need no separate subtraction here. Every capacity dimension
	// GetFlavorCapacity reports (CPU, accelerator, RAM, storage) is now a live, cluster-agent-
	// computed allocatable-minus-requested number counted only against pods actually assigned
	// to a node (see workload.GetLiveCPUCapacity/GetLiveAcceleratorCapacity/GetLiveRAMCapacity/
	// GetLiveStorageCapacity's doc comments) — a RUNNING job's pod is scheduled by definition,
	// so its footprint is already reflected in every one of those live numbers. Subtracting it
	// again here would double-count it and manufacture false scarcity, the same bug this used
	// to carve a CPU-only exception for; now that every dimension is live and assigned-pod-
	// scoped, the exception covers the whole footprint, so the loop itself is dead weight —
	// removed rather than kept as an always-empty no-op (matches important.md's "less retained
	// machinery" principle).

	// 2b. Close the pending-pod race: subtract every durably reserved-but-not-yet-confirmed-
	// running job's footprint (see pending_capacity_reservations' schema comment), across ALL
	// dimensions including CPU. A reservation only exists between MarkSubmitted and
	// job_watcher observing the pod RUNNING (see submitJob/onRunning), so by construction it is
	// never yet reflected in live capacity — subtracting it here is never a double-count. This
	// is what actually closes SCHEDULING_GENERALIZATION_PLAN.md's "durable pending-capacity
	// reservations" cross-cutting fix: a second tick before the cluster-agent has created the
	// pod now sees this capacity as already claimed instead of trusting a stale point-in-time
	// live number, for every admission dimension, not just the ones that used to have a
	// separate accelerator-only subtraction.
	pendingByCluster, err := l.store.ListPendingReservationsByCluster(ctx)
	if err != nil {
		return err
	}
	for cluster, fp := range pendingByCluster {
		if _, ok := gAvail[cluster]; !ok {
			continue
		}
		subtractFootprint(gAvail[cluster], fp)
		subtractFootprint(bAvail[cluster], fp)
	}

	// 3. Get all QUEUED experiments.
	queued, err := l.store.ListQueuedExperiments(ctx)
	if err != nil {
		return err
	}

	// 3a. Enforce the summary gate: skip agents who have COMPLETED experiments without a
	// summary. The gate runs at POST /experiments submission time, but a batch of jobs
	// submitted before any run completes will all be in QUEUED already. We re-check here
	// so that completion of one job pauses the rest until summaries are written.
	summaryBlocked := map[string]bool{} // key: agentID+"/"+platformExpID
	filtered := queued[:0]
	for _, exp := range queued {
		if exp.PlatformExperimentID == "" {
			filtered = append(filtered, exp)
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
			l.logger.Warn("summary gate check failed, allowing experiment",
				zap.String("exp", exp.ID), zap.Error(err))
			blocked = false
		}
		summaryBlocked[key] = blocked
		if !blocked {
			filtered = append(filtered, exp)
		} else {
			l.setNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedSummaryGate)
		}
	}
	queued = filtered

	// 4. Guaranteed pass: FIFO by age bucket, quota-ratio tiebreak within a bucket, then
	// completion proximity DESC, then shortest job first.
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	sortGuaranteed(guaranteed, l.fetchQuotaMap(ctx, guaranteed), l.guaranteedFairnessWindow)

	// Snapshot pre-tick availability so a skip can be classified: capacity_unavailable (no
	// capacity for this job existed even before this tick admitted anything) vs outranked
	// (capacity existed, but other guaranteed jobs earlier in this tick's sort order already
	// claimed it) — see #15 in competetors/SYNTHESIS_GAPS_AND_PLAN.md.
	gAvailInitial := cloneAvail(gAvail)

	for _, exp := range guaranteed {
		// A job already assigned a cluster (a retry after this tick previously claimed it, see
		// submitJob) stays pinned there — its flavor was already resolved on the attempt that
		// pinned it, so just recompute its footprint under that flavor; otherwise pick a
		// (cluster, flavor) pair among the requested type and any AcceptableAcceleratorTypes where the
		// job's whole footprint fits jointly across every dimension it requests — see
		// resolveClusterAndFootprint's and clusterWithBestFit's doc comments for the exact policy.
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(gAvail, exp)
		}
		if !domain.Fits(gAvail[cluster], fp) {
			// Doesn't fit on cluster — try to preempt that cluster's own burst jobs to make
			// room. Preemption is scoped to this one cluster: freeing a burst job on a
			// different cluster wouldn't make room for this job here.
			running, err := l.store.ListRunningExperiments(ctx)
			if err != nil {
				return err
			}
			burstRunning := filterTierCluster(running, domain.CapacityBurst, cluster)
			shortage := shortfall(gAvail[cluster], fp)
			l.logger.Info("guaranteed job needs preemption",
				zap.String("exp", exp.ID), zap.String("cluster", cluster),
				zap.String("avail", footprintStr(gAvail[cluster])), zap.String("need", footprintStr(fp)),
				zap.String("shortage", footprintStr(shortage)),
				zap.Int("burst_candidates", len(burstRunning)))
			freed, err := l.preempt(ctx, shortage, burstRunning)
			if err != nil {
				l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[cluster], gAvailInitial[cluster], fp))
				continue
			}
			if len(freed) > 0 {
				var freedTotal int64
				for _, v := range freed {
					freedTotal += v
				}
				if freedTotal > 0 {
					obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "preempted").Add(float64(freedTotal))
				}
			}
			if gAvail[cluster] == nil {
				gAvail[cluster] = domain.NewFootprint()
			}
			gAvail[cluster].AddFootprint(freed)
			l.logger.Info("preemption result",
				zap.String("exp", exp.ID), zap.String("freed", footprintStr(freed)),
				zap.String("avail_after", footprintStr(gAvail[cluster])),
				zap.Bool("fits_now", domain.Fits(gAvail[cluster], fp)))
			if !domain.Fits(gAvail[cluster], fp) {
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[cluster], gAvailInitial[cluster], fp))
				continue // not enough even after preemption
			}
		}
		if err := l.submitJob(ctx, exp, cluster); err != nil {
			l.logger.Error("submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			continue
		}
		subtractFootprint(gAvail[cluster], fp)
		// bAvail is the same shared physical pool's other view (see the capacity-accounting
		// comment on step 2 above) — without this, a job the burst pass admits later in this
		// same tick can still see the unit this guaranteed job just claimed as free and
		// double-book it, since bAvail was only synced against *pre-tick* occupancy.
		subtractFootprint(bAvail[cluster], fp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "admitted").Inc()
		l.setNotAdmittedReason(ctx, exp.ID, "")
	}

	// 5. Burst pass: fairness-weighted (least quota used first), then completion proximity, shortest job first.
	burst := filterTier(queued, domain.CapacityBurst)
	if len(burst) == 0 {
		return nil
	}

	// Check if any burst capacity exists at all, on any cluster.
	var totalBAvail int64
	for _, fp := range bAvail {
		for _, v := range fp {
			totalBAvail += v
		}
	}
	if totalBAvail <= 0 {
		for _, exp := range burst {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			l.setNotAdmittedReason(ctx, exp.ID, domain.NotAdmittedCapacityUnavailable)
		}
		return nil
	}

	// Fetch quota usage for fairness ordering.
	quotaMap := l.fetchQuotaMap(ctx, burst)
	sortBurst(burst, quotaMap)

	bAvailInitial := cloneAvail(bAvail)

	for _, exp := range burst {
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(bAvail, exp)
		}
		if !domain.Fits(bAvail[cluster], fp) {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(bAvail[cluster], bAvailInitial[cluster], fp))
			continue
		}
		if err := l.submitJob(ctx, exp, cluster); err != nil {
			l.logger.Error("submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			continue
		}
		subtractFootprint(bAvail[cluster], fp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
		l.setNotAdmittedReason(ctx, exp.ID, "")
	}

	return nil
}

// footprintStr renders a Footprint for logging — domain.Footprint's struct-keyed map can't be
// marshalled by zap.Any (JSON object keys must be strings), so every call site that wants to log
// one needs this instead.
func footprintStr(fp domain.Footprint) string {
	parts := make([]string, 0, len(fp))
	for k, v := range fp {
		parts = append(parts, fmt.Sprintf("%s:%s=%d", k.Kind, k.Flavor, v))
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}

// subtractFootprint subtracts fp from avail in place, dimension by dimension, clamped at zero
// per dimension (capacity never goes negative in the maps callers read back). No-op if avail is
// nil (cluster not present in the capacity map).
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

// cloneAvail deep-copies a per-cluster availability map so a pre-tick snapshot isn't mutated by
// the tick's own admission bookkeeping.
func cloneAvail(avail map[string]domain.Footprint) map[string]domain.Footprint {
	out := make(map[string]domain.Footprint, len(avail))
	for cluster, fp := range avail {
		cp := make(domain.Footprint, len(fp))
		for k, v := range fp {
			cp[k] = v
		}
		out[cluster] = cp
	}
	return out
}

// candidateAcceleratorTypes returns the flavors admission should try for exp, in preference order:
// the originally requested exp.AcceleratorType first, then any distinct AcceptableAcceleratorTypes. AcceleratorCount is
// already flavor-independent (it's the job's total footprint, fixed at submission — see
// Experiment.Footprint's doc comment), so trying an alternate only changes which accelerator key
// the footprint is keyed under, nothing else.
func candidateAcceleratorTypes(exp *domain.Experiment) []domain.AcceleratorType {
	types := []domain.AcceleratorType{exp.AcceleratorType}
	seen := map[domain.AcceleratorType]bool{exp.AcceleratorType: true}
	for _, t := range exp.Job.AcceptableAcceleratorTypes {
		if !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	return types
}

// resolveClusterAndFootprint picks a concrete (cluster, flavor) pair for a job with no cluster
// pinned yet, trying every candidate flavor (requested type first) and returning the first one
// that fits outright on some cluster — see candidateAcceleratorTypes. This is what lets a job whose
// requested flavor is saturated still land on a free AcceptableAcceleratorTypes alternative instead of
// sitting QUEUED with idle capacity elsewhere (findings.md's "acceptable_accelerator_types cannot be
// scheduled correctly"). If no candidate fits outright, falls back to the originally requested
// flavor's own best-fit cluster (possibly needing preemption, possibly "") so the caller's
// existing preemption path still runs — this fix only covers the outright-fit case; preemption
// remains scoped to the originally requested flavor.
func resolveClusterAndFootprint(avail map[string]domain.Footprint, exp *domain.Experiment) (string, domain.Footprint) {
	requested := exp.AcceleratorType
	var fallbackCluster string
	var fallbackFP domain.Footprint
	for i, t := range candidateAcceleratorTypes(exp) {
		exp.AcceleratorType = t
		fp := exp.Footprint()
		cluster := clusterWithBestFit(avail, fp)
		if i == 0 {
			fallbackCluster, fallbackFP = cluster, fp
		}
		if cluster != "" && domain.Fits(avail[cluster], fp) {
			return cluster, fp
		}
	}
	exp.AcceleratorType = requested
	return fallbackCluster, fallbackFP
}

// clusterWithBestFit picks a target cluster for footprint among every configured cluster in
// avail (iterated in stable, sorted-by-name order for determinism):
//  1. the first cluster where footprint already Fits — a job that fits outright always beats
//     one that would need preemption, and among clusters it already fits on, cluster name order
//     is as good a deterministic tie-break as any (this is a resource-fit predicate, not a
//     load-balancing one).
//  2. otherwise, the cluster with the smallest total shortage (sum across dimensions of
//     max(0, need-have)) — the best candidate for preempt() to try freeing room on, stated
//     explicitly as "fewest units to free," not implied by an ad hoc "most available" scalar
//     comparison the way the old single-dimension version was.
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
