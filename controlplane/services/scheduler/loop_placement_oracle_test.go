package scheduler

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// planPlacement is a heuristic search — hardest-rank-first, best-fit node, equivalent-state
// pruning, bounded budget — and each of those four can silently drop a valid assignment. Hand-
// written cases only ever catch the ones someone already thought of; comparing against an
// exhaustive oracle over randomized layouts is what caught the pruning bug none of them did.
//
// Soundness failing means the scheduler admits jobs the cluster cannot run. Completeness failing
// means it leaves capacity idle and sends jobs to preemption that had a home all along.

// --- the oracle ---------------------------------------------------------------------------

// oracleRank is one rank stripped to what placement actually depends on.
type oracleRank struct {
	job              int
	acceleratorCount int64
	resources        map[string]int64
	distinct         bool
	candidates       []string
}

// oracleFits is an exhaustive, deliberately naive feasibility check: try every node for every
// rank, backtracking on failure, with no ordering heuristic and no pruning whatsoever. It is
// exponential and only usable on the tiny layouts this file generates — which is the point. It
// shares no code with planPlacement, so agreement between the two is real evidence.
func oracleFits(nodeAccel, nodeRes map[string]map[string]int64, key string, ranks []oracleRank) bool {
	accel := cloneNodeCapacity(nodeAccel)
	res := cloneNodeCapacity(nodeRes)
	used := map[[2]string]bool{} // (job, node) pairs already taken, for the distinct-host rule

	var try func(i int) bool
	try = func(i int) bool {
		if i == len(ranks) {
			return true
		}
		r := ranks[i]
		for _, node := range r.candidates {
			jobNode := [2]string{fmt.Sprint(r.job), node}
			if r.distinct && used[jobNode] {
				continue
			}
			nodeKey := foldMatchingKey(accel[node], key)
			if r.acceleratorCount > 0 && accel[node][nodeKey] < r.acceleratorCount {
				continue
			}
			room := true
			for dimension, need := range r.resources {
				if res[node][dimension] < need {
					room = false
					break
				}
			}
			if !room {
				continue
			}
			if r.acceleratorCount > 0 {
				accel[node][nodeKey] -= r.acceleratorCount
			}
			for dimension, need := range r.resources {
				res[node][dimension] -= need
			}
			used[jobNode] = true

			if try(i + 1) {
				return true
			}

			used[jobNode] = false
			for dimension, need := range r.resources {
				res[node][dimension] += need
			}
			if r.acceleratorCount > 0 {
				accel[node][nodeKey] += r.acceleratorCount
			}
		}
		return false
	}
	return try(0)
}

// oracleRanks lowers jobs into oracleRank form using the same candidate-set rule planPlacement
// uses (accelerator-bearing ranks are confined to nodes that reported accelerators; others may go
// anywhere the cluster reported resources for). This much has to agree or the two would be
// answering different questions; the search itself is what is being compared.
func oracleRanks(nodeAccel, nodeRes map[string]map[string]int64, labelsByNode map[string]map[string]string, jobs []*domain.Experiment) []oracleRank {
	var out []oracleRank
	for i, exp := range jobs {
		acceleratorNodes := qualifyingNodes(nodeAccel, labelsByNode, exp)
		plainNodes := qualifyingNodes(nodeRes, labelsByNode, exp)
		distinct := requiresDistinctHosts(exp)
		for _, shape := range exp.NodeShapes() {
			candidates := plainNodes
			if shape.AcceleratorCount > 0 {
				candidates = acceleratorNodes
			}
			out = append(out, oracleRank{
				job:              i,
				acceleratorCount: shape.AcceleratorCount,
				resources:        shape.Resources,
				distinct:         distinct,
				candidates:       candidates,
			})
		}
	}
	return out
}

// --- plan validation ----------------------------------------------------------------------

// validatePlan re-executes a returned plan against a fresh copy of the capacity and reports the
// first way it is illegal. A plan that does not survive this is a job the scheduler would admit
// onto a node that cannot hold it.
// distinctByJob[i] says whether job i actually carries the distinct-host requirement — an
// accelerator-free job does not (see requiresDistinctHosts), and stacking its ranks on one node is
// legal, not a violation.
func validatePlan(nodeAccel, nodeRes map[string]map[string]int64, plan []rankAssignment, wantRanks int, distinctByJob []bool) error {
	if len(plan) != wantRanks {
		return fmt.Errorf("plan has %d assignments, job has %d ranks", len(plan), wantRanks)
	}
	accel := cloneNodeCapacity(nodeAccel)
	res := cloneNodeCapacity(nodeRes)
	used := map[[2]string]bool{}
	for _, p := range plan {
		if p.node == "" {
			return fmt.Errorf("rank of job %d was assigned no node", p.job)
		}
		jobNode := [2]string{fmt.Sprint(p.job), p.node}
		if distinctByJob[p.job] && used[jobNode] {
			return fmt.Errorf("job %d has two ranks on %s despite the distinct-host rule", p.job, p.node)
		}
		used[jobNode] = true
		if p.shape.AcceleratorCount > 0 {
			if accel[p.node][p.key] < p.shape.AcceleratorCount {
				return fmt.Errorf("node %s oversubscribed on %s: %d left, rank wants %d",
					p.node, p.key, accel[p.node][p.key], p.shape.AcceleratorCount)
			}
			accel[p.node][p.key] -= p.shape.AcceleratorCount
		}
		for dimension, need := range p.shape.Resources {
			if res[p.node][dimension] < need {
				return fmt.Errorf("node %s oversubscribed on %s: %d left, rank wants %d",
					p.node, dimension, res[p.node][dimension], need)
			}
			res[p.node][dimension] -= need
		}
	}
	return nil
}

// TestPlanPlacementDoesNotPruneDimensionSwappedNodes is the minimal reproduction of the bug the
// oracle comparison above exists to catch, kept as a named test so a regression names itself
// instead of surfacing as "case 1372 disagreed".
//
// node-a and node-b are dimension-swapped twins: 2Gi of memory and no disk versus no memory and
// 2Gi of disk. Placing the accelerator-only "big" rank leaves each with the same *total* free
// bytes, so pruning that keyed on that total treated them as one node, kept only node-a, and then
// had nowhere to put the "mem" rank — which needs the 2Gi of memory that node-a just gave away.
// The valid plan is big→node-b, mem→node-a.
func TestPlanPlacementDoesNotPruneDimensionSwappedNodes(t *testing.T) {
	exp := groupedExperiment(
		domain.JobGroup{Name: "big", Replicas: 1, AcceleratorCount: 2},
		domain.JobGroup{Name: "mem", Replicas: 1, AcceleratorCount: 1, Memory: "2Gi"},
	)
	nodeAccel := map[string]map[string]int64{"node-a": {h100: 2}, "node-b": {h100: 2}}
	nodeRes := map[string]map[string]int64{
		"node-a": {domain.NodeResourceMemoryBytes: 2 << 30, domain.NodeResourceStorageBytes: 0},
		"node-b": {domain.NodeResourceMemoryBytes: 0, domain.NodeResourceStorageBytes: 2 << 30},
	}

	// The oracle is the authority on whether a plan exists at all; assert it first so a failure
	// below is unambiguously the search's fault and not a mis-stated fixture.
	if !oracleFits(nodeAccel, nodeRes, h100, oracleRanks(nodeAccel, nodeRes, nil, []*domain.Experiment{exp})) {
		t.Fatal("fixture is wrong: big→node-b, mem→node-a should be a valid assignment")
	}
	plan, ok := planPlacement(cloneNodeCapacity(nodeAccel), cloneNodeCapacity(nodeRes), nil, []*domain.Experiment{exp})
	if !ok {
		t.Fatal("planPlacement rejected a placeable job: two nodes rich in different byte-valued dimensions were treated as interchangeable")
	}
	if err := validatePlan(nodeAccel, nodeRes, plan, 2, []bool{requiresDistinctHosts(exp)}); err != nil {
		t.Fatalf("plan is illegal: %v", err)
	}
}

// --- the generated case -------------------------------------------------------------------

type generatedCase struct {
	nodeAccel map[string]map[string]int64
	nodeRes   map[string]map[string]int64
	exp       *domain.Experiment
}

func (c generatedCase) describe() string {
	nodes := make([]string, 0, len(c.nodeRes))
	for node := range c.nodeRes {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	out := "cluster:\n"
	for _, node := range nodes {
		out += fmt.Sprintf("  %s accel=%d cpu=%d mem=%dGi disk=%dGi\n", node,
			c.nodeAccel[node][h100],
			c.nodeRes[node][domain.NodeResourceCPUMillicores],
			c.nodeRes[node][domain.NodeResourceMemoryBytes]>>30,
			c.nodeRes[node][domain.NodeResourceStorageBytes]>>30)
	}
	out += "job groups:\n"
	for _, g := range c.exp.Job.Groups {
		out += fmt.Sprintf("  %s replicas=%d accel=%d cpu=%q mem=%q disk=%q\n", g.Name, g.Replicas, g.AcceleratorCount, g.CPU, g.Memory, g.Storage)
	}
	return out
}

// generateCase builds a small random cluster and a small random grouped job. Sizes are kept tiny
// on purpose: the oracle is exponential, and small dense layouts are where placement conflicts
// actually arise. Values are drawn from a coarse grid so collisions — two nodes that differ in
// which dimension they are rich in, jobs that exactly consume a node — happen often rather than
// almost never.
func generateCase(rnd *rand.Rand) generatedCase {
	nodeCount := 2 + rnd.Intn(3) // 2..4
	nodeAccel := map[string]map[string]int64{}
	nodeRes := map[string]map[string]int64{}
	for i := 0; i < nodeCount; i++ {
		node := fmt.Sprintf("node-%c", 'a'+i)
		nodeAccel[node] = map[string]int64{h100: int64(rnd.Intn(5))} // 0..4 devices
		// Memory and storage are both byte-valued and of the same order of magnitude. That is the
		// point of including both: any bookkeeping that collapses dimensions into a single scalar
		// will confuse a memory-rich node with a storage-rich one here, whereas CPU (millicores)
		// is numerically so far from bytes that it would mask the problem on its own.
		nodeRes[node] = map[string]int64{
			domain.NodeResourceCPUMillicores: int64(rnd.Intn(4)) * 1000,
			domain.NodeResourceMemoryBytes:   int64(rnd.Intn(3)) * 1024 * 1024 * 1024,
			domain.NodeResourceStorageBytes:  int64(rnd.Intn(3)) * 1024 * 1024 * 1024,
		}
	}

	// Half the clusters get a dimension-swapped twin pair: two nodes with identical accelerators
	// whose memory and storage are exchanged. Uniform random values almost never produce this —
	// six thousand of them did not — yet it is precisely the layout that breaks any pruning which
	// compares nodes by an aggregate rather than dimension by dimension, and a real cluster of
	// mixed hardware generations produces it routinely. Biasing the generator toward the
	// adversarial shape is what makes this test capable of finding the bug rather than merely
	// re-confirming the fix.
	if nodeCount >= 2 && rnd.Intn(2) == 0 {
		a, b := "node-a", "node-b"
		nodeAccel[b][h100] = nodeAccel[a][h100]
		nodeRes[b][domain.NodeResourceCPUMillicores] = nodeRes[a][domain.NodeResourceCPUMillicores]
		nodeRes[b][domain.NodeResourceMemoryBytes] = nodeRes[a][domain.NodeResourceStorageBytes]
		nodeRes[b][domain.NodeResourceStorageBytes] = nodeRes[a][domain.NodeResourceMemoryBytes]
	}

	groupCount := 1 + rnd.Intn(3) // 1..3 groups
	groups := make([]domain.JobGroup, 0, groupCount)
	for i := 0; i < groupCount; i++ {
		// A group that asks for no fungible resources at all is common in practice (an
		// accelerator-bound rank) and is the one that provokes the twin conflict: it fits either
		// twin equally, so a wrong choice is only discovered by a later, pickier rank.
		cpu, memory, storage := "0m", "0Gi", "0Gi"
		if rnd.Intn(2) == 0 {
			cpu = fmt.Sprintf("%dm", rnd.Intn(3)*1000)
			memory = fmt.Sprintf("%dGi", rnd.Intn(3))
			storage = fmt.Sprintf("%dGi", rnd.Intn(3))
		}
		groups = append(groups, domain.JobGroup{
			Name:             fmt.Sprintf("g%d", i),
			Replicas:         1 + rnd.Intn(2), // 1..2 replicas
			AcceleratorCount: rnd.Intn(3),     // 0..2 per replica
			CPU:              cpu,
			Memory:           memory,
			Storage:          storage,
		})
	}
	return generatedCase{nodeAccel: nodeAccel, nodeRes: nodeRes, exp: groupedExperiment(groups...)}
}

// TestPlanPlacementMatchesBruteForceOracle is the core differential test: thousands of randomized
// layouts, each answered by both the real search and the exhaustive oracle, with any disagreement
// reported as a fully reproducible case.
func TestPlanPlacementMatchesBruteForceOracle(t *testing.T) {
	const iterations = 4000
	// Fixed seed: a failure a developer cannot reproduce is a failure that gets re-run until it
	// passes. Changing the seed explores different layouts and is a legitimate way to hunt for
	// more, but the committed value must stay put so CI is deterministic.
	rnd := rand.New(rand.NewSource(20260825))

	var falseNegatives, unsound int
	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)
		jobs := []*domain.Experiment{c.exp}

		plan, ok := planPlacement(cloneNodeCapacity(c.nodeAccel), cloneNodeCapacity(c.nodeRes), nil, jobs)
		want := oracleFits(c.nodeAccel, c.nodeRes, h100, oracleRanks(c.nodeAccel, c.nodeRes, nil, jobs))

		switch {
		case ok && !want:
			unsound++
			t.Errorf("case %d: planPlacement accepted a job the oracle proved cannot be placed\n%s", i, c.describe())
		case !ok && want:
			falseNegatives++
			if falseNegatives <= 3 { // a systemic bug fires on hundreds of cases; three is enough to debug
				t.Errorf("case %d: planPlacement rejected a job that has a valid assignment\n%s", i, c.describe())
			}
		}
		if ok {
			if err := validatePlan(c.nodeAccel, c.nodeRes, plan, len(c.exp.NodeShapes()), []bool{requiresDistinctHosts(c.exp)}); err != nil {
				unsound++
				t.Errorf("case %d: planPlacement returned an illegal plan: %v\n%s", i, err, c.describe())
			}
		}
	}
	if falseNegatives > 0 || unsound > 0 {
		t.Fatalf("%d/%d cases disagreed with the oracle (%d false negatives, %d unsound)",
			falseNegatives+unsound, iterations, falseNegatives, unsound)
	}
}

// TestPlanPlacementMultiJobMatchesOracle runs the same comparison with several jobs planned
// together, which is the shape desiredPlacementFits uses when it re-plans every already-desired
// job alongside the candidate. Ranks of different jobs may share a node, so the distinct-host
// bookkeeping has to be per job — getting that wrong is invisible in the single-job case above.
func TestPlanPlacementMultiJobMatchesOracle(t *testing.T) {
	const iterations = 2000
	rnd := rand.New(rand.NewSource(20260826))

	var disagreements int
	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)
		second := generateCase(rnd).exp
		jobs := []*domain.Experiment{c.exp, second}

		plan, ok := planPlacement(cloneNodeCapacity(c.nodeAccel), cloneNodeCapacity(c.nodeRes), nil, jobs)
		want := oracleFits(c.nodeAccel, c.nodeRes, h100, oracleRanks(c.nodeAccel, c.nodeRes, nil, jobs))

		if ok != want {
			disagreements++
			if disagreements <= 3 {
				t.Errorf("case %d: planPlacement=%v oracle=%v for two jobs planned together\n%s\nsecond job groups: %+v",
					i, ok, want, c.describe(), second.Job.Groups)
			}
		}
		if ok {
			totalRanks := len(c.exp.NodeShapes()) + len(second.NodeShapes())
			distinct := []bool{requiresDistinctHosts(c.exp), requiresDistinctHosts(second)}
			if err := validatePlan(c.nodeAccel, c.nodeRes, plan, totalRanks, distinct); err != nil {
				disagreements++
				t.Errorf("case %d: illegal multi-job plan: %v\n%s", i, err, c.describe())
			}
		}
	}
	if disagreements > 0 {
		t.Fatalf("%d/%d multi-job cases disagreed with the oracle", disagreements, iterations)
	}
}

// TestReservePlacementLeavesCapacityConsistent checks the bookkeeping half of the contract that
// the oracle cannot see: reservePlacement mutates the caller's maps, and every caller after it
// (the admission loop's running total, the shortfall calculation, the next job's fit check)
// trusts those maps. A success must subtract exactly the job's footprint and nothing else; a
// failure must subtract nothing at all.
func TestReservePlacementLeavesCapacityConsistent(t *testing.T) {
	const iterations = 2000
	rnd := rand.New(rand.NewSource(20260827))

	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)
		accel, res := cloneNodeCapacity(c.nodeAccel), cloneNodeCapacity(c.nodeRes)

		ok := reservePlacement(accel, res, nil, c.exp)

		if !ok {
			// Failure must be perfectly non-destructive: a rejected job that quietly ate capacity
			// would shrink the cluster a little on every tick it fails to be admitted.
			if diff := capacityDiff(c.nodeAccel, accel); diff != "" {
				t.Fatalf("case %d: failed reservePlacement mutated accelerator capacity: %s\n%s", i, diff, c.describe())
			}
			if diff := capacityDiff(c.nodeRes, res); diff != "" {
				t.Fatalf("case %d: failed reservePlacement mutated node resources: %s\n%s", i, diff, c.describe())
			}
			continue
		}

		// Success must subtract exactly the job's total footprint, cluster-wide.
		wantAccel, wantCPU, wantMem := int64(0), int64(0), int64(0)
		for _, shape := range c.exp.NodeShapes() {
			wantAccel += shape.AcceleratorCount
			wantCPU += shape.Resources[domain.NodeResourceCPUMillicores]
			wantMem += shape.Resources[domain.NodeResourceMemoryBytes]
		}
		gotAccel := totalOf(c.nodeAccel, h100) - totalOf(accel, h100)
		gotCPU := totalOf(c.nodeRes, domain.NodeResourceCPUMillicores) - totalOf(res, domain.NodeResourceCPUMillicores)
		gotMem := totalOf(c.nodeRes, domain.NodeResourceMemoryBytes) - totalOf(res, domain.NodeResourceMemoryBytes)
		if gotAccel != wantAccel || gotCPU != wantCPU || gotMem != wantMem {
			t.Fatalf("case %d: reservePlacement consumed accel=%d cpu=%d mem=%d, job needs accel=%d cpu=%d mem=%d\n%s",
				i, gotAccel, gotCPU, gotMem, wantAccel, wantCPU, wantMem, c.describe())
		}
		// And it must never drive a node negative — a negative free count reads as "room" to
		// nothing, but it does corrupt every cluster-wide total derived from these maps.
		for node, dims := range res {
			for dimension, v := range dims {
				if v < 0 {
					t.Fatalf("case %d: node %s went negative on %s (%d)\n%s", i, node, dimension, v, c.describe())
				}
			}
		}
		for node, dims := range accel {
			for dimension, v := range dims {
				if v < 0 {
					t.Fatalf("case %d: node %s went negative on %s (%d)\n%s", i, node, dimension, v, c.describe())
				}
			}
		}
	}
}

func totalOf(capacity map[string]map[string]int64, dimension string) int64 {
	var total int64
	for _, dims := range capacity {
		total += dims[dimension]
	}
	return total
}

// capacityDiff returns a description of the first difference between two capacity maps, or "" if
// they are identical.
func capacityDiff(want, got map[string]map[string]int64) string {
	nodes := make([]string, 0, len(want))
	for node := range want {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		for dimension, v := range want[node] {
			if got[node][dimension] != v {
				return fmt.Sprintf("%s/%s was %d, now %d", node, dimension, v, got[node][dimension])
			}
		}
	}
	return ""
}
