package scheduler

import (
	"context"
	"fmt"
	"maps"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ReasonRunsNoChange refuses a job that runs what its hypothesis builds on, unchanged.
const ReasonRunsNoChange = "runs_no_change"

// ReasonAlreadyMeasured refuses a job a completed job of the run already measured, on metrics
// that are deterministic.
const ReasonAlreadyMeasured = "already_measured"

func sameCodeAndEnv(a, b *domain.Experiment) bool {
	return a.CodeRef == b.CodeRef && maps.Equal(a.Job.Env, b.Job.Env)
}

// refuseRepeats refuses a job that measures nothing new: one that runs the code its hypothesis
// builds on unchanged, and, where every ranking metric is deterministic, one a completed job of
// the run already measured.
func (s *Service) refuseRepeats(ctx context.Context, pe *domain.PlatformExperiment, exp *domain.Experiment) error {
	completed, err := s.store.ListExperiments(ctx, domain.ExperimentFilter{Scope: domain.AllTenants, PlatformExperimentID: pe.ID, Status: domain.StatusCompleted})
	if err != nil {
		return fmt.Errorf("scheduler: completed jobs: %w", err)
	}
	links, err := s.hypotheses.Links(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("scheduler: links of hypothesis: %w", err)
	}
	parents := map[string]bool{}
	for _, link := range links.Out {
		if link.Type == domain.LinkBuildsOn {
			parents[link.HypothesisID] = true
		}
	}
	for _, job := range completed {
		base := parents[job.HypothesisID] || (parents[pe.ID] && job.ID == pe.BaselineJobID)
		if base && job.ID != exp.ID && sameCodeAndEnv(job, exp) {
			return &AdmissionError{Reason: ReasonRunsNoChange, Message: fmt.Sprintf("this job runs what hypothesis %s builds on unchanged (job %s): it measures the base, not %s; commit %s's change", exp.HypothesisID, job.ID, exp.HypothesisID, exp.HypothesisID)}
		}
	}
	if !domain.AllRankingDeterministic(pe.Metrics) {
		return nil
	}
	for _, job := range completed {
		if job.ID != exp.ID && sameCodeAndEnv(job, exp) {
			return &AdmissionError{Reason: ReasonAlreadyMeasured, Message: fmt.Sprintf("job %s already measured this commit and env, and the metrics are deterministic: its result is this job's", job.ID)}
		}
	}
	return nil
}
