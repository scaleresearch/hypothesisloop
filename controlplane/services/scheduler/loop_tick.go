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
	gAvail, desiredFree, err := readFlavorCapacity(ctx, l.workload, l.logger)
	if err != nil {
		return err
	}
	// Guaranteed and burst share one physical pool per cluster; preemption enforces the tier
	// boundary.
	bAvail := cloneAvail(gAvail)
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
	// Which clusters can run a job that spans more than one node. A capability each cluster
	// reports about itself, filtered on here so a distributed job is never placed on a runtime
	// that executes single-node jobs only — that used to be discovered at workload creation,
	// after the job had already been admitted and had already claimed a reservation.
	multiNodeCapable, err := l.workload.GetMultiNodeCapability(ctx)
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
	// WithSpeculation — a LoopWorkloadClient that has not implemented these two methods (every
	// pre-autoscaler test fake, and any deployment that never calls WithSpeculation) must not be
	// asked to.
	var autoscalerEnabled map[string]bool
	var clusterIDs map[string]string
	if l.triedClusterTTL > 0 {
		autoscalerEnabled, err = l.workload.GetAutoscalerCapability(ctx)
		if err != nil {
			return fmt.Errorf("autoscaler capability: %w", err)
		}
		clusterIDs, err = l.workload.GetClusterIDs(ctx)
		if err != nil {
			return fmt.Errorf("cluster ids: %w", err)
		}
	}
	// gAvail only has entries for clusters GetFlavorCapacity's heartbeat read found connected
	// (see queuebackend.Backend.GetFlavorCapacity) — reusing its key set here is the same
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
	totalCapacity, err := l.workload.GetTotalCapacity(ctx)
	if err != nil {
		l.logger.Warn("total capacity unavailable; disbalance eviction cannot judge this tick", zap.Error(err))
		totalCapacity = nil
	}
	// Per-node installed CPU/memory/storage — the stable denominator evictDisbalanced's node-local
	// fair-share math needs, since nodeResources above is free capacity and moves every tick. Same
	// degrade-gracefully treatment as totalCapacity immediately above: absent for a cluster (or
	// entirely, on a read failure) just means that cluster's disbalance judgment abstains this
	// tick, never that the whole tick fails.
	nodeResourcesTotal, err := l.workload.GetNodeTotalCapacity(ctx)
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
	nodeAvailAtTickStart := cloneNodeAvailByCluster(nodeAvail)
	nodeResourcesAtTickStart := cloneNodeAvailByCluster(nodeResources)
	clusters := tickClusters{
		start: start, registered: registeredClusters, multiNode: multiNodeCapable, clusterIDs: clusterIDs,
		nodeAvail: nodeAvail, nodeResources: nodeResources, nodeResourcesTotal: nodeResourcesTotal, nodeAvailAtTickStart: nodeAvailAtTickStart,
		nodeLabels: nodeLabels, resolveCache: resolveCache,
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

	// 3a. Enforce the summary gate: a COMPLETED job is summarised before its agent's next job
	// runs. It holds a job the agent submitted after that job ended, and never one that was
	// already queued then: a batch submitted together runs in parallel.
	// 3b. Enforce the current stage's max_job_hours: a job queued while an earlier, looser stage
	// was running must not slip through after the ladder tightened. Held, not rejected — a later
	// stage may lift the cap, and then it admits normally.
	// One unreadable row must not decide the fate of every other queued job. A QUEUED experiment
	// pointing at a deleted platform experiment is re-read on every tick forever, so aborting the
	// pass here would wedge admission for the entire platform indefinitely — the exact shape
	// important.md #19 exists to forbid. Per-experiment failures are logged, collected, and the
	// experiment is dropped from this tick's consideration; the pass finishes, and the joined
	// error surfaces at the end so the failure is still loud.
	maxJobHours := map[string]float64{} // key: platformExpID; 0 = unlimited
	filtered := queued[:0]
	environments := newEnvironmentPass()
	for _, exp := range queued {
		if exp.EnvironmentKey != "" {
			if held, err := l.heldForEnvironment(ctx, environments, exp); err != nil {
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
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, jobTooLongReason(exp.EstimatedDurationHours, limit)); err != nil {
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
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, stageCutReason()); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark stage cut: %w", err))
			}
			continue
		}
		owed, err := l.store.UnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID, exp.CreatedAt)
		if err != nil {
			l.skipExperiment(&tickErrs, exp, fmt.Errorf("summary gate: %w", err))
			continue
		}
		if len(owed) == 0 {
			filtered = append(filtered, exp)
			continue
		}
		if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, summaryGateReason(owed)); err != nil {
			l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark summary gate: %w", err))
		}
	}
	queued = filtered

	// 4. Guaranteed pass: FIFO by age bucket, quota-ratio tiebreak within a bucket, then
	// completion proximity DESC, then shortest job first.
	guaranteed := filterTier(queued, domain.CapacityGuaranteed)
	guaranteed, guaranteedQuotas := l.fetchQuotaMap(ctx, guaranteed, &tickErrs)
	sortGuaranteed(guaranteed, guaranteedQuotas, completion, l.guaranteedFairnessWindow)
	// Both snapshots are taken here, before either pass claims anything. Taking the burst one
	// after the guaranteed pass told a burst job displaced by a guaranteed one that the cluster
	// had no room for it, when what happened is that a higher tier outranked it.
	gAvailInitial := cloneAvail(gAvail)
	bAvailInitial := cloneAvail(bAvail)

	// Preemption candidates are read once per tick rather than once per job that fails to fit —
	// a saturated cluster with a deep guaranteed queue otherwise issues one full scan per queued
	// job, every tick. It is re-read after a preempt, because preempt requeues its victims and
	// the next candidate must not consider them running.
	var running []*domain.Experiment
	runningLoaded := false
	// Per cluster of slots, the slots being left that no waiting job has been promised yet.
	vacating := map[string]map[string]map[string]int64{}

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
		liveFits := scalarFits && topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
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
		if !liveFits || scalesAbove(cluster, exp, autoscalerEnabled, connectedClusters, registeredClusters) {
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
						if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, specMisfit); err != nil {
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
					if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
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
			shortage := preemptionShortfall(gAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
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
			slots := hasSlots(nodeLabels[cluster])
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
					vacating[cluster] = vacatingSlots(cluster, nodeResourcesTotal[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], claimed)
				}
				if node, free, _ := slotFor(exp, cluster, nodeResourcesTotal[cluster], vacating[cluster], nodeLabels[cluster]); free {
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
				subtractFootprint(bAvail[cluster], fp)
			}
			if promised {
				subtractFootprint(desiredFree[cluster], fp)
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
					topologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
				claimed, cerr := resolveCache.loadClaimed(ctx)
				if cerr != nil {
					l.skipExperiment(&tickErrs, exp, fmt.Errorf("list the jobs that hold capacity: %w", cerr))
					continue
				}
				reason = notAdmittedReasonFor(fitAtTickStart, totalCovers, promised, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
			}
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Log(submitErrorLevel(err), "submit guaranteed job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("guaranteed", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		subtractFootprint(gAvail[cluster], fp)
		// bAvail is the same shared pool's other view — without this, the burst pass later in
		// this tick could still see this unit as free and double-book it.
		subtractFootprint(bAvail[cluster], fp)
		subtractFootprint(desiredFree[cluster], fp)
		reservePlacement(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
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
		if !domain.Fits(bAvail[cluster], fp) || !topologyFits(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp) {
			// Same shortage vector the guaranteed pass computes, for the same reason: "short
			// {cpu: 12}" tells an operator which dimension to fix, where a bare
			// capacity_unavailable tells them only that something, somewhere, did not fit.
			shortage := preemptionShortfall(bAvail[cluster], nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp, fp)
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
				topologyFits(nodeAvailAtTickStart[cluster], nodeResourcesAtTickStart[cluster], nodeLabels[cluster], exp)
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
				freed := !scalarFits && !hasSlots(nodeLabels[cluster]) && domain.Fits(desiredFree[cluster], fp)
				reason = notAdmittedReasonFor(fitAtTickStart, scalarFits, freed, shortage, cluster, holdersOf(claimed, cluster, shortage, exp))
			}
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		if err := l.submitJob(ctx, exp, cluster, persistedFlavor); err != nil {
			l.logger.Log(submitErrorLevel(err), "submit burst job", zap.String("exp", exp.ID), zap.Error(err))
			obsmetrics.AdmissionTickResultsTotal.WithLabelValues("burst", "skipped").Inc()
			reason := notAdmittedReasonForSubmitError(err)
			if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
				l.skipExperiment(&tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
			}
			continue
		}
		subtractFootprint(bAvail[cluster], fp)
		reservePlacement(nodeAvail[cluster], nodeResources[cluster], nodeLabels[cluster], exp)
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

	nodeAvail, nodeResources, nodeResourcesTotal, nodeAvailAtTickStart map[string]map[string]map[string]int64
}

// placement is where a queued job is tried in one pass: the cluster, "" when none could be picked,
// and its footprint there. misfit is why the cluster picked does not take the job's own cpu,
// memory and storage, and betFlavor the flavor a bet on a cluster that scales up is made at.
type placement struct {
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
// footprint fits (resolveClusterAndFootprint). The search says its choice by setting
// exp.AcceleratorType, and the resolved job is set on exp.
func (l *Loop) placeQueued(ctx context.Context, pass string, exp *domain.Experiment, avail map[string]domain.Footprint, quota *domain.AgentQuota, c tickClusters, tickErrs *[]error) (placement, bool) {
	skip := func(reason string) (placement, bool) {
		obsmetrics.AdmissionTickResultsTotal.WithLabelValues(pass, "skipped").Inc()
		if err := l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason); err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
		}
		return placement{}, false
	}
	placed := placement{cluster: exp.ClusterName, betFlavor: exp.AcceleratorType}
	candidates, ineligible := eligibleClusters(avail, c.multiNode, c.registered, exp)
	if ineligible != "" {
		return skip(ineligible)
	}
	candidates = withoutFailed(candidates, c.clusterIDs, exp, l.triedClusterTTL, c.start)
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
			placed.cluster, placed.fp = resolveClusterAndFootprint(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp)
		}
		if exp.Job.AcceleratorType != "" && exp.AcceleratorType != exp.Job.AcceleratorType && !quotaCanCoverFlavor(quota, exp) {
			if !slices.Contains(candidateAcceleratorTypes(exp), exp.Job.AcceleratorType) {
				return skip(ranOnTypeUncoveredReason(exp))
			}
			// Reverting to the requested flavor re-places the job, layout included: picking on
			// scalar fit alone could point a spread job at a cluster whose nodes cannot hold it,
			// and charge it a failed placement there.
			exp.AcceleratorType = exp.Job.AcceleratorType
			placed.betFlavor = exp.AcceleratorType
			placed.fp = exp.Footprint()
			if cluster, ok := placeAtFlavor(candidates, c.registered, c.nodeAvail, c.nodeResources, c.nodeLabels, exp); ok {
				placed.cluster = cluster
			} else {
				placed.cluster = clusterWithBestFit(candidates, c.registered, placed.fp)
			}
		}
		cluster := placed.cluster
		if cluster == "" {
			break
		}
		resolvedJob, misfit, err := l.resolveClusterLocalResources(ctx, c.resolveCache, exp, cluster, c.nodeResourcesTotal[cluster], c.nodeAvailAtTickStart[cluster], c.nodeResources[cluster], c.nodeLabels[cluster])
		if err != nil {
			l.skipExperiment(tickErrs, exp, fmt.Errorf("resolve cluster-local resources: %w", err))
			return placement{}, false
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
// choice and reports what it has installed holds when idle (fitsWhenIdle), on each accelerator
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
		case in.scalesUp[c.Name] || fitsWhenIdle(total, in.nodeTotals[c.Name], exp):
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
		if reported && (!spansNodes || in.multiNode[c.Name]) && fitsWhenIdle(total, in.nodeTotals[c.Name], exp) {
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
	types := candidateAcceleratorTypes(exp)
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

// fitsWhenIdle reports whether a cluster with nothing running on it holds exp: its installed
// total covers the job, and each node of the job fits the cpu, memory and storage of some node
// of the cluster. Nodes whose totals were not read are not judged.
func fitsWhenIdle(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) bool {
	if !fitsInstalled(total, exp) {
		return false
	}
	if len(nodeTotals) == 0 {
		return true
	}
	for _, shape := range exp.NodeShapes() {
		held := false
		for _, node := range nodeTotals {
			if nodeHasRoom(node, shape.Resources) {
				held = true
				break
			}
		}
		if !held {
			return false
		}
	}
	return true
}

// fitsInstalled reports whether total, a cluster's installed capacity, covers exp on one of the
// accelerator types it accepts.
func fitsInstalled(total domain.Footprint, exp *domain.Experiment) bool {
	probe := *exp
	// A job without accelerators has no type to vary and is judged once, as it is. Ranging over
	// its empty type list judged it nowhere and so answered that it exceeds every cluster.
	types := candidateAcceleratorTypes(exp)
	if len(types) == 0 {
		types = []domain.AcceleratorType{exp.AcceleratorType}
	}
	for _, t := range types {
		probe.AcceleratorType = t
		if domain.Fits(total, probe.Footprint()) {
			return true
		}
	}
	return false
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
// (candidateAcceleratorTypes), so it waits for quota.
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

// nodeAcceleratorKey is the key a node's free accelerators of a type are under: the type in lower
// case, as the capacity read keys them (queuebackend.GetAcceleratorCapacityByNode).
func nodeAcceleratorKey(t domain.AcceleratorType) string {
	return strings.ToLower(string(t))
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
	if exp.Job.TotalAccelerators() <= 0 {
		return out
	}
	key := nodeAcceleratorKey(exp.AcceleratorType)
	capacities := make([]int64, 0, len(byNode))
	for node, capacity := range byNode {
		if labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
			capacities = append(capacities, capacity[key])
		}
	}
	sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })

	// The job's nodes, hungriest first, matched against the roomiest hosts. For an ungrouped job
	// every node wants the same count and the order is immaterial; for a grouped one, pairing the
	// learner with the emptiest host is the assignment that leaves the smallest real shortage.
	perRank := make([]int64, 0, exp.Job.Nodes())
	for _, shape := range exp.NodeShapes() {
		if shape.AcceleratorCount > 0 {
			perRank = append(perRank, shape.AcceleratorCount)
		}
	}
	sort.Slice(perRank, func(i, j int) bool { return perRank[i] > perRank[j] })
	var missing int64
	if requiresDistinctHosts(exp) {
		for rank, need := range perRank {
			available := int64(0)
			if rank < len(capacities) {
				available = capacities[rank]
			}
			if available < need {
				missing += need - available
			}
		}
	} else {
		for _, need := range perRank {
			if len(capacities) == 0 {
				missing += need
				continue
			}
			sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })
			if capacities[0] < need {
				missing += need - capacities[0]
				capacities[0] = 0
			} else {
				capacities[0] -= need
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
	// flavor's rate. Read from the type the row recorded when the job was marked RUNNING, which
	// outlives every requeue and every placement that is rolled back, rather than from observed
	// hours: metrics are delayed, so "has it consumed anything yet" can still read zero for a
	// job that has run.
	if exp.RanOnAcceleratorType != "" {
		return []domain.AcceleratorType{exp.RanOnAcceleratorType}
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

// resolveClusterAndFootprint picks a concrete (cluster, flavor) pair for an unpinned job. It
// walks the clusters by priority (placementTiers), and within one priority tries every candidate
// flavor (requested type first, see candidateAcceleratorTypes) before the next tier is looked at:
// a job lands on a higher-priority cluster at an acceptable alternative flavor before it lands on
// a lower one at the flavor it asked for, and a full tier never keeps it from a lower one that
// fits. If nothing fits outright anywhere, falls back to the requested flavor's best-fit cluster
// so the caller's preemption path still runs.
// TotalAccelerators, not the top-level per-node count: a grouped job states its counts on its
// groups and leaves the top-level field empty, so reading it literally said every heterogeneous
// job wanted no accelerators at all. Such a job then took the CPU-only shortcut below — no flavor
// substitution and, worse, no placeAtFlavor, so it was placed on scalar fit alone and could be
// handed a cluster whose nodes had not one free device of the hardware it asked for.
func resolveClusterAndFootprint(avail map[string]domain.Footprint, registered map[string]domain.Cluster, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, domain.Footprint) {
	requested := exp.AcceleratorType
	types := candidateAcceleratorTypes(exp)
	if exp.Job.TotalAccelerators() <= 0 {
		// No flavor to vary. Still judged on node layout: a cluster whose aggregate is large
		// enough but whose nodes are individually too small for one rank would otherwise be picked
		// on the scalar alone, fail topologyFits in the tick, and go to preemption while another
		// cluster could host it.
		types = []domain.AcceleratorType{requested}
	}
	if len(types) == 0 {
		return "", nil
	}
	for _, tier := range placementTiers(avail, registered) {
		inTier := within(avail, tier)
		for _, t := range types {
			exp.AcceleratorType = t
			if cluster, ok := placeAtFlavor(inTier, registered, nodeAvail, nodeResources, nodeLabels, exp); ok {
				return cluster, exp.Footprint()
			}
		}
	}
	exp.AcceleratorType = types[0]
	fallback := exp.Footprint()
	exp.AcceleratorType = requested
	return clusterWithBestFit(avail, registered, fallback), fallback
}

// placeAtFlavor names the first cluster in placement order that can host exp at the accelerator
// type it currently carries, judged on footprint and node layout alike. ok=false means no cluster
// can — the caller decides what to do with a job that does not fit anywhere, rather than being
// handed a cluster that only looks big enough in the scalar.
func placeAtFlavor(avail map[string]domain.Footprint, registered map[string]domain.Cluster, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, bool) {
	fp := exp.Footprint()
	for _, candidate := range placementOrder(avail, registered) {
		// The job is not resolved yet, and on a cluster of slots it only fits as the slot it
		// would be resolved to.
		probe, ok := onSlots(exp, candidate, nodeResources[candidate], nodeLabels[candidate])
		if !ok {
			continue
		}
		if probe != exp {
			fp = probe.Footprint()
		}
		if domain.Fits(avail[candidate], fp) && topologyFits(nodeAvail[candidate], nodeResources[candidate], nodeLabels[candidate], probe) {
			return candidate, true
		}
	}
	return "", false
}

func requiresDistinctHosts(exp *domain.Experiment) bool {
	// TotalAccelerators rather than the top-level per-node count: a grouped job states its counts
	// per group and leaves the top-level field empty, which read literally would say every
	// heterogeneous job is happy to stack all its ranks on one host.
	if exp.Job.Nodes() <= 1 || exp.Job.TotalAccelerators() <= 0 {
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

// desiredPlacementFits proves candidate fits the cluster beside the jobs already desired there.
// resolved is the candidate's job as the runtime will be handed it: when the candidate lands on a
// slot (domain.NodeSlotLabel), resolved is pinned to that slot.
func desiredPlacementFits(byNode map[string]map[string]int64, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, desired []*domain.Experiment, candidate *domain.Experiment, resolved *domain.JobSpec) bool {
	byNode, nodeResources = cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources)
	// Planned jointly, not one job after another: the runtime is free to put an already-desired
	// job on whichever node it likes, so replaying it onto one fixed node and then asking whether
	// the candidate fits around that guess rejected candidates the cluster could host.
	//
	// A desired job pinned to a slot is the exception, since its node is decided. It holds that
	// slot whether or not the runtime has rented it yet, and is not planned again: its slot reads
	// as taken from the moment its instance exists, and planning it there would find no room for
	// a job that is already in place.
	jobs := make([]*domain.Experiment, 0, len(desired)+1)
	for _, d := range desired {
		if slot := d.EffectiveJob().NodeSelector[domain.NodeSlotLabel]; slot != "" {
			holdSlot(byNode, nodeResources, labelsByNode, slot)
			continue
		}
		jobs = append(jobs, d)
	}
	jobs = append(jobs, candidate)
	plan, ok := planPlacement(byNode, nodeResources, labelsByNode, jobs)
	if !ok {
		return false
	}
	// A slot holds a whole job, so only a job that is one node can be pinned to one.
	if candidate.Job.Nodes() == 1 {
		for _, p := range plan {
			if slot := labelsByNode[p.node][domain.NodeSlotLabel]; p.job == len(jobs)-1 && slot != "" {
				selector := make(map[string]string, len(resolved.NodeSelector)+1)
				for key, value := range resolved.NodeSelector {
					selector[key] = value
				}
				selector[domain.NodeSlotLabel] = slot
				resolved.NodeSelector = selector
			}
		}
	}
	return true
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
// It walks domain.JobSpec.NodeShapes() — one entry per node, which for a heterogeneous (grouped)
// job are genuinely different shapes. An averaged shape would be the one thing that must not be
// used here: no node ever holds the average of a learner and 64 actors, so a job proven to fit
// against it fits nowhere.
//
// The walk is a search, not a single greedy pass: ranks are tried hardest-first, each on the
// tightest node that fits, and a rank that finds no host sends the search back to move an earlier
// one. A plain first-fit in declared order let a small rank take the only node a later large rank
// could use and reported a job with a valid assignment as not fitting. The input maps are
// modified only when the whole job is placed — a false answer leaves them exactly as they were.
func reservePlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	plan, ok := planPlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labelsByNode, []*domain.Experiment{exp})
	if !ok {
		return false
	}
	for _, p := range plan {
		if p.shape.AcceleratorCount > 0 {
			byNode[p.node][p.key] -= p.shape.AcceleratorCount
		}
		subtractNodeResources(nodeResources[p.node], p.shape.Resources)
	}
	return true
}

// rankAssignment is one rank of one job pinned to a node by planPlacement. key is the
// nodeAcceleratorKey of the job's type, "" for a rank that holds no accelerator.
type rankAssignment struct {
	job   int
	shape domain.NodeShape
	node  string
	key   string
}

// placementSearchBudget bounds the number of node trials one planPlacement call may make before
// it gives up. Hardest-first ordering with best-fit node choice and equivalent-node pruning places
// realistic jobs without backtracking at all; the budget is a guard against an adversarial layout
// exhausting the tick, and running out of it is reported as "does not fit" — a conservative
// answer, the same one the greedy walk gave whenever it guessed wrong.
var placementSearchBudget = 50000

// planPlacement finds nodes for every rank of every job in jobs against the given free capacity,
// consuming capacity in the maps as it goes (callers pass clones). Distinct-host rules apply per
// job; ranks of different jobs may share a node. Returns the assignment and whether all ranks
// were placed. On false the maps are left in an unspecified state.
func planPlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, jobs []*domain.Experiment) ([]rankAssignment, bool) {
	type rank struct {
		job      int
		shape    domain.NodeShape
		key      string
		distinct bool
		nodes    []string // qualifying candidate set for this rank
	}
	var ranks []rank
	for i, exp := range jobs {
		key := nodeAcceleratorKey(exp.AcceleratorType)
		// Two candidate sets: a node that reported accelerators, and any node the cluster
		// reported resources for. A node needing no accelerator must not be confined to
		// accelerator nodes.
		acceleratorNodes := qualifyingNodes(byNode, labelsByNode, exp)
		plainNodes := qualifyingNodes(nodeResources, labelsByNode, exp)
		distinct := requiresDistinctHosts(exp)
		for _, shape := range exp.NodeShapes() {
			nodes := plainNodes
			if shape.AcceleratorCount > 0 {
				nodes = acceleratorNodes
			}
			ranks = append(ranks, rank{job: i, shape: shape, key: key, distinct: distinct, nodes: nodes})
		}
	}
	// Hardest rank first: most accelerators, then the largest fungible request. The rank with the
	// fewest possible hosts is placed while the most hosts are still free.
	sort.SliceStable(ranks, func(a, b int) bool {
		if ranks[a].shape.AcceleratorCount != ranks[b].shape.AcceleratorCount {
			return ranks[a].shape.AcceleratorCount > ranks[b].shape.AcceleratorCount
		}
		return shapeResourceTotal(ranks[a].shape) > shapeResourceTotal(ranks[b].shape)
	})

	used := make([]map[string]bool, len(jobs))
	for i := range used {
		used[i] = make(map[string]bool)
	}
	plan := make([]rankAssignment, len(ranks))
	budget := placementSearchBudget

	// nodeFits reports whether node can host r right now and the accelerator key it would draw on.
	nodeFits := func(r rank, node string) (string, bool) {
		if r.distinct && used[r.job][node] {
			return "", false
		}
		nodeKey := ""
		if r.shape.AcceleratorCount > 0 {
			nodeKey = r.key
			if byNode[node][nodeKey] < r.shape.AcceleratorCount {
				return "", false
			}
		}
		if !nodeHasRoom(nodeResources[node], r.shape.Resources) {
			return "", false
		}
		if isSlot(labelsByNode[node]) && !takesWholeSlot(r.shape, nodeKey, byNode[node], nodeResources[node]) {
			return "", false
		}
		return nodeKey, true
	}
	// leftover is what node would have left after hosting r, summed into one number per family —
	// the best-fit sort key only. Summing across dimensions is fine for deciding which node to
	// *try* first (a rough "how much room is left" ranking); it is not fine for deciding two nodes
	// are the same node, which is what equivalentState below is for.
	leftover := func(r rank, node, nodeKey string) (int64, int64) {
		var accelerators, resources int64
		if r.shape.AcceleratorCount > 0 {
			accelerators = byNode[node][nodeKey] - r.shape.AcceleratorCount
		}
		for k, v := range nodeResources[node] {
			resources += v - r.shape.Resources[k]
		}
		return accelerators, resources
	}
	// equivalentState is the exact post-placement state of a node, dimension by dimension. It is
	// the key the interchangeability pruning below is allowed to use.
	//
	// The scalar sum from leftover must never be used for this. Memory and storage are both
	// byte-valued and of the same magnitude, so a node with 2Gi of memory and no disk and a node
	// with no memory and 2Gi of disk produce the identical sum — the pruning then discarded one of
	// them as a duplicate of the other and lost every assignment that needed the discarded one,
	// reporting a placeable job as not fitting.
	equivalentState := func(r rank, node, nodeKey string) string {
		dimensions := make([]string, 0, len(nodeResources[node]))
		for k := range nodeResources[node] {
			dimensions = append(dimensions, k)
		}
		sort.Strings(dimensions)
		var b strings.Builder
		if r.shape.AcceleratorCount > 0 {
			fmt.Fprintf(&b, "a=%d;", byNode[node][nodeKey]-r.shape.AcceleratorCount)
		}
		for _, k := range dimensions {
			fmt.Fprintf(&b, "%s=%d;", k, nodeResources[node][k]-r.shape.Resources[k])
		}
		return b.String()
	}

	var place func(i int) bool
	place = func(i int) bool {
		if i == len(ranks) {
			return true
		}
		r := ranks[i]
		type candidate struct {
			node, key string
			accLeft   int64
			resLeft   int64
			state     string
		}
		candidates := make([]candidate, 0, len(r.nodes))
		for _, node := range r.nodes {
			nodeKey, ok := nodeFits(r, node)
			if !ok {
				continue
			}
			accLeft, resLeft := leftover(r, node, nodeKey)
			candidates = append(candidates, candidate{node: node, key: nodeKey, accLeft: accLeft, resLeft: resLeft, state: equivalentState(r, node, nodeKey)})
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			if candidates[a].accLeft != candidates[b].accLeft {
				return candidates[a].accLeft < candidates[b].accLeft
			}
			return candidates[a].resLeft < candidates[b].resLeft
		})
		// Two nodes that would be left in the same state are interchangeable for this rank, and
		// for every rank after it. Retrying the same failed subtree from an equivalent node is
		// what turns a homogeneous cluster into an exponential search.
		tried := make(map[string]bool)
		for _, c := range candidates {
			if tried[c.state] {
				continue
			}
			tried[c.state] = true
			if budget <= 0 {
				return false
			}
			budget--
			if r.shape.AcceleratorCount > 0 {
				byNode[c.node][c.key] -= r.shape.AcceleratorCount
			}
			subtractNodeResources(nodeResources[c.node], r.shape.Resources)
			used[r.job][c.node] = true
			plan[i] = rankAssignment{job: r.job, shape: r.shape, node: c.node, key: c.key}
			if place(i + 1) {
				return true
			}
			used[r.job][c.node] = false
			addNodeResources(nodeResources[c.node], r.shape.Resources)
			if r.shape.AcceleratorCount > 0 {
				byNode[c.node][c.key] += r.shape.AcceleratorCount
			}
			if budget <= 0 {
				return false
			}
		}
		return false
	}
	if !place(0) {
		return nil, false
	}
	return plan, true
}

func shapeResourceTotal(shape domain.NodeShape) int64 {
	var total int64
	for _, amount := range shape.Resources {
		total += amount
	}
	return total
}

func addNodeResources(available, used map[string]int64) {
	for key, amount := range used {
		available[key] += amount
	}
}

// qualifyingNodes is the sorted set of nodes from capacity that carry exp's required node labels.
// The selector is the effective job's: admission may have pinned an admitted job to a slot, and
// for every other job the resolved selector is the submitted one.
func qualifyingNodes(capacity map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) []string {
	nodes := make([]string, 0, len(capacity))
	for node := range capacity {
		if labelsMatch(labelsByNode[node], exp.EffectiveJob().NodeSelector) {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	return nodes
}

// hardestNodeShape is the single node of exp that is hardest to place — most accelerators first,
// then the largest fungible request. Diagnostics that describe a job by one shape (the shortfall
// vector, and the preemption plan built from it) use this one, because covering the hardest node
// is what actually unblocks the job. Every shape is identical for an ungrouped job, so this is
// that job's per-node shape exactly as before.
func hardestNodeShape(exp *domain.Experiment) domain.NodeShape {
	var hardest domain.NodeShape
	var hardestTotal int64
	for _, shape := range exp.NodeShapes() {
		var total int64
		for _, amount := range shape.Resources {
			total += amount
		}
		if hardest.Resources == nil || shape.AcceleratorCount > hardest.AcceleratorCount ||
			(shape.AcceleratorCount == hardest.AcceleratorCount && total > hardestTotal) {
			hardest, hardestTotal = shape, total
		}
	}
	return hardest
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

// addNodeResourceShortfall folds in what the fungible dimensions still lack once the job's ranks
// are laid out over the qualifying nodes. It walks the ranks hardest-first, consuming each node it
// places one on, and reports the *least* deficient node for the first rank that finds no host —
// least deficient because covering that one node lets that rank land, which is what has to be
// freed somewhere rather than everywhere.
//
// The walk has to consume capacity as it goes, because a job's ranks compete with each other. Only
// asking whether some node could host one rank called a job satisfied whenever a single node had
// room, even when the job needed several such nodes and the cluster had one: two ranks wanting 2Gi
// of disk apiece on a cluster holding 4Gi spread as 1+1+2 passes the cluster-level check, cannot
// be placed, and was reported as needing nothing at all — leaving preemption with no vector to
// cover and the disbalance evictor with no deficit to explain, so the job sat queued forever while
// the scheduler said it wanted nothing.
func addNodeResourceShortfall(out domain.Footprint, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) {
	shapes := exp.NodeShapes()
	if len(shapes) == 0 {
		return
	}
	// Hardest first, mirroring planPlacement: the rank with the fewest possible hosts is laid down
	// while the most nodes are still free, so a deficit reported here is one the real search would
	// also have hit rather than an artefact of an unlucky order.
	sort.SliceStable(shapes, func(a, b int) bool {
		if shapes[a].AcceleratorCount != shapes[b].AcceleratorCount {
			return shapes[a].AcceleratorCount > shapes[b].AcceleratorCount
		}
		return shapeResourceTotal(shapes[a]) > shapeResourceTotal(shapes[b])
	})

	// Clones: this is a diagnostic pass over the tick's live capacity maps and must not disturb
	// them for the admission decisions that follow.
	accelerators := cloneNodeCapacity(byNode)
	resources := cloneNodeCapacity(nodeResources)
	key := nodeAcceleratorKey(exp.AcceleratorType)
	distinct := requiresDistinctHosts(exp)
	occupied := map[string]bool{}

	nodes := make([]string, 0, len(resources))
	for node := range resources {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes) // deterministic reporting across ticks

	for _, shape := range shapes {
		var bestDeficit map[string]int64
		var bestTotal int64
		placed := ""

		for _, node := range nodes {
			if !labelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
				continue
			}
			if distinct && occupied[node] {
				continue
			}
			// Only nodes that could host this rank's accelerators are candidates; where the
			// accelerators themselves are missing, the per-node accelerator shortfall reported by
			// preemptionShortfall already says so.
			if shape.AcceleratorCount > 0 && accelerators[node][key] < shape.AcceleratorCount {
				continue
			}
			deficit := map[string]int64{}
			var total int64
			for resource, need := range shape.Resources {
				if short := need - resources[node][resource]; short > 0 {
					deficit[resource] = short
					total += short
				}
			}
			if total == 0 {
				placed = node
				break
			}
			if bestDeficit == nil || total < bestTotal {
				bestDeficit, bestTotal = deficit, total
			}
		}

		if placed != "" {
			if shape.AcceleratorCount > 0 {
				accelerators[placed][key] -= shape.AcceleratorCount
			}
			subtractNodeResources(resources[placed], shape.Resources)
			occupied[placed] = true
			continue
		}

		// This rank has nowhere to go. Report what the closest node lacks and stop: freeing that
		// much lets this rank land, and a later tick re-measures whatever remains.
		for resource, short := range bestDeficit {
			kind, ok := nodeResourceKind(resource)
			if !ok {
				continue
			}
			if k := (domain.ResourceKey{Kind: kind}); out[k] < short {
				out[k] = short
			}
		}
		return
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
//  2. otherwise, among the clusters that could ever host the request, the one with the smallest
//     total shortage (sum across dimensions of max(0, need-have)) — the best candidate for
//     preempt() to free room on
//
// "Could ever host it" is the load-bearing half of step 2, and it is not the same question as
// step 1's. A cluster that has never reported a dimension the job asks for — an accelerator
// flavor it does not own — has a shortage equal to the whole request, which ties with a cluster
// that owns exactly that hardware and is merely full of preemptible burst work. The tie used to
// go to whichever name sorted first, and when it went to the cluster without the hardware the
// guaranteed job was handed a preemption target where no victim could ever free what it needs:
// preemption is scoped to the chosen cluster, found nothing to take there, and the job stayed
// QUEUED tick after tick while the real hardware sat under evictable burst jobs.
//
// Returning "" when no cluster reports the request is the honest answer and needs no special
// case at the call sites: the absent footprint fails every fit check, so the job is marked
// capacity_unavailable, which is exactly what "no cluster on this platform has this hardware"
// means.
//
// Both steps go by priority before anything else (placementTiers): step 1 takes the first fit in
// placement order, and step 2 answers from the highest-priority tier that holds a cluster which
// could ever host the request, so the room is made on the compute the org ranked first.
func clusterWithBestFit(avail map[string]domain.Footprint, registered map[string]domain.Cluster, footprint domain.Footprint) string {
	tiers := placementTiers(avail, registered)
	for _, tier := range tiers {
		for _, c := range tier {
			if domain.Fits(avail[c], footprint) {
				return c
			}
		}
	}
	for _, tier := range tiers {
		best := ""
		var bestShortage int64 = -1
		for _, c := range tier {
			if !reportsEveryDimension(avail[c], footprint) {
				continue
			}
			var shortage int64
			for _, v := range shortfall(avail[c], footprint) {
				shortage += v
			}
			if bestShortage < 0 || shortage < bestShortage {
				bestShortage = shortage
				best = c
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// reportsEveryDimension reports whether capacity has an entry for every dimension footprint asks
// for — present, not necessarily sufficient. Zero free of a flavor the cluster owns is a cluster
// preemption can unblock; no entry at all is a cluster that does not have the hardware, and no
// eviction there will ever produce it.
func reportsEveryDimension(capacity domain.Footprint, footprint domain.Footprint) bool {
	for key, need := range footprint {
		if need <= 0 {
			continue
		}
		if _, reported := capacity[key]; !reported {
			return false
		}
	}
	return true
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

// environmentPass is what one tick read of the environments queued jobs wait for: each
// environment once, after its build was assigned, and the live clusters once, when a first
// environment is assigned.
type environmentPass struct {
	seen map[string]*domain.Environment
	live []string
	read bool
}

func newEnvironmentPass() *environmentPass {
	return &environmentPass{seen: map[string]*domain.Environment{}}
}

// heldForEnvironment says whether exp waits for the environment its commit declares, whose build
// it assigns to a live cluster first (db.EnvironmentsStore.AssignEnvironment): while it is built
// it says so, once the file failed exp ends with the end of the build's output, so its agent fixes
// the file and submits again, and once the platform failed it on every attempt exp ends as the
// platform's fault at no cost, and the next submit builds it again.
func (l *Loop) heldForEnvironment(ctx context.Context, pass *environmentPass, exp *domain.Experiment) (bool, error) {
	env, ok := pass.seen[exp.EnvironmentKey]
	if !ok {
		if !pass.read {
			live, err := l.workload.GetLiveClusters(ctx)
			if err != nil {
				return false, fmt.Errorf("live clusters: %w", err)
			}
			for name, connected := range live {
				if connected {
					pass.live = append(pass.live, name)
				}
			}
			pass.read = true
		}
		var err error
		if env, err = l.store.AssignEnvironment(ctx, exp.EnvironmentKey, pass.live, domain.EnvironmentBuildLease); err != nil {
			return false, err
		}
		if env == nil {
			return false, fmt.Errorf("job %s names environment %s, which does not exist", exp.ID, exp.EnvironmentKey)
		}
		pass.seen[exp.EnvironmentKey] = env
	}
	switch {
	case env.State == domain.EnvironmentReady:
		return false, nil
	case env.State == domain.EnvironmentBuilding:
		reason := domain.NotAdmittedEnvironmentBuilding + ": " + buildingMessage(env, false)
		return true, l.store.UpdateNotAdmittedReason(ctx, exp.ID, reason)
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultFile:
		return true, l.endForEnvironment(ctx, exp, domain.StatusRejected, string(domain.EvictionEnvironmentFailed.WithDetail(fileFailed(env))))
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultPlatform:
		return true, l.endForEnvironment(ctx, exp, domain.StatusEvicted, string(domain.EvictionEnvironmentUnbuilt.WithDetail("the platform could not build the environment its commit declares in "+domain.EnvironmentFile+
			" on "+env.Base+" in "+strconv.Itoa(domain.MaxEnvironmentAttempts)+" attempts, no fault of the file's. It costs nothing: submit again and the platform builds it again")))
	}
	return false, fmt.Errorf("environment %s of job %s is %s with fault %q", env.Key, exp.ID, env.State, env.Fault)
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
	if env.ClusterName == "" {
		return "It waits for a live cluster that builds for its org to have room"
	}
	progress := "It builds on cluster " + env.ClusterName + ", attempt " + strconv.Itoa(env.Attempt) + " of " + strconv.Itoa(domain.MaxEnvironmentAttempts)
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
