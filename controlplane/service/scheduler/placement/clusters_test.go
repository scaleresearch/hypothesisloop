package placement

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const l40 = "nvidia.com/gpu.product=NVIDIA-L40"

// An org's two clusters and one of the platform's, ranked above them by the platform's org. The
// names sort against the priorities, so a search that still went by name would answer differently.
func rankedClusters() map[string]domain.Cluster {
	return map[string]domain.Cluster{
		"z-first":    {Name: "z-first", OrgID: "org-a", Priority: 500},
		"m-second":   {Name: "m-second", OrgID: "org-a", Priority: 100},
		"a-platform": {Name: "a-platform", OrgID: domain.PlatformOrgID, Priority: 900},
	}
}

func accelerators(acceleratorTypes map[string]int64) domain.Footprint {
	return domain.CapacityFootprint(16, acceleratorTypes, 64<<30, 64<<30)
}

func nodeTotal() map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: 16000,
		domain.NodeResourceMemoryBytes:   64 << 30,
		domain.NodeResourceStorageBytes:  64 << 30,
	}
}

func holdJob(id string, tier domain.CapacityTier, acceleratorType domain.AcceleratorType, count int, cpu string) *domain.Experiment {
	return &domain.Experiment{
		ID: id, AgentID: "agent-" + id, PlatformExperimentID: "pe-1", OrgID: "org-a", CapacityTier: tier,
		AcceleratorType: acceleratorType, AcceleratorCount: count,
		Job: domain.JobSpec{CPU: cpu, Memory: "1Gi", Storage: "1Gi", AcceleratorType: acceleratorType, AcceleratorCount: count},
	}
}

// free is clusters of one node each, holding the given accelerators free: what the clusters have
// in total and what their one node has.
func free(byCluster map[string]map[string]int64) (avail map[string]domain.Footprint, nodes, resources map[string]map[string]map[string]int64) {
	avail, nodes, resources = map[string]domain.Footprint{}, map[string]map[string]map[string]int64{}, map[string]map[string]map[string]int64{}
	for cluster, acceleratorTypes := range byCluster {
		avail[cluster] = accelerators(acceleratorTypes)
		nodes[cluster] = map[string]map[string]int64{"node-" + cluster: acceleratorTypes}
		resources[cluster] = map[string]map[string]int64{"node-" + cluster: nodeTotal()}
	}
	return avail, nodes, resources
}

func TestPlacementTiersGoByPriorityThenName(t *testing.T) {
	registered := rankedClusters()
	registered["b-second"] = domain.Cluster{Name: "b-second", OrgID: "org-a", Priority: 100}
	candidates := map[string]domain.Footprint{"a-platform": nil, "m-second": nil, "z-first": nil, "b-second": nil, "unregistered": nil}

	got := Tiers(candidates, registered, "org-a")

	want := [][]string{{"z-first"}, {"b-second", "m-second"}, {"a-platform", "unregistered"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placementTiers = %v, want %v", got, want)
	}
	if got := Tiers(nil, registered, "org-a"); len(got) != 0 {
		t.Fatalf("placementTiers of no candidates = %v, want none", got)
	}
}

// The search is tier, then type, then cluster. A higher cluster at an acceptable alternative
// comes before a lower cluster at the type asked for, and inside one tier the type asked for
// comes before the name.
func TestTheClusterTierIsSearchedBeforeTheAcceleratorType(t *testing.T) {
	job := func() *domain.Experiment {
		exp := holdJob("a1", domain.CapacityBurst, h100, 1, "1")
		exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40}
		return exp
	}
	for name, tc := range map[string]struct {
		free                map[string]map[string]int64
		wantCluster         string
		wantAcceleratorType domain.AcceleratorType
	}{
		"the higher cluster has only the alternative": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 1}, "m-second": map[string]int64{h100: 1}},
			"z-first", l40},
		"the higher cluster has both": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 1, h100: 1}, "m-second": map[string]int64{h100: 1}},
			"z-first", h100},
		"the higher cluster has neither free": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 0, h100: 0}, "m-second": map[string]int64{l40: 1}},
			"m-second", l40},
		"one tier, the type asked for is on the later name": {
			map[string]map[string]int64{"b-second": map[string]int64{l40: 1}, "m-second": map[string]int64{h100: 1}},
			"m-second", h100},
	} {
		t.Run(name, func(t *testing.T) {
			registered := rankedClusters()
			registered["b-second"] = domain.Cluster{Name: "b-second", OrgID: "org-a", Priority: 100}
			exp := job()

			avail, nodes, resources := free(tc.free)

			cluster, fp := ClusterAndFootprint(avail, registered, nodes, resources, nil, exp)

			if cluster != tc.wantCluster || exp.AcceleratorType != tc.wantAcceleratorType {
				t.Fatalf("resolved to %q at %q, want %q at %q", cluster, exp.AcceleratorType, tc.wantCluster, tc.wantAcceleratorType)
			}
			if !domain.Fits(avail[cluster], fp) {
				t.Fatalf("the footprint returned does not fit the cluster chosen: %v", fp)
			}
		})
	}
}

// A job that already ran keeps its accelerator type (CandidateTypes), and is placed again by the
// same order as any other.
func TestARequeuedJobIsPlacedAgainByPriority(t *testing.T) {
	exp := holdJob("a1", domain.CapacityBurst, l40, 1, "1")
	exp.Job.AcceleratorType = h100
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40}
	exp.RanOnAcceleratorType = l40
	avail, nodes, resources := free(map[string]map[string]int64{"z-first": {h100: 1}, "m-second": {l40: 1}, "a-platform": {l40: 1}})

	cluster, _ := ClusterAndFootprint(avail, rankedClusters(), nodes, resources, nil, exp)

	if cluster != "m-second" || exp.AcceleratorType != l40 {
		t.Fatalf("resolved to %q at %q, want the highest cluster with the accelerator type it ran on, m-second at %q", cluster, exp.AcceleratorType, l40)
	}
}

// A job reverted to the accelerator type it asked for, because its quota does not cover the alternative,
// is placed by the same order.
func TestPlaceAtAcceleratorTypeTakesTheHighestPriorityClusterThatFits(t *testing.T) {
	exp := holdJob("a1", domain.CapacityBurst, h100, 1, "1")
	avail, nodes, resources := free(map[string]map[string]int64{"z-first": {h100: 1}, "m-second": {h100: 1}, "a-platform": {h100: 1}})
	if cluster, ok := AtAcceleratorType(avail, rankedClusters(), nodes, resources, nil, exp); !ok || cluster != "z-first" {
		t.Fatalf("placeAtAcceleratorType = %q, %v, want z-first", cluster, ok)
	}
	avail, nodes, resources = free(map[string]map[string]int64{"z-first": {h100: 0}, "m-second": {h100: 1}, "a-platform": {h100: 1}})
	if cluster, ok := AtAcceleratorType(avail, rankedClusters(), nodes, resources, nil, exp); !ok || cluster != "m-second" {
		t.Fatalf("placeAtAcceleratorType with the first full = %q, %v, want m-second", cluster, ok)
	}
}

// With no cluster fitting, room is made on the highest-priority cluster that has the hardware,
// whatever a lower one is short of.
func TestThePreemptionTargetIsTheHighestPriorityClusterWithTheHardware(t *testing.T) {
	need := holdJob("a1", domain.CapacityGuaranteed, h100, 2, "1").Footprint()
	registered := rankedClusters()
	for name, tc := range map[string]struct {
		avail map[string]domain.Footprint
		want  string
	}{
		"both are full, the lower is short of less": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{h100: 0}), "m-second": accelerators(map[string]int64{h100: 1})}, "z-first"},
		"the higher has no such hardware": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{l40: 4}), "m-second": accelerators(map[string]int64{h100: 0})}, "m-second"},
		"a lower one fits outright": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{h100: 1}), "a-platform": accelerators(map[string]int64{h100: 2})}, "a-platform"},
		"nobody has the hardware": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{l40: 4}), "m-second": accelerators(map[string]int64{l40: 4})}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BestFit(tc.avail, registered, "org-a", need); got != tc.want {
				t.Fatalf("clusterWithBestFit = %q, want %q", got, tc.want)
			}
		})
	}
}

// The scheduler once placed 7 of 9 researcher jobs on the first name that fit, a box of 12 cores, while one
// of 28 of the same priority sat mostly idle. Within a tier a job goes where the most jobs like
// it still fit.
func TestAJobGoesToTheClusterOfItsTierWithTheMostRoom(t *testing.T) {
	registered := rankedClusters()
	registered["b-second"] = domain.Cluster{Name: "b-second", OrgID: "org-a", Priority: 100}
	place := func(byCluster map[string]map[string]int64) string {
		t.Helper()
		exp := holdJob("a1", domain.CapacityBurst, h100, 1, "1")
		avail, nodes, resources := free(byCluster)
		cluster, _ := ClusterAndFootprint(avail, registered, nodes, resources, nil, exp)
		return cluster
	}
	if got := place(map[string]map[string]int64{"b-second": {h100: 1}, "m-second": {h100: 3}}); got != "m-second" {
		t.Errorf("one h100 free on b-second and three on m-second: placed on %q, want m-second", got)
	}
	if got := place(map[string]map[string]int64{"b-second": {h100: 2}, "m-second": {h100: 2}}); got != "b-second" {
		t.Errorf("equal room and no environment: placed on %q, want b-second by name", got)
	}
	if got := place(map[string]map[string]int64{"z-first": {h100: 1}, "m-second": {h100: 3}}); got != "z-first" {
		t.Errorf("room on a higher priority: placed on %q, want z-first", got)
	}
}

func TestRoomForIsTheFewestOfAnyDimension(t *testing.T) {
	cpu, memory, h100s := domain.ResourceKey{Kind: domain.ResourceKindCPU}, domain.ResourceKey{Kind: domain.ResourceKindMemory}, domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Name: "h100"}
	free := domain.Footprint{cpu: 27800, memory: 64}
	if got := roomFor(free, domain.Footprint{cpu: 4000, memory: 8, h100s: 0}); got != 6 {
		t.Errorf("roomFor = %d, want 6 (cpu)", got)
	}
	if got := roomFor(free, domain.Footprint{cpu: 1000, memory: 16}); got != 4 {
		t.Errorf("roomFor = %d, want 4 (memory)", got)
	}
}
