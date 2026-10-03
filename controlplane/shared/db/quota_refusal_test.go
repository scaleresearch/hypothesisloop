package db

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A refused submit was followed four times by a guessed lower estimate. The refusal says the
// largest one that fits a job of the same size, and stating it is admitted.
func TestAQuotaRefusalNamesTheLargestEstimateThatFits(t *testing.T) {
	job := func(count int, hours float64) *domain.Experiment {
		return &domain.Experiment{CapacityTier: domain.CapacityGuaranteed, AcceleratorType: domain.CPUType, AcceleratorCount: count,
			EstimatedDurationHours: hours, EstimatedCostAccH: float64(count) * hours}
	}
	for name, tc := range map[string]struct {
		exp       *domain.Experiment
		remaining float64
		fits      float64
		says      string
	}{
		"16 cores for 0.2 h with 1.2 left":     {job(16, 0.2), 1.2, 0.075, "The largest estimated_duration_hours a job of this size may state now is 0.075 (270 s)."},
		"16 cores for 0.032 h with 0.434 left": {job(16, 0.03200041666666667), 0.434, 0.0271, "is 0.0271 (98 s)."},
		"16 cores for 0.032 h with 0.108 left": {job(16, 0.03200041666666667), 0.108, 0.0067, "is 0.0067 (24 s)."},
		"nothing left":                         {job(16, 0.2), 0, 0, "No estimated_duration_hours fits a job of this size now."},
		"less than a step left":                {job(16, 0.2), 0.001, 0, "No estimated_duration_hours fits a job of this size now."},
	} {
		got := quotaRefusal(tc.exp, tc.remaining, 0, 0)
		if !strings.HasPrefix(got, "insufficient_guaranteed_quota: need ") || !strings.Contains(got, tc.says) {
			t.Errorf("%s: %q, want it to say %q", name, got, tc.says)
		}
		if cost := float64(tc.exp.AcceleratorCount) * tc.fits; cost > tc.remaining {
			t.Errorf("%s: the estimate named costs %g, over the %g remaining", name, cost, tc.remaining)
		}
	}
	if got := quotaRefusal(job(16, 0.2), 1.2, 2, 1.02); !strings.Contains(got, "2 of your jobs that have not ended hold 1.02 by their estimates") {
		t.Errorf("with jobs that hold estimates: %q", got)
	}
}
