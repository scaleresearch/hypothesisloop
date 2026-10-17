package scheduler

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetExperiment returns one experiment with its filed summary, the runtime's latest phase
// detail (why a non-running job's container hasn't started) and where its workload ran merged in
// live from the metrics store — never persisted to PostgreSQL, so a stale explanation can never outlive what actually
// happened. Returns nil when no such experiment exists.
func (s *Service) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if exp == nil {
		return nil, nil
	}
	if err := s.NameJobs(ctx, exp); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if exp.Summary, err = s.store.FindingSummary(ctx, id); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	reason, message, restartCount, found, err := metricsdb.GetLatestPhaseDetail(ctx, s.metricsDBURL, id, exp.ClusterName, exp.AttemptCount)
	if err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: phase detail: %w", err)
	}
	if found {
		exp.PhaseDetail = &domain.PhaseDetail{Reason: reason, Message: message, RestartCount: restartCount}
	}
	if err := s.mergeRanOn(ctx, []*domain.Experiment{exp}); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if err := s.mergeBuildProgress(ctx, []*domain.Experiment{exp}); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if err := s.mergeMetrics(ctx, []*domain.Experiment{exp}); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if err := s.judgeRankable(ctx, exp); err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	return exp, nil
}

// judgeRankable says on a COMPLETED job of a platform experiment with a constraint metric
// whether its values rank, as every ranking judges it.
func (s *Service) judgeRankable(ctx context.Context, exp *domain.Experiment) error {
	if exp.Status != domain.StatusCompleted {
		return nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
	if err != nil {
		return err
	}
	if pe == nil || !slices.ContainsFunc(pe.Metrics, func(m domain.MetricDefinition) bool { return m.EffectiveRole() == domain.MetricRoleConstraint }) {
		return nil
	}
	unrankable, err := metricsdb.UnrankableJobs(ctx, s.metricsDBURL, pe, map[string]bool{exp.ID: true})
	if err != nil {
		return err
	}
	reason, broke := unrankable[exp.ID]
	rankable := !broke
	exp.Rankable, exp.NotRankableReason = &rankable, reason
	return nil
}

// mergeMetrics attaches what each experiment reported on its platform experiment's declared
// metrics, read live from the metrics store.
func (s *Service) mergeMetrics(ctx context.Context, exps []*domain.Experiment) error {
	return metricsdb.PopulateJobMetrics(ctx, s.metricsDBURL, func(ctx context.Context, id string) ([]domain.MetricDefinition, bool, error) {
		pe, err := s.store.GetPlatformExperiment(ctx, id)
		if err != nil || pe == nil {
			return nil, false, err
		}
		return pe.Metrics, true, nil
	}, exps)
}

// mergePhaseDetails attaches each experiment's latest phase detail in one batched metrics-store
// query, the list-endpoint equivalent of GetExperiment's single-ID merge — watching many jobs
// must not cost one query per job.
func (s *Service) mergePhaseDetails(ctx context.Context, exps []*domain.Experiment) error {
	if len(exps) == 0 {
		return nil
	}
	ids := make([]string, len(exps))
	for i, exp := range exps {
		ids[i] = exp.ID
	}
	details, err := metricsdb.GetLatestPhaseDetailBatch(ctx, s.metricsDBURL, ids)
	if err != nil {
		return fmt.Errorf("scheduler.mergePhaseDetails: %w", err)
	}
	for _, exp := range exps {
		if d, found := details[exp.ID]; found {
			exp.PhaseDetail = &domain.PhaseDetail{Reason: d.Reason, Message: d.Message, RestartCount: d.RestartCount}
		}
	}
	return nil
}

// mergeBuildProgress says in the not_admitted_message of each job that waits for its environment
// where its build runs, its attempt and the step it last reported (buildingMessage).
func (s *Service) mergeBuildProgress(ctx context.Context, exps []*domain.Experiment) error {
	envs := map[string]*domain.Environment{}
	for _, exp := range exps {
		if exp.NotAdmittedReason != domain.NotAdmittedEnvironmentBuilding {
			continue
		}
		env, read := envs[exp.EnvironmentKey]
		if !read {
			var err error
			if env, err = s.store.GetEnvironment(ctx, exp.EnvironmentKey); err != nil {
				return fmt.Errorf("scheduler.mergeBuildProgress: %w", err)
			}
			if env == nil {
				return fmt.Errorf("scheduler.mergeBuildProgress: job %s names environment %s, which does not exist", exp.ID, exp.EnvironmentKey)
			}
			envs[exp.EnvironmentKey] = env
		}
		if env.State == domain.EnvironmentBuilding {
			exp.NotAdmittedMessage = buildingMessage(env, true)
		}
	}
	return nil
}

// mergeRanOn attaches where each experiment's workload ran, in one metrics-store query for all
// of them: the read of one job and the read of a page say it the same way.
func (s *Service) mergeRanOn(ctx context.Context, exps []*domain.Experiment) error {
	ranOn, err := metricsdb.LatestRanOn(ctx, s.metricsDBURL, exps, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("scheduler.mergeRanOn: %w", err)
	}
	for _, exp := range exps {
		exp.RanOn = ranOn[exp.ID]
	}
	return nil
}

// NameJobs completes each job that is about to be answered: its actor's name, its agent's name
// and whether its summary is due.
func (s *Service) NameJobs(ctx context.Context, jobs ...*domain.Experiment) error {
	if len(jobs) == 0 {
		return nil
	}
	actors := make([]*domain.Actor, len(jobs))
	for i, job := range jobs {
		actors[i] = &job.Actor
	}
	if err := s.store.NameActors(ctx, actors); err != nil {
		return err
	}
	return s.store.DescribeJobs(ctx, jobs)
}
