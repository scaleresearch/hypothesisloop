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
// Returns committed=true when it requeued a victim set that covers the whole shortage. The
// caller uses that to stand the disbalance evictor down: capacity for this job is already on its
// way back, and a second destructive pass against the same unchanged availability would kill live
// work for a deficit that is no longer outstanding.
func (l *Loop) preempt(ctx context.Context, needed domain.Footprint, burstRunning []*domain.Experiment, preemptor *domain.Experiment) (committed bool, err error) {
	if len(burstRunning) == 0 || len(needed) == 0 {
		return false, nil
	}

	// Rank by real observed runtime, not wall-clock ElapsedHours(): a job stuck in a
	// reschedule/node-death gap hasn't made more progress than one admitted more recently.
	// One GreptimeDB query per candidate.
	//
	// A candidate whose query fails is dropped from consideration rather than failing the pass:
	// it cannot be ranked, and ranking it as zero progress would make an unmeasurable job the
	// first thing preempted. Aborting instead meant one broken series blocked every
	// guaranteed-tier preemption on the platform (important.md #19).
	elapsed := make(map[string]float64, len(burstRunning))
	rankable := burstRunning[:0]
	for _, exp := range burstRunning {
		hours, err := metricsdb.ObservedElapsedHours(ctx, l.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			l.logger.Warn("preempt: cannot rank candidate, skipping it",
				zap.String("candidate", exp.ID), zap.Error(err))
			continue
		}
		elapsed[exp.ID] = hours
		rankable = append(rankable, exp)
	}
	burstRunning = rankable
	if len(burstRunning) == 0 {
		return false, nil
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
		return false, nil
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

	requeued := 0
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
		// Rescale against THIS stint's hours, not the job's lifetime. EstimatedDurationHours is
		// already the remaining estimate a previous preemption wrote, so subtracting lifetime
		// elapsed would charge the earlier stint's hours a second time — on a second preemption
		// the two cancel and the estimate, plus every resource reservation derived from it,
		// collapses to the floor while most of the work is still ahead. The ranking above keeps
		// using lifetime elapsed, which is the right measure of "who has done the most work".
		stintElapsed, err := metricsdb.ObservedStintElapsedHours(ctx, l.metricsDBURL, victim.ID, time.Now().UTC(), ObservedMaxLookback, l.observedGapCap, l.observedStep)
		if err != nil {
			l.logger.Error("stint elapsed for preemption rescale", zap.String("id", victim.ID), zap.Error(err))
			continue
		}
		remaining := victim.EstimatedDurationHours - stintElapsed
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

		ok, err := l.store.RequeuePreempted(ctx, victim.ID, remaining, newCostAccH, newCPU, newRAM, newStorage)
		if err != nil {
			l.logger.Error("requeue preempted job", zap.String("id", victim.ID), zap.Error(err))
			continue
		}
		if !ok {
			// It reached a terminal status between selection and here — it is already releasing
			// its capacity, so the plan is short by nothing, but this tick cannot claim to have
			// executed the plan it planned.
			l.logger.Info("preemption victim was no longer running", zap.String("victim", victim.ID))
			continue
		}
		// The row now reserves only the remaining work. Record what has already been consumed as
		// observed usage, or those hours are counted nowhere until the job finally terminates,
		// possibly days later: a job preempted three quarters of the way through would look to
		// admission, quota exhaustion and stage progress like it had barely run, and its agent
		// could re-admit against budget it has already spent.
		//
		// Deliberately does not mark the experiment settled — quota_settled_at means "terminal and
		// final", and this job is going to run again. Settle writes an absolute figure for the
		// whole life so far, so the eventual terminal settlement simply overwrites it with the
		// same quantity recomputed over a longer window.
		l.settleStint(ctx, victim)
		requeued++
	}

	if requeued == len(selected) {
		return true, nil
	}
	// A partially executed plan is the worst of both worlds: some victims are already releasing
	// capacity, so the shortage this tick measured is stale, yet the plan did not cover it.
	// Reporting it as an ordinary "no plan" would let the caller answer with the disbalance
	// evictor — terminating further live work on top of the victims already requeued. It is an
	// error, so the caller stands down and a later tick re-plans from fresh state.
	return false, fmt.Errorf("preempt: requeued %d of %d planned victims for %s", requeued, len(selected), preemptor.ID)
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
			// This value is a sort tiebreak and nothing else (see sortGuaranteed). A transient
			// metrics-store failure on one job must not stop the platform admitting anything
			// this tick — and since the query runs before either admission pass, returning here
			// would do exactly that (#19). Absent means "no observed progress", which is what an
			// unranked job sorts as anyway.
			l.logger.Warn("completion fraction unavailable; ranking this job as unstarted",
				zap.String("exp", exp.ID), zap.Error(err))
			continue
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
// settleStint records a requeued preemption victim's consumed hours as observed usage.
//
// Best-effort: the requeue itself has already committed, and reporting a settlement failure as if
// the preemption had failed would leave the tick believing it never freed the capacity it did
// free. An unsettled stint is re-derived from the metrics store by the next settlement of the
// same experiment, since every settlement is an absolute set over the job's whole life.
func (l *Loop) settleStint(ctx context.Context, exp *domain.Experiment) {
	if err := l.settler.Settle(ctx, exp); err != nil {
		l.logger.Warn("settle preempted stint", zap.String("id", exp.ID), zap.Error(err))
	}
}

// persistedFlavor is what the experiments row said before this tick's placement search ran.
// Passing it in is what makes the write below correct: the search communicates its choice by
// mutating exp.AcceleratorType, so by the time submitJob runs, exp no longer knows what is
// actually stored. Comparing the choice against the immutable *requested* flavor instead used to
// leave the row stranded on a substitute — tick 1 persists flavor B and then fails its claim,
// tick 2 re-picks the originally requested A, the requested-flavor comparison sees no
// substitution and writes nothing, and the row says B for the rest of the job's life while the
// scheduler reserves and fit-checks A.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string, persistedFlavor domain.AcceleratorType) error {
	if exp.AcceleratorCount > 0 && exp.AcceleratorType != persistedFlavor {
		rate, ok := exp.AcceleratorType.LookupCost()
		if !ok {
			return fmt.Errorf("submitJob: unknown accelerator flavor %q for experiment %s", exp.AcceleratorType, exp.ID)
		}
		newEstCost := rate * float64(exp.AcceleratorCount) * exp.EstimatedDurationHours
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
		nodeResources, err := l.workload.GetNodeResourceCapacity(ctx)
		if err != nil {
			return false, err
		}
		nodeLabels, err := l.workload.GetNodeLabels(ctx)
		if err != nil {
			return false, err
		}
		return desiredPlacementFits(nodeAvail[clusterName], nodeResources[clusterName], nodeLabels[clusterName], desired, exp), nil
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
