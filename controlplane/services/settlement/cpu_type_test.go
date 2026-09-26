package settlement

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	settledCPUClass = "cpu.hypothesisloop.dev/class=EPYC-9354"
	settledL40      = "nvidia.com/gpu.product=NVIDIA-L40"
)

// settle runs one job, estimated at rate x count x estimatedHours of its type, through
// settlement after observedHours of observation, and returns what was written.
func settle(t *testing.T, flavor domain.AcceleratorType, count int, estimatedHours float64, observedHours int) float64 {
	t.Helper()
	now := time.Now().UTC()
	server := aliveServer(t, observedHours*60+1, time.Minute, now)
	defer server.Close()
	usage := &capturedUsage{}
	exp := &domain.Experiment{
		ID: "exp-1", PlatformExperimentID: "pe-1", CreatedAt: now.Add(-24 * time.Hour),
		AcceleratorType: flavor, AcceleratorCount: count, EstimatedDurationHours: estimatedHours,
		EstimatedCostAccH: flavor.Cost() * float64(count) * estimatedHours,
	}
	if err := New(usage, server.URL, 3*time.Minute).Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	return usage.amounts[domain.ResourceAcceleratorHours]
}

// A job on cores settles as a job on accelerators does: the hours it was observed running, times
// its count, times the rate of its type. Eight cores for two hours are 0.16 accelerator-hours at
// 0.01 a core-hour, whether the job estimated more, less or exactly that.
func TestACoreJobSettlesAtItsCoresTimesTheHoursItRan(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{settledCPUClass: 0.01, settledL40: 0.25})
	domain.SetAcceleratorKinds(map[string]domain.TypeKind{settledCPUClass: domain.TypeKindCPU})
	t.Cleanup(func() { domain.SetAcceleratorKinds(nil) })

	for name, tc := range map[string]struct {
		flavor         domain.AcceleratorType
		count          int
		estimatedHours float64
		observedHours  int
		want           float64
	}{
		"8 cores, ran as long as estimated":   {settledCPUClass, 8, 2, 2, 0.16},
		"8 cores, stopped early":              {settledCPUClass, 8, 6, 2, 0.16},
		"8 cores, ran past the estimate":      {settledCPUClass, 8, 1, 2, 0.16},
		"1 core for an hour":                  {settledCPUClass, 1, 1, 1, 0.01},
		"64 cores for 10 hours":               {settledCPUClass, 64, 10, 10, 6.4},
		"2 l40 beside it, as long as planned": {settledL40, 2, 2, 2, 1.0},
		"2 l40 beside it, stopped early":      {settledL40, 2, 6, 2, 1.0},
	} {
		if got := settle(t, tc.flavor, tc.count, tc.estimatedHours, tc.observedHours); !approx(got, tc.want) {
			t.Errorf("%s: settled at %v, want %v", name, got, tc.want)
		}
	}
}

// What an accelerator job settles at does not depend on the catalog holding a CPU class.
func TestAnAcceleratorJobSettlesTheSameBesideACPUClass(t *testing.T) {
	type run struct {
		count          int
		estimatedHours float64
		observedHours  int
	}
	runs := []run{{1, 1, 1}, {2, 6, 2}, {4, 1, 3}, {8, 0.5, 5}}
	settleAll := func() []float64 {
		out := make([]float64, 0, len(runs))
		for _, r := range runs {
			out = append(out, settle(t, settledL40, r.count, r.estimatedHours, r.observedHours))
		}
		return out
	}

	domain.SetAcceleratorRates(map[string]float64{settledL40: 0.25})
	domain.SetAcceleratorKinds(nil)
	before := settleAll()
	domain.SetAcceleratorRates(map[string]float64{settledCPUClass: 0.01, settledL40: 0.25})
	domain.SetAcceleratorKinds(map[string]domain.TypeKind{settledCPUClass: domain.TypeKindCPU})
	t.Cleanup(func() { domain.SetAcceleratorKinds(nil) })
	after := settleAll()

	for i := range runs {
		want := 0.25 * float64(runs[i].count) * float64(runs[i].observedHours)
		if !approx(before[i], want) || !approx(after[i], want) {
			t.Errorf("%+v: settled at %v without a CPU class and %v with one, want %v both times", runs[i], before[i], after[i], want)
		}
	}
}
