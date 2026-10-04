package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type measuredStore struct {
	poolOnlyStore
	measured float64
}

func (s measuredStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git", JobEstimatedDurationHours: s.measured}, nil
}

func (measuredStore) GetAgentSignup(context.Context, string, string) (*domain.AgentSignup, error) {
	return nil, nil
}

type baselineStore struct {
	poolOnlyStore
	maxJobHours float64
}

func (s baselineStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpOpen, Repository: "repo.git", RepositoryURL: "https://example.com/repo.git",
		Stages: []domain.Stage{{LengthPct: 100, MaxJobHours: s.maxJobHours}}, CurrentStage: 1}, nil
}

func (baselineStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleBaseline, Flavor: domain.DefaultAgentFlavor}, nil
}

// A baseline job that states no estimate was sized from a read of the quota that counts a running
// job at what it spent, and then refused 402 by the admission, which counts it at its estimate.
// The submit leaves such a job unsized for the admission transaction, with the stage's limit, and
// a stated estimate stands.
func TestABaselineJobThatStatesNoEstimateIsSizedByTheAdmission(t *testing.T) {
	for name, tc := range map[string]struct {
		stated, maxJobHours float64
	}{
		"no estimate":                  {0, 0},
		"no estimate, a per-job limit": {0, 0.125},
		"a stated one stands":          {0.0625, 0.125},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: baselineStore{maxJobHours: tc.maxJobHours}, hypotheses: oneHypothesis{}}
		exp := baselineSubmission()
		exp.EstimatedDurationHours = tc.stated
		wantLimit := tc.maxJobHours
		if tc.stated != 0 {
			wantLimit = 0
		}
		if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) || exp.EstimatedDurationHours != tc.stated || exp.MaxEstimateHours != wantLimit {
			t.Errorf("%s: got = %v with an estimate of %g within %g, want the job taken with %g within %g", name, err, exp.EstimatedDurationHours, exp.MaxEstimateHours, tc.stated, wantLimit)
		}
	}
	if got := admissionHTTPStatus(ReasonInsufficientCredits); got != 402 {
		t.Errorf("a baseline job nothing fits is answered %d, want 402", got)
	}

	// A researcher's job has the measured estimate or states one: the largest that fits would hold
	// its whole share for one trial.
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()
	exp.EstimatedDurationHours = 0
	var refused *AdmissionError
	if err := s.Submit(context.Background(), exp); !errors.As(err, &refused) || refused.Reason != ReasonEstimateRequired {
		t.Errorf("a researcher's job with no estimate and none measured: got = %v, want %s", err, ReasonEstimateRequired)
	}
}

// A control that stated no estimate was given the whole quota, 2.25 h, with 80 minutes left until
// baseline_due_at: it was held as baseline_late at once and its agent cancelled it to submit it
// again with an estimate. Before the run the estimate is bounded by the time left, then by the
// stage's limit alone.
func TestABaselineJobThatStatesNoEstimateEndsByTheBaselineDueTime(t *testing.T) {
	now := time.Date(2026, 10, 8, 20, 2, 0, 0, time.UTC)
	due := now.Add(80 * time.Minute)
	open := func(maxJobHours float64) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{Status: domain.PlatformExpOpen, BaselineDueAt: due,
			Stages: []domain.Stage{{LengthPct: 100, MaxJobHours: maxJobHours}}, CurrentStage: 1}
	}
	for name, tc := range map[string]struct {
		pe   *domain.PlatformExperiment
		want float64
	}{
		"open, no per-job limit":           {open(0), 1.3333},
		"open, a per-job limit under it":   {open(0.5), 0.5},
		"open, a per-job limit over it":    {open(2), 1.3333},
		"open, past its due time":          {&domain.PlatformExperiment{Status: domain.PlatformExpOpen, BaselineDueAt: now.Add(-time.Minute)}, 0},
		"running, the stage's limit alone": {&domain.PlatformExperiment{Status: domain.PlatformExpRunning, BaselineDueAt: due}, 0},
	} {
		if got := unestimatedLimit(tc.pe, now); got != tc.want {
			t.Errorf("%s: got = %g, want %g", name, got, tc.want)
		}
	}
}

// Held to the estimate its spec guessed before anything ran, a baseline ran twice, the second
// time only to carry a corrected one. The estimate of a job that states none is the one the
// platform measured on the baseline; a stated one stands, and with neither the job is refused.
func TestAJobThatStatesNoEstimateTakesTheOneItsExperimentMeasured(t *testing.T) {
	for name, tc := range map[string]struct {
		measured float64
		stated   float64
		reason   string
		want     float64
	}{
		"stated, where one was measured":   {0.025, 0.5, "not_signed_up", 0.5},
		"not stated, and one was measured": {0.025, 0, "not_signed_up", 0.025},
		"not stated, and none measured":    {0, 0, ReasonEstimateRequired, 0},
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, environments: declared{}, store: measuredStore{measured: tc.measured}, hypotheses: humanHypothesis("pe-1")}
		exp := humanIdeaSubmission()
		exp.EstimatedDurationHours = tc.stated
		var admission *AdmissionError
		// The signup check comes after the estimate is settled, and this store signs nobody up.
		if err := s.Submit(context.Background(), exp); !errors.As(err, &admission) || admission.Reason != tc.reason || exp.EstimatedDurationHours != tc.want {
			t.Errorf("%s: got = %v with an estimate of %g, want %s and %g", name, err, exp.EstimatedDurationHours, tc.reason, tc.want)
		}
		if tc.reason == ReasonEstimateRequired && (admissionHTTPStatus(admission.Reason) != 422 || !strings.Contains(admission.Message, "--estimated-duration-hours")) {
			t.Errorf("%s: refused %d %q, want 422 naming the flag", name, admissionHTTPStatus(admission.Reason), admission.Message)
		}
	}
}
