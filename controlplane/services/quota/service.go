package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"go.uber.org/zap"
)

// Store is the persistence interface required by the quota Service.
type Store interface {
	CreateAgent(ctx context.Context, agent *domain.Agent) error
	ListAgents(ctx context.Context) ([]*domain.Agent, error)
	GetAgentLedger(ctx context.Context, agentID string) ([]*domain.CreditLedgerEntry, error)
}

// AgentProvisioner sets up any per-agent resources a scheduling backend needs at agent
// registration time. The native backend (workload.JobWorkloadClient/ClusterSet) has no
// per-agent backend object to create — shared queues/priority classes are enough — so this is
// currently a no-op there. A different backend (e.g. one that models per-agent quota as a
// native object) would do real work here; subset of workload.Backend.
type AgentProvisioner interface {
	ProvisionAgent(ctx context.Context, agentID string) error
}

// Service implements the quota domain logic.
type Service struct {
	store       Store
	provisioner AgentProvisioner // nil = skip backend provisioning
	logger      *zap.Logger
}

// NewService constructs a quota Service. provisioner may be nil if running without
// a live cluster connection (backend provisioning will be skipped on agent registration).
func NewService(store Store, provisioner AgentProvisioner, logger *zap.Logger) *Service {
	return &Service{store: store, provisioner: provisioner, logger: logger}
}

// RegisterAgent creates a new agent and provisions its backend-side namespace/queue equivalent.
func (s *Service) RegisterAgent(ctx context.Context, id, name string) (*domain.Agent, error) {
	agent := &domain.Agent{
		ID:               id,
		Name:             name,
		PerformanceScore: 0.5,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return nil, fmt.Errorf("quota.RegisterAgent: %w", err)
	}
	if s.provisioner != nil {
		if err := s.provisioner.ProvisionAgent(ctx, id); err != nil {
			s.logger.Warn("quota.RegisterAgent: cluster provisioning failed",
				zap.String("id", id), zap.Error(err))
		}
	}
	s.logger.Info("agent registered", zap.String("id", id), zap.String("name", name))
	return agent, nil
}

// ListBalances returns every registered agent's all-time credit_ledger balance. See
// domain.AgentBalance's doc comment: nothing currently writes to credit_ledger, so Balance
// is always 0 today — real consumption tracking lives in per-platform-experiment quotas
// (PlatformExperimentsService/AgentQuota) instead.
func (s *Service) ListBalances(ctx context.Context) ([]*domain.AgentBalance, error) {
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota.ListBalances: list agents: %w", err)
	}
	out := make([]*domain.AgentBalance, 0, len(agents))
	for _, a := range agents {
		entries, err := s.store.GetAgentLedger(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("quota.ListBalances: ledger for %s: %w", a.ID, err)
		}
		var balance float64
		for _, e := range entries {
			balance += e.Amount
		}
		out = append(out, &domain.AgentBalance{
			AgentID:          a.ID,
			Balance:          balance,
			PerformanceBonus: a.PerformanceScore,
		})
	}
	return out, nil
}

// GetAgentLedger returns agentID's full credit_ledger history (see ListBalances — currently
// always empty).
func (s *Service) GetAgentLedger(ctx context.Context, agentID string) ([]*domain.CreditLedgerEntry, error) {
	entries, err := s.store.GetAgentLedger(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("quota.GetAgentLedger: %w", err)
	}
	return entries, nil
}
