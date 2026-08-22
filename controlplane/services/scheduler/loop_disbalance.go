package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// A job that asks for one accelerator but nearly all of a node's CPU strands that node's other
// accelerators: they are free, nothing can reach them, and no amount of extra hardware fixes it
// because the blockage is a ratio, not a shortage. Preemption cannot answer this — it ranks
// victims by tier and progress, and the offending job is frequently guaranteed-tier and the only
// thing running. This pass answers the different question "is some job's shape, rather than the
// cluster's size, what is keeping idle accelerators idle".
//
// The signal is deliberately the *requested* footprint, not observed utilization. Requests are
// known at submission time and are exactly what the runtime reserves, so they are what actually
// blocks a neighbour — and they can never be missing, which removes any chance of reading absent
// telemetry as a violation. See docs at the bottom of this file for the usage-based refinement
// and why it is not implemented yet.

// DefaultDisbalanceTolerance is the multiple of a cluster's per-accelerator CPU/memory/storage
// share a job may request before it counts as disbalanced. A job holding A of a cluster's N
// accelerators is entitled to A/N of that cluster's CPU; three times that is already far past
// any plausible data-loader or host-side need, so anything above it is treated as a
// misconfigured request rather than a legitimately CPU-heavy accelerator job.
const DefaultDisbalanceTolerance = 3.0

// disbalanceDimensions are the fungible per-node resources a disproportionate request can strand
// accelerators with. Accelerators and extended resources are excluded on purpose: those are the
// thing being blocked, never the block.
func isDisbalanceDimension(kind domain.ResourceKind) bool {
	switch kind {
	case domain.ResourceKindCPU, domain.ResourceKindMemory, domain.ResourceKindStorage:
		return true
	default:
		return false
	}
}

// placedExperiment pairs a RUNNING experiment with the physical node the metrics store
// attributes it to. A running job whose node is unknown never becomes a placedExperiment, and so
// can never be evicted: "we cannot prove where it is" must read as innocent, not as guilty.
type placedExperiment struct {
	experiment *domain.Experiment
	node       string
}

// pendingEviction is one disbalance victim whose termination has been requested but whose freed
// capacity a fresh capacity read cannot yet be trusted to reflect. See Loop.pendingEvictions.
type pendingEviction struct {
	cluster          string
	node             string
	footprint        domain.Footprint
	acceleratorType  string // lowercase, as looked up via foldMatchingKey against nodeAvail
	acceleratorCount int64
	evictedAt        time.Time
}

// applyPendingEvictions credits every still-fresh pendingEviction's footprint back into gAvail,
// bAvail, and nodeAvail before this tick's admission passes run, and prunes any entry older than
// l.evictionTTL. Without this, a tick reads the victim's node/accelerator as still occupied
// (the node/agent hasn't reported it back yet) even though the victim is already gone from
// ListRunningExperiments, and re-derives the same shortage — driving a fresh eviction for
// capacity that is already being freed. Mutates gAvail/bAvail/nodeAvail in place; not safe to
// call concurrently with itself, which tick()'s single-threaded invariant already guarantees.
func (l *Loop) applyPendingEvictions(now time.Time, gAvail, bAvail map[string]domain.Footprint, nodeAvail map[string]map[string]map[string]int64) {
	for id, pe := range l.pendingEvictions {
		if now.Sub(pe.evictedAt) > l.evictionTTL {
			delete(l.pendingEvictions, id)
			continue
		}
		if g, ok := gAvail[pe.cluster]; ok {
			g.AddFootprint(pe.footprint)
		}
		if b, ok := bAvail[pe.cluster]; ok {
			b.AddFootprint(pe.footprint)
		}
		if pe.acceleratorCount > 0 && pe.acceleratorType != "" {
			if byNode, ok := nodeAvail[pe.cluster]; ok {
				if capacity, ok := byNode[pe.node]; ok {
					key := foldMatchingKey(capacity, pe.acceleratorType)
					capacity[key] += pe.acceleratorCount
				}
			}
		}
	}
}

// isPendingEviction reports whether id already has an outstanding, unexpired eviction request —
// candidate victims already in flight must not be selected again, and their node's idle
// accelerator count (see disbalancePremises) must not be double-counted against them.
func (l *Loop) isPendingEviction(id string, now time.Time) bool {
	pe, ok := l.pendingEvictions[id]
	if !ok {
		return false
	}
	return now.Sub(pe.evictedAt) <= l.evictionTTL
}

// disbalanceVictim is a job selected for eviction together with the evidence that condemned it.
// The evidence is carried out of selection rather than recomputed afterwards, so what the agent is
// told is exactly what the scheduler measured — an eviction nobody asked for has to explain
// itself, and "resource_disbalance" on its own tells the agent nothing it can act on.
type disbalanceVictim struct {
	experiment *domain.Experiment
	node       string
	// dimension is the resource the job overreached on, requested/share its request against its
	// proportionate entitlement, and idleAccelerators what that request stranded.
	dimension        domain.ResourceKind
	requested        int64
	share            float64
	idleAccelerators int64
}

// explain states what the job did, what it was entitled to, and what that cost — in that order,
// because the agent reading it needs to know what to change about its next submission.
func (v disbalanceVictim) explain() string {
	return fmt.Sprintf(
		"held %d accelerator(s) on node %s while requesting %s %d against a proportionate share of %.0f (%.1fx), stranding %d idle accelerator(s) that no queued job could reach; resubmit with %s sized closer to the accelerators it holds",
		v.experiment.AcceleratorCount, v.node, v.dimension, v.requested, v.share,
		float64(v.requested)/v.share, v.idleAccelerators, v.dimension)
}

// evictDisbalanced is called for a queued job that just failed to be admitted. It terminates the
// running jobs whose disproportionate CPU/memory/storage requests are the sole reason `blocked`
// cannot reach accelerators that are sitting idle on their node. Returns without acting unless
// every one of those conditions is provable from fresh data.
//
// Like preempt(), this is fire-and-forget: it writes desired state (EVICTED) and returns. The
// freed capacity is not visible this tick; whichever later tick first observes it genuinely free
// admits `blocked` through the ordinary path.
func (l *Loop) evictDisbalanced(
	ctx context.Context,
	blocked *domain.Experiment,
	cluster string,
	avail, total domain.Footprint,
	nodeAvail map[string]map[string]int64,
	nodeLabels map[string]map[string]string,
	blockedFP domain.Footprint,
) error {
	// Victim selection needs job->node attribution, which lives only in the metrics store. With
	// no metrics store configured there is no way to prove a job sits on the stranded node, so
	// the pass disables itself rather than guessing.
	if l.metricsDBURL == "" {
		return nil
	}
	// A cluster with no fresh total-capacity report has no per-accelerator share to measure
	// against. Absent denominator, no verdict.
	if len(total) == 0 {
		return nil
	}

	// The premises that need no I/O are checked first: this runs for every job skipped in a
	// tick, and the node attribution below costs one metrics query per running job.
	if !disbalancePremisesHold(blocked, blockedFP, avail, nodeAvail, nodeLabels) {
		return nil
	}

	running, err := l.store.ListRunningExperiments(ctx)
	if err != nil {
		return fmt.Errorf("disbalance: list running: %w", err)
	}

	now := time.Now()
	placed := make([]placedExperiment, 0, len(running))
	for _, exp := range running {
		if exp.ClusterName != cluster || exp.ID == blocked.ID {
			continue
		}
		// Already being evicted by a prior tick: its capacity is already accounted for via
		// applyPendingEvictions, and it must not be condemned a second time.
		if l.isPendingEviction(exp.ID, now) {
			continue
		}
		// A burst job may never displace guaranteed work, however disproportionate that work
		// looks. preempt() enforces exactly this by only ever considering burst victims; this
		// pass terminates jobs outright, so it cannot be the laxer of the two. Without this a
		// queued burst job could evict a running guaranteed one and invert the whole tier
		// guarantee.
		if blocked.CapacityTier == domain.CapacityBurst && exp.CapacityTier != domain.CapacityBurst {
			continue
		}
		node, found, err := metricsdb.LatestExperimentNode(ctx, l.metricsDBURL, exp.ID, time.Now().UTC(), ObservedMaxLookback)
		if err != nil {
			// A failed attribution lookup is missing data, not an absent placement — abort the
			// whole pass rather than proceed with a set of candidates we know is incomplete.
			return fmt.Errorf("disbalance: node attribution for %s: %w", exp.ID, err)
		}
		if !found {
			continue
		}
		placed = append(placed, placedExperiment{experiment: exp, node: node})
	}

	victims := selectDisbalanceVictims(blocked, blockedFP, avail, total, nodeAvail, nodeLabels, placed, l.disbalanceTolerance)
	for _, victim := range victims {
		explanation := victim.explain()
		l.logger.Info("evicting resource-disbalanced job",
			zap.String("victim", victim.experiment.ID),
			zap.String("blocked", blocked.ID),
			zap.String("cluster", cluster),
			zap.String("why", explanation),
			zap.String("victim_footprint", footprintStr(victim.experiment.Footprint())),
			zap.String("cluster_total", footprintStr(total)),
		)
		// The explanation rides the eviction reason itself, in the same "code: detail" shape the
		// scheduler already uses for not_admitted_reason — so the agent that lost the job reads
		// why on the record it already checks, with no second field or channel to discover.
		if err := l.evictor.EvictExperiment(ctx, victim.experiment.ID,
			domain.EvictionResourceDisbalance.WithDetail(explanation)); err != nil {
			l.logger.Error("evict resource-disbalanced job", zap.String("victim", victim.experiment.ID), zap.Error(err))
			continue
		}
		// Record the freed capacity as pending so the next tick(s) — until evictionTTL or a
		// capacity read catches up, whichever first — credit it back rather than evicting a fresh
		// victim to cover the same shortage again. See applyPendingEvictions.
		l.pendingEvictions[victim.experiment.ID] = pendingEviction{
			cluster:          cluster,
			node:             victim.node,
			footprint:        victim.experiment.Footprint(),
			acceleratorType:  strings.ToLower(string(victim.experiment.AcceleratorType)),
			acceleratorCount: int64(victim.experiment.AcceleratorCount),
			evictedAt:        now,
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionResourceDisbalance)).Inc()
	}
	return nil
}

// disbalancePremises reports whether the two capacity-only preconditions hold, returning the
// shortage `blocked` suffers and the nodes whose idle accelerators it could have used:
//
//   - the shortage is confined to CPU/memory/storage. If the accelerator dimension is short too,
//     the accelerators are genuinely busy and no disproportion is stranding anything.
//   - at least one node `blocked` is allowed to land on has enough free accelerators of its
//     flavor. Without idle accelerators there is no harm to undo.
//
// Split out from selectDisbalanceVictims so evictDisbalanced can reject the common case before
// spending a metrics query per running job — the same code decides both times, not a cheaper
// approximation of it.
func disbalancePremises(
	blocked *domain.Experiment,
	blockedFP, clusterAvail domain.Footprint,
	nodeAvail map[string]map[string]int64,
	nodeLabels map[string]map[string]string,
) (domain.Footprint, map[string]int64, bool) {
	if blocked.Job.AcceleratorCount <= 0 {
		return nil, nil, false
	}
	// preemptionShortfall — the same function the tick's own fit check uses at the call site that
	// led here (see loop_tick.go) — rather than a plain cluster-total shortfall(): the evictor
	// must judge "is blocked's accelerator dimension really short" with the identical per-node
	// placement evidence the trigger used, not a coarser cluster-wide sum that can disagree with
	// it. shortfall() only records dimensions with a real deficit, so every entry here is
	// positive.
	shortage := preemptionShortfall(clusterAvail, nodeAvail, nodeLabels, blocked, blockedFP)
	if len(shortage) == 0 {
		return nil, nil, false
	}
	for key := range shortage {
		if !isDisbalanceDimension(key.Kind) {
			return nil, nil, false
		}
	}

	flavor := strings.ToLower(string(blocked.AcceleratorType))
	perRank := int64(blocked.Job.AcceleratorCount)
	// Value, not just presence: how many accelerators a node is stranding is the whole cost of
	// the disproportion, and the agent is told it verbatim (see disbalanceVictim.explain).
	strandedNodes := make(map[string]int64, len(nodeAvail))
	for node, capacity := range nodeAvail {
		if !labelsMatch(nodeLabels[node], blocked.Job.NodeSelector) {
			continue
		}
		if free := foldLookup(capacity, flavor); free >= perRank {
			strandedNodes[node] = free
		}
	}
	if len(strandedNodes) == 0 {
		return nil, nil, false
	}
	return shortage, strandedNodes, true
}

func disbalancePremisesHold(
	blocked *domain.Experiment,
	blockedFP, clusterAvail domain.Footprint,
	nodeAvail map[string]map[string]int64,
	nodeLabels map[string]map[string]string,
) bool {
	_, _, ok := disbalancePremises(blocked, blockedFP, clusterAvail, nodeAvail, nodeLabels)
	return ok
}

// selectDisbalanceVictims is the whole decision, as a pure function of one tick's observed state.
// It returns the running jobs to evict, or nil — nil meaning "no action", which is also every
// failure mode: an unprovable premise, an incomplete plan, and absent data all land here.
//
// All five premises must hold:
//  1. `blocked` wants accelerators and is short only on CPU/memory/storage. If its accelerator
//     dimension is also short, the accelerators are genuinely busy and nothing is being stranded.
//  2. Some node `blocked` is allowed to land on currently has enough free accelerators of its
//     flavor. Without idle accelerators there is no harm to undo.
//  3. The cluster reports installed accelerators, giving a per-accelerator CPU/memory share.
//  4. Candidate victims sit on one of those nodes, hold accelerators themselves, and request
//     more than `tolerance` times their proportionate share of a short dimension.
//  5. The selected victims jointly cover the entire shortage. A partial plan evicts jobs and
//     still leaves `blocked` queued, so it is not worth anything — same rule as preempt().
func selectDisbalanceVictims(
	blocked *domain.Experiment,
	blockedFP, clusterAvail, clusterTotal domain.Footprint,
	nodeAvail map[string]map[string]int64,
	nodeLabels map[string]map[string]string,
	placed []placedExperiment,
	tolerance float64,
) []disbalanceVictim {
	if blocked.Job.AcceleratorCount <= 0 || len(clusterTotal) == 0 {
		return nil
	}

	// (1) and (2): a shortage confined to the fungible dimensions, and idle accelerators to be
	// stranded. Both are pure functions of this tick's capacity numbers.
	shortage, strandedNodes, ok := disbalancePremises(blocked, blockedFP, clusterAvail, nodeAvail, nodeLabels)
	if !ok {
		return nil
	}

	// (3) Total installed accelerators across every flavor: a node's CPU is shared by all the
	// accelerators plugged into it regardless of model, so the fair share is per device, not per
	// device type.
	var totalAccelerators int64
	for key, count := range clusterTotal {
		if key.Kind == domain.ResourceKindAccelerator && count > 0 {
			totalAccelerators += count
		}
	}
	if totalAccelerators <= 0 {
		return nil
	}

	// (4) Score every candidate by how far past its proportionate share it reaches, in the
	// dimensions that are actually short. Dimensions the cluster reports no total for are
	// skipped, never assumed violated. Grouped by node, not pooled cluster-wide: `blocked` can
	// only land where it has both idle accelerators AND the fungible headroom freeing these
	// victims would create, so a plan is only meaningful if every victim in it sits on the same
	// node as the idle accelerators it is meant to unlock — see (5).
	type scoredVictim struct {
		victim disbalanceVictim
		ratio  float64
	}
	byNode := make(map[string][]scoredVictim)
	for _, p := range placed {
		if strandedNodes[p.node] <= 0 || p.experiment.AcceleratorCount <= 0 {
			continue
		}
		fp := p.experiment.Footprint()
		worst := 0.0
		evidence := disbalanceVictim{experiment: p.experiment, node: p.node, idleAccelerators: strandedNodes[p.node]}
		for key := range shortage {
			installed := clusterTotal[key]
			if installed <= 0 {
				continue
			}
			share := float64(installed) / float64(totalAccelerators) * float64(p.experiment.AcceleratorCount)
			if share <= 0 {
				continue
			}
			ratio := float64(fp[key]) / share
			if ratio <= worst {
				continue
			}
			worst = ratio
			evidence.dimension, evidence.requested, evidence.share = key.Kind, fp[key], share
		}
		if worst > tolerance {
			byNode[p.node] = append(byNode[p.node], scoredVictim{victim: evidence, ratio: worst})
		}
	}
	if len(byNode) == 0 {
		return nil
	}

	// (5) For each node with disproportionate candidates, accumulate until that node's own
	// victims alone cover the whole shortage — a plan that frees room on nodes A+B is worthless
	// if `blocked`'s idle accelerators (and the fungible headroom this plan buys) are only usable
	// together on one specific node. Nodes are tried in deterministic (sorted) order; among nodes
	// that produce a complete plan, the smallest plan wins, node name breaking ties.
	nodes := make([]string, 0, len(byNode))
	for node := range byNode {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	var best []disbalanceVictim
	for _, node := range nodes {
		candidates := byNode[node]
		// Most disproportionate first, so the smallest number of the most offending jobs is
		// taken; experiment ID breaks ties for a deterministic decision across ticks.
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].ratio != candidates[j].ratio {
				return candidates[i].ratio > candidates[j].ratio
			}
			return candidates[i].victim.experiment.ID < candidates[j].victim.experiment.ID
		})

		var selected []disbalanceVictim
		freed := domain.NewFootprint()
		contributions := make(map[string]domain.Footprint, len(candidates))
		for _, c := range candidates {
			if domain.Fits(freed, shortage) {
				break
			}
			fp := c.victim.experiment.Footprint()
			contributions[c.victim.experiment.ID] = fp
			selected = append(selected, c.victim)
			freed.AddFootprint(fp)
		}
		if !domain.Fits(freed, shortage) {
			continue
		}

		// Fill-back pass: reprieve any victim whose removal still leaves the shortage covered, so
		// an overshoot in the loop above never costs an extra job. Mirrors preempt()'s pass.
		for i := len(selected) - 1; i >= 0; i-- {
			trial := freed.Sub(contributions[selected[i].experiment.ID])
			if domain.Fits(trial, shortage) {
				freed = trial
				selected = append(selected[:i], selected[i+1:]...)
			}
		}
		if best == nil || len(selected) < len(best) {
			best = selected
		}
	}
	return best
}

// Follow-up, deliberately not implemented here: a usage-based refinement that also catches a job
// whose request is proportionate on paper but whose real CPU/memory consumption is a fraction of
// it. That check cannot be written today — the k8s node agent
// (runtime/k8s/cmd/node-agent/main.go) walks pod cgroups but discards the cpu.stat usage_usec it
// reads, publishing only a per-pod liveness heartbeat, and never opens memory.current at all; the
// bare-metal runtime has no per-pod collector whatsoever. Every "capacity" number in the metrics
// store is allocatable-minus-*requests*, not consumption. Implementing it means: emit
// experiment_cpu_usage_seconds_total (a counter, from the usage_usec already parsed) and
// experiment_memory_working_set_bytes from the node agent, add the matching bare-metal collector,
// add Live* query helpers to shared/metricsdb, and only then extend the score above with a
// used/requested term — gated on the sample being fresh, so a job that has not reported yet keeps
// scoring as proportionate.
