package placement

import (
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// FitsWhenIdle reports whether a cluster with nothing running on it holds exp: its installed
// total covers the job, and each node of the job fits the cpu, memory and storage of some node
// of the cluster. Nodes whose totals were not read are not judged.
func FitsWhenIdle(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) bool {
	if !fitsInstalled(total, exp) {
		return false
	}
	if len(nodeTotals) == 0 {
		return true
	}
	for _, shape := range exp.NodeShapes() {
		held := false
		for _, node := range nodeTotals {
			if NodeHasRoom(node, shape.Resources) {
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
	types := CandidateTypes(exp)
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

func CloneAvail(avail map[string]domain.Footprint) map[string]domain.Footprint {
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

// SubtractFootprint subtracts fp from avail in place, clamped at zero per dimension. No-op if
// avail is nil (cluster not present in the capacity map).
func SubtractFootprint(avail domain.Footprint, fp domain.Footprint) {
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

// Shortfall returns, for every dimension footprint requests, how much more avail would need to
// have to fit it (need - have, floored at 0 per dimension) — the vector preempt() must cover.
func Shortfall(avail domain.Footprint, footprint domain.Footprint) domain.Footprint {
	out := domain.NewFootprint()
	for k, need := range footprint {
		have := avail[k]
		if have < need {
			out.Add(k, need-have)
		}
	}
	return out
}

// PreemptionShortfall keeps accelerator availability in the placement domain declared by the
// job. Cluster-level extended-resource totals combine devices from nodes with different labels,
// so they cannot answer whether (for example) A100 capacity is available when L40 capacity is
// idle. CPU, memory, storage, and other resources retain their cluster-level shortfall.
func PreemptionShortfall(clusterAvail domain.Footprint, byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment, footprint domain.Footprint) domain.Footprint {
	out := Shortfall(clusterAvail, footprint)
	// A job runs on one node, so a deficit can be entirely per-node: every dimension can be
	// plentiful cluster-wide while no single node has the combination. Reported as an empty
	// shortage, that job looked like it needed nothing — which left preemption with no vector to
	// cover and the disbalance evictor with no deficit to explain, exactly when a neighbour's
	// disproportionate request was the reason nothing fit.
	addNodeResourceShortfall(out, byNode, nodeResources, labelsByNode, exp)
	if exp.Job.TotalAccelerators() <= 0 {
		return out
	}
	key := AcceleratorKey(exp.AcceleratorType)
	capacities := make([]int64, 0, len(byNode))
	for node, capacity := range byNode {
		if LabelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
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
	if RequiresDistinctHosts(exp) {
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
	acceleratorKey := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Name: key}
	if missing > out[acceleratorKey] {
		out[acceleratorKey] = missing
	}
	return out
}

// CandidateTypes returns the accelerator types admission should try for exp, in preference
// order: exp.AcceleratorType first, then any distinct AcceptableAcceleratorTypes.
// AcceleratorCount is already accelerator-type-independent, so trying an alternate only changes which
// accelerator key the footprint is keyed under.
func CandidateTypes(exp *domain.Experiment) []domain.AcceleratorType {
	if exp.Job.AcceleratorType == "" {
		return nil
	}
	// A job that has already executed re-admits onto the accelerator type it actually ran on, and nothing
	// else. Settlement bills lifetime observed hours at the rate the row carries, so moving a
	// requeued job to a different accelerator type silently re-prices the stint it already ran at the new
	// accelerator type's rate. Read from the type the row recorded when the job was marked RUNNING, which
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
	// same accelerator type twice — "distinct" above is this dedup, not an assumption about the input.
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

// roomFor is how many jobs of footprint fit in free capacity: the fewest of any dimension it asks
// for.
func roomFor(free, footprint domain.Footprint) int64 {
	room := int64(-1)
	for key, need := range footprint {
		if need <= 0 {
			continue
		}
		if n := free[key] / need; room < 0 || n < room {
			room = n
		}
	}
	return max(room, 0)
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
	accelerators := CloneNodes(byNode)
	resources := CloneNodes(nodeResources)
	key := AcceleratorKey(exp.AcceleratorType)
	distinct := RequiresDistinctHosts(exp)
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
			if !LabelsMatch(labelsByNode[node], exp.Job.NodeSelector) {
				continue
			}
			if distinct && occupied[node] {
				continue
			}
			// Only nodes that could host this rank's accelerators are candidates; where the
			// accelerators themselves are missing, the per-node accelerator shortfall reported by
			// PreemptionShortfall already says so.
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
			kind, ok := NodeResourceKind(resource)
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

// ReportsEveryDimension reports whether capacity has an entry for every dimension footprint asks
// for — present, not necessarily sufficient. Zero free of an accelerator type the cluster owns is a cluster
// preemption can unblock; no entry at all is a cluster that does not have the hardware, and no
// eviction there will ever produce it.
func ReportsEveryDimension(capacity domain.Footprint, footprint domain.Footprint) bool {
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
