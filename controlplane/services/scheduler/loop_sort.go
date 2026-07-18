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

// filterTierFlavorCluster returns experiments of the given tier and cluster whose
// admission-unit flavor (GPU flavor, or the cpuFlavor pseudo-flavor for CPU-only jobs)
// matches flavorName. Scoped to one cluster because freeing capacity on a different cluster
// wouldn't make room for a job being admitted onto this one.
func filterTierFlavorCluster(exps []*domain.Experiment, tier domain.CapacityTier, flavorName, clusterName string) []*domain.Experiment {
	var out []*domain.Experiment
	for _, e := range exps {
		if f, _ := admissionUnit(e); e.CapacityTier == tier && f == flavorName && e.ClusterName == clusterName {
			out = append(out, e)
		}
	}
	return out
}

// sortGuaranteed sorts guaranteed-tier experiments:
// 1. age bucket ASC, quantized to fairnessWindow (oldest bucket first) — jobs within the same
//    bucket are treated as "arrived around the same time" rather than strictly ordered by exact
//    queued_at, so...
// 2. ...quotaMap ratio ASC (least-used-guaranteed-quota agent first) breaks ties within a bucket.
//    Without this, pure exact-timestamp FIFO lets an agent with a steady submission stream get
//    its jobs consistently admitted ahead of other agents' equally-entitled jobs purely because
//    it always has *a* job ready to submit right after the last one clears — this bounds that
//    latency-fairness gap the same way Kueue's DRS bounds time-to-admission within a tier,
//    without abandoning FIFO altogether (a job's age bucket still dominates once the gap between
//    two jobs exceeds fairnessWindow, so nothing waits indefinitely).
// 3. CompletionFraction DESC (finish interrupted work first)
// 4. EstimatedDurationHours ASC (shortest job first for faster feedback)
func sortGuaranteed(exps []*domain.Experiment, quotaMap map[string]float64, fairnessWindow time.Duration) {
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
			ri, rj := quotaMap[quotaKey(ei.AgentID, ei.PlatformExperimentID)], quotaMap[quotaKey(ej.AgentID, ej.PlatformExperimentID)]
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

		// Tiebreak 2: fewest GPU-hours first (gpu_count × duration — prefers short OR small-GPU jobs).
		return ei.GPUHours() < ej.GPUHours()
	})
}

// sortBurst sorts burst-tier experiments:
// 1. quota utilisation ratio ASC (least used guaranteed quota goes first)
// 2. CompletionFraction DESC
// 3. EstimatedDurationHours ASC
// 4. queued_at ASC (final tiebreak)
func sortBurst(exps []*domain.Experiment, quotaMap map[string]float64) {
	sort.SliceStable(exps, func(i, j int) bool {
		ei, ej := exps[i], exps[j]

		ri := quotaMap[quotaKey(ei.AgentID, ei.PlatformExperimentID)]
		rj := quotaMap[quotaKey(ej.AgentID, ej.PlatformExperimentID)]
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

		// Prefer fewest GPU-hours (gpu_count × duration) — same idea as guaranteed sort.
		if ei.GPUHours() != ej.GPUHours() {
			return ei.GPUHours() < ej.GPUHours()
		}

		if ei.QueuedAt != nil && ej.QueuedAt != nil {
			return ei.QueuedAt.Before(*ej.QueuedAt)
		}
		return false
	})
}
