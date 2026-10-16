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
func (s *Service) RegisterAgent(ctx context.Context, id, name, orgID string) (*domain.Agent, error) {
	agent := &domain.Agent{
		ID:        id,
		Name:      name,
		OrgID:     orgID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return nil, fmt.Errorf("quota.RegisterAgent: %w", err)
	}
	s.logger.Info("agent registered", zap.String("id", id), zap.String("name", name))
	return agent, nil
}
