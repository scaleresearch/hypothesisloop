package scheduler

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// runBestStore answers the completed jobs, the competitors and each job by id.
type runBestStore struct {
	poolOnlyStore
	jobs        map[string]*domain.Experiment
	competitors []string
}

func (s *runBestStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	completed := map[string]bool{}
	for id := range s.jobs {
		completed[id] = true
	}
	return completed, nil
}

func (s *runBestStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, nil
	}
	return s.competitors, nil
}

func (s *runBestStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	return s.jobs[id], nil
}

// fixedStandings answers every agent's best as given, for the completed jobs it is asked about.
type fixedStandings map[string]metricsdb.AgentBest

func (f fixedStandings) BestPerAgentOnMetric(_ context.Context, _ *domain.PlatformExperiment, _ domain.MetricDefinition, completed map[string]bool) (map[string]metricsdb.AgentBest, []string, error) {
	out := map[string]metricsdb.AgentBest{}
	for agent, best := range f {
		if completed[best.ExperimentID] {
			out[agent] = best
		}
	}
	return out, nil, nil
}

// In a live run an executor's subagent submitted on a branch made before the run's best existed
// and had to redo the job. A submit's answer names the best, the commit it ran and its branch, for
// the submitter's checkout to tell whether its commit contains it.
func TestASubmitNamesTheRunsBestJobAndItsCommit(t *testing.T) {
	const repo = "https://example.com/repo.git@"
	store := &runBestStore{competitors: []string{"ag-2", "ag-1"}, jobs: map[string]*domain.Experiment{
		"job-best":     {ID: "job-best", AgentID: "ag-2", CodeRef: repo + "bbbb", Branch: "hl/pe-1/r2-stack"},
		"job-second":   {ID: "job-second", AgentID: "ag-1", CodeRef: repo + "aaaa", Branch: "hl/pe-1/r1"},
		"job-baseline": {ID: "job-baseline", AgentID: "ag-base", CodeRef: repo + "0000", Branch: "hl/pe-1/baseline"},
	}}
	s := &Service{store: store, standings: fixedStandings{
		"ag-2":    {ExperimentID: "job-best", Value: 9.048},
		"ag-1":    {ExperimentID: "job-second", Value: 8.9656},
		"ag-base": {ExperimentID: "job-baseline", Value: 12},
	}}
	pe := &domain.PlatformExperiment{ID: "pe-1", Metrics: []domain.MetricDefinition{
		{Key: "runtime_s", Direction: "minimize", Role: domain.MetricRoleConstraint},
		{Key: "ratio", Direction: "maximize"},
	}}

	got, err := s.runBest(context.Background(), pe, &domain.Experiment{ID: "job-new", AgentID: "ag-1", CodeRef: repo + "cccc"})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.RunBest{JobID: "job-best", AgentID: "ag-2", Metric: "ratio", Value: 9.048, CodeRef: repo + "bbbb", Branch: "hl/pe-1/r2-stack"}
	if got == nil || *got != want {
		t.Fatalf("run best %+v, want %+v: the competitors' best on the first ranking metric, never the baseline's", got, want)
	}

	same, err := s.runBest(context.Background(), pe, &domain.Experiment{ID: "job-new", AgentID: "ag-1", CodeRef: repo + "bbbb"})
	if err != nil || same != nil {
		t.Fatalf("a job on the best's own commit: %+v (%v), want nothing to say", same, err)
	}

	empty := &Service{store: &runBestStore{competitors: []string{"ag-1"}}, standings: fixedStandings{}}
	none, err := empty.runBest(context.Background(), pe, &domain.Experiment{ID: "job-new", AgentID: "ag-1", CodeRef: repo + "cccc"})
	if err != nil || none != nil {
		t.Fatalf("a run with no completed job: %+v (%v), want nothing to say", none, err)
	}
}

// Equal bests rank by agent id, as the standings order them.
func TestTheRunsBestBreaksATieByAgentID(t *testing.T) {
	store := &runBestStore{competitors: []string{"ag-b", "ag-a"}, jobs: map[string]*domain.Experiment{
		"job-a": {ID: "job-a", CodeRef: "r@a"},
		"job-b": {ID: "job-b", CodeRef: "r@b"},
	}}
	s := &Service{store: store, standings: fixedStandings{"ag-a": {ExperimentID: "job-a", Value: 1}, "ag-b": {ExperimentID: "job-b", Value: 1}}}
	pe := &domain.PlatformExperiment{ID: "pe-1", Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}}
	got, err := s.runBest(context.Background(), pe, &domain.Experiment{ID: "job-new", CodeRef: "r@c"})
	if err != nil || got == nil || got.JobID != "job-a" {
		t.Fatalf("run best %+v (%v), want job-a of ag-a", got, err)
	}
}
