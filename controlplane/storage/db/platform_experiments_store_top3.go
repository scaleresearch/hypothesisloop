package db

import (
	"context"
	"fmt"
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
