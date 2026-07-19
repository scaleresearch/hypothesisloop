package domain

import "testing"

func TestFootprintScaleMultiNode(t *testing.T) {
	j := JobSpec{
		CPU:      "500m",
		Memory:   "1Gi",
		Storage:  "2Gi",
		AcceleratorType:  AcceleratorH100,
		AcceleratorCount: 2,
		NumNodes: 4,
	}
	fp, err := j.Footprint()
	if err != nil {
		t.Fatalf("Footprint: %v", err)
	}
	// CPU: 500 millicores/node * 4 nodes = 2000.
	if got := fp[ResourceKey{Kind: ResourceKindCPU}]; got != 2000 {
		t.Errorf("cpu millicores = %d, want 2000", got)
	}
	// Accelerator: 2 accelerators/node * 4 nodes = 8, matches TotalAccelerators(). Flavor key uses
	// AcceleratorType.FlavorName() ("flavor-h100"), matching capacity reporting's key convention.
	if got := fp[ResourceKey{Kind: ResourceKindAccelerator, Flavor: AcceleratorH100.FlavorName()}]; got != int64(j.TotalAccelerators()) {
		t.Errorf("accelerator count = %d, want %d", got, j.TotalAccelerators())
	}
	// Memory: 1GiB/node * 4 nodes.
	want := int64(1073741824) * 4
	if got := fp[ResourceKey{Kind: ResourceKindMemory}]; got != want {
		t.Errorf("memory bytes = %d, want %d", got, want)
	}
}

func TestFootprintFractionalCPUPrecision(t *testing.T) {
	j := JobSpec{CPU: "250m", NumNodes: 1}
	fp, err := j.Footprint()
	if err != nil {
		t.Fatalf("Footprint: %v", err)
	}
	if got := fp[ResourceKey{Kind: ResourceKindCPU}]; got != 250 {
		t.Errorf("cpu millicores = %d, want 250 (fractional precision lost)", got)
	}
}

func TestFootprintMalformedQuantityRejected(t *testing.T) {
	j := JobSpec{CPU: "not-a-quantity"}
	if _, err := j.Footprint(); err == nil {
		t.Error("expected error for malformed CPU quantity, got nil")
	}
}

func TestFits(t *testing.T) {
	cpu := ResourceKey{Kind: ResourceKindCPU}
	mem := ResourceKey{Kind: ResourceKindMemory}
	accelerator := ResourceKey{Kind: ResourceKindAccelerator, Flavor: "H100"}

	capacity := Footprint{cpu: 4000, mem: 8_000_000_000}
	need := Footprint{cpu: 2000, mem: 4_000_000_000}
	if !Fits(capacity, need) {
		t.Error("expected fit when all requested dims are within capacity")
	}

	// A dimension the job needs but capacity doesn't report at all must fail closed.
	needsAccelerator := Footprint{cpu: 1000, accelerator: 1}
	if Fits(capacity, needsAccelerator) {
		t.Error("expected no-fit when capacity is missing a dimension the job requests")
	}

	// Joint fit: both dimensions must fit on the SAME capacity vector, not independently.
	tooMuchMem := Footprint{cpu: 1000, mem: 100_000_000_000}
	if Fits(capacity, tooMuchMem) {
		t.Error("expected no-fit when one dimension of a mixed request exceeds capacity")
	}
}

func TestFootprintSubDetectsShortage(t *testing.T) {
	cpu := ResourceKey{Kind: ResourceKindCPU}
	capacity := Footprint{cpu: 1000}
	occupied := Footprint{cpu: 1500}
	remaining := capacity.Sub(occupied)
	if remaining[cpu] >= 0 {
		t.Errorf("expected negative remaining capacity, got %d", remaining[cpu])
	}
}
