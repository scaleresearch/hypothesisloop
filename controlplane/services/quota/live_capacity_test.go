package quota

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// GET /resource-catalog/capacity used to answer 200 with an empty cluster list when the handler
// was never wired to a metrics DB (metricsDBURL == ""), which reads exactly like "no cluster
// currently has this type free" — a job submitted against a real type then queues forever with no
// error (see the endpoint's own doc). That silently converts an operator misconfiguration into
// starvation nobody can diagnose from the API. Fail fast instead (important.md: "no fallbacks -
// one path or error, fail fast").
func TestLiveCapacityFailsFastWhenUnconfigured(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, &fakeRepositories{}, nil)
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

// Once wired, an empty result must still be possible on its own merits (e.g. a metrics DB that is
// reachable but simply has no fresh reports) — this test just pins that WithLiveCapacity flips the
// handler out of the unconfigured error path; the live-read behavior itself is metricsdb's own.
func TestLiveCapacityConfiguredDoesNotFailFast(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, &fakeRepositories{}, nil).WithLiveCapacity("postgres://example-not-dialed/db", time.Minute, nil)
	if h.metricsDBURL == "" {
		t.Fatalf("WithLiveCapacity did not set metricsDBURL")
	}
	// Not asserting on the outcome of an actual dial here — that's metricsdb's own concern and
	// would require a live DB. This only pins that liveCapacity no longer takes the "unconfigured"
	// branch once WithLiveCapacity has been called.
	_, err := h.liveCapacity(context.Background(), "", true)
	if errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() still reports unconfigured after WithLiveCapacity was called")
	}
}

// A job's cost is its run time times the rate of its accelerator type, so the capacity read an
// agent picks its type from carries that rate.
func TestCapacityCarriesEachTypesAccHRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0, "gpu.product=L40": 0.25})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	clusters := buildClusterCapacity(
		map[string]map[string]int64{"local": {"gpu.product=L40": 3}},
		map[string]map[string]int64{"local": {"gpu.product=L40": 8}})

	want := AcceleratorCapacity{AcceleratorType: "gpu.product=L40", Kind: domain.TypeKindAccelerator, Available: 3, Total: 8, AccHPerHour: 0.25}
	if len(clusters) != 1 || len(clusters[0].Accelerators) != 1 || !reflect.DeepEqual(clusters[0].Accelerators[0], want) {
		t.Fatalf("capacity = %+v, want one cluster with %+v", clusters, want)
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
		map[string]map[string]int64{"mixed": {chip: 4, cores: 16}})

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
		"rack":   {"node-1": {"gpu.product": "L40"}},
		"rented": {"gpu-0": {domain.NodeSlotLabel: "gpu-0", "gpu.product": "L40"}, "gpu-1": {domain.NodeSlotLabel: "gpu-1"}},
	})
	if clusters[0].OnDemand || !clusters[1].OnDemand || clusters[2].OnDemand {
		t.Errorf("on demand = %v, %v, %v, want only the cluster of slots", clusters[0].OnDemand, clusters[1].OnDemand, clusters[2].OnDemand)
	}

	// Each type's line names the offered images that suit it. A cluster that runs its own image
	// and no other names none and says what its own carries.
	offered := []domain.JobImage{
		{Name: "python", Image: "registry.test/job:7", Carries: "Python"},
		{Name: "pytorch-cuda", Image: "registry.test/job-pytorch-cuda:7", Carries: "PyTorch", Suits: []string{"nvidia.com/"}},
	}
	nvidia := []AcceleratorCapacity{{AcceleratorType: "nvidia.com/gpu.product=L40"}}
	clusters = []ClusterCapacity{
		{ClusterName: "rack", Accelerators: append([]AcceleratorCapacity{{AcceleratorType: string(domain.CPUType)}}, nvidia...)},
		{ClusterName: "rented", JobImage: "registry.test/job-pytorch-cuda:7", Accelerators: append([]AcceleratorCapacity{}, nvidia...)},
	}
	withOfferedImages(offered, clusters)
	rack, rented := clusters[0].Accelerators, clusters[1]
	if !slices.Equal(rack[0].JobImages, []string{"python"}) || !slices.Equal(rack[1].JobImages, []string{"python", "pytorch-cuda"}) {
		t.Errorf("rack names %v for cores and %v for the L40, want python, and python with pytorch-cuda", rack[0].JobImages, rack[1].JobImages)
	}
	if rented.JobImageCarries != "PyTorch" || len(rented.Accelerators[0].JobImages) != 0 {
		t.Errorf("rented carries %q and names %v, want PyTorch and no image to name", rented.JobImageCarries, rented.Accelerators[0].JobImages)
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

func TestCapacitySaysWhichClustersRunEveryJobInOneImage(t *testing.T) {
	registered := map[string]domain.Cluster{
		"rented": {Name: "rented", OrgID: "org-a", JobImage: "registry.test/platform-job:7"},
		"k8s":    {Name: "k8s", OrgID: "org-a"},
	}
	clusters := []ClusterCapacity{{ClusterName: "k8s"}, {ClusterName: "phantom"}, {ClusterName: "rented"}}
	withJobImages(registered, clusters)
	raw, err := json.Marshal(clusters)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"cluster_name":"k8s","accelerators":null},{"cluster_name":"phantom","accelerators":null},{"cluster_name":"rented","job_image":"registry.test/platform-job:7","accelerators":null}]`
	if string(raw) != want {
		t.Fatalf("got  = %s\nwant = %s", raw, want)
	}
}

// A job that asks for no accelerator is placed by cores and memory, and a cluster without any
// accelerator was in no capacity answer at all, so nobody choosing where to run could see either.
func TestCapacityCarriesEachClustersCoresAndMemory(t *testing.T) {
	clusters := withResources(
		buildClusterCapacity(map[string]map[string]int64{"chips": {"chip": 1}}, map[string]map[string]int64{"chips": {"chip": 4}}),
		map[string]float64{"chips": 12, "cpu-only": 30.5}, map[string]float64{"chips": 16, "cpu-only": 32},
		map[string]int64{"chips": 1 << 30, "cpu-only": 2 << 30}, map[string]int64{"chips": 4 << 30, "cpu-only": 8 << 30})

	if len(clusters) != 2 || clusters[0].ClusterName != "chips" || clusters[1].ClusterName != "cpu-only" {
		t.Fatalf("clusters = %+v, want the one with chips and the one with none, by name", clusters)
	}
	if want := (ResourceCapacity{CPUCoresAvailable: 12, CPUCoresTotal: 16, MemoryBytesAvailable: 1 << 30, MemoryBytesTotal: 4 << 30}); *clusters[0].Resources != want {
		t.Errorf("chips: %+v, want %+v", *clusters[0].Resources, want)
	}
	if want := (ResourceCapacity{CPUCoresAvailable: 30.5, CPUCoresTotal: 32, MemoryBytesAvailable: 2 << 30, MemoryBytesTotal: 8 << 30}); *clusters[1].Resources != want {
		t.Errorf("cpu-only: %+v, want %+v", *clusters[1].Resources, want)
	}
	if clusters[1].Accelerators == nil || len(clusters[1].Accelerators) != 0 {
		t.Errorf("cpu-only accelerators = %#v, want an empty list", clusters[1].Accelerators)
	}

	// A cluster that reported accelerators and no cores in the window has no resources to show.
	silent := withResources(buildClusterCapacity(map[string]map[string]int64{"old": {"chip": 1}}, nil), nil, nil, nil, nil)
	if len(silent) != 1 || silent[0].Resources != nil {
		t.Errorf("a cluster with no cores reported: %+v", silent)
	}
}
