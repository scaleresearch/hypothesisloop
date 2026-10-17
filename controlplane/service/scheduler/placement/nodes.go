package placement

import (
	"fmt"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AcceleratorKey is the key a node's free accelerators of a type are under: the type in lower
// case, as the capacity read keys them (scheduler.ClusterReports.GetAcceleratorCapacityByNode).
func AcceleratorKey(t domain.AcceleratorType) string {
	return strings.ToLower(string(t))
}

func RequiresDistinctHosts(exp *domain.Experiment) bool {
	// TotalAccelerators rather than the top-level per-node count: a grouped job states its counts
	// per group and leaves the top-level field empty, which read literally would say every
	// heterogeneous job is happy to stack all its ranks on one host.
	if exp.Job.Nodes() <= 1 || exp.Job.TotalAccelerators() <= 0 {
		return false
	}
	return exp.Job.Topology == nil || exp.Job.Topology.SpreadAcrossHosts == nil || *exp.Job.Topology.SpreadAcrossHosts
}

// TopologyFits proves that every rank of a hard spread-across-hosts accelerator job has a
// distinct currently-schedulable node with enough free devices of the selected accelerator type.
func TopologyFits(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	return Reserve(CloneNodes(byNode), CloneNodes(nodeResources), labelsByNode, exp)
}

func LabelsMatch(actual, required map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

// DesiredFits proves candidate fits the cluster beside the jobs already desired there.
// resolved is the candidate's job as the runtime will be handed it: when the candidate lands on a
// slot (domain.NodeSlotLabel), resolved is pinned to that slot.
func DesiredFits(byNode map[string]map[string]int64, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, desired []*domain.Experiment, candidate *domain.Experiment, resolved *domain.JobSpec) bool {
	byNode, nodeResources = CloneNodes(byNode), CloneNodes(nodeResources)
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
			HoldSlot(byNode, nodeResources, labelsByNode, slot)
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

func CloneNodes(byNode map[string]map[string]int64) map[string]map[string]int64 {
	cloned := make(map[string]map[string]int64, len(byNode))
	for node, capacity := range byNode {
		cloned[node] = make(map[string]int64, len(capacity))
		for key, count := range capacity {
			cloned[node][key] = count
		}
	}
	return cloned
}

// Reserve walks exp's ranks over the candidate nodes and subtracts what each rank takes,
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
// It walks domain.NodeShapes — one entry per node, which for a heterogeneous (grouped)
// job are genuinely different shapes. An averaged shape would be the one thing that must not be
// used here: no node ever holds the average of a learner and 64 actors, so a job proven to fit
// against it fits nowhere.
//
// The walk is a search, not a single greedy pass: ranks are tried hardest-first, each on the
// tightest node that fits, and a rank that finds no host sends the search back to move an earlier
// one. A plain first-fit in declared order let a small rank take the only node a later large rank
// could use and reported a job with a valid assignment as not fitting. The input maps are
// modified only when the whole job is placed — a false answer leaves them exactly as they were.
func Reserve(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	plan, ok := planPlacement(CloneNodes(byNode), CloneNodes(nodeResources), labelsByNode, []*domain.Experiment{exp})
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
// AcceleratorKey of the job's type, "" for a rank that holds no accelerator.
type rankAssignment struct {
	job   int
	shape domain.NodeShape
	node  string
	key   string
}

// searchBudget bounds the number of node trials one planPlacement call may make before
// it gives up. Hardest-first ordering with best-fit node choice and equivalent-node pruning places
// realistic jobs without backtracking at all; the budget is a guard against an adversarial layout
// exhausting the tick, and running out of it is reported as "does not fit" — a conservative
// answer, the same one the greedy walk gave whenever it guessed wrong.
var searchBudget = 50000

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
		key := AcceleratorKey(exp.AcceleratorType)
		// Two candidate sets: a node that reported accelerators, and any node the cluster
		// reported resources for. A node needing no accelerator must not be confined to
		// accelerator nodes.
		acceleratorNodes := qualifyingNodes(byNode, labelsByNode, exp)
		plainNodes := qualifyingNodes(nodeResources, labelsByNode, exp)
		distinct := RequiresDistinctHosts(exp)
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
	budget := searchBudget

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
		if !NodeHasRoom(nodeResources[node], r.shape.Resources) {
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
			AddNodeResources(nodeResources[c.node], r.shape.Resources)
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

func AddNodeResources(available, used map[string]int64) {
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
		if LabelsMatch(labelsByNode[node], exp.EffectiveJob().NodeSelector) {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	return nodes
}

// HardestNodeShape is the single node of exp that is hardest to place — most accelerators first,
// then the largest fungible request. Diagnostics that describe a job by one shape (the shortfall
// vector, and the preemption plan built from it) use this one, because covering the hardest node
// is what actually unblocks the job. Every shape is identical for an ungrouped job, so this is
// that job's per-node shape exactly as before.
func HardestNodeShape(exp *domain.Experiment) domain.NodeShape {
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

func NodeHasRoom(available, needed map[string]int64) bool {
	if len(needed) == 0 {
		return true
	}
	// A node the cluster did not report resources for cannot be proven to have room for a job
	// that needs some. Reporting them is required (see clusterexchange), so an absent entry is a
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

func NodeResourceKind(resource string) (domain.ResourceKind, bool) {
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

// CloneNodesByCluster deep-copies a cluster -> node -> resource capacity map, so a snapshot
// survives the in-place reservations admission makes as it walks the queue.
func CloneNodesByCluster(byCluster map[string]map[string]map[string]int64) map[string]map[string]map[string]int64 {
	cloned := make(map[string]map[string]map[string]int64, len(byCluster))
	for cluster, byNode := range byCluster {
		cloned[cluster] = CloneNodes(byNode)
	}
	return cloned
}
