package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

type admittingStore struct{ poolOnlyStore }

func (admittingStore) GetRunningAndQueued(context.Context, string) ([]*domain.Experiment, error) {
	return nil, nil
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

// repeatedQuota answers every admission as the transaction answers a submission key it has seen.
type repeatedQuota struct {
	refusingQuota
	stored string
}

func (q repeatedQuota) AdmitExperiment(_ context.Context, exp *domain.Experiment, _ db.SubmissionRateLimit, _ func(context.Context) error) (db.AdmitDecision, error) {
	exp.ID = q.stored
	return db.AdmitRepeated, nil
}

type storedJobStore struct {
	admittingStore
	job *domain.Experiment
}

func (s storedJobStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	if id != s.job.ID {
		return nil, nil
	}
	return s.job, nil
}

// A submit stored after hl's timeout was sent again and a second job held quota. A submit whose
// key the agent already used is answered with the job the first created, as it is now, and wakes
// no admission: the service has no loop here, so waking one would panic.
func TestASubmitRepeatedWithItsKeyIsAnsweredWithTheJobTheFirstCreated(t *testing.T) {
	stored := humanIdeaSubmission()
	stored.ID, stored.Status, stored.EstimatedCostAccH = "job-first", domain.StatusRunning, 1
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: storedJobStore{job: stored}, hypotheses: humanHypothesis("pe-1"),
		quota: repeatedQuota{stored: stored.ID}}
	again := humanIdeaSubmission()
	again.SubmissionKey = "k1"
	if err := s.Submit(context.Background(), again); err != nil || again.ID != "job-first" || again.Status != domain.StatusRunning {
		t.Fatalf("got = %v with job %s %s, want the stored job-first, RUNNING", err, again.ID, again.Status)
	}
}

// insertingQuota admits every job as the transaction does one it inserts.
type insertingQuota struct{ refusingQuota }

func (insertingQuota) AdmitExperiment(context.Context, *domain.Experiment, db.SubmissionRateLimit, func(context.Context) error) (db.AdmitDecision, error) {
	return db.AdmitInserted, nil
}

type wokenLoop struct{ woken *int }

func (l wokenLoop) Trigger() { *l.woken++ }

// A job taken wakes the admission loop, which judges it at once.
func TestASubmitTakenWakesTheAdmissionLoopOnce(t *testing.T) {
	woken := 0
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"),
		quota: insertingQuota{}, loop: wokenLoop{&woken}}
	if err := s.Submit(context.Background(), humanIdeaSubmission()); err != nil || woken != 1 {
		t.Fatalf("got = %v after waking the loop %d times, want the job taken and the loop woken once", err, woken)
	}
}

// The transaction's refusals were told apart by the first words of their text, and every one
// that matched none was answered 402 insufficient_quota with a stage outlook: a cut agent, and
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
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"),
			quota: refusingQuota{err: &db.AdmitRefusal{Reason: reason, Message: "the sentence of " + reason}}}
		var refused *AdmissionError
		if err := s.Submit(context.Background(), humanIdeaSubmission()); !errors.As(err, &refused) || refused.Reason != reason || AdmissionStatus(refused.Reason) != status {
			t.Fatalf("%s: got = %v, want that reason answered %d", reason, err, status)
		}
		if outlook := strings.Contains(refused.Message, "share"); outlook != (reason == db.RefusedInsufficientQuota) || !strings.HasPrefix(refused.Message, "the sentence of "+reason) {
			t.Errorf("%s: message %q, want its own sentence, with the stage outlook for a quota refusal only", reason, refused.Message)
		}
	}

	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{}, hypotheses: humanHypothesis("pe-1"),
		quota: refusingQuota{err: errors.New("connection reset")}}
	var refused *AdmissionError
	if err := s.Submit(context.Background(), humanIdeaSubmission()); err == nil || errors.As(err, &refused) {
		t.Errorf("a failure of the transaction: got = %v, want a platform error and no refusal", err)
	}
}
