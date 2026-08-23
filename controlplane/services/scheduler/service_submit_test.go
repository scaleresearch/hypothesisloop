package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

func TestEstimatedAcceleratorCostIsZeroForCPUOnlyJob(t *testing.T) {
	exp := &domain.Experiment{AcceleratorCount: 0, EstimatedDurationHours: 2}
	if got := estimatedAcceleratorCost(exp); got != 0 {
		t.Fatalf("estimatedAcceleratorCost() = %v, want 0", got)
	}
}

// listingBytes serves one ListObjectsV2 page holding a single object of the given size — enough
// for the ceiling arithmetic, which only ever asks the store for a total.
func listingBytes(t *testing.T, size int64) *objectstore.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>pe-1/agent/exp/ckpt</Key><Size>%d</Size></Contents></ListBucketResult>`, size)
	}))
	t.Cleanup(srv.Close)
	client, err := objectstore.New(srv.URL, "us-east-1", "hl-data", "key", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The ceiling is checked here and nowhere else. A job is never killed mid-write for exceeding it,
// because a job killed as it saves loses the run it was about to preserve — so the refusal has to
// land on the next submission, before anything has been spent, or it never lands at all.
func TestSubmitRefusesAnAgentAtItsDurableDataCeiling(t *testing.T) {
	s := &Service{dataStore: listingBytes(t, 1200), maxDataBytesPerAgent: 1000}
	exp := &domain.Experiment{ID: "over", AgentID: "agent", PlatformExperimentID: "pe-1"}

	err := s.enforceDataCeiling(context.Background(), exp)
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("enforceDataCeiling = %v, want an AdmissionError so the submitter is told what to delete", err)
	}
	if admission.Reason != ReasonDataQuotaExceeded {
		t.Fatalf("reason = %q, want %q", admission.Reason, ReasonDataQuotaExceeded)
	}
}

// An agent under its ceiling must be admitted on the measurement alone: the figure comes from the
// store, which is the only thing that knows what is really held, and there is no counter in
// PostgreSQL that could drift from the bytes.
func TestSubmitAdmitsAnAgentUnderItsDurableDataCeiling(t *testing.T) {
	s := &Service{dataStore: listingBytes(t, 999), maxDataBytesPerAgent: 1000}
	exp := &domain.Experiment{ID: "under", AgentID: "agent", PlatformExperimentID: "pe-1"}

	if err := s.enforceDataCeiling(context.Background(), exp); err != nil {
		t.Fatalf("enforceDataCeiling = %v, want nil for an agent holding less than the ceiling", err)
	}
}

// errReachedScoring is returned by the first store call Submit makes *after* the hypothesis
// binding. A test getting it back has proved the binding admitted the job, without standing up
// quota, novelty and persistence to watch a row be written.
var errReachedScoring = errors.New("submit reached novelty scoring")

// poolOnlyStore answers just the lookups Submit makes before it reaches the hypothesis binding,
// then stops the pass at scoring. It embeds Store so anything else Submit reaches panics rather
// than silently answering zero.
type poolOnlyStore struct {
	Store
}

func (poolOnlyStore) GetRunningAndQueued(_ context.Context) ([]*domain.Experiment, error) {
	return nil, errReachedScoring
}

func (poolOnlyStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning}, nil
}
func (poolOnlyStore) IsSignedUp(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (poolOnlyStore) IsAgentCut(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (poolOnlyStore) HasUnsummarizedCompleted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// oneHypothesis is the registry reduced to the single row a submission names.
type oneHypothesis struct {
	h *domain.Hypothesis
}

func (s oneHypothesis) GetHypothesis(_ context.Context, _ string) (*domain.Hypothesis, error) {
	return s.h, nil
}

func humanIdeaSubmission() *domain.Experiment {
	retries := 0
	return &domain.Experiment{
		ID: "human-idea-job", AgentID: "agent-1", ProjectID: "proj", PlatformExperimentID: "pe-1",
		HypothesisID: "h-human", Theory: "t", Objective: "o", EstimatedDurationHours: 1,
		CodeRef: "https://example.com/repo.git@" + strings.Repeat("a", 40),
		Job: domain.JobSpec{
			Image: "img", CPU: "1", Memory: "1Gi", Storage: "1Gi", MaxRetries: &retries,
		},
	}
}

// humanHypothesis is the one human-submitted row a submission names, in platformExperimentID.
func humanHypothesis(platformExperimentID string) oneHypothesis {
	return oneHypothesis{h: &domain.Hypothesis{
		ID: "h-human", Source: domain.HypothesisSourceHuman, Author: "Ada",
		PlatformExperimentID: platformExperimentID, Text: "warmup helps",
	}}
}

// An idea nobody can test is inert, and the whole point of a human putting one in the pool is
// that the fleet can act on it. Forcing an agent to register its own near-duplicate instead
// fights the pool's dedup and splits one claim's evidence across two rows, so the binding must
// admit a job against a human row exactly as it does against an agent's.
func TestSubmitAdmitsAnAgentsJobAgainstAHumanSubmittedHypothesis(t *testing.T) {
	s := &Service{store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}

	err := s.Submit(context.Background(), humanIdeaSubmission())
	if !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit against a human-submitted hypothesis: got = %v, want it to pass the binding and reach scoring", err)
	}
}

// Quota is debited from, and standings are ranked by, the experiment's own agent_id. Nothing
// about the hypothesis reaches those paths except its text, so a human author cannot acquire a
// budget or a placement by having an agent run its idea — the run is the agent's.
func TestSubmitAgainstAHumanHypothesisLeavesTheRunningAgentAsTheJobsOnlyOwner(t *testing.T) {
	s := &Service{store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-1")}
	exp := humanIdeaSubmission()

	if err := s.Submit(context.Background(), exp); !errors.Is(err, errReachedScoring) {
		t.Fatalf("Submit: got = %v, want it to reach scoring", err)
	}
	if exp.AgentID != "agent-1" {
		t.Errorf("job owner: got = %v, want %v", exp.AgentID, "agent-1")
	}
	if exp.Hypothesis != "warmup helps" {
		t.Errorf("denormalized hypothesis text: got = %v, want %v", exp.Hypothesis, "warmup helps")
	}
}

// Dropping the human-row refusal must not loosen the check next to it: a hypothesis still has to
// belong to the job's own platform experiment, or a job could point at another program's idea
// pool and contaminate that program's scope and ranking.
func TestSubmitRefusesAJobAgainstAHumanHypothesisFromAnotherPlatformExperiment(t *testing.T) {
	s := &Service{store: poolOnlyStore{}, hypotheses: humanHypothesis("pe-other")}

	err := s.Submit(context.Background(), humanIdeaSubmission())
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("Submit against a cross-experiment hypothesis: got = %v, want an AdmissionError", err)
	}
	if admission.Reason != ReasonMalformed {
		t.Errorf("reason: got = %v, want %v", admission.Reason, ReasonMalformed)
	}
}
