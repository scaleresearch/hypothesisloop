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
	} {
		if got := shareOutlook(tc.pe); !strings.HasPrefix(got, tc.wants) {
			t.Errorf("%s: %q, want it to begin %q", name, got, tc.wants)
		}
	}
}
