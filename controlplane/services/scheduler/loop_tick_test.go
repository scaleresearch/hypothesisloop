package scheduler

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var cpuKey = domain.ResourceKey{Kind: domain.ResourceKindCPU}

func acceleratorKey(flavor string) domain.ResourceKey {
	return domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: flavor}
}

func TestClusterWithBestFitPrefersOutrightFit(t *testing.T) {
	avail := map[string]domain.Footprint{
		"b-cluster": {cpuKey: 1000}, // too little
		"a-cluster": {cpuKey: 4000}, // fits
	}
	need := domain.Footprint{cpuKey: 2000}
	got := clusterWithBestFit(avail, need)
	if got != "a-cluster" {
		t.Errorf("clusterWithBestFit = %q, want a-cluster (the one that actually fits)", got)
	}
}

func TestClusterWithBestFitFallsBackToSmallestShortage(t *testing.T) {
	avail := map[string]domain.Footprint{
		"big-gap":   {cpuKey: 500},  // short by 1500
		"small-gap": {cpuKey: 1800}, // short by 200
	}
	need := domain.Footprint{cpuKey: 2000}
	got := clusterWithBestFit(avail, need)
	if got != "small-gap" {
		t.Errorf("clusterWithBestFit = %q, want small-gap (least total shortage)", got)
	}
}

func TestSubtractFootprintClampsAtZero(t *testing.T) {
	avail := domain.Footprint{cpuKey: 500}
	subtractFootprint(avail, domain.Footprint{cpuKey: 2000})
	if avail[cpuKey] != 0 {
		t.Errorf("avail[cpu] = %d, want 0 (clamped, not negative)", avail[cpuKey])
	}
}

func TestShortfallOnlyCountsDeficitDimensions(t *testing.T) {
	avail := domain.Footprint{cpuKey: 3000, acceleratorKey("flavor-t4"): 5}
	need := domain.Footprint{cpuKey: 4000, acceleratorKey("flavor-t4"): 2}
	got := shortfall(avail, need)
	if got[cpuKey] != 1000 {
		t.Errorf("shortfall[cpu] = %d, want 1000", got[cpuKey])
	}
	if v, ok := got[acceleratorKey("flavor-t4")]; ok && v > 0 {
		t.Errorf("shortfall[accelerator] = %d, want 0/absent (capacity already covers this dimension)", v)
	}
}

func TestNotAdmittedReasonDistinguishesScarcityFromOutranking(t *testing.T) {
	need := domain.Footprint{cpuKey: 2000, acceleratorKey("flavor-t4"): 1}

	// Never fit this cluster to begin with: waiting for the queue to drain cannot admit it, so
	// telling the submitter it was outranked would point them at the wrong remedy.
	if got := notAdmittedReasonFor(false, need, nil); got != domain.NotAdmittedCapacityUnavailable {
		t.Fatalf("unchanged insufficient capacity reason = %q", got)
	}
	// Would have fit against the capacity the tick opened with; jobs ahead of it took that.
	if got := notAdmittedReasonFor(true, need, nil); got != domain.NotAdmittedOutranked {
		t.Fatalf("capacity consumed earlier in tick reason = %q", got)
	}
	shortage := domain.Footprint{cpuKey: 1000}
	if got := notAdmittedReasonFor(false, need, shortage); got != domain.NotAdmittedCapacityUnavailable+": short "+footprintStr(shortage) {
		t.Fatalf("detailed shortage reason = %q", got)
	}
	// A shortage vector alongside a fit at tick start still means outranked: the shortfall is
	// measured against what is left now, which is the definition of having lost it.
	if got := notAdmittedReasonFor(true, need, shortage); got != domain.NotAdmittedOutranked {
		t.Fatalf("outranked with a live shortfall reason = %q", got)
	}
}
