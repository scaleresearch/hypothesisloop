package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetAgentQuota returns an agent's allocation and current usage for a platform experiment, merging
// the allocation and current desired estimates (Postgres) with observed consumption (metrics DB).
func (s *PlatformExperimentsService) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: populate usage: %w", err)
	}
	if err := s.store.AddDesiredQuotaUsageOne(ctx, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: desired usage: %w", err)
	}
	if err := s.correctRunningCosts(ctx, pe, []*domain.AgentQuota{aq}); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetAgentQuota: running cost correction: %w", err)
	}
	return aq, nil
}

// GetObservedAgentQuota returns an agent's allocation with usage that counts only what has
// actually happened: settled observed consumption plus each running job's observed-elapsed cost.
// No reservation term — deliberately not GetAgentQuota. Admission asks "may I start one more?", which
// no amount of actual state can answer, so it sums reservations. Eviction and billing ask "has
// this budget been spent?", which is a claim about the actual state and nothing else; sharing one
// figure between the two evicts running jobs for budget that only queued work reserved.
//
// It lags real consumption by at most one reconcile interval, and a just-terminated job is
// invisible until settlement writes its cost. Both are undercounts, both self-correct, and both
// err toward letting real work finish — the right direction for an eviction that is irreversible
// and unrefunded.
// RunBudget is what the researchers of pe share: its budget less what its planner spent before
// the proposal was accepted. The budget a person accepts caps everything the platform
// experiment spends, planning included, so shares are carved from what planning left.
func (s *PlatformExperimentsService) RunBudget(ctx context.Context, pe *domain.PlatformExperiment) (float64, error) {
	planning, err := s.PlanningSpend(ctx, pe)
	if err != nil {
		return 0, err
	}
	return max(0, pe.BudgetAcceleratorHours-planning), nil
}

// PlanningSpend is what the planner of pe has spent, zero for one nobody planned.
func (s *PlatformExperimentsService) PlanningSpend(ctx context.Context, pe *domain.PlatformExperiment) (float64, error) {
	planner, err := s.store.PlannerID(ctx, pe.ID)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments.PlanningSpend: %w", err)
	}
	if planner == "" {
		return 0, nil
	}
	planning := &domain.AgentQuota{AgentID: planner, PlatformExperimentID: pe.ID}
	if err := metricsdb.PopulateUsageOne(ctx, s.metricsDBURL, pe.CreatedAt, planning); err != nil {
		return 0, fmt.Errorf("platform_experiments.PlanningSpend: %w", err)
	}
	return planning.Used(), nil
}

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
	if err := s.addRunningActualCosts(ctx, pe, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetObservedAgentQuota: %w", err)
	}
	return aq, nil
}

// AgentQuotaView is an agent's quota as the quota reads answer it.
type AgentQuotaView struct {
	ID                         string    `json:"id"`
	AgentID                    string    `json:"agent_id"`
	PlatformExperimentID       string    `json:"platform_experiment_id"`
	ShareAcceleratorHours      float64   `json:"share_accelerator_hours" doc:"All the agent may spend, on guaranteed and burst jobs together: its part of the budget released so far."`
	GuaranteedAcceleratorHours float64   `json:"guaranteed_accelerator_hours" doc:"How much of the share may run at guaranteed priority."`
	BurstAcceleratorHours      float64   `json:"burst_accelerator_hours" doc:"How much of the share may run at burst priority, on idle capacity. Burst hours are part of the share and never on top of it."`
	UsedGuaranteedAccH         float64   `json:"used_guaranteed_acch" doc:"Spent at guaranteed priority, with a job that has not started at its estimate and a running one at what it has spent so far."`
	UsedBurstAccH              float64   `json:"used_burst_acch" doc:"The same at burst priority."`
	RemainingGuaranteed        float64   `json:"remaining_guaranteed_accelerator_hours" doc:"The largest estimated_cost_acch a guaranteed job submitted now is admitted with. A submit holds the whole estimate of every job that has not ended, so this is less than the share minus what is used while jobs run."`
	RemainingBurst             float64   `json:"remaining_burst_accelerator_hours" doc:"The same for a burst job."`
	CreatedAt                  time.Time `json:"created_at"`
}

// QuotaView returns one agent's quota view, nil when it has none.
func (s *PlatformExperimentsService) QuotaView(ctx context.Context, agentID, platformExpID string) (*AgentQuotaView, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.QuotaView: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.QuotaView: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("platform_experiments.QuotaView: platform experiment %s not found", platformExpID)
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.QuotaView: populate usage: %w", err)
	}
	views, err := s.quotaViews(ctx, pe, []*domain.AgentQuota{aq})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.QuotaView: %w", err)
	}
	return views[0], nil
}

// ListQuotas returns every agent's quota view for a platform experiment, with consumption read
// from the metrics store in one batched query.
func (s *PlatformExperimentsService) ListQuotas(ctx context.Context, platformExpID string) ([]*AgentQuotaView, error) {
	quotas, err := s.store.ListAgentQuotas(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: %w", err)
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: get platform experiment: %w", err)
	}
	if pe == nil {
		return nil, ErrPlatformExperimentNotFound
	}
	if err := metricsdb.PopulateUsage(ctx, s.usage.URL(), pe.CreatedAt, platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: populate usage: %w", err)
	}
	views, err := s.quotaViews(ctx, pe, quotas)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: %w", err)
	}
	return views, nil
}

// quotaViews turns quotas that carry observed consumption into views. What remains is read
// where the quotas hold what admission counts, every job that has not ended at its whole
// estimate, and what is used after a running job's estimate gives way to what it has spent.
func (s *PlatformExperimentsService) quotaViews(ctx context.Context, pe *domain.PlatformExperiment, quotas []*domain.AgentQuota) ([]*AgentQuotaView, error) {
	if err := s.store.AddDesiredQuotaUsage(ctx, pe.ID, quotas); err != nil {
		return nil, fmt.Errorf("desired usage: %w", err)
	}
	views := make([]*AgentQuotaView, len(quotas))
	for i, q := range quotas {
		views[i] = &AgentQuotaView{
			ID: q.ID, AgentID: q.AgentID, PlatformExperimentID: q.PlatformExperimentID, CreatedAt: q.CreatedAt,
			ShareAcceleratorHours:      q.Share(),
			GuaranteedAcceleratorHours: q.GuaranteedAcceleratorHours,
			BurstAcceleratorHours:      min(q.BurstAcceleratorHours, q.Share()),
			RemainingGuaranteed:        q.Remaining(domain.CapacityGuaranteed),
			RemainingBurst:             q.Remaining(domain.CapacityBurst),
		}
	}
	if err := s.correctRunningCosts(ctx, pe, quotas); err != nil {
		return nil, fmt.Errorf("running cost correction: %w", err)
	}
	for i, q := range quotas {
		views[i].UsedGuaranteedAccH, views[i].UsedBurstAccH = q.UsedGuaranteedAccH, q.UsedBurstAccH
	}
	return views, nil
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
// answer by refreshing priority alone. createDataKey runs only for an inserted row, inside the
// transaction (see db.AdmitExperimentTx).
func (s *PlatformExperimentsService) AdmitExperiment(ctx context.Context, exp *domain.Experiment, rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (db.AdmitDecision, error) {
	// Read before the transaction opens: created_at never changes, and a pool read made while the
	// transaction holds its own connection needs a second one, which a burst of submissions as
	// large as the pool can never get.
	pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
	if err != nil {
		return db.AdmitRejected, fmt.Errorf("platform_experiments.AdmitExperiment: get platform experiment: %w", err)
	}
	if pe == nil {
		return db.AdmitRejected, fmt.Errorf("platform_experiments.AdmitExperiment: platform experiment %s not found", exp.PlatformExperimentID)
	}
	decision, reason, err := s.store.AdmitExperimentTx(ctx, exp, func(ctx context.Context) (*domain.AgentQuota, error) {
		aq := &domain.AgentQuota{AgentID: exp.AgentID, PlatformExperimentID: exp.PlatformExperimentID}
		if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), pe.CreatedAt, aq); err != nil {
			return nil, err
		}
		return aq, nil
	}, rateLimit, createDataKey)
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
		func(ctx context.Context, agentID, platformExpID string, platformExpStart time.Time) (*domain.AgentQuota, error) {
			aq := &domain.AgentQuota{AgentID: agentID, PlatformExperimentID: platformExpID}
			if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), platformExpStart, aq); err != nil {
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
