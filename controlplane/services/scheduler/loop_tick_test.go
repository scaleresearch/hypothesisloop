package scheduler

import (
	"testing"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
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

func TestNotAdmittedReasonForOnlyLooksAtRequestedDimensions(t *testing.T) {
	// A job that only requests CPU shouldn't be classified "outranked" just because some
	// unrelated accelerator flavor's availability dropped this tick.
	initial := domain.Footprint{cpuKey: 2000, acceleratorKey("flavor-t4"): 4}
	current := domain.Footprint{cpuKey: 2000, acceleratorKey("flavor-t4"): 0}
	fp := domain.Footprint{cpuKey: 1000}
	if got := notAdmittedReasonFor(current, initial, fp); got != domain.NotAdmittedCapacityUnavailable {
		t.Errorf("notAdmittedReasonFor = %q, want capacity_unavailable (CPU untouched)", got)
	}

	current2 := domain.Footprint{cpuKey: 500, acceleratorKey("flavor-t4"): 4}
	if got := notAdmittedReasonFor(current2, initial, fp); got != domain.NotAdmittedOutranked {
		t.Errorf("notAdmittedReasonFor = %q, want outranked (CPU dropped)", got)
	}
}
