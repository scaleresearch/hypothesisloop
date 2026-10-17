package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// sameHypothesisStore answers the completed jobs of a hypothesis and names each job's agent.
type sameHypothesisStore struct {
	poolOnlyStore
	completed []*domain.Experiment
	asked     domain.ExperimentFilter
}

func (s *sameHypothesisStore) ListExperiments(_ context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error) {
	s.asked = filter
	var out []*domain.Experiment
	for _, job := range s.completed {
		if job.HypothesisID == filter.HypothesisID {
			out = append(out, job)
		}
	}
	return out, nil
}

func (s *sameHypothesisStore) DescribeJobs(_ context.Context, jobs []*domain.Experiment) error {
	for _, job := range jobs {
		job.AgentName = strings.Replace(job.AgentID, "ag-", "Researcher ", 1)
	}
	return nil
}

// In a live run two researchers ran the same hypothesis and nobody noticed. A submit names the
// other agents' jobs on its hypothesis, in flight or just completed, and none of its own agent's,
// of other hypotheses or completed long ago.
func TestASubmitNamesOtherAgentsJobsOnTheSameHypothesis(t *testing.T) {
	recent, old := time.Now().Add(-10*time.Minute), time.Now().Add(-3*time.Hour)
	store := &sameHypothesisStore{completed: []*domain.Experiment{
		{ID: "job-done", AgentID: "ag-2", HypothesisID: "hyp-1", Status: domain.StatusCompleted, FinishedAt: &recent},
		{ID: "job-old", AgentID: "ag-3", HypothesisID: "hyp-1", Status: domain.StatusCompleted, FinishedAt: &old},
		{ID: "job-mine-done", AgentID: "ag-1", HypothesisID: "hyp-1", Status: domain.StatusCompleted, FinishedAt: &recent},
	}}
	s := &Service{store: store}
	active := []*domain.Experiment{
		{ID: "job-running", AgentID: "ag-2", HypothesisID: "hyp-1", Status: domain.StatusRunning},
		{ID: "job-mine", AgentID: "ag-1", HypothesisID: "hyp-1", Status: domain.StatusQueued},
		{ID: "job-other", AgentID: "ag-3", HypothesisID: "hyp-2", Status: domain.StatusQueued},
	}
	exp := &domain.Experiment{ID: "job-new", AgentID: "ag-1", PlatformExperimentID: "pe-1", HypothesisID: "hyp-1"}
	said, err := s.sameHypothesis(context.Background(), exp, active)
	if err != nil {
		t.Fatal(err)
	}
	want := "hypothesis hyp-1 is tested by another agent too: job-running of Researcher 2 (RUNNING), job-done of Researcher 2 (COMPLETED). " +
		"If this job repeats that work, cancel it (hl job cancel job-new)"
	if !strings.HasPrefix(said, want) {
		t.Fatalf("said %q, want it to start %q", said, want)
	}
	if store.asked.HypothesisID != "hyp-1" || store.asked.PlatformExperimentID != "pe-1" || store.asked.Status != domain.StatusCompleted {
		t.Errorf("asked %+v, want the completed jobs of hyp-1 in pe-1", store.asked)
	}
	if active[0].AgentName != "" {
		t.Error("naming the jobs changed the active jobs novelty scoring reads")
	}

	alone, err := s.sameHypothesis(context.Background(), &domain.Experiment{ID: "job-new", AgentID: "ag-1", PlatformExperimentID: "pe-1", HypothesisID: "hyp-9"}, active)
	if err != nil || alone != "" {
		t.Fatalf("a hypothesis nobody else tests: said %q (%v), want nothing", alone, err)
	}
}
