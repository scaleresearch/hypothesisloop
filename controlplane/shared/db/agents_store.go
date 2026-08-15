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
INSERT INTO agents (id, name, performance_score, created_at)
VALUES ($1, $2, $3, $4)`

	_, err := s.pool.pool.Exec(ctx, q,
		agent.ID, agent.Name, agent.PerformanceScore, agent.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("agents_store.CreateAgent: %w", err)
	}
	return nil
}

// GetAgent retrieves an agent by ID.
func (s *AgentsStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	const q = `
SELECT id, name, performance_score, created_at
FROM agents
WHERE id = $1`

	agent := &domain.Agent{}
	err := s.pool.pool.QueryRow(ctx, q, id).Scan(
		&agent.ID, &agent.Name, &agent.PerformanceScore, &agent.CreatedAt,
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
const (
	defaultAgentListLimit = 200
	maxAgentListLimit     = 200
)

// ListAgents returns agents ordered by created_at, including top3_count from experiment_top3.
// limit is defaulted and clamped to [1, maxAgentListLimit]; limit<=0 uses the default.
func (s *AgentsStore) ListAgents(ctx context.Context, limit, offset int) ([]*domain.Agent, error) {
	if limit <= 0 {
		limit = defaultAgentListLimit
	} else if limit > maxAgentListLimit {
		limit = maxAgentListLimit
	}

	const q = `
SELECT a.id, a.name, a.performance_score, a.created_at,
       COUNT(t.agent_id) AS top3_count
FROM agents a
LEFT JOIN experiment_top3 t ON t.agent_id = a.id
GROUP BY a.id, a.name, a.performance_score, a.created_at
ORDER BY a.created_at ASC
LIMIT $1 OFFSET $2`

	rows, err := s.pool.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("agents_store.ListAgents: %w", err)
	}
	defer rows.Close()

	var agents []*domain.Agent
	for rows.Next() {
		agent := &domain.Agent{}
		if err := rows.Scan(
			&agent.ID, &agent.Name, &agent.PerformanceScore, &agent.CreatedAt,
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

// CountAgents returns the total number of registered agents, ignoring limit/offset — the
// X-Total-Count a paginating caller shows.
func (s *AgentsStore) CountAgents(ctx context.Context) (int, error) {
	var total int
	if err := s.pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents`).Scan(&total); err != nil {
		return 0, fmt.Errorf("agents_store.CountAgents: %w", err)
	}
	return total, nil
}
