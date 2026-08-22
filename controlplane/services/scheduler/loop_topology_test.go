package scheduler

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func distributedExperiment(nodes, acceleratorsPerNode int) *domain.Experiment {
	return &domain.Experiment{
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		Job: domain.JobSpec{NumNodes: nodes, AcceleratorCount: acceleratorsPerNode,
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
	}
}

func TestTopologyFitsRequiresEnoughDistinctHosts(t *testing.T) {
	exp := distributedExperiment(3, 2)
	byNode := map[string]map[string]int64{
		"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 4},
		"node-b": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 2},
	}
	if topologyFits(byNode, nil, nil, exp) {
		t.Fatal("aggregate six accelerators on two hosts must not satisfy three distinct ranks")
	}
	byNode["node-c"] = map[string]int64{"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 2}
	if !topologyFits(byNode, nil, nil, exp) {
		t.Fatal("three qualifying hosts should satisfy three distinct ranks")
	}
}

func TestTopologyFitsHonorsExplicitSpreadDisablement(t *testing.T) {
	spread := false
	exp := distributedExperiment(3, 2)
	exp.Job.Topology = &domain.TopologySpec{SpreadAcrossHosts: &spread}
	if topologyFits(map[string]map[string]int64{"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 2}}, nil, nil, exp) {
		t.Fatal("three ranks must not reuse the same two accelerators")
	}
	if !topologyFits(map[string]map[string]int64{"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 6}}, nil, nil, exp) {
		t.Fatal("explicitly disabling host spreading should allow all three ranks on one sufficiently large host")
	}
}

func TestReserveTopologyPreventsSameTickDoubleClaim(t *testing.T) {
	exp := distributedExperiment(2, 1)
	byNode := map[string]map[string]int64{
		"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1},
		"node-b": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1},
	}
	if !topologyFits(byNode, nil, nil, exp) {
		t.Fatal("initial topology should fit")
	}
	if !reservePlacement(byNode, nil, nil, exp) {
		t.Fatal("initial topology reservation failed")
	}
	if topologyFits(byNode, nil, nil, exp) {
		t.Fatal("a second job in the same tick must not reuse the first job's node slots")
	}
}

func TestTopologyFitsRequiresDeclaredNodeSelector(t *testing.T) {
	exp := distributedExperiment(1, 1)
	exp.Job.NodeSelector = map[string]string{"vendor.example/model": "wanted"}
	capacity := map[string]map[string]int64{"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1}}
	if topologyFits(capacity, nil, map[string]map[string]string{"node-a": {"vendor.example/model": "other"}}, exp) {
		t.Fatal("resource capacity on a node that fails the job-declared selector was accepted")
	}
	if !topologyFits(capacity, nil, map[string]map[string]string{"node-a": {"vendor.example/model": "wanted"}}, exp) {
		t.Fatal("matching live resource capacity and node selector was rejected")
	}
}

func TestResolveClusterAndFootprintSkipsTopologyInfeasibleCluster(t *testing.T) {
	exp := distributedExperiment(2, 1)
	fp := exp.Footprint()
	avail := map[string]domain.Footprint{"cluster-a": fp, "cluster-b": fp}
	nodes := map[string]map[string]map[string]int64{
		"cluster-a": {"node-a": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 2}},
		"cluster-b": {"node-b1": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1}, "node-b2": {"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1}},
	}
	cluster, _ := resolveClusterAndFootprint(avail, nodes, nil, nil, exp)
	if cluster != "cluster-b" {
		t.Fatalf("resolved cluster = %q, want topology-feasible cluster-b", cluster)
	}
}

// A job runs on one node, so it has to fit that node in every dimension — not just in
// accelerators. Checking CPU against a cluster-wide total admitted jobs no node could run: they
// were placed in desired state, held a reservation, and sat unschedulable until the stuck-pending
// deadline, while the disbalance evictor never saw them because it only runs when admission fails.
func TestReservePlacementRequiresTheWholeFootprintOnOneNode(t *testing.T) {
	const flavor = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
	exp := distributedExperiment(1, 1)
	exp.Job.CPU = "4"

	// The only node carrying the flavor has accelerators to spare but not the CPU.
	byNode := map[string]map[string]int64{"node-a": {flavor: 8}}
	starved := map[string]map[string]int64{"node-a": {domain.NodeResourceCPUMillicores: 3000}}
	if reservePlacement(byNode, starved, nil, exp) {
		t.Fatal("admitted onto a node with 3 cores free for a job requesting 4")
	}

	byNode = map[string]map[string]int64{"node-a": {flavor: 8}}
	roomy := map[string]map[string]int64{"node-a": {domain.NodeResourceCPUMillicores: 4000}}
	if !reservePlacement(byNode, roomy, nil, exp) {
		t.Fatal("a node with exactly enough CPU should accept the job")
	}
	if left := roomy["node-a"][domain.NodeResourceCPUMillicores]; left != 0 {
		t.Fatalf("node kept %d millicores after placing a 4-core job, want 0", left)
	}
}

// Cluster-wide room is not node room: two half-full nodes cannot host one indivisible rank.
func TestReservePlacementDoesNotPoolCPUAcrossNodes(t *testing.T) {
	const flavor = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
	exp := distributedExperiment(1, 1)
	exp.Job.CPU = "6"

	byNode := map[string]map[string]int64{"node-a": {flavor: 4}, "node-b": {flavor: 4}}
	split := map[string]map[string]int64{
		"node-a": {domain.NodeResourceCPUMillicores: 4000},
		"node-b": {domain.NodeResourceCPUMillicores: 4000},
	}
	if reservePlacement(byNode, split, nil, exp) {
		t.Fatal("8 cores free cluster-wide but 4 on any node accepted a 6-core rank")
	}
}
