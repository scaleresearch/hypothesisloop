package platformexperiments

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// In a live run both executors finished with 11 to 16 minutes of an hour and most of their quota
// left, and the coordinator heard only their reasons. An executor with more than a tenth of
// both left says why it stops, blocked or with nothing left worth a job, or is told what is
// left, and every executor's finish records the time and quota it left, which agent.finished
// carries. No other role is asked.
func TestAnExecutorFinishingEarlySaysWhyAndTheTimeAndQuotaItLeavesAreTold(t *testing.T) {
	newStore := func(role domain.SignupRole, left time.Duration, spent float64) *rosterStore {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.pe.DurationHours, store.pe.EndsAt = 1, time.Now().Add(left)
		store.pe.StartsAt = store.pe.EndsAt.Add(-time.Hour)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: role}}
		store.quota = &domain.AgentQuota{AgentID: "dana", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 3, UsedGuaranteedAccH: spent}
		return store
	}
	finish := func(store *rosterStore, reason string, because domain.FinishBecause) (*domain.AgentSignup, error) {
		svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), 3*time.Minute)
		return svc.FinishAgent(context.Background(), "pe-1", "dana", reason, because, "")
	}

	store := newStore(domain.SignupRoleExecutor, 16*time.Minute+30*time.Second, 0.2)
	_, err := finish(store, "nothing beat the control", "")
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.Reason != "time_and_quota_left" || refused.GetStatus() != http.StatusConflict ||
		!strings.Contains(refused.Message, "16 of the run's 60 minutes and 2.8 of the agent's 3 AccH were left") || !strings.Contains(refused.Message, "--because blocked") || !strings.Contains(refused.Message, "--because nothing_left") ||
		store.signups[0].FinishedAt != nil {
		t.Fatalf("an early finish that says no why: got %v, want 409 time_and_quota_left saying what is left, and no finish", err)
	}
	finished, err := finish(store, "every job needs a toolchain the environment cannot build", domain.FinishBlocked)
	if err != nil || !strings.HasPrefix(finished.FinishedReason, "Blocked: every job needs a toolchain the environment cannot build. When it finished, 16 of the run's 60 minutes") {
		t.Errorf("an early finish that is blocked: got %+v, %v, want finished with why, the reason and what it left", finished, err)
	}
	store = newStore(domain.SignupRoleExecutor, 16*time.Minute+30*time.Second, 0.2)
	finished, err = finish(store, "depth, width and dropout all stayed inside the seed spread", domain.FinishNothingLeft)
	if err != nil || !strings.HasPrefix(finished.FinishedReason, "Nothing left worth a job: depth, width and dropout all stayed inside the seed spread. When it finished, 16 of the run's 60 minutes") {
		t.Errorf("an early finish with nothing left worth a job: got %+v, %v, want finished with why, the reason and what it left", finished, err)
	}

	for name, store := range map[string]*rosterStore{
		"little time left":  newStore(domain.SignupRoleExecutor, 5*time.Minute, 0.2),
		"little quota left": newStore(domain.SignupRoleExecutor, 30*time.Minute, 2.8),
	} {
		finished, err := finish(store, "nothing beat the control", "")
		if err != nil || !strings.HasPrefix(finished.FinishedReason, "nothing beat the control. When it finished, ") {
			t.Errorf("%s: got %+v, %v, want finished with what it left and no why asked", name, finished, err)
		}
	}
	store = newStore(domain.SignupRoleReviewer, 30*time.Minute, 0.2)
	if finished, err := finish(store, "nothing beat the control", ""); err != nil || finished.FinishedReason != "nothing beat the control" {
		t.Errorf("a reviewer: got %+v, %v, want finished with its reason as it gave it", finished, err)
	}
}

// An executor was refused a finish with 4 minutes of 30 left, more than a tenth, when its
// jobs took 2 to 7 minutes, and sat idle until the close. Less time left than the agent's jobs
// typically take, or with none of its own the experiment's, is too little to ask why it stops.
func TestAnExecutorFinishesWithoutSayingWhyWhenNoJobOfItFitsInTheTimeLeft(t *testing.T) {
	took := func(agent string, minutes int) *domain.Experiment {
		end := time.Now().Add(-time.Hour)
		return &domain.Experiment{AgentID: agent, Status: domain.StatusCompleted, CreatedAt: end.Add(-time.Duration(minutes) * time.Minute), FinishedAt: &end}
	}
	finish := func(completed ...*domain.Experiment) error {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.pe.DurationHours, store.pe.EndsAt = 0.5, time.Now().Add(4*time.Minute+30*time.Second)
		store.pe.StartsAt = store.pe.EndsAt.Add(-30 * time.Minute)
		store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Role: domain.SignupRoleExecutor}}
		store.quota = &domain.AgentQuota{AgentID: "dana", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 3, UsedGuaranteedAccH: 0.2}
		store.completed = completed
		svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), 3*time.Minute)
		_, err := svc.FinishAgent(context.Background(), "pe-1", "dana", "nothing beat the control", "", "")
		return err
	}
	for name, completed := range map[string][]*domain.Experiment{
		"its own jobs take longer":          {took("dana", 2), took("dana", 6), took("dana", 7), took("ali", 1)},
		"the experiment's jobs take longer": {took("ali", 5), took("ali", 6)},
	} {
		if err := finish(completed...); err != nil {
			t.Errorf("%s: %v, want finished without saying why", name, err)
		}
	}
	for name, completed := range map[string][]*domain.Experiment{
		"its own jobs are quick": {took("dana", 1), took("dana", 2), took("ali", 20)},
		"no job completed":       nil,
	} {
		var refused *refusal.Refusal
		if err := finish(completed...); !errors.As(err, &refused) || refused.Reason != "time_and_quota_left" {
			t.Errorf("%s: %v, want 409 time_and_quota_left", name, err)
		}
	}
}
