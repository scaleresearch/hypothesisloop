package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AgentsStore provides persistence for domain.Agent.
type AgentsStore struct {
	pool *Pool
}

// NewAgentsStore creates an AgentsStore backed by pool.
func NewAgentsStore(pool *Pool) *AgentsStore {
	return &AgentsStore{pool: pool}
}

// ErrAgentExists reports that an agent with this id already exists.
var ErrAgentExists = errors.New("agent already exists")

// CreateAgent inserts a new agent row: ErrAgentExists for an id an agent already has.
func (s *AgentsStore) CreateAgent(ctx context.Context, agent *domain.Agent) error {
	const q = `
INSERT INTO agents (id, name, org_id, created_at)
VALUES ($1, $2, $3, $4)`

	_, err := s.pool.pool.Exec(ctx, q,
		agent.ID, agent.Name, agent.OrgID, agent.CreatedAt,
	)
	// The primary key is the table's only unique constraint.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %s", ErrAgentExists, agent.ID)
	}
	if err != nil {
		return fmt.Errorf("agents_store.CreateAgent: %w", err)
	}
	return nil
}

// GetAgent retrieves an agent by ID.
func (s *AgentsStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	const q = `
SELECT id, name, org_id, created_at
FROM agents
WHERE id = $1`

	agent := &domain.Agent{}
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&agent.ID, &agent.Name, &agent.OrgID, &agent.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("agents_store.GetAgent: %w", err)
	}
	return agent, nil
}

// defaultAgentListLimit is the page of a ListAgents read that states none, and MaxAgentListLimit
// the largest page GET /agents takes: the handler refuses a larger one, so the store never
// shortens a page on its own.
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultAgentListLimit = 20
	MaxAgentListLimit     = 200
)

// AgentsFilter narrows ListAgents and CountAgents. Limit and Offset apply to ListAgents only.
type AgentsFilter struct {
	// PlatformExperimentID keeps the agents signed up to that platform experiment.
	PlatformExperimentID string
	// Search matches id and name, case-insensitively, substring match.
	Search string
	// Scope keeps the agents of the caller's orgs; a project or platform experiment in it keeps
	// those signed up within it.
	Scope  domain.TenantScope
	Limit  int
	Offset int
}

func agentFilterClauses(filter AgentsFilter) (string, []any) {
	where := "TRUE"
	args := []any{}
	if !filter.Scope.AllOrgs {
		args = append(args, filter.Scope.OrgIDs)
		where += fmt.Sprintf(" AND a.org_id = ANY($%d)", len(args))
	}
	if filter.Scope.ProjectID != "" || filter.Scope.PlatformExperimentID != "" {
		var clause string
		clause, args = tenantClause(filter.Scope, "platform_experiment_id", args)
		where += " AND a.id IN (SELECT agent_id FROM experiment_signups WHERE " + clause + ")"
	}
	if filter.PlatformExperimentID != "" {
		args = append(args, filter.PlatformExperimentID)
		where += fmt.Sprintf(" AND a.id IN (SELECT agent_id FROM experiment_signups WHERE platform_experiment_id = $%d)", len(args))
	}
	if filter.Search != "" {
		args = append(args, "%"+filter.Search+"%")
		where += fmt.Sprintf(" AND (a.id ILIKE $%d OR a.name ILIKE $%d)", len(args), len(args))
	}
	return where, args
}

// ListAgents returns the agents matching filter newest first, including top3_count from
// experiment_top3. A Limit of 0 or less is the default page.
func (s *AgentsStore) ListAgents(ctx context.Context, filter AgentsFilter) ([]*domain.Agent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAgentListLimit
	}

	where, args := agentFilterClauses(filter)
	args = append(args, limit, filter.Offset)
	q := fmt.Sprintf(`
SELECT a.id, a.name, a.org_id, a.created_at,
       (SELECT COUNT(*) FROM experiment_top3 t WHERE t.agent_id = a.id) AS top3_count
FROM agents a
WHERE %s
ORDER BY a.created_at DESC, a.id
LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("agents_store.ListAgents: %w", err)
	}
	defer rows.Close()

	var agents []*domain.Agent
	for rows.Next() {
		agent := &domain.Agent{}
		if err := rows.Scan(
			&agent.ID, &agent.Name, &agent.OrgID, &agent.CreatedAt,
			&agent.Top3Count,
		); err != nil {
			return nil, fmt.Errorf("agents_store.ListAgents: scan: %w", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agents_store.ListAgents: rows: %w", err)
	}
	return agents, nil
}

// AgentStats aggregates every agent matching filter, ignoring limit/offset. Total is the
// X-Total-Count a paginating caller shows.
func (s *AgentsStore) AgentStats(ctx context.Context, filter AgentsFilter) (*domain.AgentStats, error) {
	where, args := agentFilterClauses(filter)
	stats := &domain.AgentStats{}
	if err := s.pool.pool.QueryRow(ctx, `
SELECT COUNT(*),
       COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM experiment_top3 t WHERE t.agent_id = a.id))
FROM agents a WHERE `+where, args...).Scan(&stats.Total, &stats.Top3Eligible); err != nil {
		return nil, fmt.Errorf("agents_store.AgentStats: %w", err)
	}
	return stats, nil
}
