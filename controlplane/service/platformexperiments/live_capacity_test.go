package platformexperiments

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"go.uber.org/zap"
)

// GET /resource-catalog/capacity used to answer 200 with an empty cluster list when the handler
// was never wired to a metrics DB (WithLiveCapacity never called), which reads exactly like "no cluster
// currently has this type free" — a job submitted against a real type then queues forever with no
// error (see the endpoint's own doc). That silently converts an operator misconfiguration into
// starvation nobody can diagnose from the API. Fail fast instead (important.md: "no fallbacks -
// one path or error, fail fast").
func TestLiveCapacityFailsFastWhenUnconfigured(t *testing.T) {
	h := NewService(nil, platformexperimentstest.Settings, zap.NewNop(), nil, time.Minute).WithRepositories(&fakeRepositories{}).WithPlanning(testPlanning)
	// Deliberately never call WithLiveCapacity.

	clusters, err := h.liveCapacity(context.Background(), "org-a", false)
	if err == nil {
		t.Fatalf("liveCapacity() with no metrics DB configured returned nil error and clusters=%v, want ErrLiveCapacityUnconfigured", clusters)
	}
	if !errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() error = %v, want ErrLiveCapacityUnconfigured", err)
	}
	if clusters != nil {
		t.Fatalf("liveCapacity() clusters = %v, want nil on error", clusters)
	}
}

// A job's cost is its run time times the rate of its accelerator type, so the capacity read an
// agent picks its type from carries that rate.
func TestCapacityCarriesEachTypesAccHRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0, "gpu.product=L40": 0.25})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	clusters := buildClusterCapacity(
		map[string]map[string]int64{"local": {"gpu.product=L40": 3}},
		map[string]map[string]int64{"local": {"gpu.product=L40": 8}}, nil)

	want := AcceleratorCapacity{AcceleratorType: "gpu.product=L40", Kind: domain.TypeKindAccelerator, Available: 3, Total: 8, AccHPerHour: 0.25}
	if len(clusters) != 1 || len(clusters[0].Accelerators) != 1 || !reflect.DeepEqual(clusters[0].Accelerators[0], want) {
		t.Fatalf("capacity = %+v, want one cluster with %+v", clusters, want)
	}
}

// A Blackhole chip's driver maps hugepages, which the platform adds to every job on the type. The
// capacity read says so on that type, so nobody states them in a job or wonders where they came from.
func TestCapacityCarriesWhatThePlatformAddsToAJobOnAType(t *testing.T) {
	const blackhole, l40 = "tenstorrent.com/chipArch=blackhole", "gpu.product=L40"
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0, blackhole: 0.5, l40: 0.25})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	clusters := buildClusterCapacity(
		map[string]map[string]int64{"local": {blackhole: 1, l40: 3}},
		map[string]map[string]int64{"local": {blackhole: 2, l40: 8}},
		domain.NewPodResources(map[string]map[string]string{blackhole: {"hugepages-1Gi": "4Gi"}}))
	if len(clusters) != 1 || len(clusters[0].Accelerators) != 2 {
		t.Fatalf("capacity = %+v, want one cluster with two types", clusters)
	}
	chips, gpus := clusters[0].Accelerators[1], clusters[0].Accelerators[0]
	if chips.AcceleratorType != blackhole || !reflect.DeepEqual(chips.PodResources, map[string]string{"hugepages-1Gi": "4Gi"}) {
		t.Errorf("the Blackhole type: got = %+v, want pod_resources hugepages-1Gi 4Gi", chips)
	}
	raw, err := json.Marshal(gpus)
	if err != nil || gpus.PodResources != nil || strings.Contains(string(raw), "pod_resources") {
		t.Errorf("a type the catalog adds nothing to: got = %s (%v), want no pod_resources", raw, err)
	}
}

// Cores are in the capacity read as a type of kind cpu, so that a reader counts its units as
// cores and never adds them to devices.
func TestCapacitySaysWhichTypeCountsCores(t *testing.T) {
	const cores, chip = string(domain.CPUType), "gpu.product=L40"
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0, chip: 0.25, cores: 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	clusters := buildClusterCapacity(
		map[string]map[string]int64{"mixed": {chip: 3, cores: 12}},
		map[string]map[string]int64{"mixed": {chip: 4, cores: 16}}, nil)

	want := []AcceleratorCapacity{
		{AcceleratorType: chip, Kind: domain.TypeKindAccelerator, Available: 3, Total: 4, AccHPerHour: 0.25},
		{AcceleratorType: cores, Kind: domain.TypeKindCPU, Available: 12, Total: 16, AccHPerHour: 1},
	}
	if len(clusters) != 1 || !reflect.DeepEqual(clusters[0].Accelerators, want) {
		t.Fatalf("capacity = %+v, want %+v", clusters, want)
	}
}

// A cluster that rents its machines reports them as slots. Its totals are a limit and nothing
// stands by, and a reader is told so by the cluster's own report, never by its name.
func TestCapacitySaysWhichClustersRentOnDemand(t *testing.T) {
	clusters := []ClusterCapacity{{ClusterName: "rack"}, {ClusterName: "rented"}, {ClusterName: "silent"}}
	withOnDemand(clusters, map[string]map[string]map[string]string{
		"rack": {"node-1": {"gpu.product": "L40"}},
		"rented": {
			"gpu-0": {domain.NodeSlotLabel: "gpu-0", "gpu.product": "L40"},
			"gpu-1": {domain.NodeSlotLabel: "gpu-1", domain.NodeSlotNoOfferLabel: "shape gpu: no vast offer of its size passes regions"},
			"gpu-2": {domain.NodeSlotLabel: "gpu-2", domain.NodeSlotNoOfferLabel: "shape gpu: no vast offer of its size passes regions"},
			"cpu-0": {domain.NodeSlotLabel: "cpu-0", domain.NodeSlotNoOfferLabel: "shape cpu: vast has no on-demand offer of its size"},
		},
	})
	if clusters[0].OnDemand || !clusters[1].OnDemand || clusters[2].OnDemand {
		t.Errorf("on demand = %v, %v, %v, want only the cluster of slots", clusters[0].OnDemand, clusters[1].OnDemand, clusters[2].OnDemand)
	}
	if want := []string{"shape cpu: vast has no on-demand offer of its size", "shape gpu: no vast offer of its size passes regions"}; !reflect.DeepEqual(clusters[1].NoOffer, want) || clusters[0].NoOffer != nil {
		t.Errorf("no offer = %q and %q, want each reason of the cluster of slots once: %q", clusters[0].NoOffer, clusters[1].NoOffer, want)
	}
}

// Capacity is read for one org: the clusters that org's jobs can land on and no other. Knowing
// what another org runs on is knowing something of that org, also for a person who is in both.
func TestCapacityShowsOnlyTheClustersThatTakeTheOrgsJobs(t *testing.T) {
	registered := map[string]domain.Cluster{
		"of-a":   {Name: "of-a", OrgID: "org-a"},
		"of-b":   {Name: "of-b", OrgID: "org-b"},
		"shared": {Name: "shared", OrgID: "org-staff", Shared: true, DisabledOrgs: []string{"org-d"}},
		"of-e":   {Name: "of-e", OrgID: "org-e", DisabledOrgs: []string{"org-e"}},
	}
	connected := []ClusterCapacity{{ClusterName: "of-a"}, {ClusterName: "of-b"}, {ClusterName: "of-e"}, {ClusterName: "phantom"}, {ClusterName: "shared"}}
	for org, want := range map[string][]string{
		"org-a": {"of-a", "shared"}, "org-b": {"of-b", "shared"}, "org-c": {"shared"}, "org-staff": {"shared"},
		// A cluster an org turned off for its jobs is not capacity it can use, its own or shared.
		"org-d": {}, "org-e": {"shared"},
	} {
		got := []string{}
		for _, c := range placeableBy(org, registered, connected) {
			got = append(got, c.ClusterName)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got = %v, want = %v", org, got, want)
		}
	}
}

func TestCapacityOmitsResourcesOfAClusterWithNoCores(t *testing.T) {
	// A cluster that reported accelerators and no cores in the window has no resources to show.
	silent := withResources(buildClusterCapacity(map[string]map[string]int64{"old": {"chip": 1}}, nil, nil), nil, nil, nil, nil)
	if len(silent) != 1 || silent[0].Resources != nil {
		t.Errorf("a cluster with no cores reported: %+v", silent)
	}
}

// A proposal is held to the types `hl capacity` prints, so the types it may state are read from
// the same answer, and a type a cluster has none of is on neither.
func TestTheTypesAProposalMayStateAreTheLinesCapacityLists(t *testing.T) {
	const cores, chip, gone = string(domain.CPUType), "gpu.product=L40", "gpu.product=A100"
	clusters := buildClusterCapacity(
		map[string]map[string]int64{"mixed": {chip: 0, cores: 4, gone: 0}},
		map[string]map[string]int64{"mixed": {chip: 4, cores: 16, gone: 0}}, nil)

	var lines []string
	for _, line := range clusters[0].Accelerators {
		lines = append(lines, line.AcceleratorType)
	}
	want := map[string][]domain.Cluster{
		chip:  {{Name: "mixed"}},
		cores: {{Name: "mixed"}},
	}
	if got := typesListed(clusters); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(lines, []string{chip, cores}) {
		t.Fatalf("listed %v and may state %v, want the lines %v and %v", lines, got, []string{chip, cores}, want)
	}
}

// Capacity names the architectures of each cluster's nodes, once each, so an agent picks an image
// that runs there; a cluster whose nodes name none lists none.
func TestCapacityNamesTheArchitecturesOfEachClustersNodes(t *testing.T) {
	clusters := []ClusterCapacity{{ClusterName: "mixed"}, {ClusterName: "rented"}, {ClusterName: "unlabelled"}}
	withOnDemand(clusters, map[string]map[string]map[string]string{
		"mixed": {
			"a": {domain.NodeArchitectureLabel: "arm64"},
			"b": {domain.NodeArchitectureLabel: "amd64"},
			"c": {domain.NodeArchitectureLabel: "arm64"},
		},
		"rented":     {"gpu-0": {domain.NodeSlotLabel: "gpu-0", domain.NodeArchitectureLabel: "amd64"}},
		"unlabelled": {"node-1": {"gpu.product": "L40"}},
	})
	if got := clusters[0].Architectures; !reflect.DeepEqual(got, []string{"amd64", "arm64"}) {
		t.Errorf("mixed: %v, want amd64 and arm64", got)
	}
	if got := clusters[1].Architectures; !reflect.DeepEqual(got, []string{"amd64"}) {
		t.Errorf("rented: %v, want amd64", got)
	}
	raw, _ := json.Marshal(clusters[2])
	if clusters[2].Architectures != nil || strings.Contains(string(raw), "architectures") {
		t.Errorf("unlabelled: %s, want no architectures", raw)
	}
}

// Capacity names what a cluster's nodes need in a job's image, once each, and nothing for a
// cluster whose nodes need nothing.
func TestCapacityNamesWhatEachClustersNodesNeedInAnImage(t *testing.T) {
	clusters := []ClusterCapacity{{ClusterName: "rented"}, {ClusterName: "box"}}
	withOnDemand(clusters, map[string]map[string]map[string]string{
		"rented": {
			"gpu-0": {domain.NodeSlotLabel: "gpu-0", domain.NodeImageNeedsLabel: "curl,sleep"},
			"cpu-0": {domain.NodeSlotLabel: "cpu-0", domain.NodeImageNeedsLabel: "curl,sleep"},
		},
		"box": {"node-1": {domain.NodeArchitectureLabel: "amd64"}},
	})
	if got := clusters[0].ImageNeeds; !reflect.DeepEqual(got, []string{"curl", "sleep"}) {
		t.Errorf("rented: %v, want curl and sleep", got)
	}
	raw, _ := json.Marshal(clusters[1])
	if clusters[1].ImageNeeds != nil || strings.Contains(string(raw), "image_needs") {
		t.Errorf("box: %s, want no image_needs", raw)
	}
}
