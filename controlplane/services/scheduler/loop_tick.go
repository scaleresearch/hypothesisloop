package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler/placement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// tick runs one full admission pass. Single-threaded — no concurrent ticks.
func (l *Loop) tick(ctx context.Context) (tickErr error) {
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

	// autoscaler.md's backlog signal (secondary path, for clusters with no native autoscaler to
	// react to a Pending pod). Published once at the end of this tick, whatever else happens.
	backlog := newBacklogAggregator()

	// Amortizes the running-experiment node-attribution lookups the per-accelerator share needs
	// (see loop_resolve.go) across every queued job this tick considers, in both passes.
	resolveCache := newResolutionCache(l)

	// Reprioritize all queued jobs after every tick, regardless of outcome — deferred so every
	// exit path (including early returns below) runs it. Its failure is the tick's.
	defer func() {
		if err := l.reprioritizer.RePrioritize(ctx); err != nil {
			tickErr = errors.Join(tickErr, fmt.Errorf("reprioritize after tick: %w", err))
		}
	}()

	// 1. Get available physical capacity as a domain.Footprint per cluster — a pooled total
	// would hide which cluster has room, admitting against a combined number while the actual
	// target cluster is full.
	gAvail, desiredFree, err := readFlavorCapacity(ctx, l.clusters, l.logger)
	if err != nil {
		return err
	}
	// Guaranteed and burst share one physical pool per cluster; preemption enforces the tier
	// boundary.
	bAvail := placement.CloneAvail(gAvail)
	nodeAvail, err := l.clusters.GetAcceleratorCapacityByNode(ctx)
	if err != nil {
		return fmt.Errorf("per-node accelerator capacity: %w", err)
	}
	nodeResources, err := l.clusters.GetNodeResourceCapacity(ctx)
	if err != nil {
		return fmt.Errorf("per-node resource capacity: %w", err)
	}
	nodeLabels, err := l.clusters.GetNodeLabels(ctx)
	if err != nil {
		return fmt.Errorf("node labels: %w", err)
	}
	// Which clusters can run a job that spans more than one node. A capability each cluster
	// reports about itself, filtered on here so a distributed job is never placed on a runtime
	// that executes single-node jobs only — that used to be discovered at workload creation,
	// after the job had already been admitted and had already claimed a reservation.
	multiNodeCapable, err := l.clusters.GetMultiNodeCapability(ctx)
	if err != nil {
		return fmt.Errorf("multi-node capability: %w", err)
	}
	// Which clusters exist for the platform, whose each is, and which are shared. Placement by
	// org is decided against this one read: a job goes to a cluster of its own org or to a
	// shared one, and a name nobody registered is no cluster at all.
	registeredClusters, err := l.store.RegisteredClusters(ctx)
	if err != nil {
		return fmt.Errorf("registered clusters: %w", err)
	}
	// Speculative-submit inputs (autoscaler.md), read only when a deployment has opted in via
	// WithSpeculation — a ClusterCapacity that has not implemented these two methods (every
	// pre-autoscaler test fake, and any deployment that never calls WithSpeculation) must not be
	// asked to.
	var autoscalerEnabled map[string]bool
	var clusterIDs map[string]string
	if l.triedClusterTTL > 0 {
		autoscalerEnabled, err = l.clusters.GetAutoscalerCapability(ctx)
		if err != nil {
			return fmt.Errorf("autoscaler capability: %w", err)
		}
		clusterIDs, err = l.clusters.GetClusterIDs(ctx)
		if err != nil {
			return fmt.Errorf("cluster ids: %w", err)
		}
	}
	// gAvail only has entries for clusters GetFlavorCapacity's heartbeat read found connected
	// (see ClusterReports.GetFlavorCapacity) — reusing its key set here is the same
	// "unreachable cluster is never a speculative candidate" rule the doc calls out as a
	// pre-existing bug fixed in step 0, applied to the speculative path too.
	connectedClusters := make(map[string]bool, len(gAvail))
	for cluster := range gAvail {
		connectedClusters[cluster] = true
	}
	// speculativeFootprintByCluster approximates each cluster's outstanding speculative
	// accelerator footprint as its whole SUBMITTED footprint — a job's live-fit-vs-speculative
	// distinction lives in the metrics store (scheduled_nodes), which the loop does not read per
	// job. Counting every SUBMITTED accelerator here makes clusterSpeculativeCap strictly more
	// conservative than the design's exact definition, never less — the safe direction for a cap.
	speculativeFootprintByCluster := map[string]int{}
	if l.triedClusterTTL > 0 {
		submitted, err := l.store.ListSubmittedExperiments(ctx)
		if err != nil {
			return fmt.Errorf("list submitted for speculative footprint: %w", err)
		}
		for _, s := range submitted {
			if autoscalerEnabled[s.ClusterName] {
				speculativeFootprintByCluster[s.ClusterName] += s.AcceleratorCount
			}
		}
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
	totalCapacity, err := l.clusters.GetTotalCapacity(ctx)
	if err != nil {
		l.logger.Warn("total capacity unavailable; disbalance eviction cannot judge this tick", zap.Error(err))
		totalCapacity = nil
	}
	// Per-node installed CPU/memory/storage — the stable denominator evictDisbalanced's node-local
	// fair-share math needs, since nodeResources above is free capacity and moves every tick. Same
	// degrade-gracefully treatment as totalCapacity immediately above: absent for a cluster (or
	// entirely, on a read failure) just means that cluster's disbalance judgment abstains this
	// tick, never that the whole tick fails.
	nodeResourcesTotal, err := l.clusters.GetNodeTotalCapacity(ctx)
	if err != nil {
		l.logger.Warn("per-node total capacity unavailable; disbalance eviction cannot judge this tick", zap.Error(err))
		nodeResourcesTotal = nil
	}
	whenIdle := installed{registered: registeredClusters, totals: totalCapacity, nodeTotals: nodeResourcesTotal, scalesUp: autoscalerEnabled, multiNode: multiNodeCapable}

	// Credit back capacity from disbalance victims a prior tick already evicted but that this
	// fresh read still can't see as free (the node/agent hasn't reported it back yet) — otherwise
	// this tick re-derives the same shortage and evicts a fresh set of victims to free capacity
	// that is already being freed. See loop_disbalance.go's applyPendingEvictions.
	l.applyPendingEvictions(start, gAvail, bAvail, nodeAvail, nodeResources)

	// The capacity this tick started with, before any admission below claims a slice of it. It
	// is what separates "you lost this to jobs ahead of you, wait" from "this cluster cannot
	// host your request, shrink it" — a distinction a submitter cannot make from the outside
	// and cannot act on if it is wrong. Both passes classify against the same snapshot.
	//
	// It is also what installed accelerators per node are rebuilt from (free plus claimed): a job
	// admitted this tick is in neither the live free count nor the claimed list, so a share
	// measured against live capacity grows with every admission.
	nodeAvailAtTickStart := placement.CloneNodesByCluster(nodeAvail)
	nodeResourcesAtTickStart := placement.CloneNodesByCluster(nodeResources)
	clusters := tickClusters{
		start: start, registered: registeredClusters, multiNode: multiNodeCapable, clusterIDs: clusterIDs,
		nodeAvail: nodeAvail, nodeResources: nodeResources, nodeResourcesTotal: nodeResourcesTotal, nodeAvailAtTickStart: nodeAvailAtTickStart,
		nodeLabels: nodeLabels, resolveCache: resolveCache, whenIdle: whenIdle,
	}

	// 2. GetFlavorCapacity already subtracts the complete desired footprint (SUBMITTED/
	// RUNNING). No second reservation or live-usage subtraction here — either would
	// double-count. MarkSubmitted is itself the durable capacity claim.

	// 3. Get all QUEUED experiments.
	queued, err := l.store.ListQueuedExperiments(ctx)
	if err != nil {
		return err
	}
	completion := l.completionFractions(ctx, queued)

	// 3a. Enforce the current stage's max_job_hours: a job queued while an earlier, looser stage
	// was running must not slip through after the ladder tightened. Held, not rejected — a later
	// stage may lift the cap, and then it admits normally.
	// One unreadable row must not decide the fate of every other queued job. A QUEUED experiment
	// pointing at a deleted platform experiment is re-read on every tick forever, so aborting the
	// pass here would wedge admission for the entire platform indefinitely — the exact shape
	// important.md #19 exists to forbid. Per-experiment failures are logged, collected, and the
	// experiment is dropped from this tick's consideration; the pass finishes, and the joined
	// error surfaces at the end so the failure is still loud.
	maxJobHours := map[string]float64{} // key: platformExpID; 0 = unlimited
	// Read once per agent of a platform experiment, as quota is (quotaKey).
	cut := map[string]bool{}
	filtered := queued[:0]
	environments := newEnvironmentPass()
	for _, exp := range queued {
		if exp.EnvironmentKey != "" {
			if held, err := l.heldForEnvironment(ctx, environments, exp, registeredClusters, nodeLabels); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("environment: %w", err))
				continue
			} else if held {
				continue
			}
		}
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
			if err := l.markNotAdmitted(ctx, exp, jobTooLongReason(exp.EstimatedDurationHours, limit)); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark job too long: %w", err))
			}
			continue
		}
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		isCut, read := cut[key]
		if !read {
			isCut, err = l.store.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID)
			if err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("stage cut: %w", err))
				continue
			}
			cut[key] = isCut
		}
		if isCut {
			if err := l.markNotAdmitted(ctx, exp, stageCutReason()); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark stage cut: %w", err))
			}
			continue
		}
		filtered = append(filtered, exp)
	}
	queued = filtered

	// 4. Guaranteed pass: FIFO by age bucket, quota-ratio tiebreak within a bucket, then
	// completion proximity DESC, then shortest job first.
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	guaranteed, guaranteedQuotas := l.fetchQuotaMap(ctx, guaranteed, &tickErrs)
	sortGuaranteed(guaranteed, guaranteedQuotas, completion, l.guaranteedFairnessWindow)
	// A backlog one agent queued at once is older than every job the others queue after it, and
	// by age alone it took each core that freed until it drained.
	guaranteed = interleaveByAgent(guaranteed)
	// Both snapshots are taken here, before either pass claims anything. Taking the burst one
	// after the guaranteed pass told a burst job displaced by a guaranteed one that the cluster
	// had no room for it, when what happened is that a higher tier outranked it.
	gAvailInitial := placement.CloneAvail(gAvail)
	bAvailInitial := placement.CloneAvail(bAvail)

	// Preemption candidates are read once per tick rather than once per job that fails to fit —
	// a saturated cluster with a deep guaranteed queue otherwise issues one full scan per queued
	// job, every tick. It is re-read after a preempt, because preempt requeues its victims and
	// the next candidate must not consider them running.
	var running []*domain.Experiment
	runningLoaded := false
	// Per cluster of slots, the slots being left that no waiting job has been promised yet.
	vacating := map[string]map[string]map[string]int64{}
	// The clusters a job that waited long holds this tick, in both passes (holdRoom).
	held := map[string]bool{}

	// Snapshot pre-tick availability so a skip can be classified: capacity_unavailable (no
	// capacity existed before this tick) vs outranked (capacity existed, but other guaranteed
	// jobs earlier in sort order already claimed it).
	for _, exp := range guaranteed {
		// What the row says right now, captured before the placement search mutates
		// exp.AcceleratorType to communicate its choice. submitJob needs both to know whether it
		// has a new flavor to persist.
		persistedFlavor := exp.AcceleratorType
		placed, ok := l.placeQueued(ctx, "guaranteed", exp, gAvail, guaranteedQuotas[quotaKey(exp.AgentID, exp.PlatformExperimentID)], clusters, &tickErrs)
		if !ok {
			continue
		}
		cluster, fp, betFlavor, misfit := placed.cluster, placed.fp, placed.betFlavor, placed.misfit
		// Which of the two fit checks failed decides whether preemption can help at all, so they
		// are evaluated separately rather than or-ed into one condition.
		scalarFits := domain.Fits(gAvail[cluster], fp)
		liveFits := scalarFits && placement.TopologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		if exp.EnvironmentImage != "" && (misfit != "" || !liveFits) {
			l.waitForPush(ctx, "guaranteed", environments, exp, &tickErrs)
			continue
		}
		// Speculative submit (autoscaler.md): before spending a guaranteed job's preemption
		// power, try every autoscaler-enabled cluster this job hasn't already failed over
		// from. A candidate has no live capacity by definition — the SUBMITTED row itself is the
		// scale-up request the native autoscaler reacts to. Within one priority live fit wins
		// over speculating, and speculating anywhere wins over preempting a burst job. Across
		// priorities the higher cluster wins: a job that fits live on a lower cluster first asks
		// a scaling cluster its org ranked above it, and comes back to the lower one when that
		// bet times out (job_watcher's scale-up deadline) and the cluster is excluded for it.
		var bets []string
		var provenFitByCluster map[string]bool
		if !liveFits || (exp.EnvironmentImage == "" && scalesAbove(cluster, exp, autoscalerEnabled, connectedClusters, registeredClusters)) {
			chosenFlavor := exp.AcceleratorType
			if liveFits {
				// The flavor the lower cluster fit at is that cluster's. The bet is for the one
				// the job is queued at, as it is when nothing fits.
				exp.AcceleratorType = betFlavor
			}
			var cerr error
			bets, provenFitByCluster, cerr = l.speculativeCandidates(ctx, resolveCache, exp, autoscalerEnabled, connectedClusters, clusterIDs, multiNodeCapable, registeredClusters, nodeAvailAtTickStart, nodeResourcesTotal, nodeLabels, speculativeFootprintByCluster, gAvail)
			if cerr != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("speculative candidates: %w", cerr))
				continue
			}
			if liveFits {
				if bets = above(bets, cluster, registeredClusters); len(bets) == 0 {
					exp.AcceleratorType = chosenFlavor
				}
			}
		}
		if !liveFits || len(bets) > 0 {
			candidates := bets
			// waitingForScaleUp: this cluster's own desired-free already went negative in this
			// job's accelerator dimension (a SUBMITTED row is already outstanding here, possibly
			// this same job a moment ago). Preempting a burst job to cover that shortage would be
			// a wrong eviction for a shortage the incoming node is already going to fill; the
			// deadline in job_watcher_scan.go bounds the wait (autoscaler.md's skip-preemption rule).
			waitingForScaleUp := autoscalerEnabled[cluster] && negativeInDimension(gAvail[cluster], exp.AcceleratorType)
			if len(candidates) > 0 {
				specCluster := candidates[0]
				// The resolution above ran against whichever cluster looked live-fit-eligible
				// before this job fell through to speculation -- a different cluster than
				// specCluster, or none at all if every candidate failed that pre-check.
				// Re-resolve against the actual speculative target now that it's chosen: a
				// stale resolution would persist a slot size of the wrong cluster.
				if specCluster != cluster {
					resolvedJob, specMisfit, rerr := l.resolveClusterLocalResources(ctx, resolveCache, exp, specCluster, nodeResourcesTotal[specCluster], nodeAvailAtTickStart[specCluster], nodeResources[specCluster], nodeLabels[specCluster])
					if rerr != nil {
						l.skipExperiment(&tickErrs, exp, fmt.Errorf("resolve cluster-local resources for speculative target: %w", rerr))
						continue
					}
					if specMisfit != "" {
						// This job's own request is above its share on the speculative target or
						// no slot there takes it. Nothing else to try this tick.
						obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
						if err := l.markNotAdmitted(ctx, exp, specMisfit); err != nil {
							l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
						}
						continue
					}
					exp.ResolvedJob = resolvedJob
				}
				if err := l.submitJobTo(ctx, exp, specCluster, clusterIDs[specCluster], persistedFlavor, true, provenFitByCluster[specCluster]); err != nil {
					l.logger.Log(submitErrorLevel(err), "speculative submit", zap.String("exp", exp.ID), zap.String("cluster", specCluster), zap.Error(err))
					obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
					reason := notAdmittedReasonForSubmitError(err)
					if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
						l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
					}
					continue
				}
				obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "submitted").Inc()
				obsmetrics.SpeculativeSubmitsTotal.Inc()
				speculativeFootprintByCluster[specCluster] += exp.AcceleratorCount
				// The claimed footprint is not subtracted from gAvail/bAvail/nodeAvail here: a
				// speculative claim has no live node to subtract from, and GetFlavorCapacity
				// already carries desired-free negative for this cluster starting next tick via
				// SumDesiredFootprintByCluster. Nothing else in this tick reads gAvail[specCluster]
				// again for a decision this job's SUBMITTED row should have already influenced.
				continue
			}
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
			shortage := placement.PreemptionShortfall(gAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
			// With no burst job to preempt this is a full cluster and a job waiting its turn, every
			// tick until capacity frees: the row's not_admitted_reason says so once.
			level := zapcore.DebugLevel
			if len(burstRunning) > 0 {
				level = zapcore.InfoLevel
			}
			l.logger.Log(level, "guaranteed job needs preemption",
				zap.String("exp", exp.ID), zap.String("cluster", cluster),
				zap.String("avail", footprintStr(gAvail[cluster])), zap.String("need", footprintStr(fp)),
				zap.String("shortage", footprintStr(shortage)),
				zap.Int("burst_candidates", len(burstRunning)))
			// The job fits once the platform jobs already told to stop are gone: earlier requeues
			// freed its capacity, and taking more victims while their pods terminate takes one
			// per tick.
			//
			// On a cluster of slots both questions are asked per slot, of the resolver that sized
			// the job: the total is covered by a free slot of another shape, which never takes it.
			slots := placement.HasSlots(nodeLabels[cluster])
			totalCovers := scalarFits && !slots
			promised := !slots && !scalarFits && domain.Fits(desiredFree[cluster], fp)
			leaving := ""
			if slots {
				if vacating[cluster] == nil {
					claimed, err := resolveCache.loadClaimed(ctx)
					if err != nil {
						l.skipExperiment(&tickErrs, exp, fmt.Errorf("list claimed for vacating slots: %w", err))
						continue
					}
					vacating[cluster] = placement.VacatingSlots(cluster, nodeResourcesTotal[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], claimed)
				}
				if node, free, _ := placement.SlotFor(exp, cluster, nodeResourcesTotal[cluster], vacating[cluster], nodeLabels[cluster]); free {
					leaving, promised = nodeLabels[cluster][node][domain.NodeSlotLabel], true
					delete(vacating[cluster], node)
				}
			}
			var committed bool
			var err error
			switch {
			case totalCovers:
				// A preemption here would repeat every tick while its victim's pod terminates:
				// the total already covers the job, so nothing below reads the capacity as
				// promised.
				l.logger.Info("skipping preemption: cluster has the capacity, the layout does not fit",
					zap.String("exp", exp.ID), zap.String("cluster", cluster))
			case waitingForScaleUp:
				l.logger.Info("skipping preemption: cluster is autoscaler-enabled and already waiting for scale-up",
					zap.String("exp", exp.ID), zap.String("cluster", cluster))
			case promised:
				l.logger.Info("skipping preemption: the capacity is already being freed",
					zap.String("exp", exp.ID), zap.String("cluster", cluster))
			default:
				committed, err = l.preempt(ctx, cluster, gAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeResourcesTotal[cluster], nodeLabels[cluster], burstRunning, exp)
				if err != nil {
					l.logger.Warn("preemption failed", zap.String("exp", exp.ID), zap.Error(err))
					tickErrs = append(tickErrs, fmt.Errorf("experiment %s: preempt on %s: %w", exp.ID, cluster, err))
				}
			}
			// A guaranteed job whose capacity is being freed holds what is free against the burst
			// pass, or burst refills the room and the job never fits. On a cluster of slots the hold
			// is the slot itself, taken until its instance leaves: the total is the room of other
			// slots, which never take this job.
			if !slots && (promised || committed) {
				placement.SubtractFootprint(bAvail[cluster], fp)
			}
			if promised {
				placement.SubtractFootprint(desiredFree[cluster], fp)
			}
			// Only a preempt that actually requeued victims makes the running set stale. Marking
			// it stale unconditionally cost a full re-read for every blocked job behind it on a
			// saturated cluster, where preempt most often finds nothing to take.
			if committed {
				runningLoaded = false
			}
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
			if err == nil && !committed && !promised && !disbalanceRan[cluster] {
				disbalanceRan[cluster] = true
				if err := l.evictDisbalanced(ctx, exp, cluster, gAvail[cluster], totalCapacity[cluster], nodeResourcesTotal[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
					tickErrs = append(tickErrs, fmt.Errorf("experiment %s: disbalance eviction on %s: %w", exp.ID, cluster, err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			backlog.record(start, cluster, "guaranteed", exp, shortage)
			noOffer := noOfferReason(exp, cluster, nodeResourcesTotal[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster])
			neverFits := whenIdle.neverFitsReason(exp)
			reason := ""
			switch {
			case leaving != "":
				reason = waitingForSlotReason(cluster, leaving)
			case waitingForScaleUp:
				reason = waitingForScaleUpReason(cluster)
			case allSpeculativeCandidatesTried(exp, autoscalerEnabled, connectedClusters, clusterIDs, registeredClusters, l.triedClusterTTL):
				reason = noScalableCapacityReason()
			case misfit != "":
				reason = misfit
			case neverFits != "":
				reason = neverFits
			case noOffer != "":
				reason = noOffer
			default:
				fitAtTickStart := domain.Fits(gAvailInitial[cluster], fp) &&
					placement.TopologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
				claimed, cerr := resolveCache.loadClaimed(ctx)
				if cerr != nil {
					l.skipExperiment(&tickErrs, exp, fmt.Errorf("list the jobs that hold capacity: %w", cerr))
					continue
				}
				reason = notAdmittedReasonFor(fitAtTickStart, totalCovers, promised, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
				l.holdRoom(exp, cluster, fp, clusters, held, gAvail, bAvail)
			}
			if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Log(submitErrorLevel(err), "submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		placement.SubtractFootprint(gAvail[cluster], fp)
		// bAvail is the same shared pool's other view — without this, the burst pass later in
		// this tick could still see this unit as free and double-book it.
		placement.SubtractFootprint(bAvail[cluster], fp)
		placement.SubtractFootprint(desiredFree[cluster], fp)
		placement.Reserve(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "admitted").Inc()
	}

	// 5. Burst pass: fairness-weighted (least quota used first), then completion proximity, shortest job first.
	burst := filterTier(queued, domain.CapacityBurst)

	// No cluster-wide "is there any capacity at all" shortcut here: a footprint's dimensions are
	// different units (millicores, bytes, device counts), so summing them into one number answers
	// no question anyone asked — RAM bytes dominate by orders of magnitude and the sum is
	// positive whatever the accelerator situation is. Fit is decided per job, per dimension, by
	// domain.Fits in the loop below, which writes the same capacity_unavailable reason.

	// Fetch quota usage for fairness ordering.
	burst, quotaMap := l.fetchQuotaMap(ctx, burst, &tickErrs)
	sortBurst(burst, quotaMap, completion)
	// Interleave by agent so one agent's queue depth can't claim every unit of burst capacity
	// that frees up this tick ahead of another agent with fewer jobs waiting — see
	// interleaveByAgent's doc comment.
	burst = interleaveByAgent(burst)

	for _, exp := range burst {
		persistedFlavor := exp.AcceleratorType
		placed, ok := l.placeQueued(ctx, "burst", exp, bAvail, quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)], clusters, &tickErrs)
		if !ok {
			continue
		}
		cluster, fp, misfit := placed.cluster, placed.fp, placed.misfit
		fits := domain.Fits(bAvail[cluster], fp) && placement.TopologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		if exp.EnvironmentImage != "" && (misfit != "" || !fits) {
			l.waitForPush(ctx, "burst", environments, exp, &tickErrs)
			continue
		}
		if !fits {
			// Same shortage vector the guaranteed pass computes, for the same reason: "short
			// {cpu: 12}" tells an operator which dimension to fix, where a bare
			// capacity_unavailable tells them only that something, somewhere, did not fit.
			shortage := placement.PreemptionShortfall(bAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
			// The guaranteed pass logs every skip ("guaranteed job needs preemption" below); the
			// burst pass had no equivalent, so a burst job stuck QUEUED for many ticks left no
			// trail at all — the actual bAvail[cluster]/nodeAvail[cluster] values a tick computed
			// could only be reconstructed after the fact from Postgres/metrics archaeology, not
			// read off the logs. cluster=="" (every candidate flavor/cluster combination failed to
			// resolve) is called out explicitly because bAvail[""] is always the nil map, so the
			// shortage vector below is the job's whole footprint regardless of real capacity —
			// worth distinguishing from a genuine per-dimension shortfall at a glance.
			l.logger.Info("burst job not admitted this tick",
				zap.String("exp", exp.ID), zap.String("cluster", cluster), zap.Bool("cluster_unresolved", cluster == ""),
				zap.String("avail", footprintStr(bAvail[cluster])), zap.String("need", footprintStr(fp)),
				zap.String("shortage", footprintStr(shortage)))
			fitAtTickStart := domain.Fits(bAvailInitial[cluster], fp) &&
				placement.TopologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
			// A job that fit when the tick began lost its room to a higher rank and waits; nothing
			// running is evicted for it.
			if !fitAtTickStart && !disbalanceRan[cluster] {
				disbalanceRan[cluster] = true
				if err := l.evictDisbalanced(ctx, exp, cluster, bAvail[cluster], totalCapacity[cluster], nodeResourcesTotal[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], fp); err != nil {
					l.logger.Warn("disbalance eviction failed", zap.String("exp", exp.ID), zap.Error(err))
					tickErrs = append(tickErrs, fmt.Errorf("experiment %s: disbalance eviction on %s: %w", exp.ID, cluster, err))
				}
			}
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			backlog.record(start, cluster, "burst", exp, shortage)
			reason := misfit
			if reason == "" {
				reason = whenIdle.neverFitsReason(exp)
			}
			if reason == "" {
				reason = noOfferReason(exp, cluster, nodeResourcesTotal[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster])
			}
			if reason == "" {
				claimed, cerr := resolveCache.loadClaimed(ctx)
				if cerr != nil {
					l.skipExperiment(&tickErrs, exp, fmt.Errorf("list the jobs that hold capacity: %w", cerr))
					continue
				}
				scalarFits := domain.Fits(bAvail[cluster], fp)
				freed := !scalarFits && !placement.HasSlots(nodeLabels[cluster]) && domain.Fits(desiredFree[cluster], fp)
				reason = notAdmittedReasonFor(fitAtTickStart, scalarFits, freed, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
				l.holdRoom(exp, cluster, fp, clusters, held, bAvail)
			}
			if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Log(submitErrorLevel(err), "submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		placement.SubtractFootprint(bAvail[cluster], fp)
		placement.Reserve(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "admitted").Inc()
	}

	backlog.publish(speculativeFootprintByCluster)

	// After both passes, so a job told it is held is told the reason this tick gave it.
	if err := l.store.MarkHeldJobs(ctx, l.heldJobNotice); err != nil {
		tickErrs = append(tickErrs, fmt.Errorf("mark held jobs: %w", err))
	}

	return errors.Join(tickErrs...)
}

// markNotAdmitted writes why exp waits, and nothing when its row already says so: a full queue
// otherwise rewrote every waiting row on every tick.
func (l *Loop) markNotAdmitted(ctx context.Context, exp *domain.Experiment, reason string) error {
	code, message := domain.SplitQueueReason(reason)
	if code == exp.NotAdmittedReason && message == exp.NotAdmittedMessage {
		return nil
	}
	if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
		return err
	}
	exp.NotAdmittedReason, exp.NotAdmittedMessage = code, message
	return nil
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
