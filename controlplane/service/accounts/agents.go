package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// AgentStore keeps the registered agents. *db.Store satisfies it.
type AgentStore interface {
	CreateAgent(ctx context.Context, agent *domain.Agent) error
	ListAgents(ctx context.Context, filter db.AgentsFilter) ([]*domain.Agent, error)
	AgentStats(ctx context.Context, filter db.AgentsFilter) (*domain.AgentStats, error)
}

// Agents registers agents whose standing carries from one platform experiment to the next, and
// lists them.
type Agents struct {
	store  AgentStore
	logger *zap.Logger
}

// NewAgents constructs Agents. All dependencies are required.
func NewAgents(store AgentStore, logger *zap.Logger) *Agents {
	if store == nil || logger == nil {
		panic("accounts.NewAgents: store and logger are required")
	}
	return &Agents{store: store, logger: logger}
}

// Register creates the agent id of orgID, refusing an id that is taken.
func (a *Agents) Register(ctx context.Context, id, name, orgID string) (*domain.Agent, error) {
	agent := &domain.Agent{
		ID:        id,
		Name:      name,
		OrgID:     orgID,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.store.CreateAgent(ctx, agent); err != nil {
		if errors.Is(err, db.ErrAgentExists) {
			return nil, refusal.Refuse(http.StatusConflict, "agent_exists", "an agent with id "+id+" already exists: register under another id, or join with this one")
		}
		return nil, auth.Internal(a.logger, "register agent", fmt.Errorf("quota.RegisterAgent: %w", err), zap.String("agentID", id))
	}
	a.logger.Info("agent registered", zap.String("id", id), zap.String("name", name))
	return agent, nil
}

// List is one page of the agents signed up to platformExperimentID, or of every agent in scope
// when it is empty, whose id or name holds search, and how many match in all.
func (a *Agents) List(ctx context.Context, platformExperimentID, search string, scope domain.TenantScope, limit, offset int) ([]*domain.Agent, int, error) {
	if limit < 0 || limit > db.MaxAgentListLimit || offset < 0 {
		return nil, 0, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(db.MaxAgentListLimit)+" and offset must not be negative")
	}
	filter := db.AgentsFilter{PlatformExperimentID: platformExperimentID, Search: search, Scope: scope, Limit: limit, Offset: offset}
	agents, err := a.store.ListAgents(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(a.logger, "list agents", err)
	}
	stats, err := a.store.AgentStats(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(a.logger, "list agents", err)
	}
	return agents, stats.Total, nil
}

// Stats counts the agents List matches, and those with a top-3 placement.
func (a *Agents) Stats(ctx context.Context, platformExperimentID, search string, scope domain.TenantScope) (*domain.AgentStats, error) {
	stats, err := a.store.AgentStats(ctx, db.AgentsFilter{PlatformExperimentID: platformExperimentID, Search: search, Scope: scope})
	if err != nil {
		return nil, auth.Internal(a.logger, "agent stats", err)
	}
	return stats, nil
}
