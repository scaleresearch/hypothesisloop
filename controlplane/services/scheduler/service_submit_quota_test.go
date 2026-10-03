package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// An agent refused for quota asked four times with a guessed lower estimate. Whether waiting
// helps is the other half of what it needs: a share rises at a stage boundary that cuts, and
// nowhere else.
func TestAQuotaRefusalSaysWhetherALaterStageRaisesTheShare(t *testing.T) {
	started := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)
	run := func(current int, stages ...domain.Stage) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: "pe-1", Stages: stages, CurrentStage: current, StartedAt: started, EndsAt: started.Add(10 * time.Hour)}
	}
	cutting := []domain.Stage{{LengthPct: 30, EvictPct: 50}, {LengthPct: 30, EvictPct: 0}, {LengthPct: 40}}
	for name, tc := range map[string]struct {
		pe    *domain.PlatformExperiment
		wants string
	}{
		"one stage":                {run(1, domain.Stage{LengthPct: 100}), "No later stage raises the agent's share"},
		"the last stage":           {run(3, cutting...), "No later stage raises the agent's share"},
		"a stage that cuts nobody": {run(2, cutting...), "No later stage raises the agent's share"},
		"a stage that cuts":        {run(1, cutting...), "Stage 1 of 3 ends by 2026-10-06T23:00:00Z, or sooner as the budget is spent"},
		"a stage the row misnames": {run(0, cutting...), "No later stage raises the agent's share"},
		// The baseline's job is sent before the start: the refusal once read "ends by 0001-01-01T00:00:00Z".
		"before the run starts": {&domain.PlatformExperiment{ID: "pe-1", Stages: cutting, CurrentStage: 1, DurationHours: 10}, "Stage 1 of 3 ends within 3h0m0s of the run's start, and the run has not started, or sooner"},
	} {
		if got := shareOutlook(tc.pe); !strings.HasPrefix(got, tc.wants) {
			t.Errorf("%s: %q, want it to begin %q", name, got, tc.wants)
		}
	}
}
