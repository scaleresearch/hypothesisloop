package quota

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetQuota returns an agent's allocation and current usage for a platform experiment, merging
// the allocation and current desired estimates (Postgres) with observed consumption (metrics DB).
func (s *PlatformExperimentsService) GetQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: populate usage: %w", err)
	}
	if err := s.store.AddDesiredQuotaUsageOne(ctx, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: desired usage: %w", err)
	}
	if err := s.correctRunningCosts(ctx, platformExpID, []*domain.AgentQuota{aq}); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: running cost correction: %w", err)
	}
	return aq, nil
}

// GetAgentQuota is an alias for GetQuota satisfying the scheduler's QuotaService interface.
func (s *PlatformExperimentsService) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	return s.GetQuota(ctx, agentID, platformExpID)
}

// GetObservedAgentQuota returns an agent's allocation with usage that counts only what has
// actually happened: settled observed consumption plus each running job's observed-elapsed cost.
// No reservation term — deliberately not GetQuota. Admission asks "may I start one more?", which
// no amount of actual state can answer, so it sums reservations. Eviction and billing ask "has
// this budget been spent?", which is a claim about the actual state and nothing else; sharing one
// figure between the two evicts running jobs for budget that only queued work reserved.
//
// It lags real consumption by at most one reconcile interval, and a just-terminated job is
// invisible until settlement writes its cost. Both are undercounts, both self-correct, and both
// err toward letting real work finish — the right direction for an eviction that is irreversible
// and unrefunded.
func (s *PlatformExperimentsService) GetObservedAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: populate usage: %w", err)
	}
	if err := s.addRunningActualCosts(ctx, platformExpID, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: %w", err)
	}
	return aq, nil
}

// ListQuotas returns every agent's allocation and current usage for a platform experiment,
// merging the allocation (Postgres) with consumption (metrics DB) in one batched usage query.
func (s *PlatformExperimentsService) ListQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	quotas, err := s.store.ListAgentQuotas(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: %w", err)
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsage(ctx, s.usage.URL(), pe.CreatedAt, platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: populate usage: %w", err)
	}
	if err := s.store.AddDesiredQuotaUsage(ctx, platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: desired usage: %w", err)
	}
	if err := s.correctRunningCosts(ctx, platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: running cost correction: %w", err)
	}
	return quotas, nil
}

type insufficientQuotaError struct{ message string }

func (e *insufficientQuotaError) Error() string           { return e.message }
func (e *insufficientQuotaError) InsufficientQuota() bool { return true }

// concurrencyCapError signals autoscaler.md's max_concurrent_accelerators rejection specifically,
// distinct from a quota (accelerator-hours) rejection, so the scheduler can set
// domain.NotAdmittedConcurrencyCap rather than the generic quota reason.
type concurrencyCapError struct{}

func (e *concurrencyCapError) Error() string                { return domain.NotAdmittedConcurrencyCap }
func (e *concurrencyCapError) ConcurrencyCapExceeded() bool { return true }

// AdmitExperiment atomically validates every quota dimension and inserts the PostgreSQL desired
// state under one per-agent advisory lock. No provisional row is exposed and no cleanup race is
// possible: concurrent submissions observe a strict before-or-after ordering.
// AdmitExperiment reports what the transaction decided: whether the desired-state row was
// inserted, or the id was already QUEUED and this is a legal re-submission the caller should
// answer by refreshing priority alone.
func (s *PlatformExperimentsService) AdmitExperiment(ctx context.Context, exp *domain.Experiment, rateLimit db.SubmissionRateLimit) (db.AdmitDecision, error) {
	decision, reason, err := s.store.AdmitExperimentTx(ctx, exp, func(ctx context.Context) (*domain.AgentQuota, error) {
		aq := &domain.AgentQuota{AgentID: exp.AgentID, PlatformExperimentID: exp.PlatformExperimentID}
		pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
		if err != nil {
			return nil, err
		}
		if pe == nil {
			return nil, fmt.Errorf("platform_experiments.AdmitExperiment: platform experiment %s not found", exp.PlatformExperimentID)
		}
		if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
			return nil, err
		}
		return aq, nil
	}, rateLimit)
	if err != nil {
		return decision, err
	}
	if reason != "" {
		return decision, &insufficientQuotaError{message: reason}
	}
	return decision, nil
}

// ReserveAdmittedFlavor revalidates quota and the platform experiment's concurrency cap
// (autoscaler.md's spend-rate control) before persisting the selected accelerator flavor.
// acceleratorCount is the job's total accelerator footprint (0 for jobs with none).
func (s *PlatformExperimentsService) ReserveAdmittedFlavor(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, acceleratorCount int) error {
	reason, err := s.store.ReserveAdmittedFlavorTx(ctx, experimentID, acceleratorType, estimatedCost, acceleratorCount, s.cfg.DefaultMaxConcurrentAccelerators,
		func(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
			aq := &domain.AgentQuota{AgentID: agentID, PlatformExperimentID: platformExpID}
			pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
			if err != nil {
				return nil, err
			}
			if pe == nil {
				return nil, fmt.Errorf("platform_experiments.ReserveAdmittedFlavor: platform experiment %s not found", platformExpID)
			}
			if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
				return nil, err
			}
			return aq, nil
		})
	if err != nil {
		return err
	}
	if reason == domain.NotAdmittedConcurrencyCap {
		return &concurrencyCapError{}
	}
	if reason != "" {
		return &insufficientQuotaError{message: reason}
	}
	return nil
}

func (s *PlatformExperimentsService) SetObservedUsage(ctx context.Context, exp *domain.Experiment, amounts map[domain.ResourceType]float64) error {
	return s.usage.SetObservedBatch(ctx, exp.AgentID, exp.PlatformExperimentID, exp.ID, exp.CapacityTier, amounts)
}
