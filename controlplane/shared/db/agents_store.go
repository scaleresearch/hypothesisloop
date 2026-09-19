package db

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// AgentsStore provides persistence for domain.Agent.
type AgentsStore struct {
	pool *Pool
}

// NewAgentsStore creates an AgentsStore backed by pool.
func NewAgentsStore(pool *Pool) *AgentsStore {
	return &AgentsStore{pool: pool}
}

// CreateAgent inserts a new agent row.
func (s *AgentsStore) CreateAgent(ctx context.Context, agent *domain.Agent) error {
	const q = `
INSERT INTO agents (id, name, kind, performance_score, created_at)
VALUES ($1, $2, $3, $4, $5)`

	_, err := s.pool.pool.Exec(ctx, q,
		agent.ID, agent.Name, agent.Kind, agent.PerformanceScore, agent.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("agents_store.CreateAgent: %w", err)
	}
	return nil
}

// GetAgent retrieves an agent by ID.
func (s *AgentsStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	const q = `
SELECT id, name, kind, performance_score, created_at
FROM agents
WHERE id = $1`

	agent := &domain.Agent{}
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&agent.ID, &agent.Name, &agent.Kind, &agent.PerformanceScore, &agent.CreatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("agents_store.GetAgent: %w", err)
	}
	return agent, nil
}

// UpdateAgent updates the mutable fields of an agent.
func (s *AgentsStore) UpdateAgent(ctx context.Context, agent *domain.Agent) error {
	const q = `
UPDATE agents SET
	name              = $2,
	performance_score = $3
WHERE id = $1`

	_, err := s.pool.pool.Exec(ctx, q,
		agent.ID, agent.Name, agent.PerformanceScore,
	)
	if err != nil {
		return fmt.Errorf("agents_store.UpdateAgent: %w", err)
	}
	return nil
}

// defaultAgentListLimit and maxAgentListLimit bound every ListAgents read the same way
// ListHypotheses bounds its own — see hypotheses_store.go.
// The default is deliberately far below the maximum. Every caller of this API is an autonomous
// agent whose whole response lands in a bounded context window, so a list that answers "here is
// everything" hands it a truncated or poisoned context and no way to recover. A caller that
// genuinely wants a large page asks for one; a caller that does not think about it gets a page it
// can read, plus the exact total in X-Total-Count telling it what it has not seen.
const (
	defaultAgentListLimit = 20
	maxAgentListLimit     = 200
)

// AgentsFilter narrows ListAgents and CountAgents. Limit and Offset apply to ListAgents only.
type AgentsFilter struct {
	// PlatformExperimentID keeps the agents signed up to that platform experiment.
	PlatformExperimentID string
	// Search matches id and name, case-insensitively, substring match.
	Search string
	Limit  int
	Offset int
}

func agentFilterClauses(filter AgentsFilter) (string, []any) {
	where := "TRUE"
	args := []any{}
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
// experiment_top3. Limit is defaulted and clamped to [1, maxAgentListLimit]; limit<=0 uses the default.
func (s *AgentsStore) ListAgents(ctx context.Context, filter AgentsFilter) ([]*domain.Agent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAgentListLimit
	} else if limit > maxAgentListLimit {
		limit = maxAgentListLimit
	}

	where, args := agentFilterClauses(filter)
	args = append(args, limit, filter.Offset)
	q := fmt.Sprintf(`
SELECT a.id, a.name, a.kind, a.performance_score, a.created_at,
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
			&agent.ID, &agent.Name, &agent.Kind, &agent.PerformanceScore, &agent.CreatedAt,
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

// CountAgents returns the number of agents matching filter, ignoring limit/offset — the
// X-Total-Count a paginating caller shows.
func (s *AgentsStore) CountAgents(ctx context.Context, filter AgentsFilter) (int, error) {
	where, args := agentFilterClauses(filter)
	var total int
	if err := s.pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents a WHERE `+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("agents_store.CountAgents: %w", err)
	}
	return total, nil
}
