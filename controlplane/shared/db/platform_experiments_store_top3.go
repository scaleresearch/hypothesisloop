package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ---- top-3 history (Domain 6) ----

// RecordTop3 records that agentID placed in the top 3 for a platform experiment.
func (s *PlatformExperimentsStore) RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error {
	const q = `
INSERT INTO experiment_top3 (platform_experiment_id, agent_id, final_metric, recorded_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET final_metric = EXCLUDED.final_metric`

	_, err := s.pool.pool.Exec(ctx, q, platformExpID, agentID, finalMetric)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RecordTop3: %w", err)
	}
	return nil
}

// HasTop3History returns true if the agent has ever placed in the top 3.
func (s *PlatformExperimentsStore) HasTop3History(ctx context.Context, agentID string) (bool, error) {
	const q = `SELECT 1 FROM experiment_top3 WHERE agent_id = $1 LIMIT 1`
	var dummy int
	err := s.pool.pool.QueryRow(ctx, q, agentID).Scan(&dummy)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.HasTop3History: %w", err)
	}
	return true, nil
}
