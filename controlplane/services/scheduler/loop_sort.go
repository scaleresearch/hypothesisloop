package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// completionBucket quantizes a completion fraction to a 0.01 grid so "close enough" comparisons
// form a transitive equivalence relation. A sliding epsilon band isn't transitive (a chain of
// near-neighbors can drift beyond 0.01 end-to-end), which breaks sort's strict-weak-ordering
// contract. Rounding to a fixed grid avoids that.
func completionBucket(f float64) float64 {
	return math.Round(f*100) / 100
}

// filterTier returns experiments of the given capacity tier.
func filterTier(exps []*domain.Experiment, tier domain.CapacityTier) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if e.CapacityTier == tier {
			out = append(out, e)
		}
	}
	return out
}

// filterTierCluster returns experiments of the given tier and cluster — candidates preempt()
// can evict to cover a shortage that may span multiple dimensions (preempt() itself decides
// which victims' combined footprint covers it). Scoped to one cluster since freeing capacity
// elsewhere wouldn't help a job being admitted onto this one.
func filterTierCluster(exps []*domain.Experiment, tier domain.CapacityTier, clusterName string) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if e.CapacityTier == tier && e.ClusterName == clusterName {
			out = append(out, e)
		}
	}
	return out
}

// dominantUtilization looks up exp's agent/platform-experiment quota in quotaMap and returns its
// dominant-utilization fairness ratio for exp's own requested dimensions (see
// domain.AgentQuota.DominantUtilization) — 0 if no quota row was found.
func dominantUtilization(quotaMap map[string]*domain.AgentQuota, exp *domain.Experiment) float64 {
	aq := quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)]
	if aq == nil {
		return 0
	}
	return aq.DominantUtilization(exp)
}

// dominantCostFraction is dominantUtilization's counterpart for "how big is this one job" —
// replaces the old accelerator-only tiebreak, which was always zero for CPU-only jobs (see
// domain.AgentQuota.DominantCostFraction).
func dominantCostFraction(quotaMap map[string]*domain.AgentQuota, exp *domain.Experiment) float64 {
	aq := quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)]
	if aq == nil {
		return 0
	}
	return aq.DominantCostFraction(exp)
}

// sortGuaranteed sorts guaranteed-tier experiments:
//  1. age bucket ASC, quantized to fairnessWindow (oldest bucket first) — jobs in the same
//     bucket are treated as "arrived around the same time" rather than strictly FIFO
//  2. dominant-utilization ASC breaks ties within a bucket (least-used-quota agent first, over
//     the dimensions each job requests). Without this, an agent with a steady submission stream
//     could keep landing ahead of equally-entitled agents just by always having a job ready —
//     this bounds that latency-fairness gap (like Kueue's DRS) without abandoning FIFO once the
//     age gap exceeds fairnessWindow
//  3. CompletionFraction DESC (finish interrupted work first)
//  4. dominant cost fraction ASC (smallest job first, dimensionless across resource types)
//  5. PriorityScore DESC (novelty + cost-efficiency — see computePriority) as final tiebreak
func sortGuaranteed(exps []*domain.Experiment, quotaMap map[string]*domain.AgentQuota, completion map[string]float64, fairnessWindow time.Duration) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		// Primary: age bucket (oldest queued_at, quantized to fairnessWindow, first).
		if ei.QueuedAt != nil && ej.QueuedAt != nil && fairnessWindow > 0 {
			bi := ei.QueuedAt.Truncate(fairnessWindow)
			bj := ej.QueuedAt.Truncate(fairnessWindow)
			if !bi.Equal(bj) {
				return bi.Before(bj)
			}
			// Same bucket: least-used-guaranteed-quota agent goes first.
			ri, rj := dominantUtilization(quotaMap, ei), dominantUtilization(quotaMap, ej)
			if ri != rj {
				return ri < rj
			}
		} else if ei.QueuedAt != nil && ej.QueuedAt != nil && !ei.QueuedAt.Equal(*ej.QueuedAt) {
			return ei.QueuedAt.Before(*ej.QueuedAt)
		}

		// Tiebreak 1: completion proximity DESC.
		bi, bj := completionBucket(completion[ei.ID]), completionBucket(completion[ej.ID])
		if bi != bj {
			return bi > bj
		}

		// Tiebreak 2: smallest dominant cost fraction first (prefers short OR small-footprint
		// jobs, across whichever dimension — CPU or accelerator — the job actually requests).
		ci, cj := dominantCostFraction(quotaMap, ei), dominantCostFraction(quotaMap, ej)
		if ci != cj {
			return ci < cj
		}

		// Tiebreak 3: higher PriorityScore first.
		return ei.PriorityScore > ej.PriorityScore
	})
}

// sortBurst sorts burst-tier experiments:
//  1. dominant-utilization ASC (least used guaranteed quota goes first, over each job's own
//     requested dimensions)
//  2. CompletionFraction DESC
//  3. dominant cost fraction ASC
//  4. PriorityScore DESC
//  5. queued_at ASC (final tiebreak)
func sortBurst(exps []*domain.Experiment, quotaMap map[string]*domain.AgentQuota, completion map[string]float64) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		ri, rj := dominantUtilization(quotaMap, ei), dominantUtilization(quotaMap, ej)
		if ri != rj {
			return ri < rj
		}

		bi, bj := completionBucket(completion[ei.ID]), completionBucket(completion[ej.ID])
		if bi != bj {
			return bi > bj
		}

		// Prefer smallest dominant cost fraction — same idea as guaranteed sort.
		ci, cj := dominantCostFraction(quotaMap, ei), dominantCostFraction(quotaMap, ej)
		if ci != cj {
			return ci < cj
		}

		if ei.PriorityScore != ej.PriorityScore {
			return ei.PriorityScore > ej.PriorityScore
		}

		if ei.QueuedAt != nil && ej.QueuedAt != nil {
			return ei.QueuedAt.Before(*ej.QueuedAt)
		}
		return false
	})
}

// interleaveByAgent regroups exps — already priority-ordered within each agent by sortBurst —
// into round-robin order across agents: first job of agent 1, first of agent 2, ..., second of
// agent 1, second of agent 2, .... Burst-hour quotas only cap an agent's cumulative consumption
// over an experiment's lifetime; they say nothing about how much of the cluster's *physical*
// burst pool one agent may hold at once. Without this, a single linear admission pass over a
// priority-sorted list lets one agent's continuously replenished queue claim every unit of
// capacity that frees up in a tick, ahead of another agent that only has one job waiting.
// Interleaving bounds that to roughly a one-job lead per round, with no preemption or new
// tracked state — agent order (and each agent's own job order) is exactly the input order, so
// existing fairness/priority signals inside one agent's jobs are preserved untouched.
func interleaveByAgent(exps []*domain.Experiment) []*domain.Experiment {
	if len(exps) == 0 {
		return exps
	}
	byAgent := make(map[string][]*domain.Experiment, len(exps))
	var agentOrder []string
	for _, e := range exps {
		if _, seen := byAgent[e.AgentID]; !seen {
			agentOrder = append(agentOrder, e.AgentID)
		}
		byAgent[e.AgentID] = append(byAgent[e.AgentID], e)
	}
	out := make([]*domain.Experiment, 0, len(exps))
	for round := 0; len(out) < len(exps); round++ {
		for _, agentID := range agentOrder {
			if round < len(byAgent[agentID]) {
				out = append(out, byAgent[agentID][round])
			}
		}
	}
	return out
}
