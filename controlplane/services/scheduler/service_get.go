package scheduler

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetExperiment returns one experiment with the runtime's latest phase detail (why a non-running
// job's container hasn't started) merged in live from the metrics store — never persisted to
// PostgreSQL, so a stale explanation can never outlive what actually happened. Returns nil when
// no such experiment exists.
func (s *Service) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	exp, err := s.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: %w", err)
	}
	if exp == nil {
		return nil, nil
	}
	reason, message, restartCount, found, err := metricsdb.GetLatestPhaseDetail(ctx, s.metricsDBURL, id, exp.ClusterName, exp.AttemptCount)
	if err != nil {
		return nil, fmt.Errorf("scheduler.GetExperiment: phase detail: %w", err)
	}
	if found {
		exp.PhaseDetail = &domain.PhaseDetail{Reason: reason, Message: message, RestartCount: restartCount}
	}
	return exp, nil
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
