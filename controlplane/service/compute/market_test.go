package compute

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"reflect"
	"strings"
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
	// taken, and the other one is free until the market's read is marketInterval old, for
	// Create to find no offer for it.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer[h100] = 0
	available, _, _, labels, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 1 || labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("one rented and none on offer: available %v, labels %v, %v", available, labels, err)
	}
}

func supplyOf(count int) provider.Supply {
	if count == 0 {
		return provider.Supply{Why: "no offer passes the filters", Detail: "4 offers of the size; left out by price 4"}
	}
	return provider.Supply{Count: count}
}

// A shape the market has nothing for says why on every slot of it no instance holds, in the
// market's words, and logs the figures behind it once.
func TestAShapeWithNothingToRentSaysWhyOnItsSlotsAndInTheLog(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	p.Market = func(spec provider.Spec) provider.Supply {
		if spec.AcceleratorCount == 0 {
			return provider.Supply{Count: 1}
		}
		return supplyOf(0)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

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
	lines := strings.Count(logged.String(), "shape h100 on amd64 has nothing to rent: no offer passes the filters (4 offers of the size; left out by price 4)")
	if lines != 1 || strings.Contains(logged.String(), "shape cpu ") {
		t.Fatalf("log:\n%s\nwant the h100 shape's reason once", logged.String())
	}
}

// The market is asked once per shape, however many capacity reads and passes there are within
// marketInterval.
func TestTheMarketIsAskedOncePerShapeWithinItsInterval(t *testing.T) {
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
	if calls := p.Calls("Rentable"); calls != len(testShapes) {
		t.Fatalf("%d market calls in five passes, want one per shape (%d)", calls, len(testShapes))
	}

	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer = 0
	if got := free(); got != 1 {
		t.Fatalf("after one rent with the read standing: free = %d, want the slot no instance holds", got)
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

// A shape allowed more instances than one read used to ask for counts every slot the vendor has
// a machine for, up to its max_instances.
func TestAShapeCountsAsManyRentableSlotsAsItsMaxInstances(t *testing.T) {
	p := fake.New()
	cfg := testConfig(p)
	cfg.Shapes = []Shape{testShapes[0]}
	cfg.Shapes[0].MaxInstances = 12
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
	if calls := p.Calls("Rentable"); calls != 1 {
		t.Fatalf("%d market calls, want the one read standing", calls)
	}
}

// A provider that rents two architectures deals each shape's open slots to them in turn while
// each has a machine, and an architecture with none still names an offerless slot, so a job of it
// is admitted to wait. A job is rented on its own architecture, which its slot then reports.
func TestOpenSlotsAreDealtToEveryArchitectureTheProviderRents(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	e, err := New(testConfig(p))
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

func TestAnArchitectureWithNothingToRentStillNamesAnOfferlessSlot(t *testing.T) {
	p := fake.New()
	p.Rents = []string{"amd64", "arm64"}
	e, err := New(testConfig(p))
	if err != nil {
		t.Fatal(err)
	}
	p.Market = func(spec provider.Spec) provider.Supply {
		if spec.Architecture == "arm64" {
			return supplyOf(0)
		}
		return supplyOf(2)
	}
	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if labels["h100-0"][clusterapi.NodeArchitectureLabel] != "amd64" || labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("h100-0 %v, want a free amd64 slot", labels["h100-0"])
	}
	if labels["h100-1"][clusterapi.NodeArchitectureLabel] != "arm64" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100 on arm64: no offer passes the filters" {
		t.Fatalf("h100-1 %v, want an offerless arm64 slot saying why", labels["h100-1"])
	}
}
