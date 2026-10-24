package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// A merged hypothesis is tested under the one it was merged into, and a retired one is off the
// frontier: a job on either is refused with 409, the merged one naming where to test it.
func TestSubmitRefusesAJobOnAMergedOrRetiredHypothesis(t *testing.T) {
	for _, c := range []struct {
		h      domain.Hypothesis
		reason string
		names  string
	}{
		{domain.Hypothesis{Status: domain.HypothesisMerged, MergedInto: "h-into"}, platformapi.ReasonHypothesisMerged, "h-into"},
		{domain.Hypothesis{Status: domain.HypothesisRetired, StatusReason: "the sweep covers it"}, platformapi.ReasonHypothesisRetired, "the sweep covers it"},
	} {
		h := humanHypothesis("pe-1").h
		h.Status, h.MergedInto, h.StatusReason = c.h.Status, c.h.MergedInto, c.h.StatusReason
		s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: poolOnlyStore{}, hypotheses: oneHypothesis{h: h}}
		var refused *AdmissionError
		err := s.Submit(context.Background(), humanIdeaSubmission())
		if !errors.As(err, &refused) || refused.Reason != c.reason || refused.Field != "hypothesis_id" || !strings.Contains(refused.Message, c.names) {
			t.Errorf("a job on a %s hypothesis: got = %v, want %s naming %q", c.h.Status, err, c.reason, c.names)
		}
		if status := AdmissionStatus(c.reason); status != http.StatusConflict {
			t.Errorf("%s answered with: got = %d, want 409", c.reason, status)
		}
	}
}

// curatedOnSecondRead is the registry of a hypothesis a researcher merged after the submit first
// read it open and before the job's row was written.
type curatedOnSecondRead struct {
	oneHypothesis
	reads *int
}

func (s curatedOnSecondRead) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	*s.reads++
	h := *s.h
	if *s.reads > 1 {
		h.Status, h.MergedInto = domain.HypothesisMerged, "h-into"
	}
	return &h, nil
}

// insertedQuota runs what the transaction runs once it wrote the row, and answers its error.
type insertedQuota struct{ refusingQuota }

func (insertedQuota) AdmitExperiment(ctx context.Context, _ *domain.Experiment, _ db.SubmissionRateLimit, inserted func(context.Context) error) (db.AdmitDecision, error) {
	if err := inserted(ctx); err != nil {
		return db.AdmitRejected, err
	}
	return db.AdmitInserted, nil
}

// A merge checked the jobs of its hypothesis, found none and committed while a submit on it was
// between its first read and its insert, and the job ran on a merged hypothesis. The submit reads
// the hypothesis again under the hypothesis's lock, inside the transaction that writes the job.
func TestASubmitReadsItsHypothesisAgainInTheTransactionThatWritesTheJob(t *testing.T) {
	reads := 0
	s := &Service{credits: testCaps, clusters: installedCapacity{}, codeHost: testCodeHost{}, store: admittingStore{},
		hypotheses: curatedOnSecondRead{oneHypothesis: humanHypothesis("pe-1"), reads: &reads}, quota: insertedQuota{}}
	var refused *AdmissionError
	err := s.Submit(context.Background(), humanIdeaSubmission())
	if !errors.As(err, &refused) || refused.Reason != platformapi.ReasonHypothesisMerged || reads != 2 {
		t.Errorf("a job on a hypothesis merged mid-submit: got = %v after %d reads, want %s after 2", err, reads, platformapi.ReasonHypothesisMerged)
	}
}
