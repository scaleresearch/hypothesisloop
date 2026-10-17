package settlement

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const settledL40 = "nvidia.com/gpu.product=NVIDIA-L40"

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
// its count, times the rate of its type, whether it estimated more, less or exactly that. At a
// rate of 1 that is its core-hours.
func TestAJobSettlesAtItsUnitsTimesTheHoursItRan(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{string(domain.CPUType): 1.0, settledL40: 0.25})

	for name, tc := range map[string]struct {
		flavor         domain.AcceleratorType
		count          int
		estimatedHours float64
		observedHours  int
		want           float64
	}{
		"4 cores for an hour":            {domain.CPUType, 4, 1, 1, 4},
		"8 cores, stopped early":         {domain.CPUType, 8, 6, 2, 16},
		"8 cores, ran past the estimate": {domain.CPUType, 8, 1, 2, 16},
		"2 l40, as long as planned":      {settledL40, 2, 2, 2, 1.0},
		"2 l40, stopped early":           {settledL40, 2, 6, 2, 1.0},
	} {
		if got := settle(t, tc.flavor, tc.count, tc.estimatedHours, tc.observedHours); !approx(got, tc.want) {
			t.Errorf("%s: settled at %v, want %v", name, got, tc.want)
		}
	}
}
