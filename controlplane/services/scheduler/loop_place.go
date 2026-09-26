package scheduler

import (
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// placementTiers is the one order every placement tries a job's candidate clusters in: the
// clusters of the highest priority first, then the next, each tier by name. Within a tier the
// accelerator type a job prefers decides before the name does, which is why this returns tiers
// and not one list: the search is tier, then type, then cluster. A name nobody registered ranks
// with the shared clusters, last.
//
// The order is read off the tick's snapshot of capacity. The claim that follows locks the chosen
// cluster alone, so room freed on a higher cluster after the snapshot does not move the job.
func placementTiers(candidates map[string]domain.Footprint, registered map[string]domain.Cluster) [][]string {
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if pi, pj := registered[names[i]].Priority, registered[names[j]].Priority; pi != pj {
			return pi > pj
		}
		return names[i] < names[j]
	})
	var tiers [][]string
	for i, name := range names {
		if i == 0 || registered[name].Priority != registered[names[i-1]].Priority {
			tiers = append(tiers, nil)
		}
		tiers[len(tiers)-1] = append(tiers[len(tiers)-1], name)
	}
	return tiers
}

// placementOrder is placementTiers as one list, for a search with one accelerator type.
func placementOrder(candidates map[string]domain.Footprint, registered map[string]domain.Cluster) []string {
	order := make([]string, 0, len(candidates))
	for _, tier := range placementTiers(candidates, registered) {
		order = append(order, tier...)
	}
	return order
}

// within is the view of avail over one tier. It shares its Footprint values with avail, as
// eligibleClusters does, so a reservation made through either is seen through both.
func within(avail map[string]domain.Footprint, tier []string) map[string]domain.Footprint {
	out := make(map[string]domain.Footprint, len(tier))
	for _, name := range tier {
		out[name] = avail[name]
	}
	return out
}

// scalesAbove reports whether a cluster of a higher priority than chosen could be asked to scale
// up for exp: connected, scaling, and taking the job.
func scalesAbove(chosen string, exp *domain.Experiment, autoscalerEnabled, connected map[string]bool, registered map[string]domain.Cluster) bool {
	for cluster := range autoscalerEnabled {
		if autoscalerEnabled[cluster] && connected[cluster] && hostsJob(registered, cluster, exp) && registered[cluster].Priority > registered[chosen].Priority {
			return true
		}
	}
	return false
}

// above keeps the clusters of a higher priority than chosen, in the order given.
func above(clusters []string, chosen string, registered map[string]domain.Cluster) []string {
	out := clusters[:0:0]
	for _, cluster := range clusters {
		if registered[cluster].Priority > registered[chosen].Priority {
			out = append(out, cluster)
		}
	}
	return out
}
