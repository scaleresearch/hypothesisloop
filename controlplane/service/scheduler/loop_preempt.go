package scheduler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

// preempt requeues the burst victims whose eviction lets the preemptor be placed: the cluster
// total covers its footprint and its ranks fit the nodes the victims sit on, by the same placement
// check admission uses. A victim set chosen on the cluster total alone can free devices on a node
// that still cannot host the job, which is then preempted for again on a later tick.
//
// A victim whose node is unknown is never taken. The metrics store records one node per
// experiment, so a multi-node victim frees its whole footprint in the cluster total and, on that
// one node, the single rank proven to be there.
//
// Returns committed=true when it requeued the whole planned set. The caller uses that to stand the
// disbalance evictor down: capacity for this job is already on its way back, and a second
// destructive pass against the same unchanged availability would kill live work for a deficit that
// is no longer outstanding.
//
// On a cluster of slots the preemptor arrives sized to the smallest slot that takes it, which may
// hold no victim while a larger one does. It is sized again here, by the same rule, against what
// is free once every candidate is gone.
func (l *Loop) preempt(ctx context.Context, cluster string, avail domain.Footprint, nodeAvail, nodeResources, nodeResourcesTotal map[string]map[string]int64, nodeLabels map[string]map[string]string, burstRunning []*domain.Experiment, preemptor *domain.Experiment) (committed bool, err error) {
	// Rank by real observed runtime, not wall-clock ElapsedHours(): a job stuck in a
	// reschedule/node-death gap hasn't made more progress than one admitted more recently.
	//
	// A candidate whose query fails is dropped from consideration rather than failing the pass:
	// it cannot be ranked, and ranking it as zero progress would make an unmeasurable job the
	// first thing preempted. Aborting instead meant one broken series blocked every
	// guaranteed-tier preemption on the platform (important.md #19).
	now := time.Now().UTC()
	elapsed := make(map[string]float64, len(burstRunning))
	nodeOf := make(map[string]string, len(burstRunning))
	candidates := burstRunning[:0]
	for _, exp := range burstRunning {
		hours, err := l.observed.ObservedElapsedHours(ctx, exp.ID, exp.CreatedAt, now)
		if err != nil {
			l.logger.Warn("preempt: cannot rank candidate, skipping it",
				zap.String("candidate", exp.ID), zap.Error(err))
			continue
		}
		node, found, err := l.observed.LatestExperimentNode(ctx, exp.ID, exp.CreatedAt, now)
		if err != nil {
			l.logger.Warn("preempt: cannot attribute candidate to a node, skipping it",
				zap.String("candidate", exp.ID), zap.Error(err))
			continue
		}
		if !found {
			continue
		}
		elapsed[exp.ID] = hours
		nodeOf[exp.ID] = node
		candidates = append(candidates, exp)
	}
	if len(candidates) == 0 {
		return false, nil
	}
	if placement.HasSlots(nodeLabels) {
		freeable := placement.CloneNodes(nodeResources)
		for _, victim := range candidates {
			creditNode(nil, freeable[nodeOf[victim.ID]], victim)
		}
		resolved, misfit := placement.ResolveOntoSlot(preemptor, cluster, nodeResourcesTotal, freeable, nodeLabels)
		if misfit != "" {
			return false, nil
		}
		sized := *preemptor
		sized.ResolvedJob = resolved
		preemptor = &sized
	}

	// footprintSize collapses a multi-dimension footprint into one scalar, purely for the
	// "largest footprint first" tiebreak below — never used for actual accounting.
	footprintSize := func(exp *domain.Experiment) int64 {
		var n int64
		for _, v := range exp.Footprint() {
			n += v
		}
		return n
	}

	// Least observed runtime first (minimizes wasted work), largest footprint as tiebreak
	// (minimizes eviction count). No preemption history retained between ticks.
	sort.Slice(candidates, func(i, j int) bool {
		ei, ej := elapsed[candidates[i].ID], elapsed[candidates[j].ID]
		if ei != ej {
			return ei < ej
		}
		return footprintSize(candidates[i]) > footprintSize(candidates[j])
	})

	fp := preemptor.Footprint()
	placeableWithout := func(victims []*domain.Experiment) bool {
		free := domain.NewFootprint()
		free.AddFootprint(avail)
		accelerators, resources := placement.CloneNodes(nodeAvail), placement.CloneNodes(nodeResources)
		for _, victim := range victims {
			free.AddFootprint(victim.Footprint())
			node := nodeOf[victim.ID]
			creditNode(accelerators[node], resources[node], victim)
		}
		return domain.Fits(free, fp) && placement.TopologyFits(accelerators, resources, nodeLabels, preemptor)
	}

	// Never disrupt jobs for a partial plan. If evicting every candidate still leaves the job
	// unplaceable, leave all of them running.
	var selected []*domain.Experiment
	for _, victim := range candidates {
		if placeableWithout(selected) {
			break
		}
		selected = append(selected, victim)
	}
	if len(selected) == 0 || !placeableWithout(selected) {
		return false, nil
	}

	// Fill-back pass: the walk above takes victims in rank order whatever node they hold. Walk
	// backward and reprieve any victim the job is still placeable without.
	for i := len(selected) - 1; i >= 0; i-- {
		trial := append(append([]*domain.Experiment{}, selected[:i]...), selected[i+1:]...)
		if placeableWithout(trial) {
			selected = trial
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
		stintElapsed, err := l.observed.ObservedStintElapsedHours(ctx, victim.ID, victim.CreatedAt, time.Now().UTC())
		if err != nil {
			l.logger.Error("stint elapsed for preemption rescale", zap.String("id", victim.ID), zap.Error(err))
			continue
		}
		// The floor is for a stint that used up its whole estimate. Applied to a positive
		// remainder it reserves more than the job was admitted with, and a tight quota then
		// refuses every re-claim.
		remaining := victim.EstimatedDurationHours - stintElapsed
		if remaining <= 0 {
			remaining = min(domain.MinRemainingHours, victim.EstimatedDurationHours)
		}
		ratio := 0.0
		if victim.EstimatedDurationHours > 0 {
			ratio = remaining / victim.EstimatedDurationHours
		}
		newCostAccH := victim.EstimatedCostAccH * ratio

		ok, err := l.store.RequeuePreempted(ctx, victim.ID, remaining, newCostAccH)
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

// creditNode returns to a node's free capacity what evicting exp is proven to free there: one
// rank, and for a job whose ranks differ the least of them in every dimension. A nil map is a node
// the cluster did not report and is left alone.
func creditNode(accelerators, resources map[string]int64, exp *domain.Experiment) {
	shapes := exp.NodeShapes()
	if len(shapes) == 0 {
		return
	}
	count := shapes[0].AcceleratorCount
	least := map[string]int64{}
	for name, amount := range shapes[0].Resources {
		least[name] = amount
	}
	for _, shape := range shapes[1:] {
		count = min(count, shape.AcceleratorCount)
		for name := range least {
			least[name] = min(least[name], shape.Resources[name])
		}
	}
	if accelerators != nil && count > 0 {
		accelerators[placement.AcceleratorKey(exp.AcceleratorType)] += count
	}
	if resources != nil {
		placement.AddNodeResources(resources, least)
	}
}

// completionFractions derives queue ordering progress from metrics on each tick. The returned
// map is ephemeral scratch data, never retained or persisted.
func (l *Loop) completionFractions(ctx context.Context, exps []*domain.Experiment) map[string]float64 {
	out := make(map[string]float64, len(exps))
	now := time.Now().UTC()
	for _, exp := range exps {
		// A job that never started has nothing observed, and asking for each of a long queue
		// every tick was a read per job.
		if exp.EstimatedDurationHours <= 0 || exp.RanOnAcceleratorType == "" {
			continue
		}
		hours, err := l.observed.ObservedElapsedHours(ctx, exp.ID, exp.CreatedAt, now)
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
	return out
}

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
