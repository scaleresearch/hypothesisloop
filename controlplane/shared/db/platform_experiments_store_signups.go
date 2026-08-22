package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ---- experiment_signups ----

// Signup records an agent's intent to participate in a platform experiment. The insert is
// conditional on the experiment still being open so it cannot land after StartPlatformExperimentTx
// has decided who participates — such a row would be a durable signup with no quota row and no
// path to ever get one. inserted=false means the experiment is no longer open, or the agent was
// already signed up.
func (s *PlatformExperimentsStore) Signup(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `
INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at)
SELECT $1, $2, NOW()
WHERE EXISTS (SELECT 1 FROM platform_experiments WHERE id = $1 AND status = 'open')
ON CONFLICT (platform_experiment_id, agent_id) DO NOTHING`

	tag, err := s.pool.pool.Exec(ctx, q, platformExpID, agentID)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.Signup: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListSignups returns all agent IDs signed up for a platform experiment.
func (s *PlatformExperimentsStore) ListSignups(ctx context.Context, platformExpID string) ([]string, error) {
	const q = `
SELECT agent_id FROM experiment_signups
WHERE platform_experiment_id = $1
ORDER BY signed_up_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListSignups: %w", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.ListSignups: scan: %w", err)
		}
		agents = append(agents, id)
	}
	return agents, rows.Err()
}

// IsSignedUp returns true if the agent is signed up for the platform experiment.
func (s *PlatformExperimentsStore) IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error) {
	const q = `
SELECT 1 FROM experiment_signups
WHERE platform_experiment_id = $1 AND agent_id = $2`

	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, platformExpID, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.IsSignedUp: %w", err)
	}
	return true, nil
}

// CountSignups returns how many agents have signed up for a platform experiment.
func (s *PlatformExperimentsStore) CountSignups(ctx context.Context, platformExpID string) (int, error) {
	const q = `SELECT COUNT(*) FROM experiment_signups WHERE platform_experiment_id = $1`
	var n int
	if err := s.pool.pool.QueryRow(ctx, q, platformExpID).Scan(&n); err != nil {
		return 0, fmt.Errorf("platform_experiments_store.CountSignups: %w", err)
	}
	return n, nil
}
