package domain

import "testing"

func TestFairShareExactDivision(t *testing.T) {
	// 8 accelerators installed, 800000 millicores total: exactly 100000 millicores/accelerator.
	got, err := FairShare(800000, 2, 8)
	if err != nil {
		t.Fatalf("FairShare: %v", err)
	}
	if got != 200000 {
		t.Errorf("FairShare exact = %d, want 200000", got)
	}
}

func TestFairShareFloorsNonExact(t *testing.T) {
	// 3 accelerators installed, 5 canonical units total, holding 2: true share is 10/3 = 3.33,
	// must floor to 3, never ceiling to 4 — a resolved "max" built from this value must never be
	// judged as exceeding its own fair share later (see FairShare's doc comment).
	got, err := FairShare(5, 2, 3)
	if err != nil {
		t.Fatalf("FairShare: %v", err)
	}
	if got != 3 {
		t.Errorf("FairShare non-exact = %d, want 3 (floored)", got)
	}
}

// installedAccelerators <= 0 is a hard error, never a silent 0 — see FairShare's doc comment on
// important.md's "one path or error, fail fast" rule. Every caller must recognize "nothing
// installed" itself and skip before calling, not rely on this function to paper over it.
func TestFairShareZeroInstalledAcceleratorsIsAHardError(t *testing.T) {
	if _, err := FairShare(1000, 1, 0); err == nil {
		t.Fatal("FairShare(installedAccelerators=0) returned no error, want a hard error (fail fast, not a silent 0)")
	}
	if _, err := FairShare(1000, 1, -1); err == nil {
		t.Fatal("FairShare(installedAccelerators=-1) returned no error, want a hard error")
	}
}

func TestFairShareZeroHeld(t *testing.T) {
	got, err := FairShare(1000, 0, 4)
	if err != nil {
		t.Fatalf("FairShare: %v", err)
	}
	if got != 0 {
		t.Errorf("FairShare zero-held = %d, want 0", got)
	}
}

// A legitimate zero share (nothing held, or nothing to share) must never be conflated with the
// installedAccelerators<=0 hard error — they are different callers' different situations, and
// FairShare must answer each on its own terms rather than one masking the other.
func TestFairShareLegitimateZeroNotConflatedWithInstalledZeroError(t *testing.T) {
	// heldAccelerators == 0: a node reporting the flavor but this job holding none of it — legit 0.
	got, err := FairShare(1000, 0, 8)
	if err != nil || got != 0 {
		t.Fatalf("FairShare(heldAccelerators=0) = (%d, %v), want (0, nil)", got, err)
	}
	// totalCanonical == 0: nothing to share out at all — legit 0, not an error either.
	got, err = FairShare(0, 5, 8)
	if err != nil || got != 0 {
		t.Fatalf("FairShare(totalCanonical=0) = (%d, %v), want (0, nil)", got, err)
	}
	// installedAccelerators == 0 is the ONLY case that must hard-error, regardless of the other
	// two arguments being perfectly ordinary positive values.
	if _, err := FairShare(1000, 5, 0); err == nil {
		t.Fatal("FairShare(installedAccelerators=0) with positive total/held returned no error, want a hard error")
	}
}

// heldAccelerators reaching FairShare negative is not something any current caller can produce
// (loop_resolve.go/loop_disbalance.go's own AcceleratorCount is validated non-negative before
// FairShare is ever called), but the function itself treats it via the same "<=0" branch as
// zero — a defensive 0, not a second hard-error path — which this pins down explicitly so a
// future caller can rely on the documented behavior rather than have to re-discover it.
func TestFairShareNegativeHeldTreatedAsZeroNotAnError(t *testing.T) {
	got, err := FairShare(1000, -3, 8)
	if err != nil {
		t.Fatalf("FairShare(heldAccelerators=-3) returned an error: %v, want (0, nil)", err)
	}
	if got != 0 {
		t.Errorf("FairShare(heldAccelerators=-3) = %d, want 0", got)
	}
}

// Multi-terabyte storage canonical totals times a realistic high per-job accelerator count must
// never approach int64 overflow — this is the shape resolveClusterLocalResources actually
// computes against for a large bare-metal storage node.
func TestFairShareLargeStorageValuesDoNotOverflow(t *testing.T) {
	const tenTB = int64(10) << 40 // 10 TiB in bytes
	got, err := FairShare(tenTB, 64, 8)
	if err != nil {
		t.Fatalf("FairShare: %v", err)
	}
	want := (tenTB * 64) / 8
	if got != want {
		t.Errorf("FairShare large storage = %d, want %d", got, want)
	}
	if got <= 0 {
		t.Errorf("FairShare large storage = %d, want a large positive share (overflow would show up as a wraparound to negative)", got)
	}
}

func TestFootprintScaleMultiNode(t *testing.T) {
	j := JobSpec{
		CPU:              "500m",
		Memory:           "1Gi",
		Storage:          "2Gi",
		AcceleratorCount: 2,
		AcceleratorType:  "example.com/product=test-accelerator",
		NumNodes:         4,
	}
	fp, err := j.Footprint(j.AcceleratorType)
	if err != nil {
		t.Fatalf("Footprint: %v", err)
	}
	// CPU: 500 millicores/node * 4 nodes = 2000.
	if got := fp[ResourceKey{Kind: ResourceKindCPU}]; got != 2000 {
		t.Errorf("cpu millicores = %d, want 2000", got)
	}
	// Accelerator: 2 accelerators/node * 4 nodes = 8, matches TotalAccelerators(). Flavor key uses
	if got := fp[ResourceKey{Kind: ResourceKindAccelerator, Flavor: "example.com/product=test-accelerator"}]; got != int64(j.TotalAccelerators()) {
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
	fp, err := j.Footprint(j.AcceleratorType)
	if err != nil {
		t.Fatalf("Footprint: %v", err)
	}
	if got := fp[ResourceKey{Kind: ResourceKindCPU}]; got != 250 {
		t.Errorf("cpu millicores = %d, want 250 (fractional precision lost)", got)
	}
}

func TestFootprintMalformedQuantityRejected(t *testing.T) {
	j := JobSpec{CPU: "not-a-quantity"}
	if _, err := j.Footprint(""); err == nil {
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
