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

	// Sort victims: least elapsed time first (minimize wasted work), largest footprint tiebreak
	// so we free the needed capacity by evicting the fewest possible jobs.
	sort.Slice(burstRunning, func(i, j int) bool {
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
// (atomically — see MarkSubmitted), then creates the backend workload. Order matters: marking
// first ensures the in-flight footprint is always visible to the next tick, on the right
// cluster, even if the backend write is slow. On backend failure we roll back to QUEUED so the
// job re-enters the queue rather than leaking in an untracked SUBMITTED state.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string) error {
	if err := l.store.MarkSubmitted(ctx, exp.ID, clusterName); err != nil {
		return err
	}
	exp.ClusterName = clusterName
	if err := l.workload.CreateWorkload(ctx, exp); err != nil {
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

// fetchQuotaMap builds a map of (agentID, platformExperimentID) -> used T4H fraction for burst
// ordering. Keyed by the composite (AgentID, PlatformExperimentID) pair, not AgentID alone: an
// agent can run multiple platform experiments concurrently, each with its own quota pool, so a
// map keyed by AgentID alone would let a second platform experiment's ratio silently overwrite
// the first's.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment) map[string]float64 {
	seen := map[string]bool{}
	result := map[string]float64{}
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
		if aq.GuaranteedT4Hours > 0 {
			result[key] = aq.UsedGuaranteedT4H / aq.GuaranteedT4Hours
		}
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
