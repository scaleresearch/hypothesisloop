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

const testAccelerator = domain.AcceleratorType("example.com/product=test-accelerator")

func acceleratorExperiment(acceleratorCount int, cpu, storage string) *domain.Experiment {
	return &domain.Experiment{
		ID:               "exp-share",
		AcceleratorType:  testAccelerator,
		AcceleratorCount: acceleratorCount,
		Job: domain.JobSpec{Image: "registry.test/job:1",
			CPU: cpu, Memory: "1Gi", Storage: storage,
			AcceleratorCount: acceleratorCount, AcceleratorType: testAccelerator,
		},
	}
}

func shareNode(cpuMillicores, storageBytes int64) map[string]int64 {
	return map[string]int64{domain.NodeResourceCPUMillicores: cpuMillicores, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: storageBytes}
}

// A job runs with what it stated, and each quantity is held to the MINIMUM per-accelerator share
// across the nodes of the selected cluster that report its accelerator: never the average, never
// one of them, and never a node of another cluster.
func TestResolveClusterLocalResourcesHoldsAStatedQuantityToTheSmallestShare(t *testing.T) {
	one := map[string]int64{string(testAccelerator): 1}
	for name, tc := range map[string]struct {
		cpu, storage string
		totals       map[string]map[string]int64
		avail        map[string]map[string]int64
		misfit       string
	}{
		"at the smaller of two shares": {
			cpu: "2", storage: "200",
			totals: map[string]map[string]int64{"roomy": shareNode(8000, 800), "tight": shareNode(2000, 200)},
			avail:  map[string]map[string]int64{"roomy": one, "tight": one},
		},
		"within the only node's share": {
			cpu: "5", storage: "100",
			totals: map[string]map[string]int64{"roomy": shareNode(8000, 800)},
			avail:  map[string]map[string]int64{"roomy": one},
		},
		"at the only node's share": {
			cpu: "4", storage: "400",
			totals: map[string]map[string]int64{"only": shareNode(4000, 400)},
			avail:  map[string]map[string]int64{"only": one},
		},
		"cpu above the smaller share": {
			cpu: "9", storage: "100",
			totals: map[string]map[string]int64{"tight-a": shareNode(2000, 800), "tight-b": shareNode(4000, 800)},
			avail:  map[string]map[string]int64{"tight-a": one, "tight-b": one},
			misfit: `exceeds_accelerator_share: cpu 9 is above the 2 that 1 accelerator(s) get on cluster cluster-a (each node's cpu split evenly across its installed accelerators); request at most 2`,
		},
		"storage above the smaller share": {
			cpu: "2", storage: "300",
			totals: map[string]map[string]int64{"roomy": shareNode(8000, 800), "tight": shareNode(2000, 200)},
			avail:  map[string]map[string]int64{"roomy": one, "tight": one},
			misfit: `exceeds_accelerator_share: storage 300 is above the 200 that 1 accelerator(s) get on cluster cluster-a (each node's storage split evenly across its installed accelerators); request at most 200`,
		},
		// No node reports the accelerator, so nothing bounds the job here: the capacity check
		// decides, and a share is never computed against zero installed.
		"no node reports the accelerator": {
			cpu: "64", storage: "100",
			totals: map[string]map[string]int64{"node-a": shareNode(8000, 800)},
			avail:  map[string]map[string]int64{"node-a": {string(testAccelerator): 0}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			loop := resolveTestLoop()
			exp := acceleratorExperiment(1, tc.cpu, tc.storage)
			resolved, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", tc.totals, tc.avail, nil, nil)
			if err != nil {
				t.Fatalf("resolveClusterLocalResources: %v", err)
			}
			if misfit != tc.misfit {
				t.Fatalf("misfit = %q\nwant      %q", misfit, tc.misfit)
			}
			if tc.misfit != "" {
				if resolved != nil {
					t.Fatalf("resolved = %+v beside a misfit", resolved)
				}
				return
			}
			if resolved.CPU != tc.cpu || resolved.Memory != "1Gi" || resolved.Storage != tc.storage {
				t.Errorf("resolved = cpu %q, memory %q, storage %q, want the job as stated", resolved.CPU, resolved.Memory, resolved.Storage)
			}
		})
	}
}

// A quantity that is none is the job's own fault and never read as fitting.
func TestResolveClusterLocalResourcesRefusesAQuantityItCannotRead(t *testing.T) {
	loop := resolveTestLoop()
	for _, cpu := range []string{"", "max"} {
		exp := acceleratorExperiment(1, cpu, "100")
		totals := map[string]map[string]int64{"only": shareNode(4000, 400)}
		avail := map[string]map[string]int64{"only": {string(testAccelerator): 1}}
		if resolved, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", totals, avail, nil, nil); err == nil {
			t.Errorf("cpu %q: resolved %+v, misfit %q, want an error", cpu, resolved, misfit)
		}
	}
}

// A grouped job's groups have their own per-node accelerator count (Replicas is nodes, not a
// multiplier on AcceleratorCount) and each is held to the share of THAT count, never the job's
// total.
func TestResolveClusterLocalResourcesGroupedJobUsesPerGroupAcceleratorCount(t *testing.T) {
	// One node of 9000 millicores and 3 accelerators: 3000m each, so 6 cores for the learner's
	// two and 3 for the actor's one.
	totals := map[string]map[string]int64{"node-a": {domain.NodeResourceCPUMillicores: 9000, domain.NodeResourceMemoryBytes: 10 << 30, domain.NodeResourceStorageBytes: 10 << 30}}
	avail := map[string]map[string]int64{"node-a": {string(testAccelerator): 3}}
	for name, tc := range map[string]struct {
		learner, actor string
		fits           bool
	}{
		"each at its own share":        {"6", "3", true},
		"the actor at the job's share": {"6", "4", false},
		"the learner above its share":  {"7", "3", false},
	} {
		loop := resolveTestLoop()
		exp := &domain.Experiment{
			ID: "exp-grouped", AcceleratorType: testAccelerator, AcceleratorCount: 3,
			Job: domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: testAccelerator,
				Groups: []domain.JobGroup{
					{Name: "learner", Replicas: 1, CPU: tc.learner, Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 2},
					{Name: "actor", Replicas: 1, CPU: tc.actor, Memory: "1Gi", Storage: "1Gi", AcceleratorCount: 1},
				},
			},
		}
		resolved, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", totals, avail, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (misfit == "") != tc.fits || (tc.fits && (resolved.Groups[0].CPU != tc.learner || resolved.Groups[1].CPU != tc.actor)) {
			t.Errorf("%s: resolved %+v, misfit %q, want fits = %v", name, resolved, misfit, tc.fits)
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
		Job: domain.JobSpec{Image: "registry.test/job:1",
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
		Job: domain.JobSpec{Image: "registry.test/job:1",
			AcceleratorType:  flavor,
			AcceleratorCount: 5, // 5 accelerators, but on ONE node: Nodes() == 1
			NumNodes:         1,
		},
	}

	loop := &Loop{store: &resolveTestStore{running: []*domain.Experiment{multiNode, singleNodeHighCount}}, logger: zap.NewNop(), observed: observedOnNode([]string{"exp-multi", "exp-single-high-count"}, "node-a")}
	cache := newResolutionCache(loop)
	nodeAvail := map[string]map[string]int64{"node-a": {string(flavor): 0}}

	installed, err := cache.installedAcceleratorsByNode(context.Background(), "cluster-a", flavor, nodeAvail, nil)
	if err != nil {
		t.Fatalf("installedAcceleratorsByNode: %v", err)
	}
	// Only the single-node job's 5 accelerators are attributed; the multi-node job's 4 are
	// excluded entirely, never partially or wholly charged to node-a.
	if installed["node-a"] != 5 {
		t.Errorf("installed[node-a] = %d, want 5 (only the single-node claim, not the excluded multi-node one)", installed["node-a"])
	}
}

// The regression this whole cache exists to prevent: SUBMITTED claims (not just
// RUNNING) must be counted toward a node's installed total, because they have already reduced
// nodeAvail's free count without yet reaching RUNNING. Undercounting them corrupts every
// FairShare computed against the reconstructed total. Named explicitly so this can never quietly
// regress back to a RUNNING-only view.
func TestInstalledAcceleratorsByNodeCountsSubmittedNotJustRunning(t *testing.T) {
	const flavor = domain.AcceleratorType("example.com/product=test-accelerator")
	claimByStatus := func(id string, status domain.ExperimentStatus, count int) *domain.Experiment {
		return &domain.Experiment{
			ID:               id,
			ClusterName:      "cluster-a",
			Status:           status,
			AcceleratorType:  flavor,
			AcceleratorCount: count,
			CreatedAt:        pastTime(),
			Job:              domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: flavor, AcceleratorCount: count, NumNodes: 1},
		}
	}
	claimed := []*domain.Experiment{
		claimByStatus("exp-submitted", domain.StatusSubmitted, 1),
		claimByStatus("exp-running", domain.StatusRunning, 3),
	}

	loop := &Loop{store: &resolveTestStore{running: claimed}, logger: zap.NewNop(), observed: observedOnNode([]string{"exp-submitted", "exp-running"}, "node-a")}
	cache := newResolutionCache(loop)
	nodeAvail := map[string]map[string]int64{"node-a": {string(flavor): 0}}

	installed, err := cache.installedAcceleratorsByNode(context.Background(), "cluster-a", flavor, nodeAvail, nil)
	if err != nil {
		t.Fatalf("installedAcceleratorsByNode: %v", err)
	}
	if want := int64(1 + 3); installed["node-a"] != want {
		t.Errorf("installed[node-a] = %d, want %d (SUBMITTED and RUNNING both counted)", installed["node-a"], want)
	}
}

func pastTime() time.Time { return time.Now().Add(-time.Hour) }

type resolvedCPUStore struct {
	holdStore
	cpu map[string]string
}

func (s *resolvedCPUStore) ClaimSubmitted(_ context.Context, id, _ string, resolved *domain.JobSpec, _ func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	if s.cpu == nil {
		s.cpu = map[string]string{}
	}
	s.cpu[id] = resolved.CPU
	return true, nil
}

// Free accelerators shrink as a tick admits, and jobs admitted this tick are in no claimed list
// yet, so a share measured against live free capacity grows with every admission: the second
// job, asking for more than a third of a node of three devices, would be let through.
func TestSharesDoNotGrowAsATickAdmits(t *testing.T) {
	first := holdJob("first", domain.CapacityBurst, h100, 1, "4")
	second := holdJob("second", domain.CapacityBurst, h100, 1, "6")
	second.AgentID = "agent-2"
	first.ClusterName, second.ClusterName = "cluster-a", "cluster-a"
	store := &resolvedCPUStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{first, second}}}}
	l := holdLoop(store, holdWorkload{cpuCores: 16}, noopEvictor{})

	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
	if _, admitted := store.cpu["second"]; store.cpu["first"] != "4" || admitted {
		t.Fatalf("admitted with cpu %v, want the first alone: 6 of 16 cores is above the share of one device of three", store.cpu)
	}
}

func (*resolvedCPUStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }
