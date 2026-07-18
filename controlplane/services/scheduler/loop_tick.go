package scheduler

import (
	"context"
	"sort"
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

	// 1. Get available physical capacity, keyed by cluster then flavor name — a pooled
	// flavor-only total would hide which specific cluster has room, so a job could get
	// admitted against a combined number while the one cluster it actually lands on is full.
	gAvail, bAvail, err := l.workload.GetFlavorCapacity(ctx)
	if err != nil {
		return err
	}

	// 2. Subtract the footprint of every job currently holding physical capacity: SUBMITTED
	// and ADMITTED (in-flight, not yet observed RUNNING) plus RUNNING. gAvail/bAvail are two
	// views of the *same* shared physical pool per cluster (see LoopWorkloadClient.GetFlavorCapacity's
	// doc comment), so a unit held by a job of either tier is unavailable in both views —
	// preemption, not a capacity split, is what enforces the tier boundary. Subtracting only
	// within the holder's own tier (as this used to) left the other tier's view at the full
	// nominal capacity forever, so once anything was RUNNING the guaranteed pass never saw a
	// shortfall and never triggered preempt(). Each job's footprint is charged against its own
	// persisted ClusterName, not a combined pool.
	//
	// This footprint is skipped for cpuFlavor: unlike GPU flavors (a static nominal count),
	// GetFlavorCapacity's CPU number is a cluster-agent-reported live figure that is already
	// allocatable-minus-requested against real k8s Jobs — which already includes every
	// SUBMITTED/ADMITTED/RUNNING job's request. Subtracting their footprint again here would
	// double-count it and manufacture false scarcity.
	submitted, err := l.store.ListSubmittedExperiments(ctx)
	if err != nil {
		return err
	}
	admittedInFlight, err := l.store.ListAdmittedExperiments(ctx)
	if err != nil {
		return err
	}
	runningNow, err := l.store.ListRunningExperiments(ctx)
	if err != nil {
		return err
	}
	occupied := make([]*domain.Experiment, 0, len(submitted)+len(admittedInFlight)+len(runningNow))
	occupied = append(occupied, submitted...)
	occupied = append(occupied, admittedInFlight...)
	occupied = append(occupied, runningNow...)
	for _, exp := range occupied {
		flavor, need := admissionUnit(exp)
		if flavor == cpuFlavor {
			continue
		}
		// A job's cluster may no longer be configured (removed from clusters.yaml) — nothing
		// to subtract from in that case; its capacity simply isn't tracked any more.
		if _, ok := gAvail[exp.ClusterName]; !ok {
			continue
		}
		gAvail[exp.ClusterName][flavor] -= need
		if gAvail[exp.ClusterName][flavor] < 0 {
			gAvail[exp.ClusterName][flavor] = 0
		}
		bAvail[exp.ClusterName][flavor] -= need
		if bAvail[exp.ClusterName][flavor] < 0 {
			bAvail[exp.ClusterName][flavor] = 0
		}
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
	// capacity for this flavor existed even before this tick admitted anything) vs outranked
	// (capacity existed, but other guaranteed jobs earlier in this tick's sort order already
	// claimed it) — see #15 in competetors/SYNTHESIS_GAPS_AND_PLAN.md.
	gAvailInitial := cloneAvail(gAvail)

	for _, exp := range guaranteed {
		flavor, need := admissionUnit(exp)
		// A job already assigned a cluster (a retry after this tick previously claimed it, see
		// submitJob) stays pinned there; otherwise pick whichever configured cluster currently
		// has the most room for this flavor — if any cluster can fit it outright, that's the
		// one this always picks (see cluster's doc comment for why).
		cluster := exp.ClusterName
		if cluster == "" {
			cluster = clusterWithMostAvail(gAvail, flavor)
		}
		if need > gAvail[cluster][flavor] {
			// Doesn't fit on cluster — try to preempt that cluster's own burst jobs of the
			// same flavor to make room. Preemption is scoped to this one cluster: freeing a
			// burst job on a different cluster wouldn't make room for this job here.
			running, err := l.store.ListRunningExperiments(ctx)
			if err != nil {
				return err
			}
			burstRunning := filterTierFlavorCluster(running, domain.CapacityBurst, flavor, cluster)
			freed, err := l.preempt(ctx, need-gAvail[cluster][flavor], burstRunning)
			if err != nil {
				l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[cluster][flavor], gAvailInitial[cluster][flavor]))
				continue
			}
			if freed > 0 {
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "preempted").Add(float64(freed))
			}
			gAvail[cluster][flavor] += freed
			if need > gAvail[cluster][flavor] {
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
				l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(gAvail[cluster][flavor], gAvailInitial[cluster][flavor]))
				continue // not enough even after preemption
			}
		}
		if err := l.submitJob(ctx, exp, cluster); err != nil {
			l.logger.Error("submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			continue
		}
		gAvail[cluster][flavor] -= need
		// bAvail is the same shared physical pool's other view (see the capacity-accounting
		// comment on step 2 above) — without this, a job the burst pass admits later in this
		// same tick can still see the unit this guaranteed job just claimed as free and
		// double-book it, since bAvail was only synced against *pre-tick* occupancy.
		bAvail[cluster][flavor] -= need
		if bAvail[cluster][flavor] < 0 {
			bAvail[cluster][flavor] = 0
		}
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
	for _, flavors := range bAvail {
		for _, v := range flavors {
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
		flavor, need := admissionUnit(exp)
		cluster := exp.ClusterName
		if cluster == "" {
			cluster = clusterWithMostAvail(bAvail, flavor)
		}
		if need > bAvail[cluster][flavor] {
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			l.setNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(bAvail[cluster][flavor], bAvailInitial[cluster][flavor]))
			continue
		}
		if err := l.submitJob(ctx, exp, cluster); err != nil {
			l.logger.Error("submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			continue
		}
		bAvail[cluster][flavor] -= need
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
		l.setNotAdmittedReason(ctx, exp.ID, "")
	}

	// Reprioritize all queued jobs after each admission pass so the queue order
	// stays fresh (novelty shifts as jobs start/finish, age increases, etc.).
	// This runs synchronously in the same goroutine — single-threaded, no races.
	if l.reprioritizer != nil {
		if err := l.reprioritizer.RePrioritize(ctx); err != nil {
			l.logger.Warn("reprioritize after tick", zap.Error(err))
		}
	}

	return nil
}

// cloneAvail deep-copies a per-cluster-per-flavor availability map so a pre-tick snapshot
// isn't mutated by the tick's own admission bookkeeping.
func cloneAvail(avail map[string]map[string]int64) map[string]map[string]int64 {
	out := make(map[string]map[string]int64, len(avail))
	for cluster, flavors := range avail {
		cp := make(map[string]int64, len(flavors))
		for f, n := range flavors {
			cp[f] = n
		}
		out[cluster] = cp
	}
	return out
}

// clusterWithMostAvail returns the configured cluster with the most available capacity for
// flavor (ties broken by cluster name, ascending, for determinism). This is the whole
// placement policy: if any cluster can fit a job, the one with the most room can too, since
// it has at least as much available as every other candidate — so picking it is always at
// least as good as any other choice, without needing to search further.
func clusterWithMostAvail(avail map[string]map[string]int64, flavor string) string {
	best := ""
	var bestN int64 = -1
	names := make([]string, 0, len(avail))
	for c := range avail {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if n := avail[c][flavor]; n > bestN {
			bestN = n
			best = c
		}
	}
	return best
}
