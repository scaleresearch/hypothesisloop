package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// Store is the persistence interface required by the quota Service.
type Store interface {
	CreateAgent(ctx context.Context, agent *domain.Agent) error
	ListAgents(ctx context.Context, limit, offset int) ([]*domain.Agent, error)
	CountAgents(ctx context.Context) (int, error)
}

// AgentProvisioner performs any backend-specific durable registration work. The production
// desired-state backend implements this as an explicit no-op because agents share the same
// PostgreSQL schema.
type AgentProvisioner interface {
	ProvisionAgent(ctx context.Context, agentID string) error
}

// Service implements the quota domain logic.
type Service struct {
	store       Store
	provisioner AgentProvisioner
	logger      *zap.Logger
}

// NewService constructs a quota Service. All dependencies are required.
func NewService(store Store, provisioner AgentProvisioner, logger *zap.Logger) *Service {
	if store == nil || provisioner == nil || logger == nil {
		panic("quota: store, provisioner, and logger are required")
	}
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

