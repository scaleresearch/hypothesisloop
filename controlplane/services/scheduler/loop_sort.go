package scheduler

import (
	"sort"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
// can evict to cover a shortage Footprint that may span multiple dimensions at once, so
// candidates are no longer narrowed to a single matching flavor here (preempt() itself decides
// which victims' combined footprint actually covers the shortage). Scoped to one cluster
// because freeing capacity on a different cluster wouldn't make room for a job being admitted
// onto this one.
func filterTierCluster(exps []*domain.Experiment, tier domain.CapacityTier, clusterName string) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if e.CapacityTier == tier && e.ClusterName == clusterName {
			out = append(out, e)
		}
	}
	return out
}

// dominantUtilization looks up exp's agent/platform-experiment quota in quotaMap and returns
// its dominant-utilization fairness ratio for exp's own requested dimensions (see
// domain.AgentQuota.DominantUtilization) — 0 if no quota row was found (nothing tracked yet, or
// exp has no PlatformExperimentID at all).
func dominantUtilization(quotaMap map[string]*domain.AgentQuota, exp *domain.Experiment) float64 {
	aq := quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)]
	if aq == nil {
		return 0
	}
	return aq.DominantUtilization(exp)
}

// dominantCostFraction is dominantUtilization's counterpart for "how big is this one job",
// replacing the old GPU-only GPUHours() tiebreak (which was always zero for CPU-only jobs —
// see domain.AgentQuota.DominantCostFraction for why this generalizes correctly across
// CPU/GPU/RAM/storage jobs instead of comparing raw, unit-incompatible hours).
func dominantCostFraction(quotaMap map[string]*domain.AgentQuota, exp *domain.Experiment) float64 {
	aq := quotaMap[quotaKey(exp.AgentID, exp.PlatformExperimentID)]
	if aq == nil {
		return 0
	}
	return aq.DominantCostFraction(exp)
}

// sortGuaranteed sorts guaranteed-tier experiments:
// 1. age bucket ASC, quantized to fairnessWindow (oldest bucket first) — jobs within the same
//    bucket are treated as "arrived around the same time" rather than strictly ordered by exact
//    queued_at, so...
// 2. ...dominant-utilization ASC (least-used-guaranteed-quota agent first, over the dimensions
//    each job actually requests — see domain.AgentQuota.DominantUtilization) breaks ties within
//    a bucket. Without this, pure exact-timestamp FIFO lets an agent with a steady submission
//    stream get its jobs consistently admitted ahead of other agents' equally-entitled jobs
//    purely because it always has *a* job ready to submit right after the last one clears — this
//    bounds that latency-fairness gap the same way Kueue's DRS bounds time-to-admission within a
//    tier, without abandoning FIFO altogether (a job's age bucket still dominates once the gap
//    between two jobs exceeds fairnessWindow, so nothing waits indefinitely).
// 3. CompletionFraction DESC (finish interrupted work first)
// 4. dominant cost fraction ASC (smallest job first, dimensionless across CPU/GPU/RAM/storage)
// 5. PriorityScore DESC (novelty + cost-efficiency — see computePriority) as the final tiebreak,
//    so the score every submission computes and persists is actually consumed by ordering
//    instead of being a dead, API-only number.
func sortGuaranteed(exps []*domain.Experiment, quotaMap map[string]*domain.AgentQuota, fairnessWindow time.Duration) {
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
		cf := ei.CompletionFraction() - ej.CompletionFraction()
		if cf > 0.01 {
			return true
		}
		if cf < -0.01 {
			return false
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
// 1. dominant-utilization ASC (least used guaranteed quota goes first, over each job's own
//    requested dimensions)
// 2. CompletionFraction DESC
// 3. dominant cost fraction ASC
// 4. PriorityScore DESC
// 5. queued_at ASC (final tiebreak)
func sortBurst(exps []*domain.Experiment, quotaMap map[string]*domain.AgentQuota) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		ri, rj := dominantUtilization(quotaMap, ei), dominantUtilization(quotaMap, ej)
		if ri != rj {
			return ri < rj
		}

		cf := ei.CompletionFraction() - ej.CompletionFraction()
		if cf > 0.01 {
			return true
		}
		if cf < -0.01 {
			return false
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
