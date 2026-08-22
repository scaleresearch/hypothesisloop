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
	gAvailInitial := cloneAvail(gAvail)

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
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(gAvail, nodeAvail, nodeResources, nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(guaranteedQuotas[quotaKey(exp.AgentID, exp.PlatformExperimentID)], exp) {
			exp.AcceleratorType = exp.Job.AcceleratorType
			fp = exp.Footprint()
			cluster = clusterWithBestFit(gAvail, fp)
		}
		if !domain.Fits(gAvail[cluster], fp) || !topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp) {
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
			committed, err := l.preempt(ctx, shortage, burstRunning, exp)
			if err != nil {
				l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
			}
			runningLoaded = false
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
				if err := l.evictDisbalanced(ctx, exp, cluster, gAvail[cluster], totalCapacity[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			fitAtTickStart := domain.Fits(gAvailInitial[cluster], fp) &&
				topologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, notAdmittedReasonFor(fitAtTickStart, fp, shortage)); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Error("submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			reason := domain.NotAdmittedWorkloadCreation
			if errors.Is(err, errAdmissionCapacityChanged) {
				reason = domain.NotAdmittedCapacityUnavailable
			}
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
	bAvailInitial := cloneAvail(bAvail)

	for _, exp := range burst {
		persistedFlavor := exp.AcceleratorType
		cluster := exp.ClusterName
		var fp domain.Footprint
		if cluster != "" {
			fp = exp.Footprint()
		} else {
			cluster, fp = resolveClusterAndFootprint(bAvail, nodeAvail, nodeResources, nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)], exp) {
			exp.AcceleratorType = exp.Job.AcceleratorType
			fp = exp.Footprint()
			cluster = clusterWithBestFit(bAvail, fp)
		}
		if !domain.Fits(bAvail[cluster], fp) || !topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp) {
			// Same shortage vector the guaranteed pass computes, for the same reason: "short
			// {cpu: 12}" tells an operator which dimension to fix, where a bare
			// capacity_unavailable tells them only that something, somewhere, did not fit.
			shortage := preemptionShortfall(bAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
			if !disbalanceRan[cluster] {
				disbalanceRan[cluster] = true
				if err := l.evictDisbalanced(ctx, exp, cluster, bAvail[cluster], totalCapacity[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
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
			reason := domain.NotAdmittedWorkloadCreation
			if errors.Is(err, errAdmissionCapacityChanged) {
				reason = domain.NotAdmittedCapacityUnavailable
			}
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		subtractFootprint(bAvail[cluster], fp)
		reservePlacement(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
	}

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
	if exp.Job.AcceleratorCount <= 0 {
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

	perRank := int64(exp.Job.AcceleratorCount)
	var missing int64
	if requiresDistinctHosts(exp) {
		for rank := 0; rank < exp.Job.Nodes(); rank++ {
			available := int64(0)
			if rank < len(capacities) {
				available = capacities[rank]
			}
			if available < perRank {
				missing += perRank - available
			}
		}
	} else {
		for rank := 0; rank < exp.Job.Nodes(); rank++ {
			if len(capacities) == 0 {
				missing += perRank
				continue
			}
			sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })
			if capacities[0] < perRank {
				missing += perRank - capacities[0]
				capacities[0] = 0
			} else {
				capacities[0] -= perRank
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
func resolveClusterAndFootprint(avail map[string]domain.Footprint, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, domain.Footprint) {
	if exp.Job.AcceleratorCount <= 0 {
		fp := exp.Footprint()
		return clusterWithBestFit(avail, fp), fp
	}

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
		clusters := make([]string, 0, len(avail))
		for name := range avail {
			clusters = append(clusters, name)
		}
		sort.Strings(clusters)
		for _, candidate := range clusters {
			if domain.Fits(avail[candidate], fp) && topologyFits(nodeAvail[candidate], nodeResources[candidate], nodeLabels[candidate], exp) {
				return candidate, fp
			}
		}
	}
	exp.AcceleratorType = requested
	return fallbackCluster, fallbackFP
}

func requiresDistinctHosts(exp *domain.Experiment) bool {
	if exp.Job.Nodes() <= 1 || exp.Job.AcceleratorCount <= 0 {
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
	remaining := cloneNodeCapacity(byNode)
	remainingResources := cloneNodeCapacity(nodeResources)
	for _, exp := range desired {
		if !reservePlacement(remaining, remainingResources, labelsByNode, exp) {
			return false
		}
	}
	return reservePlacement(remaining, remainingResources, labelsByNode, candidate)
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
func reservePlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	perRank := perRankNodeResources(exp)
	if exp.Job.AcceleratorCount <= 0 {
		// No accelerator to anchor it to a node, but it still has to land somewhere with room.
		return reserveAnyNode(nodeResources, labelsByNode, exp, perRank)
	}
	key := string(exp.AcceleratorType)
	nodes := make([]string, 0, len(byNode))
	for node := range byNode {
		if labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	used := make(map[string]bool)
	for rank := 0; rank < exp.Job.Nodes(); rank++ {
		selected := ""
		var nodeKey string
		for _, node := range nodes {
			if requiresDistinctHosts(exp) && used[node] {
				continue
			}
			// byNode retains each device's own raw driver-reported casing (see
			// domain.AcceleratorType.MatchesLabels re: casing) — find the matching key
			// case-insensitively rather than assuming it equals key verbatim.
			nodeKey = foldMatchingKey(byNode[node], key)
			if byNode[node][nodeKey] < int64(exp.Job.AcceleratorCount) {
				continue
			}
			if !nodeHasRoom(nodeResources[node], perRank) {
				continue
			}
			selected = node
			break
		}
		if selected == "" {
			return false
		}
		byNode[selected][nodeKey] -= int64(exp.Job.AcceleratorCount)
		subtractNodeResources(nodeResources[selected], perRank)
		used[selected] = true
	}
	return true
}

// perRankNodeResources is what one rank of exp takes from the node it lands on. Experiment
// footprints are whole-job totals (Footprint scales the per-rank request by the rank count), so
// they are divided back down here rather than re-parsed — one definition of a job's shape.
func perRankNodeResources(exp *domain.Experiment) map[string]int64 {
	ranks := int64(exp.Job.Nodes())
	if ranks < 1 {
		ranks = 1
	}
	fp := exp.Footprint()
	out := map[string]int64{}
	for kind, key := range map[domain.ResourceKind]string{
		domain.ResourceKindCPU:     domain.NodeResourceCPUMillicores,
		domain.ResourceKindMemory:  domain.NodeResourceMemoryBytes,
		domain.ResourceKindStorage: domain.NodeResourceStorageBytes,
	} {
		if total := fp[domain.ResourceKey{Kind: kind}]; total > 0 {
			out[key] = total / ranks
		}
	}
	return out
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

// reserveAnyNode places a job with no accelerator on the first node with room for it.
func reserveAnyNode(nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment, perRank map[string]int64) bool {
	nodes := make([]string, 0, len(nodeResources))
	for node := range nodeResources {
		if labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	used := make(map[string]bool)
	for rank := 0; rank < exp.Job.Nodes(); rank++ {
		selected := ""
		for _, node := range nodes {
			if requiresDistinctHosts(exp) && used[node] {
				continue
			}
			if nodeHasRoom(nodeResources[node], perRank) {
				selected = node
				break
			}
		}
		if selected == "" {
			return false
		}
		subtractNodeResources(nodeResources[selected], perRank)
		used[selected] = true
	}
	return true
}

// addNodeResourceShortfall folds in what the *least* deficient qualifying node still lacks in the
// fungible dimensions. Least deficient because covering that node covers the job: the shortage is
// what has to be freed somewhere, not everywhere.
func addNodeResourceShortfall(out domain.Footprint, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) {
	needed := perRankNodeResources(exp)
	if len(needed) == 0 {
		return
	}
	key := strings.ToLower(string(exp.AcceleratorType))
	perRank := int64(exp.Job.AcceleratorCount)

	var best map[string]int64
	var bestTotal int64
	for node := range nodeResources {
		if !labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			continue
		}
		// Only nodes that could host a rank's accelerators are candidates; where the accelerators
		// themselves are missing, the per-node accelerator shortfall below already says so.
		if perRank > 0 && foldLookup(byNode[node], key) < perRank {
			continue
		}
		deficit := map[string]int64{}
		var total int64
		for resource, need := range needed {
			if short := need - nodeResources[node][resource]; short > 0 {
				deficit[resource] = short
				total += short
			}
		}
		if best == nil || total < bestTotal {
			best, bestTotal = deficit, total
		}
		if bestTotal == 0 {
			return // some qualifying node already has room; nothing is short
		}
	}
	for resource, short := range best {
		kind, ok := nodeResourceKind(resource)
		if !ok {
			continue
		}
		if k := (domain.ResourceKey{Kind: kind}); out[k] < short {
			out[k] = short
		}
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
//  2. otherwise, the cluster with the smallest total shortage (sum across dimensions of
//     max(0, need-have)) — the best candidate for preempt() to free room on
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

// cloneNodeAvailByCluster deep-copies a cluster -> node -> resource capacity map, so a snapshot
// survives the in-place reservations admission makes as it walks the queue.
func cloneNodeAvailByCluster(byCluster map[string]map[string]map[string]int64) map[string]map[string]map[string]int64 {
	cloned := make(map[string]map[string]map[string]int64, len(byCluster))
	for cluster, byNode := range byCluster {
		cloned[cluster] = cloneNodeCapacity(byNode)
	}
	return cloned
}
