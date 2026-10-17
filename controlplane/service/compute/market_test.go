package compute

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

// The configuration allows two H100 instances and the vendor has one machine on offer: one slot
// is free, the other reports nothing free and says why, and the totals stay the configuration's.
func TestFreeCapacityIsWhatTheVendorHasOnOfferWithinTheShapes(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	var asked []provider.Spec
	onOffer := map[string]int{h100: 1, "": 1}
	p.Market = func(spec provider.Spec) provider.Supply {
		asked = append(asked, spec)
		return supplyOf(onOffer[spec.AcceleratorType])
	}

	available, total, byNode, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 1 || total[h100] != 2 || byNode["h100-0"][h100] != 1 || byNode["h100-1"][h100] != 0 {
		t.Fatalf("available %v of %v, by node %v, %v", available, total, byNode, err)
	}
	if labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on amd64: the shape's lower slots take the machines its market was last read to have" || labels["cpu-0"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("labels %v, want only h100-1 without an offer", labels)
	}
	want := provider.Spec{Architecture: "amd64", AcceleratorType: h100, AcceleratorCount: 1, CPUMillis: 8000, MemoryBytes: 32 * gib, DiskBytes: 100 * gib}
	if !reflect.DeepEqual(asked[0], want) {
		t.Fatalf("asked the market about %+v, want the shape as it is rented: %+v", asked[0], want)
	}
	nodes, err := e.GetLiveNodeResourceCapacity(ctx)
	if err != nil || nodes["h100-0"][clusterapi.NodeResourceCPUMillicores] != 8000 || nodes["h100-1"][clusterapi.NodeResourceCPUMillicores] != 0 {
		t.Fatalf("node resources %v, %v", nodes, err)
	}
	if cpu, cpuTotal, err := e.GetLiveCPUCapacity(ctx); err != nil || cpu != 12 || cpuTotal != 20 {
		t.Fatalf("cpu %v of %v, %v", cpu, cpuTotal, err)
	}
	if sizes, err := e.GetNodeTotalCapacity(ctx); err != nil || sizes["h100-1"][clusterapi.NodeResourceCPUMillicores] != 8000 {
		t.Fatalf("an offerless slot lost its size: %v, %v", sizes, err)
	}

	// The one machine is rented and the vendor has no other: the slot its instance holds is
	// taken, and the other one has nothing to rent.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer[h100] = 0
	available, _, _, labels, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 0 || labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on amd64: no offer passes the filters" {
		t.Fatalf("one rented and none on offer: available %v, labels %v, %v", available, labels, err)
	}
}

// waitingFor is a Demand of the jobs it lists.
type waitingFor []Waiting

func (w waitingFor) Waiting(context.Context) ([]Waiting, error) { return w, nil }

type demandDown struct{}

func (demandDown) Waiting(context.Context) ([]Waiting, error) { return nil, errors.New("queue unreadable") }

func waitingJob(acceleratorType clusterapi.AcceleratorType, count int, cpu, memory, storage, arch string) Waiting {
	return Waiting{AcceleratorType: acceleratorType, Job: clusterapi.JobSpec{
		AcceleratorCount: count, CPU: cpu, Memory: memory, Storage: storage,
		NodeSelector: map[string]string{clusterapi.NodeArchitectureLabel: arch},
	}}
}

func waitingH100(arch string) Waiting { return waitingJob(h100, 1, "8", "32Gi", "100Gi", arch) }

// everySlotWaitedFor waits for every slot of testShapes on amd64.
var everySlotWaitedFor = waitingFor{waitingH100("amd64"), waitingH100("amd64"), waitingJob(clusterapi.CPUType, 4, "4", "16Gi", "50Gi", "amd64")}

func supplyOf(count int) provider.Supply {
	if count == 0 {
		return provider.Supply{Why: "no offer passes the filters"}
	}
	return provider.Supply{Count: count}
}

// A shape the market has nothing for says why on every slot of it no instance holds, in the
// market's words.
func TestAShapeWithNothingToRentSaysWhyOnItsSlots(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	p.Market = func(spec provider.Spec) provider.Supply {
		if spec.AcceleratorCount == 0 {
			return provider.Supply{Count: 1}
		}
		return supplyOf(0)
	}
	for range 3 {
		pass, err := e.ForPass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, labels, err := pass.GetLiveAcceleratorCapacitySnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, slot := range []string{"h100-0", "h100-1"} {
			if got := labels[slot][clusterapi.NodeSlotNoOfferLabel]; got != "shape h100 on amd64: no offer passes the filters" {
				t.Fatalf("slot %s says %q", slot, got)
			}
		}
		if got := labels["cpu-0"][clusterapi.NodeSlotNoOfferLabel]; got != "" {
			t.Fatalf("the cpu slot, which has an offer, says %q", got)
		}
	}
}

// The market is asked once per shape a pass, however many capacity reads the pass makes. How
// long a read stands is the provider's own pacing of its vendor.
func TestTheMarketIsAskedOncePerShapeAPass(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	onOffer := 2
	p.Market = func(provider.Spec) provider.Supply { return supplyOf(onOffer) }
	free := func() int64 {
		t.Helper()
		pass, err := e.ForPass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := pass.GetLiveCPUCapacity(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := pass.GetLiveNodeResourceCapacity(ctx); err != nil {
			t.Fatal(err)
		}
		available, _, _, _, err := pass.GetLiveAcceleratorCapacitySnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return available[h100]
	}

	for range 5 {
		if got := free(); got != 2 {
			t.Fatalf("free = %d, want 2", got)
		}
	}
	if calls := p.Calls("Rentable"); calls != 5*len(testShapes) {
		t.Fatalf("%d market calls in five passes, want one per shape a pass (%d)", calls, 5*len(testShapes))
	}
}

func TestAMarketThatDoesNotAnswerLeavesItsShapeNothingToRent(t *testing.T) {
	e, p := newTestExecutor(t)
	down := errors.New("search is down")
	p.Fail = func(method, _ string) error {
		if method == "Rentable" {
			return down
		}
		return nil
	}
	available, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil || available[h100] != 0 || labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on amd64: the provider's market could not be read: "+down.Error() {
		t.Fatalf("available %v, labels %v, %v, want the market's error on the shape's slots", available, labels, err)
	}
}

// A vendor that starts a machine whenever asked has no market: every slot no instance holds is free.
func TestAProviderWithoutAMarketReportsEverySlotNoInstanceHoldsFree(t *testing.T) {
	_, p := newTestExecutor(t)
	e, err := New(testConfig(struct{ provider.Provider }{p}))
	if err != nil {
		t.Fatal(err)
	}
	available, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil || available[h100] != 2 || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "" || p.Calls("Rentable") != 0 {
		t.Fatalf("available %v, labels %v, market calls %d, %v", available, labels, p.Calls("Rentable"), err)
	}
}

// As many jobs waiting as a shape has slots count every slot the vendor has a machine for, up
// to its max_instances.
func TestAShapeCountsAsManyRentableSlotsAsItsMaxInstances(t *testing.T) {
	p := fake.New()
	cfg := testConfig(p)
	cfg.Shapes = []Shape{testShapes[0]}
	cfg.Shapes[0].MaxInstances = 12
	cfg.Demand = waitingFor(slices.Repeat([]Waiting{waitingH100("amd64")}, 12))
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.Market = func(provider.Spec) provider.Supply { return supplyOf(15) }
	ctx := context.Background()

	available, total, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 12 || total[h100] != 12 || labels["h100-11"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("available %v of %v, h100-11 labels %v, %v, want all 12 slots free", available, total, labels["h100-11"], err)
	}
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	if available, _, _, _, err = e.GetLiveAcceleratorCapacitySnapshot(ctx); err != nil || available[h100] != 11 {
		t.Fatalf("one rented: available %v, %v, want 11", available, err)
	}
}

// A provider that rents two architectures deals each shape's open slots in turn to every one of
// them, those some job waits for first. A job is rented on its own architecture, which its slot
// then reports.
func TestOpenSlotsAreDealtToEveryArchitectureTheProviderRents(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	cfg := testConfig(p)
	cfg.Demand = waitingFor{waitingH100("amd64"), waitingH100("arm64")}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	onOffer := map[string]int{"amd64": 1, "arm64": 1}
	p.Market = func(spec provider.Spec) provider.Supply { return supplyOf(onOffer[spec.Architecture]) }

	arch := func(labels map[string]map[string]string, slot string) string {
		return labels[slot][clusterapi.NodeArchitectureLabel]
	}
	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || arch(labels, "h100-0") != "amd64" || arch(labels, "h100-1") != "arm64" || arch(labels, "cpu-0") != "amd64" {
		t.Fatalf("labels %v, %v, want h100-0 amd64, h100-1 arm64", labels, err)
	}
	if labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("h100-1 has an arm64 machine on offer and says %q", labels["h100-1"][clusterapi.NodeSlotNoOfferLabel])
	}

	exp := on(job(t, "exp-arm", 0, 1), "h100-1")
	exp.ResolvedJob.NodeSelector[clusterapi.NodeArchitectureLabel] = "arm64"
	mustCreate(t, e, exp)
	if got := p.Spec(e.instanceName("exp-arm", 0, "h100", 1)).Architecture; got != "arm64" {
		t.Fatalf("rented on %q, want arm64", got)
	}
	_, _, _, labels, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || arch(labels, "h100-1") != "arm64" {
		t.Fatalf("the slot of the arm64 instance reports %v, %v", labels["h100-1"], err)
	}
}

// What architectures a shape names never waits on the market: with nothing on offer on arm64, or
// on any, its slots still name both, each without a machine saying why. A shape with one open slot
// names the architecture a job waits for on it.
func TestAShapeNamesEveryArchitectureWhateverTheMarketHas(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	cfg := testConfig(p)
	cfg.Demand = waitingFor{waitingH100("amd64"), waitingH100("arm64"), waitingJob(clusterapi.CPUType, 4, "4", "16Gi", "50Gi", "arm64")}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	onOffer := map[string]int{"amd64": 1}
	p.Market = func(spec provider.Spec) provider.Supply { return supplyOf(onOffer[spec.Architecture]) }
	for _, offered := range []map[string]int{{"amd64": 1}, {}} {
		onOffer = offered
		_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if labels["h100-0"][clusterapi.NodeArchitectureLabel] != "amd64" || labels["h100-1"][clusterapi.NodeArchitectureLabel] != "arm64" ||
			labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on arm64: no offer passes the filters" {
			t.Fatalf("on offer %v: h100-0 %v, h100-1 %v, want both architectures, arm64 offerless", offered, labels["h100-0"], labels["h100-1"])
		}
		if (labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] == "") != (offered["amd64"] > 0) {
			t.Fatalf("on offer %v: h100-0 %v", offered, labels["h100-0"])
		}
		if labels["cpu-0"][clusterapi.NodeArchitectureLabel] != "arm64" {
			t.Fatalf("the one cpu slot names %v, want the arm64 a job waits for", labels["cpu-0"])
		}
	}
}

// With no job waiting the market is not read at all: every open slot is offerless and says the
// market is read once a job waits, and the architectures and totals stay the configuration's.
func TestNoJobWaitingReadsNoMarket(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	cfg := testConfig(p)
	cfg.Demand = waitingFor{}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		pass, err := e.ForPass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		available, total, _, labels, err := pass.GetLiveAcceleratorCapacitySnapshot(context.Background())
		if err != nil || available[h100] != 0 || total[h100] != 2 {
			t.Fatalf("available %v of %v, %v", available, total, err)
		}
		if labels["h100-1"][clusterapi.NodeArchitectureLabel] != "arm64" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on arm64: no job waits for it, and its market is read once one does" {
			t.Fatalf("h100-1 %v", labels["h100-1"])
		}
	}
	if calls := p.Calls("Rentable"); calls != 0 {
		t.Fatalf("%d market reads with nothing waiting", calls)
	}
}

// The market is read only for the shapes on the architectures a waiting job could be placed on,
// for as many machines as wait there plus demandMargin, never more than the open slots. A job
// no shape takes, by its size, accelerators or architecture, reads nothing.
func TestTheMarketIsReadOnlyForWhatWaitingJobsCouldTake(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	cfg := testConfig(p)
	cfg.Shapes = []Shape{testShapes[0], testShapes[1]}
	cfg.Shapes[0].MaxInstances = 200
	cfg.Demand = waitingFor{
		waitingH100("arm64"), waitingH100("arm64"),
		waitingJob(h100, 1, "64", "32Gi", "100Gi", "amd64"),
		waitingJob(h100, 2, "8", "32Gi", "100Gi", "amd64"),
		waitingJob("nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe", 1, "8", "32Gi", "100Gi", "amd64"),
		waitingJob(clusterapi.CPUType, 4, "4", "16Gi", "50Gi", "riscv64"),
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	p.Market = func(spec provider.Spec) provider.Supply {
		asked = append(asked, spec.AcceleratorType+" on "+spec.Architecture)
		return supplyOf(100)
	}
	available, _, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{h100 + " on arm64"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("read %v, want %v", asked, want)
	}
	if available[h100] != 2+demandMargin {
		t.Fatalf("%d h100 slots free, want the %d asked about", available[h100], 2+demandMargin)
	}
}

func TestAQueueThatCannotBeReadReadsNoMarketAndSaysWhy(t *testing.T) {
	_, p := newTestExecutor(t)
	cfg := testConfig(p)
	cfg.Demand = demandDown{}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	available, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil || available[h100] != 0 || p.Calls("Rentable") != 0 ||
		labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on amd64: the jobs waiting for the cluster could not be read: queue unreadable" {
		t.Fatalf("available %v, labels %v, %d reads, %v", available, labels["h100-0"], p.Calls("Rentable"), err)
	}
}

func TestAProviderWithAMarketNeedsTheClustersDemand(t *testing.T) {
	cfg := testConfig(fake.New())
	cfg.Demand = nil
	if _, err := New(cfg); err == nil {
		t.Fatal("built without a demand")
	}
}

type needing struct{ *fake.Provider }

func (needing) ImageNeeds() []string { return []string{"curl", "sleep"} }

// A provider that needs programs in a job's image says so on every slot.
func TestEverySlotSaysWhatTheProviderNeedsInAJobsImage(t *testing.T) {
	e, err := New(testConfig(needing{fake.New()}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{"h100-0", "h100-1", "cpu-0"} {
		if got := labels[slot][clusterapi.NodeImageNeedsLabel]; got != "curl,sleep" {
			t.Errorf("slot %s says it needs %q", slot, got)
		}
	}
}
