package cloudexec

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
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
	if labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100: the provider has fewer machines on offer for it than max_instances, and the shape's lower slots take them" || labels["cpu-0"][clusterapi.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("labels %v, want only h100-1 without an offer", labels)
	}
	want := provider.Spec{AcceleratorType: h100, AcceleratorCount: 1, CPUMillis: 8000, MemoryBytes: 32 * gib, DiskBytes: 100 * gib}
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
	// taken, and the other one waits for an offer.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer[h100] = 0
	available, _, _, labels, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 0 || labels["h100-0"][clusterapi.NodeSlotNoOfferLabel] != "" || labels["h100-1"][clusterapi.NodeSlotNoOfferLabel] != "shape h100: every machine on offer for it when last counted is rented by this cluster since" {
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
// market's words, and the reason is logged once per count and not once per pass.
func TestAShapeWithNothingToRentSaysWhyOnItsSlotsAndInTheLog(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	clock := time.Now()
	e.now = func() time.Time { return clock }
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
			if got := labels[slot][clusterapi.NodeSlotNoOfferLabel]; got != "shape h100: no offer passes the filters" {
				t.Fatalf("slot %s says %q", slot, got)
			}
		}
		if got := labels["cpu-0"][clusterapi.NodeSlotNoOfferLabel]; got != "" {
			t.Fatalf("the cpu slot, which has an offer, says %q", got)
		}
		clock = clock.Add(10 * time.Second)
	}
	lines := strings.Count(logged.String(), "shape h100 has nothing to rent: no offer passes the filters (4 offers of the size; left out by price 4)")
	if lines != 1 || strings.Contains(logged.String(), "shape cpu ") {
		t.Fatalf("log:\n%s\nwant the h100 shape's reason once", logged.String())
	}
	clock = clock.Add(3 * time.Minute)
	if _, _, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(logged.String(), "shape h100 has nothing to rent"); lines != 2 {
		t.Fatalf("log after an offer wait:\n%s\nwant the reason again", logged.String())
	}
}

// The vendor's search is rate limited, and a pass comes every few seconds: the market is asked
// once per shape and OfferWait, whatever the cluster is doing, and in between the last count
// stands less what was rented since.
func TestTheMarketIsAskedOncePerShapeAndOfferWaitAndNotOncePerPass(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	clock := time.Now()
	e.now = func() time.Time { return clock }
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
		clock = clock.Add(10 * time.Second)
	}
	if calls := p.Calls("Rentable"); calls != len(testShapes) {
		t.Fatalf("%d market calls in five passes, want one per shape (%d)", calls, len(testShapes))
	}

	// One of the two machines counted is rented, and the vendor's other one goes too: the count
	// in hand already knows the first, and learns the second when it is next asked.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer = 0
	if got := free(); got != 1 || p.Calls("Rentable") != len(testShapes) {
		t.Fatalf("after one rent: free = %d with %d market calls, want 1 from the count in hand", got, p.Calls("Rentable"))
	}
	clock = clock.Add(3 * time.Minute)
	if got := free(); got != 0 || p.Calls("Rentable") != 2*len(testShapes) {
		t.Fatalf("an offer wait later: free = %d with %d market calls, want the market asked again and 0", got, p.Calls("Rentable"))
	}
}

func TestAMarketThatDoesNotAnswerFailsTheCapacityRead(t *testing.T) {
	e, p := newTestExecutor(t)
	down := errors.New("search is down")
	p.Fail = func(method, _ string) error {
		if method == "Rentable" {
			return down
		}
		return nil
	}
	if _, _, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(context.Background()); !errors.Is(err, down) || !strings.Contains(err.Error(), "h100") {
		t.Fatalf("err = %v, want the market's error naming the shape", err)
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
