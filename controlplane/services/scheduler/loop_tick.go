package scheduler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
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

// tickClusters is what one tick read of the clusters, which every placement of both passes is
// decided against. The maps are the tick's own: an admission subtracts from them in place.
type tickClusters struct {
	start        time.Time
	registered   map[string]domain.Cluster
	multiNode    map[string]bool
	clusterIDs   map[string]string
	nodeLabels   map[string]map[string]map[string]string
	resolveCache *resolutionCache
	whenIdle     installed

	nodeAvail, nodeResources, nodeResourcesTotal, nodeAvailAtTickStart map[string]map[string]map[string]int64
}

// queuedPlacement is where a queued job is tried in one pass: the cluster, "" when none could be picked,
// and its footprint there. misfit is why the cluster picked does not take the job's own cpu,
// memory and storage, and betFlavor the flavor a bet on a cluster that scales up is made at.
type queuedPlacement struct {
	cluster   string
	fp        domain.Footprint
	betFlavor domain.AcceleratorType
	misfit    string
}

// placeQueued settles where exp is tried in one pass, for the guaranteed and the burst pass
// alike, against avail, the pass's view of free capacity. ok is false when the job is out of this
// tick: no cluster may run it, its quota no longer covers the type it ran on, or it could not be
// resolved, and the reason is written or the failure recorded.
//
// A job already assigned a cluster stays there and only its footprint is recomputed. Any other is
// given a (cluster, flavor) pair among the requested type and its acceptable ones where the whole
// footprint fits (placement.ClusterAndFootprint). The search says its choice by setting
// exp.AcceleratorType, and the resolved job is set on exp.
func (l *Loop) placeQueued(ctx context.Context, pass string, exp *domain.Experiment, avail map[string]domain.Footprint, quota *domain.AgentQuota, c tickClusters, tickErrs *[]error) (queuedPlacement, bool) {
	skip := func(reason string) (queuedPlacement, bool) {
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues(pass, "skipped").Inc()
		if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
		}
		return queuedPlacement{}, false
	}
	placed := queuedPlacement{cluster: exp.ClusterName, betFlavor: exp.AcceleratorType}
	candidates, ineligible := eligibleClusters(avail, c.multiNode, c.registered, exp)
	if ineligible != "" {
		return skip(ineligible)
	}
	candidates = withoutFailed(candidates, c.clusterIDs, exp, l.triedClusterTTL, c.start)
	candidates = c.whenIdle.couldHold(candidates, exp)
	pinned := placed.cluster != ""
	if pinned {
		placed.fp = exp.Footprint()
	}
	// Check the job's cpu, memory and storage against the picked cluster's own fair share (or size
	// it to a slot), once cluster and accelerator flavor are settled: see loop_resolve.go. An
	// unpinned job whose own resources the cluster cannot fit is placed again among the rest, and
	// is told it misfits only when every candidate misfits: preemption cannot fix a job whose own
	// request is oversized for every node.
	var misfits []string
	queuedAt := exp.AcceleratorType
	for {
		if !pinned {
			exp.AcceleratorType, placed.betFlavor = queuedAt, queuedAt
			placed.cluster, placed.fp = placement.ClusterAndFootprint(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(quota, exp) {
			if !slices.Contains(placement.CandidateTypes(exp), exp.Job.AcceleratorType) {
				return skip(ranOnTypeUncoveredReason(exp))
			}
			// Reverting to the requested flavor re-places the job, layout included: picking on
			// scalar fit alone could point a spread job at a cluster whose nodes cannot hold it,
			// and charge it a failed placement there.
			exp.AcceleratorType = exp.Job.AcceleratorType
			placed.betFlavor = exp.AcceleratorType
			placed.fp = exp.Footprint()
			if cluster, ok := placement.AtFlavor(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp); ok {
				placed.cluster = cluster
			} else {
				placed.cluster = placement.BestFit(candidates, c.registered, placed.fp)
			}
		}
		cluster := placed.cluster
		if cluster == "" {
			break
		}
		resolvedJob, misfit, err := l.resolveClusterLocalResources(ctx, c.resolveCache, exp, cluster, c.nodeResourcesTotal[cluster], c.nodeAvailAtTickStart[cluster], c.nodeResources[cluster], c.nodeLabels[cluster])
		if err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("resolve cluster-local resources: %w", err))
			return queuedPlacement{}, false
		}
		if misfit == "" {
			exp.ResolvedJob = resolvedJob
			placed.fp = exp.Footprint()
			return placed, true
		}
		misfits = append(misfits, misfit)
		placed.cluster = ""
		if pinned {
			break
		}
		candidates = maps.Clone(candidates)
		delete(candidates, cluster)
	}
	if len(misfits) > 0 {
		placed.misfit = joinMisfits(misfits)
	}
	return placed, true
}

// holdRoom keeps what cluster has free from every job after exp in this tick, once exp is held
// (MarkHeldJobs): it has waited longer than heldJobNotice and than its own estimate. Smaller jobs
// behind it otherwise take each core as it frees and exp never starts. One job holds a cluster per
// tick, and only a cluster that holds it when idle, so the room left idle is one waiting job's.
func (l *Loop) holdRoom(exp *domain.Experiment, cluster string, fp domain.Footprint, c tickClusters, held map[string]bool, views ...map[string]domain.Footprint) {
	if l.heldJobNotice <= 0 || exp.QueuedAt == nil || held[cluster] || placement.HasSlots(c.nodeLabels[cluster]) {
		return
	}
	if waited := c.start.Sub(*exp.QueuedAt); waited < l.heldJobNotice || waited.Hours() < exp.EstimatedDurationHours {
		return
	}
	if total, ok := c.whenIdle.totals[cluster]; !ok || !placement.FitsWhenIdle(total, c.whenIdle.nodeTotals[cluster], exp) {
		return
	}
	held[cluster] = true
	for _, view := range views {
		placement.SubtractFootprint(view[cluster], fp)
	}
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

// notAdmittedReasonFor names why a queued job was skipped, as "code: message". fitAtTickStart is
// the whole discriminator: outranked means the job *would* have been admitted against the
// capacity this tick started with and lost it to jobs earlier in sort order, so waiting is the
// right response. A request oversized for the hardware is never told it was outranked.
//
// scalarFits is the case waiting on the cluster total cannot explain: the total has room and no
// arrangement of the job's nodes over the cluster's nodes holds it. cluster is the one the job
// was judged on, "" when none could be picked, and held the jobs that hold there what it is
// short of.
//
// freedInDesiredState is the case the cluster's own figures cannot explain yet: with every job
// the platform has placed there counted, the cluster has room for this one, and its last capacity
// report still shows less. Its totals are net of everything that is no platform job, so what is
// missing is a job that ended and that the runtime has not reported gone. That holds for cores,
// memory and storage. A device may be held by something the platform never placed, so a job
// short of one is not told a report will free it.
func notAdmittedReasonFor(fitAtTickStart, scalarFits, freedInDesiredState bool, shortage domain.Footprint, cluster string, held holders) string {
	if fitAtTickStart {
		return domain.NotAdmittedOutranked + ": it fit when this pass began, and jobs ahead of it in the queue took the room"
	}
	short := describeFootprint(shortage)
	switch {
	case cluster == "" && short == "":
		return domain.NotAdmittedCapacityUnavailable + ": no cluster that runs it has room for it now"
	case cluster == "":
		return domain.NotAdmittedCapacityUnavailable + ": no cluster that runs it has room for it now: it is short of " + short
	case scalarFits && short == "":
		return fmt.Sprintf("%s: cluster %s has room for it in total and no node of it holds the job", domain.NotAdmittedCapacityUnavailable, cluster)
	case scalarFits:
		return fmt.Sprintf("%s: cluster %s has room for it in total and no node of it holds the job: the node nearest to holding it is short of %s", domain.NotAdmittedCapacityUnavailable, cluster, short)
	case short == "":
		return fmt.Sprintf("%s: cluster %s has no room for it now", domain.NotAdmittedCapacityUnavailable, cluster)
	case freedInDesiredState && !shortOfADevice(shortage):
		return fmt.Sprintf("%s: cluster %s is short of %s by its last capacity report, and no job the platform has placed there holds it: a job that ended is not reported gone yet, and this one is waiting for the cluster's next capacity report",
			domain.NotAdmittedCapacityUnavailable, cluster, short)
	}
	return fmt.Sprintf("%s: cluster %s is short of %s, %s", domain.NotAdmittedCapacityUnavailable, cluster, short, held)
}

func shortOfADevice(shortage domain.Footprint) bool {
	for key, short := range shortage {
		if short > 0 && key.Kind == domain.ResourceKindAccelerator && !domain.CPUType.Is(domain.AcceleratorType(key.Flavor)) {
			return true
		}
	}
	return false
}

// holders counts the jobs that hold, on one cluster, what a queued job is short of there: those
// of the queued job's own platform experiment and those of any other. Another platform
// experiment may be another org's, so it is counted and never named.
type holders struct {
	own, others int
}

func (h holders) String() string {
	jobs := func(n int) string {
		if n == 1 {
			return "1 job"
		}
		return fmt.Sprintf("%d jobs", n)
	}
	switch {
	case h.own+h.others == 0:
		return "which no job holds: the cluster's nodes do not have it free"
	case h.others == 0:
		return "held by " + jobs(h.own) + " of this platform experiment"
	case h.own == 0:
		return "held by " + jobs(h.others) + " of other platform experiments"
	}
	return fmt.Sprintf("held by %s: %d of this platform experiment and %d of other platform experiments", jobs(h.own+h.others), h.own, h.others)
}

// holdersOf counts, among the jobs that have claimed capacity, those on cluster that hold some of
// a dimension exp is short of.
func holdersOf(claimed []*domain.Experiment, cluster string, shortage domain.Footprint, exp *domain.Experiment) holders {
	var held holders
	for _, other := range claimed {
		if other.ClusterName != cluster || other.ID == exp.ID {
			continue
		}
		footprint := other.Footprint()
		holds := false
		for key, short := range shortage {
			if short > 0 && footprint[key] > 0 {
				holds = true
				break
			}
		}
		switch {
		case !holds:
		case other.PlatformExperimentID == exp.PlatformExperimentID:
			held.own++
		default:
			held.others++
		}
	}
	return held
}

// installed is what the clusters have with nothing running on them. A job is held to it at submit
// and in the queue by the one function that reads it, neverFitsReason.
type installed struct {
	registered map[string]domain.Cluster
	// totals has a cluster only while it is connected and reports all it has.
	totals     map[string]domain.Footprint
	nodeTotals map[string]map[string]map[string]int64
	scalesUp   map[string]bool
	multiNode  map[string]bool
}

// neverFitsReason is the not_admitted_reason of a job that no wait admits as it is stated, and ""
// for one that may yet run. Such a job asks for more than every cluster that runs its image
// choice and reports what it has installed holds when idle (placement.FitsWhenIdle), on each accelerator
// type the job accepts. A cluster that scales up may yet hold it. A cluster that reports nothing
// is away or not whole: it is named and not judged, and with none judged the job waits.
//
// The cause is the job's image where a cluster that runs another holds the job when idle, and its
// size otherwise. Both say what each cluster judged has.
func (in installed) neverFitsReason(exp *domain.Experiment) string {
	runs, others := byImageChoice(in.registered, exp)
	if len(runs) == 0 {
		return noClusterRunsImageReason(others, exp)
	}
	spansNodes := exp.Job.Nodes() > 1
	var judged []string
	lacking := 0
	for _, c := range runs {
		total, reported := in.totals[c.Name]
		switch {
		case !reported:
			judged = append(judged, c.Name+" reports nothing now")
		case spansNodes && !in.multiNode[c.Name]:
		case in.scalesUp[c.Name] || placement.FitsWhenIdle(total, in.nodeTotals[c.Name], exp):
			return ""
		default:
			lacking++
			judged = append(judged, c.Name+" has "+describeOffer(total, in.nodeTotals[c.Name], exp))
		}
	}
	if lacking == 0 {
		return ""
	}
	var holding []string
	for _, c := range others {
		total, reported := in.totals[c.Name]
		if reported && (!spansNodes || in.multiNode[c.Name]) && placement.FitsWhenIdle(total, in.nodeTotals[c.Name], exp) {
			holding = append(holding, c.Name+" runs every job in "+c.JobImage)
		}
	}
	asks := describeFootprint(exp.Footprint())
	if spansNodes {
		asks += fmt.Sprintf(" over %d nodes", exp.Job.Nodes())
	}
	has := strings.Join(judged, "; ")
	if len(holding) == 0 {
		return fmt.Sprintf("%s: the job asks for %s, more than any connected cluster that runs it has when idle: %s. Ask for less (hl capacity lists what each cluster has)",
			domain.NotAdmittedExceedsClusterCapacity, asks, has)
	}
	return fmt.Sprintf("%s: the job names image %s, and no connected cluster that runs it has what the job asks for, %s, when idle: %s. A cluster with one job image has it (%s): %s",
		domain.NotAdmittedNoClusterForImage, exp.Job.Image, asks, has, strings.Join(holding, "; "), nameThatImage)
}

// couldHold narrows candidates to the clusters that hold exp when idle, so a job that fits nowhere
// now waits on, and is held room on, a cluster it can run on. A cluster that reports no totals is
// kept, and all of candidates when none is left.
func (in installed) couldHold(candidates map[string]domain.Footprint, exp *domain.Experiment) map[string]domain.Footprint {
	kept := make(map[string]domain.Footprint, len(candidates))
	for cluster, free := range candidates {
		if total, ok := in.totals[cluster]; !ok || placement.FitsWhenIdle(total, in.nodeTotals[cluster], exp) {
			kept[cluster] = free
		}
	}
	if len(kept) == 0 {
		return candidates
	}
	return kept
}

// nameThatImage is how a job gets onto a cluster that runs one image: every job names its image.
const nameThatImage = "state that image as job.image to run there"

// noClusterRunsImageReason is the reason of a job whose image no cluster of its org runs, with
// which image runs where. "" when the org has no cluster with another image either, which is the
// org's matter and none of the job's.
func noClusterRunsImageReason(others []domain.Cluster, exp *domain.Experiment) string {
	if len(others) == 0 {
		return ""
	}
	serving := make([]string, len(others))
	for i, c := range others {
		serving[i] = c.Name + " runs every job in " + c.JobImage
	}
	return domain.NotAdmittedNoClusterForImage + ": the job names image " + exp.Job.Image + ", and " + strings.Join(serving, "; ") + ": " + nameThatImage
}

// describeOffer says what a cluster has installed of what exp asks for, and the most one node of
// it has where it has several.
func describeOffer(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) string {
	asked := domain.NewFootprint()
	probe := *exp
	types := placement.CandidateTypes(exp)
	if len(types) == 0 {
		types = []domain.AcceleratorType{exp.AcceleratorType}
	}
	for _, t := range types {
		probe.AcceleratorType = t
		for key := range probe.Footprint() {
			asked[key] = total[key]
		}
	}
	offer := describeFootprint(asked)
	if len(nodeTotals) < 2 {
		return offer
	}
	largest := map[string]int64{}
	for _, node := range nodeTotals {
		for name, amount := range node {
			largest[name] = max(largest[name], amount)
		}
	}
	return offer + ", at most " + describeNodeResources(largest) + " on one node"
}

// eligibleClusters narrows the clusters a job may be placed on to the ones that may and can run
// it: registered clusters of the job's own org and shared ones that the org has not turned off,
// and of those, for a job that spans nodes, the ones that report multi-node execution. This is
// the one place the platform decides which org's jobs a cluster takes.
//
// The returned map shares its Footprint values with avail rather than copying them, so the
// in-place reservations admission makes against the returned view are the same numbers every
// later job in this tick reads. A copy here would let a job admitted through the narrowed view
// be double-booked by one placed through the full one.
//
// ineligible is the not_admitted_reason when a filter is what emptied the result, and it is the
// only thing that makes an empty result diagnosable. Whether the job's org has a cluster at all
// is read from what is registered, never from what is connected: an org whose own cluster is
// away has a cluster, and what other orgs have connected is not its business to learn. With no
// cluster of its own connected a single-node job gets an empty set and no reason: that is a
// heartbeat gap, and reporting it as a missing cluster or a missing capability would tell the
// submitter to fix something that is not broken.
func eligibleClusters(avail map[string]domain.Footprint, multiNodeCapable map[string]bool, registered map[string]domain.Cluster, exp *domain.Experiment) (clusters map[string]domain.Footprint, ineligible string) {
	hosted := make(map[string]domain.Footprint, len(avail))
	for cluster, footprint := range avail {
		if hostsJob(registered, cluster, exp) {
			hosted[cluster] = footprint
		}
	}
	// A job never reaches the queue holding a cluster, so this refuses only a pin written
	// around admission.
	pinnedElsewhere := exp.ClusterName != "" && !hostsJob(registered, exp.ClusterName, exp)
	if pinnedElsewhere || !orgHasCluster(registered, exp.OrgID, domain.Cluster.OpenTo) {
		return map[string]domain.Footprint{}, noClusterForOrgReason()
	}
	if !orgHasCluster(registered, exp.OrgID, domain.Cluster.Hosts) {
		return map[string]domain.Footprint{}, clustersDisabledReason()
	}
	if runs, others := byImageChoice(registered, exp); len(runs) == 0 {
		return map[string]domain.Footprint{}, noClusterRunsImageReason(others, exp)
	}
	if exp.Job.Nodes() <= 1 {
		return hosted, ""
	}
	out := make(map[string]domain.Footprint, len(hosted))
	for cluster, footprint := range hosted {
		if multiNodeCapable[cluster] {
			out[cluster] = footprint
		}
	}
	if len(out) == 0 {
		return out, noMultiNodeClusterReason(exp.Job.Nodes())
	}
	return out, ""
}

// orgHasCluster reports whether any registered cluster, connected or not, is the org's in the
// given sense: open to it (domain.Cluster.OpenTo), or taking its jobs (domain.Cluster.Hosts).
func orgHasCluster(registered map[string]domain.Cluster, orgID string, is func(domain.Cluster, string) bool) bool {
	for _, cluster := range registered {
		if is(cluster, orgID) {
			return true
		}
	}
	return false
}

// hostsJob reports whether cluster is registered, takes jobs of exp's org and runs the image exp
// names (domain.Cluster.HostsJob).
func hostsJob(registered map[string]domain.Cluster, cluster string, exp *domain.Experiment) bool {
	c, ok := registered[cluster]
	return ok && c.HostsJob(exp.OrgID, exp.Job.Image)
}

// byImageChoice splits the clusters that take jobs of exp's org into those that run the image
// exp names, or run it naming none (domain.Cluster.Runs), and the others, each sorted by name.
func byImageChoice(registered map[string]domain.Cluster, exp *domain.Experiment) (runs, others []domain.Cluster) {
	for _, c := range registered {
		switch {
		case !c.Hosts(exp.OrgID):
		case c.Runs(exp.Job.Image):
			runs = append(runs, c)
		default:
			others = append(others, c)
		}
	}
	byName := func(a, b domain.Cluster) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(runs, byName)
	slices.SortFunc(others, byName)
	return runs, others
}

// quotaCanCoverFlavor is an early, non-authoritative filter for a more expensive acceptable
// flavor; ReserveAdmittedFlavor's transaction remains the final concurrency-safe check.
func quotaCanCoverFlavor(quota *domain.AgentQuota, exp *domain.Experiment) bool {
	if quota == nil || exp.AcceleratorCount <= 0 {
		return false
	}
	newCost, err := exp.EstimateHeld()
	if err != nil {
		return false
	}
	used, limit := quota.UsedBurstAccH, quota.BurstAcceleratorHours
	if exp.CapacityTier == domain.CapacityGuaranteed {
		used, limit = quota.UsedGuaranteedAccH, quota.GuaranteedAcceleratorHours
	}
	return used-exp.EstimatedCostAccH+newCost <= limit && quota.Used()-exp.EstimatedCostAccH+newCost <= quota.Share()
}

// ranOnTypeUncoveredReason is the not_admitted_reason of a job that ran on an alternative type
// its quota no longer covers. The type it asked for is not one it may be placed on
// (placement.CandidateTypes), so it waits for quota.
func ranOnTypeUncoveredReason(exp *domain.Experiment) string {
	return fmt.Sprintf("%s: the job ran on %s and is placed on that type alone, which the agent's quota no longer covers", domain.NotAdmittedInsufficientQuota, exp.RanOnAcceleratorType)
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

// environmentPass is what one tick read of the environments queued jobs wait for: each
// environment once, after its build was assigned, and the live clusters once, when a first
// environment is assigned.
type environmentPass struct {
	seen    map[string]*domain.Environment
	live    []string
	holding []string
	read    bool
}

func newEnvironmentPass() *environmentPass {
	return &environmentPass{seen: map[string]*domain.Environment{}}
}

// assignBuild assigns the build of the environment of key to one of the live clusters the pass
// read, reading them first if it has not (db.EnvironmentsStore.AssignEnvironment).
func (l *Loop) assignBuild(ctx context.Context, pass *environmentPass, key string) (*domain.Environment, error) {
	if !pass.read {
		live, err := l.clusters.GetLiveClusters(ctx)
		if err != nil {
			return nil, fmt.Errorf("live clusters: %w", err)
		}
		for name, connected := range live {
			if connected {
				pass.live = append(pass.live, name)
			}
			pass.holding = append(pass.holding, name)
		}
		pass.read = true
	}
	return l.store.AssignEnvironment(ctx, key, pass.live, pass.holding, domain.EnvironmentBuildLease)
}

// AssignBuild assigns the build of the environment of key to a live cluster now, as a tick does
// for a job waiting for it, and returns the environment as it is then, nil when there is none.
// It is one statement, so it races with no tick.
func (l *Loop) AssignBuild(ctx context.Context, key string) (*domain.Environment, error) {
	return l.assignBuild(ctx, newEnvironmentPass(), key)
}

// heldForEnvironment says whether exp waits for the environment its commit declares, whose build
// it assigns to a live cluster first (db.EnvironmentsStore.AssignEnvironment): while it is built
// it says so, once the file failed exp ends with the end of the build's output, so its agent fixes
// the file and submits again, and once the platform failed it on every attempt exp ends as the
// platform's fault at no cost, and the next submit builds it again. A build its cluster already
// holds on one node does not hold exp: exp is pinned there for this pass (pinToLoadedNode).
func (l *Loop) heldForEnvironment(ctx context.Context, pass *environmentPass, exp *domain.Experiment, registered map[string]domain.Cluster, nodeLabels map[string]map[string]map[string]string) (bool, error) {
	env, ok := pass.seen[exp.EnvironmentKey]
	if !ok {
		var err error
		if env, err = l.assignBuild(ctx, pass, exp.EnvironmentKey); err != nil {
			return false, err
		}
		if env == nil {
			return false, fmt.Errorf("job %s names environment %s, which does not exist", exp.ID, exp.EnvironmentKey)
		}
		pass.seen[exp.EnvironmentKey] = env
	}
	switch {
	case env.State == domain.EnvironmentReady:
		exp.EnvironmentCluster = env.ClusterName
		return false, nil
	case env.State == domain.EnvironmentBuilding:
		if pinToLoadedNode(exp, env, registered, nodeLabels) {
			return false, nil
		}
		reason := domain.NotAdmittedEnvironmentBuilding + ": " + buildingMessage(env, false)
		return true, l.markNotAdmitted(ctx, exp, reason)
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultFile:
		return true, l.endForEnvironment(ctx, exp, domain.StatusRejected, string(domain.EvictionEnvironmentFailed.WithDetail(fileFailed(env))))
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultPlatform:
		return true, l.endForEnvironment(ctx, exp, domain.StatusEvicted, string(domain.EvictionEnvironmentUnbuilt.WithDetail("the platform could not build the environment its commit declares in "+domain.EnvironmentFile+
			" on "+env.Base+" in "+strconv.Itoa(domain.MaxEnvironmentAttempts)+" attempts, no fault of the file's. It costs nothing: submit again and the platform builds it again")))
	}
	return false, fmt.Errorf("environment %s of job %s is %s with fault %q", env.Key, exp.ID, env.State, env.Fault)
}

// pinToLoadedNode pins exp, for this pass, to the node of the cluster that built its environment
// and holds the build's image before it is pushed, and to that image (domain.Experiment.
// EnvironmentImage), and says whether it did. The pin is the node's hostname label in exp's node
// selector, which placement honours as any other and the runtime is handed with the resolved job:
// a job placed so starts there without a pull while the push goes on. A job on more than one node,
// one whose own selector names another node, and one that cluster does not host wait for the push.
func pinToLoadedNode(exp *domain.Experiment, env *domain.Environment, registered map[string]domain.Cluster, nodeLabels map[string]map[string]map[string]string) bool {
	if env.LoadedImage == "" || env.LoadedCluster == "" || exp.Job.Nodes() > 1 || !hostsJob(registered, env.LoadedCluster, exp) {
		return false
	}
	hostname := nodeLabels[env.LoadedCluster][env.LoadedNode][domain.NodeHostnameLabel]
	if hostname == "" {
		return false
	}
	if own, ok := exp.Job.NodeSelector[domain.NodeHostnameLabel]; ok && own != hostname {
		return false
	}
	selector := make(map[string]string, len(exp.Job.NodeSelector)+1)
	maps.Copy(selector, exp.Job.NodeSelector)
	selector[domain.NodeHostnameLabel] = hostname
	exp.Job.NodeSelector = selector
	exp.ClusterName = env.LoadedCluster
	exp.EnvironmentImage = env.LoadedImage
	return true
}

// waitForPush leaves exp, pinned to the node its environment is loaded on, queued for this pass:
// that node has no room for it now, and nothing is preempted or bet for a wait that ends with the
// push, after which it is placed anywhere.
func (l *Loop) waitForPush(ctx context.Context, pass string, environments *environmentPass, exp *domain.Experiment, tickErrs *[]error) {
	obsmetrics.AdmissionTickResultsTotal.WithLabelValues(pass, "skipped").Inc()
	reason := domain.NotAdmittedEnvironmentBuilding + ": " + buildingMessage(environments.seen[exp.EnvironmentKey], false)
	if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
		l.skipExperiment(tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
	}
}

// buildingMessage is what a job waiting for env to build is told. Where the build runs, its attempt
// and its last step are said only with live, on a read: the stored queue reason changes with none
// of them, since its every change is an update that wakes the job's agent.
func buildingMessage(env *domain.Environment, live bool) string {
	message := "the platform builds the environment its commit declares in " + domain.EnvironmentFile + ", on " + env.Base + ". The job runs in it once it is built"
	if live {
		message += ". " + buildProgress(env)
	}
	if env.FailedLog != "" {
		message += ". This file failed to build before, and the platform builds it once more. The end of that build's output:\n" + env.FailedLog
	}
	return message
}

func buildProgress(env *domain.Environment) string {
	var progress string
	switch {
	case env.LoadedImage != "" && env.LoadedCluster == env.ClusterName:
		progress = "It is built and loaded on node " + env.LoadedNode + " of cluster " + env.LoadedCluster + ", which pushes it to the registry now: " +
			"a job of one node that fits there runs there at once, any other once the push ends. "
	case env.LoadedImage != "":
		progress = "It is built and loaded on node " + env.LoadedNode + " of cluster " + env.LoadedCluster + ", whose push to the registry failed: " +
			"a job of one node that fits there runs there at once, any other once the platform has built and pushed it again. "
	}
	if env.ClusterName == "" {
		return progress + "It waits for a live cluster that builds for its org to have room"
	}
	progress += "It builds on cluster " + env.ClusterName + ", attempt " + strconv.Itoa(env.Attempt) + " of " + strconv.Itoa(domain.MaxEnvironmentAttempts)
	if env.Attempt > 1 {
		progress += " (the platform builds a file again only after a failure of its own)"
	}
	if env.Progress != "" && env.ProgressAt != nil {
		progress += ", and reported at " + env.ProgressAt.UTC().Format(time.RFC3339) + ": " + env.Progress
	}
	return progress
}

// fileFailed says that the environment file of env did not build, with the end of its output.
func fileFailed(env *domain.Environment) string {
	return domain.EnvironmentFile + " of its commit did not build on " + env.Base + ". Fix it, commit and submit again; the same file submitted again is built once more. The end of the build's output:\n" + env.LogTail
}

// endForEnvironment ends exp, queued for its environment, as to with reason, settled: its usage
// is written first and the hold goes with the status, so the agent the status notifies reads its
// quota as it stands. An environment the platform did not build costs nothing.
func (l *Loop) endForEnvironment(ctx context.Context, exp *domain.Experiment, to domain.ExperimentStatus, reason string) error {
	ended := *exp
	ended.Status, ended.EvictionReason = to, reason
	if err := l.settler.Settle(ctx, &ended); err != nil {
		return fmt.Errorf("settle job %s whose environment did not build: %w", exp.ID, err)
	}
	written, err := l.store.EndQueuedSettled(ctx, exp.ID, to, reason)
	if err != nil || !written {
		return err
	}
	*exp = ended
	return nil
}
