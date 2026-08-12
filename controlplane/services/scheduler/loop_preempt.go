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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

var errAdmissionCapacityChanged = errors.New("capacity changed during admission")

// preempt selects and requeues burst victims sufficient to cover needed, a shortage Footprint
// that may span multiple dimensions (e.g. CPU+accelerator both short). The whole victim set is
// planned and verified before anything is evicted (see the fill-back pass below) — vector
// preemption, not a scalar count. Requeuing is fire-and-forget: this tick never waits for a
// victim's Job to disappear; whichever future tick first observes the resource genuinely free
// admits the preempting job.
func (l *Loop) preempt(ctx context.Context, needed domain.Footprint, burstRunning []*domain.Experiment, preemptor *domain.Experiment) error {
	if len(burstRunning) == 0 || len(needed) == 0 {
		return nil
	}

	// Rank by real observed runtime, not wall-clock ElapsedHours(): a job stuck in a
	// reschedule/node-death gap hasn't made more progress than one admitted more recently.
	// Computed once up front since it's a GreptimeDB query per job; an error aborts the pass
	// rather than inventing zero progress.
	elapsed := make(map[string]float64, len(burstRunning))
	for _, exp := range burstRunning {
		hours, err := metricsdb.ObservedElapsedHours(ctx, l.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			return fmt.Errorf("preempt: observed elapsed hours for %s: %w", exp.ID, err)
		}
		elapsed[exp.ID] = hours
	}

	contributions := make(map[string]domain.Footprint, len(burstRunning))
	for _, exp := range burstRunning {
		contributions[exp.ID] = preemptionContribution(exp, preemptor)
	}

	// footprintSize collapses a multi-dimension footprint into one scalar, purely for the
	// "largest footprint first" tiebreak below — never used for actual accounting.
	footprintSize := func(exp *domain.Experiment) int64 {
		var n int64
		for _, v := range contributions[exp.ID] {
			n += v
		}
		return n
	}

	// Least observed runtime first (minimizes wasted work), largest footprint as tiebreak
	// (minimizes eviction count). No preemption history retained between ticks.
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
		contribution := contributions[victim.ID]
		useful := false
		for key, want := range needed {
			if freed[key] < want && contribution[key] > 0 {
				useful = true
				break
			}
		}
		if !useful {
			continue
		}
		selected = append(selected, victim)
		freed.AddFootprint(contribution)
	}

	// Never disrupt jobs for a partial plan. If the complete candidate set cannot free every
	// deficient dimension, leave all victims running and retry from fresh actual state later.
	if len(selected) == 0 || !domain.Fits(freed, needed) {
		return nil
	}

	// Fill-back pass: heterogeneous footprints can make the loop above overshoot (e.g. victim
	// N-1 alone already covers needed but N still got selected). Walk backward and reprieve any
	// victim whose removal still leaves freed covering needed — minimizes total evictions.
	for i := len(selected) - 1; i >= 0; i-- {
		vfp := contributions[selected[i].ID]
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

	// Requeue every victim (RUNNING -> QUEUED) and stop — never waits for its Job to actually
	// disappear. Live capacity, read fresh each tick, reflects the resource as free once that's
	// genuinely true.
	//
	// Do not refund quota on preemption: the job returns to QUEUED and will run again, so its
	// remaining cost stays debited — but rescaled to the new, shortened estimate. All four
	// resource dimensions are rescaled by the same ratio so a job's $/hour rate stays intact
	// across preemption, and the metrics-DB reservation is corrected to match in the same step
	// (otherwise reconcile's quota-exhaustion delta and settlement's rate would compare a stale
	// reservation against a rescaled estimate). The completion handler refunds unused budget
	// when the job eventually finishes.
	for _, victim := range selected {
		remaining := victim.EstimatedDurationHours - elapsed[victim.ID]
		minimum := domain.MinRemainingHours
		if victim.EstimatedDurationHours < minimum {
			minimum = victim.EstimatedDurationHours
		}
		if remaining < minimum {
			remaining = minimum
		}
		ratio := 0.0
		if victim.EstimatedDurationHours > 0 {
			ratio = remaining / victim.EstimatedDurationHours
		}
		newCostAccH := victim.EstimatedCostAccH * ratio
		newCPU := victim.EstimatedCPUCoreHours * ratio
		newRAM := victim.EstimatedRAMGBHours * ratio
		newStorage := victim.EstimatedStorageGBHours * ratio

		if err := l.store.RequeuePreempted(ctx, victim.ID, remaining, newCostAccH, newCPU, newRAM, newStorage); err != nil {
			l.logger.Error("requeue preempted job", zap.String("id", victim.ID), zap.Error(err))
			continue
		}

	}

	return nil
}

// preemptionContribution is the capacity a victim can actually return to the preemptor's
// placement domain. Extended resources such as nvidia.com/gpu are shared by multiple hardware
// models; equal resource names alone do not mean evicting one model makes another available.
func preemptionContribution(victim, preemptor *domain.Experiment) domain.Footprint {
	contribution := victim.Footprint()
	if preemptor.Job.AcceleratorCount <= 0 {
		return contribution
	}
	// Accelerator capacity is keyed by the driver-published type, so "does evicting this victim
	// free the hardware the preemptor needs" is now just string equality — no comparing resource
	// names and node selectors to guess whether two jobs sit on the same hardware.
	if !strings.EqualFold(string(victim.AcceleratorType), string(preemptor.AcceleratorType)) {
		delete(contribution, domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(string(preemptor.AcceleratorType))})
	}
	return contribution
}

// completionFractions derives queue ordering progress from metrics on each tick. The returned
// map is ephemeral scratch data, never retained or persisted.
func (l *Loop) completionFractions(ctx context.Context, exps []*domain.Experiment) (map[string]float64, error) {
	out := make(map[string]float64, len(exps))
	now := time.Now().UTC()
	for _, exp := range exps {
		if exp.EstimatedDurationHours <= 0 {
			continue
		}
		hours, err := metricsdb.ObservedElapsedHours(ctx, l.metricsDBURL, exp.ID, now, ObservedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			return nil, fmt.Errorf("completion fraction: observed elapsed hours for %s: %w", exp.ID, err)
		}
		fraction := hours / exp.EstimatedDurationHours
		if fraction < 0 {
			fraction = 0
		} else if fraction > 1 {
			fraction = 1
		}
		out[exp.ID] = fraction
	}
	return out, nil
}

// submitJob marks the experiment SUBMITTED and assigns it to clusterName in the DB first
// (atomically — see MarkSubmitted), then creates the backend workload. MarkSubmitted is itself
// the durable capacity claim because GetFlavorCapacity subtracts all desired experiments. On
// backend failure we roll back to QUEUED so the next tick can retry from durable state.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string) error {
	// resolveClusterAndFootprint may have substituted an AcceptableAcceleratorTypes alternative
	// for the originally requested flavor to find a cluster the job fits on — persist that now
	// so the record matches from the start. This is also what lets onRunning's flavor-
	// substitution debit correctly detect whether our guess here matches the real k8s placement.
	if exp.AcceleratorCount > 0 && exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType {
		newEstCost := exp.AcceleratorType.Cost() * float64(exp.AcceleratorCount) * exp.EstimatedDurationHours
		if err := l.quota.ReserveAdmittedFlavor(ctx, exp.ID, exp.AcceleratorType, newEstCost); err != nil {
			return fmt.Errorf("reserve selected accelerator flavor: %w", err)
		}
		exp.EstimatedCostAccH = newEstCost
	}
	fp := exp.Footprint()
	claimed, err := l.store.ClaimSubmitted(ctx, exp.ID, clusterName, func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
		guaranteed, burst, err := l.workload.GetFlavorCapacity(ctx)
		if err != nil {
			return false, err
		}
		available := guaranteed
		if exp.CapacityTier == domain.CapacityBurst {
			available = burst
		}
		if !domain.Fits(available[clusterName], fp) {
			return false, nil
		}
		nodeAvail, err := l.workload.GetAcceleratorCapacityByNode(ctx)
		if err != nil {
			return false, err
		}
		nodeLabels, err := l.workload.GetNodeLabels(ctx)
		if err != nil {
			return false, err
		}
		return desiredPlacementFits(nodeAvail[clusterName], nodeLabels[clusterName], desired, exp), nil
	})
	if err != nil {
		return err
	}
	if !claimed {
		return errAdmissionCapacityChanged
	}
	exp.ClusterName = clusterName
	if err := l.workload.CreateWorkload(ctx, exp); err != nil {
		if rbErr := l.store.MarkQueued(ctx, exp.ID, domain.NotAdmittedWorkloadCreation); rbErr != nil {
			l.logger.Error("rollback to QUEUED failed after workload creation error",
				zap.String("exp", exp.ID), zap.Error(rbErr))
		}
		return err
	}
	return nil
}

// quotaKey builds the (AgentID, PlatformExperimentID) composite key quota is tracked under —
// matches fetchQuotaMap's dedup key, so every consumer agrees on the lookup.
func quotaKey(agentID, platformExpID string) string {
	return agentID + "/" + platformExpID
}

// fetchQuotaMap builds a map of (agentID, platformExperimentID) -> *domain.AgentQuota for
// guaranteed/burst ordering. Keyed by the composite pair, not AgentID alone, since an agent can
// run multiple platform experiments concurrently each with its own quota pool. The full
// AgentQuota (not a precomputed ratio) is kept so sortGuaranteed/sortBurst can compute each
// experiment's own dominant-utilization ratio against the dimensions that job actually requests.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment) (map[string]*domain.AgentQuota, error) {
	seen := map[string]bool{}
	result := map[string]*domain.AgentQuota{}
	for _, exp := range exps {
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		if seen[key] || exp.PlatformExperimentID == "" {
			continue
		}
		seen[key] = true
		aq, err := l.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
		if err != nil {
			return nil, fmt.Errorf("fetch quota for %s: %w", key, err)
		}
		if aq == nil {
			return nil, fmt.Errorf("fetch quota for %s: quota not found", key)
		}
		result[key] = aq
	}
	return result, nil
}
