package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// preempt selects and evicts a set of burst victims sufficient to cover needed — a shortage
// Footprint that may span multiple dimensions at once (e.g. a mixed CPU+accelerator job that's
// short on both). The whole victim set is planned and verified before anything is evicted (see
// the fill-back pass below) — vector preemption, not a scalar count. Returns the Footprint
// actually freed (only for victims whose deletion was positively confirmed — see the wait loop
// at the bottom).
func (l *Loop) preempt(ctx context.Context, needed domain.Footprint, burstRunning []*domain.Experiment) (domain.Footprint, error) {
	if len(burstRunning) == 0 || len(needed) == 0 {
		return domain.NewFootprint(), nil
	}

	// Rank by real observed runtime, not wall-clock ElapsedHours(): a job that spent most of its
	// wall-clock life in a reschedule/node-death gap hasn't actually made more progress than one
	// admitted more recently, and shouldn't be spared preemption on that basis. Computed once up
	// front (not in the sort comparator) since it's a GreptimeDB query per job. A query error is
	// logged and treated as 0 observed hours — the conservative choice for a "prefer to evict the
	// job that's made the least progress" heuristic, not a wall-clock fallback for accounting.
	elapsed := make(map[string]float64, len(burstRunning))
	for _, exp := range burstRunning {
		hours, err := metricsdb.ObservedElapsedHours(ctx, l.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			l.logger.Warn("preempt: observed elapsed hours", zap.String("id", exp.ID), zap.Error(err))
			hours = 0
		}
		elapsed[exp.ID] = hours
	}

	// footprintSize collapses a heterogeneous multi-dimension footprint into one comparable
	// scalar (sum of all dimensions' canonical units) purely for the "largest footprint first"
	// tiebreak below — not used anywhere accounting actually happens.
	footprintSize := func(exp *domain.Experiment) int64 {
		var n int64
		for _, v := range exp.Footprint() {
			n += v
		}
		return n
	}

	// Sort victims: fewest prior preemptions first — without this, "least observed elapsed
	// hours" alone repeatedly re-selects the same burst job every time it's barely restarted
	// before the next preemption need arrives, starving it indefinitely while its
	// less-recently-hit siblings are never touched. Preempt count is the fairness primary key;
	// least elapsed time (minimize wasted work) and largest footprint (fewest evictions) remain
	// the tiebreaks among jobs that have been preempted equally often.
	sort.Slice(burstRunning, func(i, j int) bool {
		pi, pj := burstRunning[i].PreemptCount, burstRunning[j].PreemptCount
		if pi != pj {
			return pi < pj
		}
		ei, ej := elapsed[burstRunning[i].ID], elapsed[burstRunning[j].ID]
		if ei != ej {
			return ei < ej
		}
		return footprintSize(burstRunning[i]) > footprintSize(burstRunning[j])
	})

	// Select victims until their combined footprint covers every dimension of needed.
	var selected []*domain.Experiment
	freed := domain.NewFootprint()
	for _, victim := range burstRunning {
		if domain.Fits(freed, needed) {
			break
		}
		selected = append(selected, victim)
		freed.AddFootprint(victim.Footprint())
	}

	if len(selected) == 0 {
		return domain.NewFootprint(), nil
	}

	// Fill-back pass: footprints are heterogeneous, so the loop above can overshoot (e.g.
	// victim N-1 alone already covers needed but victim N still got selected). Walk backward
	// and reprieve any victim whose removal from the selected set still leaves the remaining
	// freed footprint covering needed — minimizes total evictions instead of evicting whatever
	// fit first. This is the "verify the post-preemption vector fits" step the plan calls for,
	// applied per-candidate-removal rather than once at the end, so it also states the fewest-
	// victims objective explicitly.
	for i := len(selected) - 1; i >= 0; i-- {
		vfp := selected[i].Footprint()
		trial := freed.Sub(vfp)
		if domain.Fits(trial, needed) {
			freed = trial
			selected = append(selected[:i], selected[i+1:]...)
		}
	}

	for _, victim := range selected {
		l.logger.Info("preempting burst job",
			zap.String("victim", victim.ID),
			zap.String("for_tier", "guaranteed"),
			zap.Float64("observed_elapsed_hours", elapsed[victim.ID]),
		)
	}

	// Requeue every victim FIRST (status RUNNING -> QUEUED), sequentially: this is the actual
	// "delete" instruction — it moves each victim out of the desired-running set a
	// cluster-agent reconciles against, before we ever wait for its Job to disappear. Doing
	// this before waiting is required, not just a style choice: if we waited first, the
	// cluster-agent would still see the (still-RUNNING) experiment as desired and would never
	// remove the Job, so WaitForJobDeletion would time out on every preemption.
	//
	// Do not refund quota on preemption: the job is returning to QUEUED and will run again,
	// so its remaining estimated cost must stay debited — but at the new, shortened estimate,
	// not the stale original one. All four resource dimensions are rescaled by the same ratio
	// (remaining/original duration) that estimated_duration_hours itself is rescaled by, so a
	// job's $/hour-equivalent rate stays intact across preemption, and the metrics-DB
	// reservation for every dimension is corrected to match in the same step — otherwise
	// reconcile's quota-exhaustion delta (actual − current estimate) and settlement's per-hour
	// rate would be comparing a still-original reservation against a now-rescaled estimate. The
	// completion handler issues the unused-budget refund when the job eventually finishes.
	var requeued []*domain.Experiment
	for _, victim := range selected {
		remaining := victim.RemainingEstimatedHours()
		ratio := 0.0
		if victim.EstimatedDurationHours > 0 {
			ratio = remaining / victim.EstimatedDurationHours
		}
		newCostT4H := victim.EstimatedCostT4H * ratio
		newCPU := victim.EstimatedCPUCoreHours * ratio
		newRAM := victim.EstimatedRAMGBHours * ratio
		newStorage := victim.EstimatedStorageGBHours * ratio

		if err := l.store.RequeuePreempted(ctx, victim.ID, remaining, newCostT4H, newCPU, newRAM, newStorage); err != nil {
			l.logger.Error("requeue preempted job", zap.String("id", victim.ID), zap.Error(err))
			continue
		}
		requeued = append(requeued, victim)

		if victim.PlatformExperimentID == "" {
			continue
		}
		for _, dim := range []struct {
			rt     domain.ResourceType
			amount float64
		}{
			{domain.ResourceGPUHours, newCostT4H},
			{domain.ResourceCPUCoreHours, newCPU},
			{domain.ResourceRAMGBHours, newRAM},
			{domain.ResourceStorageGBHours, newStorage},
		} {
			if dim.amount <= 0 {
				continue
			}
			if err := l.quota.CorrectReservation(ctx, victim.AgentID, victim.PlatformExperimentID, victim.ID, dim.rt, victim.CapacityTier, dim.amount); err != nil {
				l.logger.Error("correct reservation after requeue",
					zap.String("id", victim.ID), zap.String("resource", string(dim.rt)), zap.Error(err))
			}
		}
	}

	// Now wait for each victim's Job to actually disappear, in parallel — avoids serialising
	// WaitForJobDeletion across hundreds of jobs. A victim only contributes to actualFreed if
	// its own wait positively confirms deletion within the timeout: a timeout means Kubernetes
	// may still be holding those GPUs, so counting them as freed here would let this same tick
	// admit a guaranteed job against capacity that physically doesn't exist yet. Timed-out
	// victims stay requeued (out of the running set) but their capacity is simply not counted
	// this tick — the next tick reads fresh live capacity and will pick it up once actually gone.
	var mu sync.Mutex
	actualFreed := domain.NewFootprint()
	var wg sync.WaitGroup
	for _, victim := range requeued {
		wg.Add(1)
		go func(v *domain.Experiment) {
			defer wg.Done()
			if err := l.workload.WaitForJobDeletion(ctx, v, l.preemptTimeout); err != nil {
				l.logger.Warn("wait for job deletion", zap.String("id", v.ID), zap.Error(err))
				return
			}
			mu.Lock()
			actualFreed.AddFootprint(v.Footprint())
			mu.Unlock()
		}(victim)
	}
	wg.Wait()

	return actualFreed, nil
}

// submitJob marks the experiment SUBMITTED and assigns it to clusterName in the DB first
// (atomically — see MarkSubmitted), durably reserves its footprint against that cluster (see
// pending_capacity_reservations' schema comment — closes the race where a second tick, before
// the cluster-agent has actually created the pod, would otherwise see live capacity as still
// free), then creates the backend workload. Order matters: marking and reserving first ensures
// the in-flight footprint is always visible to the next tick, on the right cluster, even if the
// backend write is slow. On backend failure we roll back to QUEUED and drop the reservation so
// the job re-enters the queue rather than leaking in an untracked SUBMITTED state or holding a
// phantom claim on capacity it never actually got.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string) error {
	if err := l.store.MarkSubmitted(ctx, exp.ID, clusterName); err != nil {
		return err
	}
	exp.ClusterName = clusterName
	// resolveClusterAndFootprint may have substituted an AcceptableGPUTypes alternative for the
	// originally requested flavor (exp.Job.GPUType) to find a cluster this job actually fits on
	// — persist that now so the record matches from the start, not just once job_watcher's
	// onRunning observes the real k8s placement. onRunning's own flavor-substitution debit
	// compares its k8s-observed type against exp.GPUType, so this write is also what lets that
	// comparison correctly detect "no further correction needed" when our guess here matches
	// where the pod actually lands, vs. "the real placement differs from what we reserved" when
	// it doesn't.
	if exp.GPUCount > 0 && exp.GPUType != exp.Job.GPUType {
		newEstCost := exp.GPUType.Cost() * float64(exp.GPUCount) * exp.EstimatedDurationHours
		if err := l.store.UpdateAdmittedFlavor(ctx, exp.ID, exp.GPUType, newEstCost); err != nil {
			l.logger.Warn("persist admission-time flavor substitution",
				zap.String("exp", exp.ID), zap.Error(err))
		}
	}
	if err := l.store.UpsertPendingReservation(ctx, exp.ID, clusterName, exp.Footprint()); err != nil {
		// Fail closed, not best-effort: tick() step 2 (the SUBMITTED/ADMITTED/RUNNING fallback
		// accounting this comment used to describe) was removed once GetFlavorCapacity became
		// fully live — there is no longer any other mechanism protecting this job's claimed
		// capacity, so a silently dropped reservation would reopen the exact double-admission
		// race pending reservations exist to close. Roll back to QUEUED instead.
		l.logger.Error("upsert pending reservation", zap.String("exp", exp.ID), zap.Error(err))
		if rbErr := l.store.MarkQueued(ctx, exp.ID); rbErr != nil {
			l.logger.Error("rollback to QUEUED failed after reservation error",
				zap.String("exp", exp.ID), zap.Error(rbErr))
		}
		return err
	}
	if err := l.workload.CreateWorkload(ctx, exp); err != nil {
		if delErr := l.store.DeletePendingReservation(ctx, exp.ID); delErr != nil {
			l.logger.Warn("delete pending reservation after workload creation error",
				zap.String("exp", exp.ID), zap.Error(delErr))
		}
		if rbErr := l.store.MarkQueued(ctx, exp.ID); rbErr != nil {
			l.logger.Error("rollback to QUEUED failed after workload creation error",
				zap.String("exp", exp.ID), zap.Error(rbErr))
		}
		return err
	}
	return nil
}

// quotaKey builds the (AgentID, PlatformExperimentID) composite key quota is actually tracked
// under — matches fetchQuotaMap's dedup key. Exported as a helper so every consumer of the map
// fetchQuotaMap returns agrees on how to look an experiment's own ratio up.
func quotaKey(agentID, platformExpID string) string {
	return agentID + "/" + platformExpID
}

// fetchQuotaMap builds a map of (agentID, platformExperimentID) -> *domain.AgentQuota for
// guaranteed/burst ordering. Keyed by the composite (AgentID, PlatformExperimentID) pair, not
// AgentID alone: an agent can run multiple platform experiments concurrently, each with its own
// quota pool, so a map keyed by AgentID alone would let a second platform experiment's quota
// silently overwrite the first's. The full AgentQuota (not a single precomputed ratio) is kept
// so sortGuaranteed/sortBurst can compute each experiment's own dominant-utilization fairness
// ratio (see domain.AgentQuota.DominantUtilization) against the dimensions THAT job actually
// requests — a fixed single ratio per agent can't do that, since two jobs from the same agent
// competing on different dimensions (e.g. one GPU-only, one CPU-only) need different ratios.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment) map[string]*domain.AgentQuota {
	seen := map[string]bool{}
	result := map[string]*domain.AgentQuota{}
	for _, exp := range exps {
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		if seen[key] || exp.PlatformExperimentID == "" {
			continue
		}
		seen[key] = true
		aq, err := l.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil || aq == nil {
			continue
		}
		result[key] = aq
	}
	return result
}

// notAdmittedReasonFor classifies a skipped job: if current availability, for every dimension
// the job actually needs, still equals what it was before this tick touched it, nothing was
// ever available (capacity never existed this tick, independent of competition); if any needed
// dimension is now lower, other jobs admitted earlier in this same tick's sort order consumed
// it (outranked).
func notAdmittedReasonFor(current, initial domain.Footprint, footprint domain.Footprint) string {
	for k := range footprint {
		if current[k] < initial[k] {
			return domain.NotAdmittedOutranked
		}
	}
	return domain.NotAdmittedCapacityUnavailable
}

// setNotAdmittedReason is a best-effort write — a failure here never blocks scheduling, it only
// degrades the "why hasn't this been admitted" debugging signal for one tick.
func (l *Loop) setNotAdmittedReason(ctx context.Context, expID, reason string) {
	if err := l.store.UpdateNotAdmittedReason(ctx, expID, reason); err != nil {
		l.logger.Warn("set not_admitted_reason", zap.String("exp", expID), zap.Error(err))
	}
}
