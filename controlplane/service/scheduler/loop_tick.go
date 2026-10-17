package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

// tick runs one full admission pass. Single-threaded — no concurrent ticks.
func (l *Loop) tick(ctx context.Context) (tickErr error) {
	if !l.ticking.CompareAndSwap(false, true) {
		panic("scheduler: tick() re-entered concurrently — capacity accounting is not safe under concurrency, see Loop.ticking")
	}
	defer l.ticking.Store(false)

	start := time.Now()

	// Reprioritize all queued jobs after every tick, regardless of outcome — deferred so every
	// exit path (including early returns below) runs it. Its failure is the tick's.
	defer func() {
		if err := l.reprioritizer.RePrioritize(ctx); err != nil {
			tickErr = errors.Join(tickErr, fmt.Errorf("reprioritize after tick: %w", err))
		}
	}()

	t, err := l.readTickState(ctx, start)
	if err != nil {
		return err
	}

	// Credit back capacity from disbalance victims a prior tick already evicted but that this
	// fresh read still can't see as free (the node/agent hasn't reported it back yet) — otherwise
	// this tick re-derives the same shortage and evicts a fresh set of victims to free capacity
	// that is already being freed. See loop_disbalance.go's applyPendingEvictions.
	l.applyPendingEvictions(start, t.gAvail, t.bAvail, t.clusters.nodeAvail, t.clusters.nodeResources)
	t.snapshotTickStart()

	// GetAcceleratorTypeCapacity already subtracts the complete desired footprint (SUBMITTED/RUNNING). No
	// second reservation or live-usage subtraction here — either would double-count.
	// MarkSubmitted is itself the durable capacity claim.
	queued, err := l.store.ListQueuedExperiments(ctx)
	if err != nil {
		return err
	}
	t.completion = l.completionFractions(ctx, queued)
	queued = l.admissible(ctx, t, queued)

	l.guaranteedPass(ctx, t, queued)
	l.burstPass(ctx, t, queued)

	return l.finishTick(ctx, t)
}

// tickState is what one tick works against: what it read of the clusters, the views of free
// capacity both passes claim from in place, and what one pass leaves the other. Every map is the
// tick's own.
type tickState struct {
	clusters tickClusters

	// Free capacity per cluster as a domain.Footprint — a pooled total would hide which cluster
	// has room, admitting against a combined number while the actual target cluster is full.
	// Guaranteed and burst share one physical pool per cluster, viewed as gAvail and bAvail;
	// preemption enforces the tier boundary. desiredFree is free capacity net of every job the
	// platform has placed.
	gAvail, bAvail, desiredFree map[string]domain.Footprint

	// The capacity this tick started with, before any admission claims a slice of it
	// (snapshotTickStart).
	gAvailInitial, bAvailInitial map[string]domain.Footprint
	nodeResourcesAtTickStart     map[string]map[string]map[string]int64

	// Installed (not free) capacity, evidence for the disbalance eviction alone; nil when it
	// could not be read.
	totalCapacity map[string]domain.Footprint

	// Speculative-submit inputs (readSpeculation).
	autoscalerEnabled    map[string]bool
	connected            map[string]bool
	speculativeFootprint map[string]int

	completion map[string]float64

	// The disbalance evictor is a cluster-level verdict about stranded accelerators — at most one
	// per cluster per tick, shared by both admission passes. See its call sites for why.
	disbalanceRan map[string]bool
	// The clusters a job that waited long holds this tick, in both passes (holdRoom).
	held map[string]bool

	// Per-experiment failures collected across every pass and joined at the end (#19). A tick
	// reads state that is shared by every queued job but acts on each one individually, so only
	// a failure to read that shared state is grounds for abandoning the pass.
	errs []error
}

// readTickState reads the clusters once for the whole tick. A failed read admission decides on
// fails the tick.
func (l *Loop) readTickState(ctx context.Context, start time.Time) (*tickState, error) {
	gAvail, desiredFree, err := readAcceleratorTypeCapacity(ctx, l.clusters, l.logger)
	if err != nil {
		return nil, err
	}
	nodeAvail, err := l.clusters.GetAcceleratorCapacityByNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("per-node accelerator capacity: %w", err)
	}
	nodeResources, err := l.clusters.GetNodeResourceCapacity(ctx)
	if err != nil {
		return nil, fmt.Errorf("per-node resource capacity: %w", err)
	}
	nodeLabels, err := l.clusters.GetNodeLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("node labels: %w", err)
	}
	// Which clusters can run a job that spans more than one node. A capability each cluster
	// reports about itself, filtered on here so a distributed job is never placed on a runtime
	// that executes single-node jobs only — that used to be discovered at workload creation,
	// after the job had already been admitted and had already claimed a reservation.
	multiNodeCapable, err := l.clusters.GetMultiNodeCapability(ctx)
	if err != nil {
		return nil, fmt.Errorf("multi-node capability: %w", err)
	}
	// Which clusters exist for the platform, whose each is, and which are shared. Placement by
	// org is decided against this one read: a job goes to a cluster of its own org or to a
	// shared one, and a name nobody registered is no cluster at all.
	registeredClusters, err := l.store.RegisteredClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("registered clusters: %w", err)
	}
	autoscalerEnabled, clusterIDs, connected, speculativeFootprint, err := l.readSpeculation(ctx, gAvail)
	if err != nil {
		return nil, err
	}
	// Installed (not free) capacity — evidence for one decision, the disbalance eviction, and an
	// input to nothing else in this tick. Unlike the reads above it, admission never consults it.
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
	return &tickState{
		clusters: tickClusters{
			start: start, registered: registeredClusters, multiNode: multiNodeCapable, clusterIDs: clusterIDs,
			nodeAvail: nodeAvail, nodeResources: nodeResources, nodeResourcesTotal: nodeResourcesTotal,
			nodeLabels: nodeLabels,
			// Amortizes the running-experiment node-attribution lookups the per-accelerator share
			// needs (see loop_resolve.go) across every queued job this tick considers, in both passes.
			resolveCache: newResolutionCache(l),
			whenIdle:     installed{registered: registeredClusters, totals: totalCapacity, nodeTotals: nodeResourcesTotal, scalesUp: autoscalerEnabled, multiNode: multiNodeCapable, nodeLabels: nodeLabels},
		},
		gAvail: gAvail, bAvail: placement.CloneAvail(gAvail), desiredFree: desiredFree,
		totalCapacity:     totalCapacity,
		autoscalerEnabled: autoscalerEnabled, connected: connected, speculativeFootprint: speculativeFootprint,
		disbalanceRan: map[string]bool{},
		held:          map[string]bool{},
	}, nil
}

// readSpeculation reads the speculative-submit inputs (autoscaler.md).
func (l *Loop) readSpeculation(ctx context.Context, gAvail map[string]domain.Footprint) (autoscalerEnabled map[string]bool, clusterIDs map[string]string, connected map[string]bool, speculativeFootprint map[string]int, err error) {
	autoscalerEnabled, err = l.clusters.GetAutoscalerCapability(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("autoscaler capability: %w", err)
	}
	clusterIDs, err = l.clusters.GetClusterIDs(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("cluster ids: %w", err)
	}
	// gAvail only has entries for clusters GetAcceleratorTypeCapacity's heartbeat read found connected
	// (see ClusterReports.GetAcceleratorTypeCapacity) — reusing its key set here is the same
	// "unreachable cluster is never a speculative candidate" rule the doc calls out as a
	// pre-existing bug fixed in step 0, applied to the speculative path too.
	connected = make(map[string]bool, len(gAvail))
	for cluster := range gAvail {
		connected[cluster] = true
	}
	// speculativeFootprint approximates each cluster's outstanding speculative accelerator
	// footprint as its whole SUBMITTED footprint — a job's live-fit-vs-speculative distinction
	// lives in the metrics store (scheduled_nodes), which the loop does not read per job.
	// Counting every SUBMITTED accelerator here makes clusterSpeculativeCap strictly more
	// conservative than the design's exact definition, never less — the safe direction for a cap.
	speculativeFootprint = map[string]int{}
	submitted, err := l.store.ListSubmittedExperiments(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("list submitted for speculative footprint: %w", err)
	}
	for _, s := range submitted {
		if autoscalerEnabled[s.ClusterName] {
			speculativeFootprint[s.ClusterName] += s.AcceleratorCount
		}
	}
	return autoscalerEnabled, clusterIDs, connected, speculativeFootprint, nil
}

// snapshotTickStart keeps the capacity this tick started with, before any admission claims a
// slice of it. It is what separates "you lost this to jobs ahead of you, wait" from "this cluster
// cannot host your request, shrink it" — a distinction a submitter cannot make from the outside
// and cannot act on if it is wrong. Both passes classify against the same snapshot: taking the
// burst one after the guaranteed pass told a burst job displaced by a guaranteed one that the
// cluster had no room for it, when what happened is that a higher tier outranked it.
//
// It is also what installed accelerators per node are rebuilt from (free plus claimed): a job
// admitted this tick is in neither the live free count nor the claimed list, so a share measured
// against live capacity grows with every admission.
func (t *tickState) snapshotTickStart() {
	t.clusters.nodeAvailAtTickStart = placement.CloneNodesByCluster(t.clusters.nodeAvail)
	t.nodeResourcesAtTickStart = placement.CloneNodesByCluster(t.clusters.nodeResources)
	t.gAvailInitial = placement.CloneAvail(t.gAvail)
	t.bAvailInitial = placement.CloneAvail(t.bAvail)
}

// admissible keeps the queued jobs this tick may place, and writes why each other one waits or
// ends: one longer than its stage's max_job_hours allows, and one whose agent was cut.
//
// The current stage's max_job_hours is enforced here: a job queued while an earlier, looser stage
// was running must not slip through after the ladder tightened. Held, not rejected — a later
// stage may lift the cap, and then it admits normally.
//
// One unreadable row must not decide the fate of every other queued job. A QUEUED experiment
// pointing at a deleted platform experiment is re-read on every tick forever, so aborting the
// pass here would wedge admission for the entire platform indefinitely — the exact shape
// important.md #19 exists to forbid. Per-experiment failures are logged, collected, and the
// experiment is dropped from this tick's consideration; the pass finishes, and the joined error
// surfaces at the end so the failure is still loud.
func (l *Loop) admissible(ctx context.Context, t *tickState, queued []*domain.Experiment) []*domain.Experiment {
	maxJobHours := map[string]float64{} // key: platformExpID; 0 = unlimited
	// Read once per agent of a platform experiment, as quota is (quotaKey).
	cut := map[string]bool{}
	filtered := queued[:0]
	for _, exp := range queued {
		if exp.PlatformExperimentID == "" {
			filtered = append(filtered, exp)
			continue
		}
		limit, seen := maxJobHours[exp.PlatformExperimentID]
		if !seen {
			pe, err := l.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
			if err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("stage job length: %w", err))
				continue
			}
			if pe == nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("stage job length: platform experiment %s not found", exp.PlatformExperimentID))
				continue
			}
			limit = pe.CurrentMaxJobHours()
			maxJobHours[exp.PlatformExperimentID] = limit
		}
		if limit > 0 && exp.EstimatedDurationHours > limit {
			if err := l.markNotAdmitted(ctx, exp, jobTooLongReason(exp.EstimatedDurationHours, limit)); err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("mark job too long: %w", err))
			}
			continue
		}
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		isCut, read := cut[key]
		if !read {
			var err error
			isCut, err = l.store.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID)
			if err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("stage cut: %w", err))
				continue
			}
			cut[key] = isCut
		}
		if isCut {
			if err := l.markNotAdmitted(ctx, exp, stageCutReason()); err != nil {
				l.skipExperiment(&t.errs, exp, fmt.Errorf("mark stage cut: %w", err))
			}
			continue
		}
		filtered = append(filtered, exp)
	}
	return filtered
}

// admit submits exp to cluster and claims fp from each view of free capacity and its nodes, or
// writes why it could not be submitted.
func (l *Loop) admit(ctx context.Context, t *tickState, pass string, exp *domain.Experiment, cluster string, persistedAcceleratorType domain.AcceleratorType, fp domain.Footprint, views ...map[string]domain.Footprint) {
	if err := l.submitJob(ctx, exp, cluster, persistedAcceleratorType); err != nil {
		l.logger.Log(submitErrorLevel(err), "submit "+pass+" job", zap.String("exp", exp.ID), zap.Error(err))
		reason := notAdmittedReasonForSubmitError(err)
		if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
			l.skipExperiment(&t.errs, exp, fmt.Errorf("mark not admitted: %w", err))
		}
		return
	}
	for _, view := range views {
		placement.SubtractFootprint(view[cluster], fp)
	}
	c := t.clusters
	placement.Reserve(c.nodeAvail[cluster], c.nodeResources[cluster], c.nodeLabels[cluster], exp)
}

// finishTick publishes the notice to every job held for long, after both passes so a job told it is held is told the reason this tick gave it.
func (l *Loop) finishTick(ctx context.Context, t *tickState) error {
	if err := l.store.MarkHeldJobs(ctx, l.heldJobNotice); err != nil {
		t.errs = append(t.errs, fmt.Errorf("mark held jobs: %w", err))
	}
	return errors.Join(t.errs...)
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
