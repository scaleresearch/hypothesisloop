package placement

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AcceleratorKey is the key a node's free accelerators of a type are under, as the capacity read
// keys them (scheduler.ClusterReports.GetAcceleratorCapacityByNode).
func AcceleratorKey(t domain.AcceleratorType) string {
	return string(t)
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

// FitsOnlyApart says whether exp's nodes fit the free nodes only with some of them outside the
// place every other is in (JobSpec.NodesWithin).
func FitsOnlyApart(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) bool {
	if exp.EffectiveJob().NodesWithin() == "" || TopologyFits(byNode, nodeResources, labelsByNode, exp) {
		return false
	}
	_, apart := planPlacement(CloneNodes(byNode), CloneNodes(nodeResources), labelsByNode, []*domain.Experiment{exp}, true)
	return apart
}

func LabelsMatch(actual, required map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

// RunsOn says whether a node of labels may run exp as submitted: it carries every label exp's job
// selects and the architecture of exp's image.
func RunsOn(labels map[string]string, exp *domain.Experiment) bool {
	return LabelsMatch(labels, exp.Job.NodeSelector) && exp.OnImageArchitecture(labels)
}

// DesiredFits proves candidate fits the cluster beside the jobs already desired there.
// resolved is the candidate's job as the runtime will be handed it: when the candidate lands on
// slots (domain.NodeSlotLabel), resolved is pinned to them, one per node in rank order.
func DesiredFits(byNode map[string]map[string]int64, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, desired []*domain.Experiment, candidate *domain.Experiment, resolved *domain.JobSpec) bool {
	byNode, nodeResources = CloneNodes(byNode), CloneNodes(nodeResources)
	// Planned jointly, not one job after another: the runtime is free to put an already-desired
	// job on whichever node it likes, so replaying it onto one fixed node and then asking whether
	// the candidate fits around that guess rejected candidates the cluster could host.
	//
	// A desired job pinned to slots is the exception, since its nodes are decided. It holds those
	// slots whether or not the runtime has rented them yet, and is not planned again: a slot reads
	// as taken from the moment its instance exists, and planning the job there would find no room
	// for a job that is already in place.
	jobs := make([]*domain.Experiment, 0, len(desired)+1)
	for _, d := range desired {
		if slots := d.EffectiveJob().Slots; len(slots) > 0 {
			HoldSlots(byNode, nodeResources, labelsByNode, slots)
			continue
		}
		jobs = append(jobs, d)
	}
	jobs = append(jobs, candidate)
	plan, ok := planPlacement(byNode, nodeResources, labelsByNode, jobs, false)
	if !ok {
		return false
	}
	// A job is on slots with every node or with none: one runtime rents all of its nodes.
	slots := make([]string, len(candidate.NodeShapes()))
	for _, p := range plan {
		if p.job == len(jobs)-1 {
			slots[p.rank] = labelsByNode[p.node][domain.NodeSlotLabel]
		}
	}
	switch {
	case !slices.Contains(slots, ""):
		resolved.Slots = slots
	case slices.ContainsFunc(slots, func(slot string) bool { return slot != "" }):
		return false
	}
	for _, p := range plan {
		if p.job != len(jobs)-1 {
			continue
		}
		place, _ := nearness(labelsByNode, p.node, candidate.EffectiveJob().NodesWithin(), labelledLevels(labelsByNode))
		if len(place) > 0 {
			selector := maps.Clone(resolved.NodeSelector)
			if selector == nil {
				selector = map[string]string{}
			}
			maps.Copy(selector, place)
			resolved.NodeSelector = selector
		}
		break
	}
	return true
}

// nearness is the place of node at level and every wider one, as the node labels that name it,
// which every node of a job within level shares; ok is false for a node no such job may take.
// A slot as near as level (slotWithin) is in every place of its job's: the runtime rents a job's
// slots on one private network. A node without the label of a region or zone is in the one region or zone of a cluster none of
// whose nodes is labelled with one (labelled), and a node with no placement group takes no job
// within one.
func nearness(labelsByNode map[string]map[string]string, node string, level domain.NodesLevel, labelled map[string]bool) (place map[string]string, ok bool) {
	labels := labelsByNode[node]
	if level == "" {
		return nil, true
	}
	if isSlot(labels) {
		return nil, slotWithin(labels).AsNearAs(level)
	}
	place = map[string]string{}
	for _, l := range clusterapi.NodesLevels {
		key := levelLabels[l]
		if l == domain.NodesWithinPlacementGroup {
			key = labels[domain.NodePlacementGroupKeyLabel]
		}
		switch value := labels[key]; {
		case key != "" && value != "":
			place[key] = value
		case l == domain.NodesWithinPlacementGroup || labelled[key]:
			return nil, false
		}
		if l == level {
			break
		}
	}
	return place, true
}

// levelLabels are the node labels that name a node's region and zone.
var levelLabels = map[domain.NodesLevel]string{
	domain.NodesWithinRegion: domain.NodeRegionLabel,
	domain.NodesWithinZone:   domain.NodeZoneLabel,
}

// labelledLevels says which of levelLabels some node of labelsByNode carries.
func labelledLevels(labelsByNode map[string]map[string]string) map[string]bool {
	labelled := map[string]bool{}
	for _, labels := range labelsByNode {
		for _, key := range levelLabels {
			if labels[key] != "" {
				labelled[key] = true
			}
		}
	}
	return labelled
}

// placeKey is place as one comparable string.
func placeKey(place map[string]string) string {
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(place)) {
		fmt.Fprintf(&b, "%s=%s;", key, place[key])
	}
	return b.String()
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
	plan, ok := planPlacement(CloneNodes(byNode), CloneNodes(nodeResources), labelsByNode, []*domain.Experiment{exp}, false)
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

// rankAssignment is one rank of one job pinned to a node by planPlacement: rank is its index in
// the job's NodeShapes. key is the AcceleratorKey of the job's type, "" for a rank that holds no
// accelerator.
type rankAssignment struct {
	job   int
	rank  int
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
// were placed. Every node of a job is in one place of its JobSpec.NodesWithin unless apart. On
// false the maps are left in an unspecified state.
func planPlacement(byNode, nodeResources map[string]map[string]int64, labelsByNode map[string]map[string]string, jobs []*domain.Experiment, apart bool) ([]rankAssignment, bool) {
	type rank struct {
		job      int
		rank     int
		shape    domain.NodeShape
		key      string
		distinct bool
		// network is how many nodes of the job are of this rank's shape, which a slot's private
		// network must hold (SlotNetwork); 0 for a job on one node or one already on its slots.
		network int
		nodes   []string // qualifying candidate set for this rank
	}
	var ranks []rank
	// places is, for each job within a level, the place each node it may take is in there
	// (nearness); nil for a job on one node.
	places := make([]map[string]string, len(jobs))
	labelled := labelledLevels(labelsByNode)
	for i, exp := range jobs {
		key := AcceleratorKey(exp.AcceleratorType)
		// Two candidate sets: a node that reported accelerators, and any node the cluster
		// reported resources for. A node needing no accelerator must not be confined to
		// accelerator nodes.
		acceleratorNodes := qualifyingNodes(byNode, labelsByNode, exp)
		plainNodes := qualifyingNodes(nodeResources, labelsByNode, exp)
		distinct := RequiresDistinctHosts(exp)
		shapes := exp.NodeShapes()
		pinned := exp.EffectiveJob().Slots
		if level := exp.EffectiveJob().NodesWithin(); level != "" && !apart {
			places[i] = map[string]string{}
			for _, node := range slices.Concat(acceleratorNodes, plainNodes) {
				if place, ok := nearness(labelsByNode, node, level, labelled); ok {
					places[i][node] = placeKey(place)
				}
			}
			outside := func(node string) bool { _, ok := places[i][node]; return !ok }
			acceleratorNodes = slices.DeleteFunc(acceleratorNodes, outside)
			plainNodes = slices.DeleteFunc(plainNodes, outside)
		}
		for r, shape := range shapes {
			nodes := plainNodes
			if shape.AcceleratorCount > 0 {
				nodes = acceleratorNodes
			}
			network := 0
			switch {
			case len(pinned) > 0:
				nodes = slices.DeleteFunc(slices.Clone(nodes), func(node string) bool { return labelsByNode[node][domain.NodeSlotLabel] != pinned[r] })
			case len(shapes) > 1:
				network = sameShape(shapes, shape)
			}
			ranks = append(ranks, rank{job: i, rank: r, shape: shape, key: key, distinct: distinct, network: network, nodes: nodes})
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
	// at is the place a job within a level is held to once one of its ranks is placed, and
	// placed how many of its ranks are.
	at := make([]string, len(jobs))
	placed := make([]int, len(jobs))
	plan := make([]rankAssignment, len(ranks))
	budget := searchBudget

	// nodeFits reports whether node can host r right now and the accelerator key it would draw on.
	nodeFits := func(r rank, node string) (string, bool) {
		if r.distinct && used[r.job][node] {
			return "", false
		}
		if places[r.job] != nil && placed[r.job] > 0 && places[r.job][node] != at[r.job] {
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
		if isSlot(labelsByNode[node]) && (!takesWholeSlot(r.shape, nodeKey, byNode[node], nodeResources[node]) || SlotNetwork(labelsByNode[node]) < r.network) {
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
		if places[r.job] != nil {
			fmt.Fprintf(&b, "at=%s;", places[r.job][node])
		}
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
			if placed[r.job] == 0 && places[r.job] != nil {
				at[r.job] = places[r.job][c.node]
			}
			placed[r.job]++
			plan[i] = rankAssignment{job: r.job, rank: r.rank, shape: r.shape, node: c.node, key: c.key}
			if place(i + 1) {
				return true
			}
			placed[r.job]--
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

// sameShape is how many of shapes are shape: the nodes of a job that hold slots of one shape.
func sameShape(shapes []domain.NodeShape, shape domain.NodeShape) int {
	count := 0
	for _, other := range shapes {
		if other.AcceleratorCount == shape.AcceleratorCount && maps.Equal(other.Resources, shape.Resources) {
			count++
		}
	}
	return count
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

// qualifyingNodes is the sorted set of nodes from capacity that carry exp's required node labels
// and its image's architecture.
// The selector is the effective job's, which for a job admitted to a cluster also names its image's
// architecture. The slots an admitted job is pinned to narrow each rank further (planPlacement).
func qualifyingNodes(capacity map[string]map[string]int64, labelsByNode map[string]map[string]string, exp *domain.Experiment) []string {
	nodes := make([]string, 0, len(capacity))
	for node := range capacity {
		if LabelsMatch(labelsByNode[node], exp.EffectiveJob().NodeSelector) && exp.OnImageArchitecture(labelsByNode[node]) {
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
