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

// preempt selects and evicts burst victims until neededGPUs are freed.
// Returns the number of GPUs actually freed.
func (l *Loop) preempt(ctx context.Context, neededGPUs int64, burstRunning []*domain.Experiment) (int64, error) {
	if len(burstRunning) == 0 {
		return 0, nil
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

	// Sort victims: least elapsed time first (minimize wasted work), largest footprint
	// (GPU count, or CPU cores for CPU-only jobs) tiebreak so we free the needed capacity by
	// evicting the fewest possible jobs.
	sort.Slice(burstRunning, func(i, j int) bool {
		ei, ej := elapsed[burstRunning[i].ID], elapsed[burstRunning[j].ID]
		if ei != ej {
			return ei < ej
		}
		_, ni := admissionUnit(burstRunning[i])
		_, nj := admissionUnit(burstRunning[j])
		return ni > nj
	})

	// Select victims to free enough capacity.
	var selected []*domain.Experiment
	var freed int64
	for _, victim := range burstRunning {
		if freed >= neededGPUs {
			break
		}
		_, need := admissionUnit(victim)
		selected = append(selected, victim)
		freed += need
	}

	if len(selected) == 0 {
		return 0, nil
	}

	// Fill-back pass: footprints are heterogeneous, so the loop above can overshoot (e.g.
	// victim N-1 alone already covers neededGPUs but victim N still got selected). Walk
	// backward and reprieve any victim whose removal from the selected set still leaves enough
	// freed capacity — minimizes total evictions instead of evicting whatever fit first.
	for i := len(selected) - 1; i >= 0; i-- {
		_, vn := admissionUnit(selected[i])
		if freed-vn >= neededGPUs {
			freed -= vn
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
	var actualFreed int64
	var wg sync.WaitGroup
	for _, victim := range requeued {
		wg.Add(1)
		go func(v *domain.Experiment) {
			defer wg.Done()
			if err := l.workload.WaitForJobDeletion(ctx, v, l.preemptTimeout); err != nil {
				l.logger.Warn("wait for job deletion", zap.String("id", v.ID), zap.Error(err))
				return
			}
			_, need := admissionUnit(v)
			mu.Lock()
			actualFreed += need
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

// notAdmittedReasonFor classifies a skipped job: if current availability for its flavor still
// equals what it was before this tick touched it, nothing was ever available (capacity never
// existed this tick, independent of competition); if it's lower, other jobs admitted earlier in
// this same tick's sort order consumed it (outranked).
func notAdmittedReasonFor(current, initial int64) string {
	if current < initial {
		return domain.NotAdmittedOutranked
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
