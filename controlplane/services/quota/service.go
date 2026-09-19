package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// Store is the persistence interface required by the quota Service.
type Store interface {
	CreateAgent(ctx context.Context, agent *domain.Agent) error
	ListAgents(ctx context.Context, filter db.AgentsFilter) ([]*domain.Agent, error)
	AgentStats(ctx context.Context, filter db.AgentsFilter) (*domain.AgentStats, error)
}

// Service implements the quota domain logic.
type Service struct {
	store  Store
	logger *zap.Logger
}

// NewService constructs a quota Service. All dependencies are required.
func NewService(store Store, logger *zap.Logger) *Service {
	if store == nil || logger == nil {
		panic("quota: store and logger are required")
	}
	return &Service{store: store, logger: logger}
}

// RegisterAgent creates a new agent.
// kind distinguishes a human participant from an AI agent (domain.AgentKind); an empty kind
// defaults to domain.AgentKindAgent, same as the pre-existing single-kind behavior.
func (s *Service) RegisterAgent(ctx context.Context, id, name string, kind domain.AgentKind) (*domain.Agent, error) {
	if kind == "" {
		kind = domain.AgentKindAgent
	}
	agent := &domain.Agent{
		ID:               id,
		Name:             name,
		Kind:             kind,
		PerformanceScore: 0.5,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return nil, fmt.Errorf("quota.RegisterAgent: %w", err)
	}
	s.logger.Info("agent registered", zap.String("id", id), zap.String("name", name))
	return agent, nil
}
