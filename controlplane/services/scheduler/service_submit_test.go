package scheduler

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestEstimatedAcceleratorCostIsZeroForCPUOnlyJob(t *testing.T) {
	exp := &domain.Experiment{AcceleratorCount: 0, EstimatedDurationHours: 2}
	if got := estimatedAcceleratorCost(exp); got != 0 {
		t.Fatalf("estimatedAcceleratorCost() = %v, want 0", got)
	}
}
