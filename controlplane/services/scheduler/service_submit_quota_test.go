package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
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

type admittingStore struct{ poolOnlyStore }

func (admittingStore) GetRunningAndQueued(context.Context, string) ([]*domain.Experiment, error) {
	return nil, nil
}

type novel struct{}

func (novel) ComputeNovelty(context.Context, string, []*domain.Experiment) (float64, error) {
	return 1, nil
}

// refusingQuota answers every admission with one error of the transaction.
type refusingQuota struct {
	QuotaService
	err error
}

func (refusingQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return nil, nil
}

func (q refusingQuota) AdmitExperiment(context.Context, *domain.Experiment, db.SubmissionRateLimit, func(context.Context) error) (db.AdmitDecision, error) {
	return db.AdmitRejected, q.err
}

// The transaction's refusals were told apart by the first words of their text, and every one
// that matched none was answered 402 insufficient_credits with a stage outlook: a cut agent, and
// one with no quota. Each is answered with its own reason, and only a quota refusal says whether
// a stage raises the share.
func TestEachRefusalOfTheAdmissionTransactionIsAnsweredWithItsOwnReason(t *testing.T) {
	for reason, status := range map[string]int{
		db.RefusedInsufficientQuota: http.StatusPaymentRequired,
		db.RefusedRateLimited:       http.StatusTooManyRequests,
		db.RefusedAgentCut:          http.StatusUnprocessableEntity,
		db.RefusedAgentFinished:     http.StatusUnprocessableEntity,
		db.RefusedNoQuota:           http.StatusUnprocessableEntity,
	} {
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
			quota: refusingQuota{err: &db.AdmitRefusal{Reason: reason, Message: "the sentence of " + reason}}}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != reason || admissionHTTPStatus(refused.Reason) != status {
			t.Fatalf("%s: got = %v, want that reason answered %d", reason, err, status)
		}
		if outlook := strings.Contains(refused.Message, "share"); outlook != (reason == db.RefusedInsufficientQuota) || !strings.HasPrefix(refused.Message, "the sentence of "+reason) {
			t.Errorf("%s: message %q, want its own sentence, with the stage outlook for a quota refusal only", reason, refused.Message)
		}
	}

	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"), novelty: novel{},
		quota: refusingQuota{err: errors.New("connection reset")}}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); err == nil || errors.As(err, &refused) {
		t.Errorf("a failure of the transaction: got = %v, want a platform error and no refusal", err)
	}
}
