package scheduler

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// resolveTestStore is the minimal LoopStore a resolveClusterLocalResources test needs: no running
// experiments, so installedAcceleratorsByNode never has to attribute anything via the metrics
// store and every node's "installed" count is exactly its reported free count.
type resolveTestStore struct {
	LoopStore
	running []*domain.Experiment
}

func (s *resolveTestStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}

func (s *resolveTestStore) ListCapacityClaimedExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}

func resolveTestLoop(running ...*domain.Experiment) *Loop {
	return &Loop{store: &resolveTestStore{running: running}, logger: zap.NewNop(), observed: observedOnNode(nil, "node-a")}
}

func maxAcceleratorExperiment(acceleratorCount int) *domain.Experiment {
	return &domain.Experiment{
		ID:               "exp-max",
		AcceleratorType:  "example.com/product=test-accelerator",
		AcceleratorCount: acceleratorCount,
		Job: domain.JobSpec{
			CPU:              domain.MaxResourceSentinel,
			Memory:           "1Gi", // literal alongside a "max" field, per JobSpec.CPU's doc comment
			Storage:          domain.MaxResourceSentinel,
			AcceleratorCount: acceleratorCount,
			AcceleratorType:  "example.com/product=test-accelerator",
		},
	}
}

// Two differently-shaped nodes in the selected cluster, both eligible: a "max" resolution must
// use the MINIMUM fair share across them, never the average and never just one of them.
func TestResolveClusterLocalResourcesMaxUsesMinimumAcrossEligibleNodes(t *testing.T) {
	loop := resolveTestLoop()
	exp := maxAcceleratorExperiment(1)

	nodeResourcesTotal := map[string]map[string]int64{
		// 8000 millicores / 1 installed accelerator = 8000m per-accelerator share.
		"roomy": {domain.NodeResourceCPUMillicores: 8000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 800},
		// 2000 millicores / 1 installed accelerator = 2000m per-accelerator share — the minimum.
		"tight": {domain.NodeResourceCPUMillicores: 2000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 200},
	}
	nodeAvail := map[string]map[string]int64{
		"roomy": {"example.com/product=test-accelerator": 1},
		"tight": {"example.com/product=test-accelerator": 1},
	}

	resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if !fits {
		t.Fatal("fits = false, want true")
	}
	if resolved.CPU != "2" {
		t.Errorf("resolved.CPU = %q, want \"2\" (2000m, the minimum across both nodes, not the average 5000m or the roomy node's 8000m)", resolved.CPU)
	}
	if resolved.Storage != "200" {
		t.Errorf("resolved.Storage = %q, want \"200\" (the minimum)", resolved.Storage)
	}
	if resolved.Memory != "1Gi" {
		t.Errorf("resolved.Memory = %q, want unchanged \"1Gi\" (not a \"max\" field)", resolved.Memory)
	}
}

// A grouped job's groups have their own per-node accelerator count (Replicas is nodes, not a
// multiplier on AcceleratorCount) and must each resolve against THAT count, never the job's total.
func TestResolveClusterLocalResourcesGroupedJobUsesPerGroupAcceleratorCount(t *testing.T) {
	loop := resolveTestLoop()
	const flavor = domain.AcceleratorType("example.com/product=test-accelerator")
	exp := &domain.Experiment{
		ID:               "exp-grouped",
		AcceleratorType:  flavor,
		AcceleratorCount: 3, // total: 2 (learner) + 1 (actor)
		Job: domain.JobSpec{
			AcceleratorType: flavor,
			Groups: []domain.JobGroup{
				{Name: "learner", Replicas: 1, CPU: domain.MaxResourceSentinel, Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 2},
				{Name: "actor", Replicas: 1, CPU: domain.MaxResourceSentinel, Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
			},
		},
	}
	// One node, 9000 total millicores, 3 accelerators installed (matches AcceleratorCount total on
	// this single node, e.g. an 3-accelerator host running both groups).
	nodeResourcesTotal := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 9000, domain.NodeResourceMemoryBytes: 10 << 30, domain.NodeResourceStorageBytes: 10 << 30},
	}
	nodeAvail := map[string]map[string]int64{
		"node-a": {string(flavor): 3},
	}

	resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if !fits {
		t.Fatal("fits = false, want true")
	}
	// Per-accelerator share = 9000/3 = 3000m. Learner (2 accelerators) gets 6000m; actor (1) gets
	// 3000m — never the job's TotalAccelerators (3) applied to both groups alike.
	if resolved.Groups[0].CPU != "6" {
		t.Errorf("learner CPU = %q, want \"6\" (2 accelerators x 3000m share)", resolved.Groups[0].CPU)
	}
	if resolved.Groups[1].CPU != "3" {
		t.Errorf("actor CPU = %q, want \"3\" (1 accelerator x 3000m share)", resolved.Groups[1].CPU)
	}
}

// An explicit (non-"max") number is rejected only when it exceeds every eligible node's fair
// share IN THE SELECTED CLUSTER — a number that fits the selected cluster must be accepted even
// though a smaller number is all some other, unselected node/cluster could ever have offered.
func TestResolveClusterLocalResourcesExplicitNumberJudgedOnlyAgainstSelectedCluster(t *testing.T) {
	loop := resolveTestLoop()
	exp := maxAcceleratorExperiment(1)
	exp.Job.CPU = "5" // 5 cores = 5000m, explicit rather than "max"
	exp.Job.Storage = "100"

	// The selected cluster's only eligible node offers an 8000m/accelerator share — comfortably
	// above the 5000m explicit request.
	nodeResourcesTotal := map[string]map[string]int64{
		"roomy": {domain.NodeResourceCPUMillicores: 8000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 800},
	}
	nodeAvail := map[string]map[string]int64{
		"roomy": {"example.com/product=test-accelerator": 1},
	}

	_, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if !fits {
		t.Fatal("fits = false, want true: 5000m is within this cluster's 8000m/accelerator share, regardless of what any other cluster might offer")
	}
}

// The same explicit number must be rejected — as "doesn't fit this cluster/tick", not a hard
// error — once it exceeds every eligible node's fair share in the selected cluster.
func TestResolveClusterLocalResourcesExplicitNumberExceedingEveryEligibleNodeDoesNotFit(t *testing.T) {
	loop := resolveTestLoop()
	exp := maxAcceleratorExperiment(1)
	exp.Job.CPU = "9" // 9 cores = 9000m, exceeds every eligible node's share below
	exp.Job.Storage = "100"

	nodeResourcesTotal := map[string]map[string]int64{
		"tight-a": {domain.NodeResourceCPUMillicores: 2000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 800},
		"tight-b": {domain.NodeResourceCPUMillicores: 4000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 800},
	}
	nodeAvail := map[string]map[string]int64{
		"tight-a": {"example.com/product=test-accelerator": 1},
		"tight-b": {"example.com/product=test-accelerator": 1},
	}

	resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if fits {
		t.Fatalf("fits = true, want false: 9000m exceeds every eligible node's fair share (max 4000m) — resolved=%+v", resolved)
	}
}

// No node in the cluster currently reports the job's accelerator flavor at all (nodeAvail has an
// entry, but the flavor's free count is 0/absent): haveBound never becomes true, and this must be
// read as "not admitted this tick" — never a panic on an empty min-reduction, and never a share
// silently computed against zero installed.
func TestResolveClusterLocalResourcesNoEligibleNodeDoesNotFit(t *testing.T) {
	loop := resolveTestLoop()
	exp := maxAcceleratorExperiment(1)

	nodeResourcesTotal := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 8000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 800},
	}
	// node-a reports zero of the requested flavor — not currently eligible.
	nodeAvail := map[string]map[string]int64{
		"node-a": {"example.com/product=test-accelerator": 0},
	}

	resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if fits {
		t.Fatalf("fits = true, want false: no node is eligible to resolve against, resolved=%+v", resolved)
	}
}

// The trivial minimum-of-one: exactly one eligible node in the cluster. A future refactor of the
// "take the minimum across eligible nodes" logic must not silently break the single-node case by
// e.g. requiring at least two candidates before it starts comparing.
func TestResolveClusterLocalResourcesSingleEligibleNodeIsItsOwnMinimum(t *testing.T) {
	loop := resolveTestLoop()
	exp := maxAcceleratorExperiment(1)

	nodeResourcesTotal := map[string]map[string]int64{
		"only": {domain.NodeResourceCPUMillicores: 4000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 400},
	}
	nodeAvail := map[string]map[string]int64{
		"only": {"example.com/product=test-accelerator": 1},
	}

	resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
	if err != nil {
		t.Fatalf("resolveClusterLocalResources: %v", err)
	}
	if !fits {
		t.Fatal("fits = false, want true")
	}
	if resolved.CPU != "4" {
		t.Errorf("resolved.CPU = %q, want \"4\" (the single eligible node's own share)", resolved.CPU)
	}
	if resolved.Storage != "400" {
		t.Errorf("resolved.Storage = %q, want \"400\"", resolved.Storage)
	}
}

// Two nodes with an IDENTICAL fair share must resolve to the same answer every time, regardless
// of Go's randomized map iteration order — a tie must never be a source of flakiness.
func TestResolveClusterLocalResourcesTiedNodesResolveDeterministically(t *testing.T) {
	loop := resolveTestLoop()
	nodeResourcesTotal := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 3000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 300},
		"node-b": {domain.NodeResourceCPUMillicores: 3000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 300},
	}
	nodeAvail := map[string]map[string]int64{
		"node-a": {"example.com/product=test-accelerator": 1},
		"node-b": {"example.com/product=test-accelerator": 1},
	}

	for i := 0; i < 25; i++ {
		exp := maxAcceleratorExperiment(1)
		resolved, fits, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", nodeResourcesTotal, nodeAvail, nil)
		if err != nil {
			t.Fatalf("resolveClusterLocalResources: %v", err)
		}
		if !fits {
			t.Fatal("fits = false, want true")
		}
		if resolved.CPU != "3" {
			t.Fatalf("run %d: resolved.CPU = %q, want \"3\" every time (tie must be deterministic)", i, resolved.CPU)
		}
	}
}

// A claimed experiment spanning more than one node (grouped or NumNodes>1) must be excluded from
// the reconstructed "installed" total — metricsdb only attributes one node per experiment, so
// charging its whole AcceleratorCount to that node would overcount it. This must NOT be conflated
// with an ungrouped single-node job that simply asks for AcceleratorCount > 1 on that one node:
// Nodes() > 1 means "spans multiple nodes", never "requests more than one accelerator", and the
// latter must still be counted in full.
func TestInstalledAcceleratorsByNodeExcludesMultiNodeSpanButCountsHighSingleNodeAcceleratorCount(t *testing.T) {
	const flavor = domain.AcceleratorType("example.com/product=test-accelerator")
	multiNode := &domain.Experiment{
		ID:               "exp-multi",
		ClusterName:      "cluster-a",
		AcceleratorType:  flavor,
		AcceleratorCount: 4,
		CreatedAt:        pastTime(),
		Job: domain.JobSpec{
			AcceleratorType:  flavor,
			AcceleratorCount: 4,
			NumNodes:         2, // spans 2 nodes: Nodes() > 1
		},
	}
	singleNodeHighCount := &domain.Experiment{
		ID:               "exp-single-high-count",
		ClusterName:      "cluster-a",
		AcceleratorType:  flavor,
		AcceleratorCount: 5,
		CreatedAt:        pastTime(),
		Job: domain.JobSpec{
			AcceleratorType:  flavor,
			AcceleratorCount: 5, // 5 accelerators, but on ONE node: Nodes() == 1
			NumNodes:         1,
		},
	}

	loop := &Loop{store: &resolveTestStore{running: []*domain.Experiment{multiNode, singleNodeHighCount}}, logger: zap.NewNop(), observed: observedOnNode([]string{"exp-multi", "exp-single-high-count"}, "node-a")}
	cache := newResolutionCache(loop)
	nodeAvail := map[string]map[string]int64{"node-a": {string(flavor): 0}}

	installed, err := cache.installedAcceleratorsByNode(context.Background(), "cluster-a", flavor, nodeAvail)
	if err != nil {
		t.Fatalf("installedAcceleratorsByNode: %v", err)
	}
	// Only the single-node job's 5 accelerators are attributed; the multi-node job's 4 are
	// excluded entirely, never partially or wholly charged to node-a.
	if installed["node-a"] != 5 {
		t.Errorf("installed[node-a] = %d, want 5 (only the single-node claim, not the excluded multi-node one)", installed["node-a"])
	}
}

// The regression this whole cache exists to prevent: SUBMITTED and ADMITTED claims (not just
// RUNNING) must be counted toward a node's installed total, because they have already reduced
// nodeAvail's free count without yet reaching RUNNING. Undercounting them corrupts every
// FairShare computed against the reconstructed total. Named explicitly so this can never quietly
// regress back to a RUNNING-only view.
func TestInstalledAcceleratorsByNodeCountsSubmittedAndAdmittedNotJustRunning(t *testing.T) {
	const flavor = domain.AcceleratorType("example.com/product=test-accelerator")
	claimByStatus := func(id string, status domain.ExperimentStatus, count int) *domain.Experiment {
		return &domain.Experiment{
			ID:               id,
			ClusterName:      "cluster-a",
			Status:           status,
			AcceleratorType:  flavor,
			AcceleratorCount: count,
			CreatedAt:        pastTime(),
			Job:              domain.JobSpec{AcceleratorType: flavor, AcceleratorCount: count, NumNodes: 1},
		}
	}
	claimed := []*domain.Experiment{
		claimByStatus("exp-submitted", domain.StatusSubmitted, 1),
		claimByStatus("exp-admitted", domain.StatusAdmitted, 2),
		claimByStatus("exp-running", domain.StatusRunning, 3),
	}

	loop := &Loop{store: &resolveTestStore{running: claimed}, logger: zap.NewNop(), observed: observedOnNode([]string{"exp-submitted", "exp-admitted", "exp-running"}, "node-a")}
	cache := newResolutionCache(loop)
	nodeAvail := map[string]map[string]int64{"node-a": {string(flavor): 0}}

	installed, err := cache.installedAcceleratorsByNode(context.Background(), "cluster-a", flavor, nodeAvail)
	if err != nil {
		t.Fatalf("installedAcceleratorsByNode: %v", err)
	}
	if want := int64(1 + 2 + 3); installed["node-a"] != want {
		t.Errorf("installed[node-a] = %d, want %d (SUBMITTED + ADMITTED + RUNNING all counted)", installed["node-a"], want)
	}
}

func pastTime() time.Time { return time.Now().Add(-time.Hour) }
