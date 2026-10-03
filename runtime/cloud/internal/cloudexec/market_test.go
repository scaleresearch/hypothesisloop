package cloudexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// The configuration allows two H100 instances and the vendor has one machine on offer: one slot
// is free, the other reports nothing free and says why, and the totals stay the configuration's.
func TestFreeCapacityIsWhatTheVendorHasOnOfferWithinTheShapes(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	var asked []provider.Spec
	onOffer := map[string]int{h100: 1, "": 1}
	p.Market = func(spec provider.Spec) int {
		asked = append(asked, spec)
		return onOffer[spec.AcceleratorType]
	}

	available, total, byNode, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 1 || total[h100] != 2 || byNode["h100-0"][h100] != 1 || byNode["h100-1"][h100] != 0 {
		t.Fatalf("available %v of %v, by node %v, %v", available, total, byNode, err)
	}
	if labels["h100-0"][domain.NodeSlotNoOfferLabel] != "" || labels["h100-1"][domain.NodeSlotNoOfferLabel] != "true" || labels["cpu-0"][domain.NodeSlotNoOfferLabel] != "" {
		t.Fatalf("labels %v, want only h100-1 without an offer", labels)
	}
	want := provider.Spec{AcceleratorType: h100, AcceleratorCount: 1, CPUMillis: 8000, MemoryBytes: 32 * gib, DiskBytes: 100 * gib}
	if !reflect.DeepEqual(asked[0], want) {
		t.Fatalf("asked the market about %+v, want the shape as it is rented: %+v", asked[0], want)
	}
	nodes, err := e.GetLiveNodeResourceCapacity(ctx)
	if err != nil || nodes["h100-0"][domain.NodeResourceCPUMillicores] != 8000 || nodes["h100-1"][domain.NodeResourceCPUMillicores] != 0 {
		t.Fatalf("node resources %v, %v", nodes, err)
	}
	if cpu, cpuTotal, err := e.GetLiveCPUCapacity(ctx); err != nil || cpu != 12 || cpuTotal != 20 {
		t.Fatalf("cpu %v of %v, %v", cpu, cpuTotal, err)
	}
	if sizes, err := e.GetNodeTotalCapacity(ctx); err != nil || sizes["h100-1"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatalf("an offerless slot lost its size: %v, %v", sizes, err)
	}

	// The one machine is rented and the vendor has no other: the slot its instance holds is
	// taken, and the other one waits for an offer.
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	onOffer[h100] = 0
	available, _, _, labels, err = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || available[h100] != 0 || labels["h100-0"][domain.NodeSlotNoOfferLabel] != "" || labels["h100-1"][domain.NodeSlotNoOfferLabel] != "true" {
		t.Fatalf("one rented and none on offer: available %v, labels %v, %v", available, labels, err)
	}
}

// A pass asks the market once per shape, however many capacity figures it reports, and the next
// pass asks again: a market that dried up since the last pass is not reported free from a count
// kept in the process, which placed a job on a slot there was no machine for.
func TestEveryPassAsksTheMarketOncePerShapeAndKeepsNoCountForTheNext(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	onOffer := 2
	p.Market = func(provider.Spec) int { return onOffer }
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

	if got := free(); got != 2 || p.Calls("Rentable") != len(testShapes) {
		t.Fatalf("free = %d with %d market calls, want 2 from one call per shape (%d)", got, p.Calls("Rentable"), len(testShapes))
	}

	// The vendor's machines go to somebody else between two passes.
	onOffer = 0
	if got := free(); got != 0 || p.Calls("Rentable") != 2*len(testShapes) {
		t.Fatalf("the pass after the market dried up: free = %d with %d market calls, want the market asked again and 0", got, p.Calls("Rentable"))
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
	if err != nil || available[h100] != 2 || labels["h100-1"][domain.NodeSlotNoOfferLabel] != "" || p.Calls("Rentable") != 0 {
		t.Fatalf("available %v, labels %v, market calls %d, %v", available, labels, p.Calls("Rentable"), err)
	}
}
