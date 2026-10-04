package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var errAdmissionCapacityChanged = errors.New("capacity changed during admission")

// concurrencyCapExceeded is implemented by the quota package's concurrencyCapError; checked via
// errors.As so submitJob's callers can distinguish autoscaler.md's max_concurrent_accelerators
// rejection from a generic submit failure without importing the quota package's concrete type.
type concurrencyCapExceeded interface{ ConcurrencyCapExceeded() bool }

// skippedClusters is implemented by queuebackend.InconsistentSnapshots: the capacity read is
// valid for every other cluster and names the ones it left out.
type skippedClusters interface{ SkippedClusters() map[string]error }

// readFlavorCapacity is GetFlavorCapacity with the clusters it left out logged and the read kept.
func readFlavorCapacity(ctx context.Context, client interface {
	GetFlavorCapacity(ctx context.Context) (available, desiredFree map[string]domain.Footprint, err error)
}, logger *zap.Logger) (available, desiredFree map[string]domain.Footprint, err error) {
	available, desiredFree, err = client.GetFlavorCapacity(ctx)
	var skipped skippedClusters
	if errors.As(err, &skipped) {
		for cluster, why := range skipped.SkippedClusters() {
			logger.Warn("cluster left out of the capacity read", zap.String("cluster", cluster), zap.Error(why))
		}
		return available, desiredFree, nil
	}
	return available, desiredFree, err
}

type insufficientQuota interface {
	error
	InsufficientQuota() bool
}

// notAdmittedReasonForSubmitError maps a submitJob/submitJobTo error to the not_admitted_reason
// the row should carry. Order matters: check the most specific error first.
func notAdmittedReasonForSubmitError(err error) string {
	var capErr concurrencyCapExceeded
	var quotaErr insufficientQuota
	switch {
	case errors.As(err, &capErr):
		return concurrencyCapReason()
	case errors.As(err, &quotaErr):
		return domain.NotAdmittedInsufficientQuota + ": " + quotaErr.Error()
	case errors.Is(err, errAdmissionCapacityChanged):
		return capacityTakenReason()
	default:
		return workloadCreationReason()
	}
}

// submitErrorLevel logs a lost race for the last free capacity and a quota refusal at info: the
// job stays queued with the reason on its row, so neither is an error.
func submitErrorLevel(err error) zapcore.Level {
	var quotaErr insufficientQuota
	if errors.Is(err, errAdmissionCapacityChanged) || errors.As(err, &quotaErr) {
		return zapcore.InfoLevel
	}
	return zapcore.ErrorLevel
}

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
	if hasSlots(nodeLabels) {
		freeable := cloneNodeCapacity(nodeResources)
		for _, victim := range candidates {
			creditNode(nil, freeable[nodeOf[victim.ID]], victim)
		}
		resolved, misfit := resolveOntoSlot(preemptor, cluster, nodeResourcesTotal, freeable, nodeLabels)
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
		accelerators, resources := cloneNodeCapacity(nodeAvail), cloneNodeCapacity(nodeResources)
		for _, victim := range victims {
			free.AddFootprint(victim.Footprint())
			node := nodeOf[victim.ID]
			creditNode(accelerators[node], resources[node], victim)
		}
		return domain.Fits(free, fp) && topologyFits(accelerators, resources, nodeLabels, preemptor)
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
		accelerators[nodeAcceleratorKey(exp.AcceleratorType)] += count
	}
	if resources != nil {
		addNodeResources(resources, least)
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
// speculative marks a submit made against a cluster with no live capacity — the SUBMITTED row
// itself is the scale-up request, so there is no fresh node to re-check a live-fit predicate
// against at claim time. See ClaimSubmitted: a nil capacityAvailable callback skips that check
// and relies on the lock plus the status='QUEUED' predicate alone.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string, persistedFlavor domain.AcceleratorType) error {
	return l.submitJobTo(ctx, exp, clusterName, "", persistedFlavor, false, false)
}

// provenFit mirrors speculativeCandidates' own signal for this cluster (false for a non-speculative
// submit, where it is unused) — the claim-time recheck below must apply the exact same default cap
// speculativeCandidates applied a moment ago, or a cluster with no live proof and no operator cap
// stays unbounded at the one place multiple concurrent scheduler replicas actually serialize
// against each other: the tick-local snapshot alone cannot close this.
func (l *Loop) submitJobTo(ctx context.Context, exp *domain.Experiment, clusterName, clusterID string, persistedFlavor domain.AcceleratorType, speculative, provenFit bool) error {
	// ReserveAdmittedFlavor is called on every submit now, not only on a flavor substitution: it
	// is also where the max_concurrent_accelerators cap (autoscaler.md's concurrency cap) is
	// enforced atomically against this pool's live SUBMITTED+RUNNING accelerator count, and
	// speculation makes live capacity appear on demand — the cap is the only thing left bounding
	// concurrency once that gate is open. Every job holds units of a priced type (ValidateExperiment),
	// cores on CPU and devices otherwise, so there is none it does not apply to.
	newEstCost := exp.EstimatedCostAccH
	flavor := persistedFlavor
	if exp.AcceleratorType != persistedFlavor {
		held, err := exp.EstimateHeld()
		if err != nil {
			return fmt.Errorf("submitJob: %w", err)
		}
		newEstCost = held
		flavor = exp.AcceleratorType
	}
	if err := l.quota.ReserveAdmittedFlavor(ctx, exp.ID, flavor, newEstCost, exp.AcceleratorCount); err != nil {
		return fmt.Errorf("reserve selected accelerator flavor: %w", err)
	}
	exp.EstimatedCostAccH = newEstCost
	fp := exp.Footprint()
	var capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)
	if speculative {
		// There is no live node to re-check a fit predicate against (that's why this is
		// speculative), but ClaimSubmitted's per-cluster advisory lock and its `desired` read
		// (every SUBMITTED row on clusterName) are still the one place a fresh,
		// serialized-across-replicas view of this cluster's speculative footprint exists —
		// reuse them to recheck max_speculative_accelerators here rather than trusting the
		// tick-local snapshot speculativeCandidates saw a moment ago (a snapshot other replicas
		// could have raced past concurrently).
		capacityAvailable = func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
			if clusterID == "" {
				return true, nil
			}
			cap, err := l.clusterSpeculativeCap(ctx, clusterID)
			if err != nil {
				return false, err
			}
			if cap == nil && !provenFit {
				// Same default speculativeCandidates applied at selection time: no live proof and
				// no operator override means bound the exposure to this one job's own footprint,
				// not leave it unbounded — otherwise many concurrent replicas, each seeing a
				// tick-local snapshot with zero footprint, could each independently land their own
				// "one job's worth" on the same unproven cluster before any single failure ever
				// triggers tried_clusters backoff.
				oneJob := exp.AcceleratorCount
				cap = &oneJob
			}
			if cap == nil {
				return true, nil
			}
			// desired is every SUBMITTED row on this cluster, not only the speculative
			// ones — the doc's "None: derived from autoscaler_enabled ∧ scheduled_nodes <
			// Nodes()" decision means whether a given row is still riding on a scale-up or has
			// since landed on live capacity is metrics-store state, unavailable inside this
			// Postgres transaction. Counting every SUBMITTED row here is deliberately
			// conservative: it can delay a legitimate speculative submit behind a cluster's
			// live-fit traffic (retried next tick, nothing lost), which is the safe direction to
			// err in for an optional operator-set cap — the opposite (undercounting) is the
			// unbounded-overshoot failure mode this recheck exists to close.
			footprint := exp.AcceleratorCount
			for _, d := range desired {
				footprint += d.AcceleratorCount
			}
			return footprint <= *cap, nil
		}
	} else {
		capacityAvailable = func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
			available, _, err := readFlavorCapacity(ctx, l.workload, l.logger)
			if err != nil {
				return false, err
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
			return desiredPlacementFits(nodeAvail[clusterName], nodeResources[clusterName], nodeLabels[clusterName], desired, exp, exp.ResolvedJob), nil
		}
	}
	resolved, err := l.grace.Resolve(exp.EffectiveJob())
	if err != nil {
		return err
	}
	resolved.AcceleratorPodResources = exp.AcceleratorType.WithPodResources(resolved.AcceleratorPodResources)
	if exp.EnvironmentImage != "" {
		resolved.Image = exp.EnvironmentImage
	}
	exp.ResolvedJob = &resolved
	if err := l.observed.RecordAdmission(ctx, exp.ID, time.Now().UTC()); err != nil {
		return err
	}
	claimed, err := l.store.ClaimSubmitted(ctx, exp.ID, clusterName, exp.ResolvedJob, capacityAvailable)
	if err != nil {
		return err
	}
	if !claimed {
		return errAdmissionCapacityChanged
	}
	exp.ClusterName = clusterName
	if err := l.workload.CreateWorkload(ctx, exp); err != nil {
		if rbErr := l.store.MarkQueued(ctx, exp.ID, workloadCreationReason()); rbErr != nil {
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
//
// An agent whose quota cannot be read has its jobs skipped this tick; the returned slice is exps
// without them.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment, tickErrs *[]error) ([]*domain.Experiment, map[string]*domain.AgentQuota) {
	result := map[string]*domain.AgentQuota{}
	failed := map[string]error{}
	kept := exps[:0]
	for _, exp := range exps {
		if exp.PlatformExperimentID == "" {
			kept = append(kept, exp)
			continue
		}
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		if result[key] == nil && failed[key] == nil {
			aq, err := l.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
			if err == nil && aq == nil {
				err = errors.New("quota not found")
			}
			if err != nil {
				failed[key] = fmt.Errorf("fetch quota for %s: %w", key, err)
			} else {
				result[key] = aq
			}
		}
		if err := failed[key]; err != nil {
			l.skipExperiment(tickErrs, exp, err)
			continue
		}
		kept = append(kept, exp)
	}
	return kept, result
}
