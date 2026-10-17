package placement

import (
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ClusterAndFootprint picks a concrete (cluster, flavor) pair for an unpinned job. It
// walks the clusters by priority (Tiers), and within one priority tries every candidate
// flavor (requested type first, see CandidateTypes) before the next tier is looked at:
// a job lands on a higher-priority cluster at an acceptable alternative flavor before it lands on
// a lower one at the flavor it asked for, and a full tier never keeps it from a lower one that
// fits. If nothing fits outright anywhere, falls back to the requested flavor's best-fit cluster
// so the caller's preemption path still runs.
// TotalAccelerators, not the top-level per-node count: a grouped job states its counts on its
// groups and leaves the top-level field empty, so reading it literally said every heterogeneous
// job wanted no accelerators at all. Such a job then took the CPU-only shortcut below — no flavor
// substitution and, worse, no AtFlavor, so it was placed on scalar fit alone and could be
// handed a cluster whose nodes had not one free device of the hardware it asked for.
func ClusterAndFootprint(avail map[string]domain.Footprint, registered map[string]domain.Cluster, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, domain.Footprint) {
	requested := exp.AcceleratorType
	types := CandidateTypes(exp)
	if exp.Job.TotalAccelerators() <= 0 {
		// No flavor to vary. Still judged on node layout: a cluster whose aggregate is large
		// enough but whose nodes are individually too small for one rank would otherwise be picked
		// on the scalar alone, fail TopologyFits in the tick, and go to preemption while another
		// cluster could host it.
		types = []domain.AcceleratorType{requested}
	}
	if len(types) == 0 {
		return "", nil
	}
	for _, tier := range Tiers(avail, registered) {
		inTier := within(avail, tier)
		for _, t := range types {
			exp.AcceleratorType = t
			if cluster, ok := AtFlavor(inTier, registered, nodeAvail, nodeResources, nodeLabels, exp); ok {
				return cluster, exp.Footprint()
			}
		}
	}
	exp.AcceleratorType = types[0]
	fallback := exp.Footprint()
	exp.AcceleratorType = requested
	return BestFit(avail, registered, fallback), fallback
}

// AtFlavor names the cluster that can host exp at the accelerator type it currently carries,
// judged on footprint and node layout alike: in the highest priority tier that has one, the one
// with room for the most jobs like exp (roomFor), so jobs spread over the clusters an org ranked
// alike and keep each busy. One run placed 7 of 9 researcher jobs on the first name that fit, a box
// of 12 cores, while one of 28 sat mostly idle. Among those with equal room the cluster that built
// exp's environment, which holds its image, goes first, then the name. ok=false means no cluster
// can: the caller decides what to do with a job that does not fit anywhere, rather than being
// handed a cluster that only looks big enough in the scalar.
func AtFlavor(avail map[string]domain.Footprint, registered map[string]domain.Cluster, nodeAvail, nodeResources map[string]map[string]map[string]int64, nodeLabels map[string]map[string]map[string]string, exp *domain.Experiment) (string, bool) {
	for _, tier := range Tiers(avail, registered) {
		best, bestRoom := "", int64(0)
		for _, candidate := range tier {
			// The job is not resolved yet, and on a cluster of slots it only fits as the slot it
			// would be resolved to.
			probe, ok := OnSlots(exp, candidate, nodeResources[candidate], nodeLabels[candidate])
			if !ok {
				continue
			}
			fp := probe.Footprint()
			if !domain.Fits(avail[candidate], fp) || !TopologyFits(nodeAvail[candidate], nodeResources[candidate], nodeLabels[candidate], probe) {
				continue
			}
			room := roomFor(avail[candidate], fp)
			loaded := exp.EnvironmentCluster != "" && candidate == exp.EnvironmentCluster && best != exp.EnvironmentCluster
			if best == "" || room > bestRoom || room == bestRoom && loaded {
				best, bestRoom = candidate, room
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

// BestFit picks a target cluster for footprint among every configured cluster in
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
// Both steps go by priority before anything else (Tiers): step 1 takes the first fit in
// placement order, and step 2 answers from the highest-priority tier that holds a cluster which
// could ever host the request, so the room is made on the compute the org ranked first.
func BestFit(avail map[string]domain.Footprint, registered map[string]domain.Cluster, footprint domain.Footprint) string {
	tiers := Tiers(avail, registered)
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
			if !ReportsEveryDimension(avail[c], footprint) {
				continue
			}
			var shortage int64
			for _, v := range Shortfall(avail[c], footprint) {
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

// Tiers is the one order every placement tries a job's candidate clusters in: the
// clusters of the highest priority first, then the next, each tier by name. Within a tier the
// accelerator type a job prefers decides first, then the room left (AtFlavor), which is why
// this returns tiers and not one list: the search is tier, then type, then cluster. A name nobody
// registered ranks with the shared clusters, last.
//
// The order is read off the tick's snapshot of capacity. The claim that follows locks the chosen
// cluster alone, so room freed on a higher cluster after the snapshot does not move the job.
func Tiers(candidates map[string]domain.Footprint, registered map[string]domain.Cluster) [][]string {
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

// within is the view of avail over one tier. It shares its Footprint values with avail, as
// the scheduler's eligibleClusters does, so a reservation made through either is seen through both.
func within(avail map[string]domain.Footprint, tier []string) map[string]domain.Footprint {
	out := make(map[string]domain.Footprint, len(tier))
	for _, name := range tier {
		out[name] = avail[name]
	}
	return out
}
