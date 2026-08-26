package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ClusterSettingsStore provides persistence for per-cluster operator overrides
// (cluster_settings), keyed by the runtime-derived cluster_id.
type ClusterSettingsStore struct {
	pool *Pool
}

// NewClusterSettingsStore creates a ClusterSettingsStore backed by pool.
func NewClusterSettingsStore(pool *Pool) *ClusterSettingsStore {
	return &ClusterSettingsStore{pool: pool}
}

// GetClusterSettings reads the settings row for clusterID, if one has ever been set. Returns
// nil (not an error) when the operator has never set anything for this cluster — callers fall
// back to the global defaults.
func (s *ClusterSettingsStore) GetClusterSettings(ctx context.Context, clusterID string) (*domain.ClusterSettings, error) {
	const q = `SELECT cluster_id, scale_up_timeout_seconds, max_speculative_accelerators FROM cluster_settings WHERE cluster_id = $1`
	cs := &domain.ClusterSettings{}
	err := s.pool.pool.QueryRow(ctx, q, clusterID).Scan(&cs.ClusterID, &cs.ScaleUpTimeoutSeconds, &cs.MaxSpeculativeAccelerators)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("cluster_settings_store.Get: %w", err)
	}
	return cs, nil
}

// PutClusterSettings upserts the settings row for clusterID. Both fields are nullable — passing
// nil clears that field back to the global default / no cap, exactly like never having set it.
func (s *ClusterSettingsStore) PutClusterSettings(ctx context.Context, cs *domain.ClusterSettings) error {
	const q = `
INSERT INTO cluster_settings (cluster_id, scale_up_timeout_seconds, max_speculative_accelerators, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (cluster_id) DO UPDATE SET
    scale_up_timeout_seconds = EXCLUDED.scale_up_timeout_seconds,
    max_speculative_accelerators = EXCLUDED.max_speculative_accelerators,
    updated_at = NOW()`
	_, err := s.pool.pool.Exec(ctx, q, cs.ClusterID, cs.ScaleUpTimeoutSeconds, cs.MaxSpeculativeAccelerators)
	if err != nil {
		return fmt.Errorf("cluster_settings_store.Put: %w", err)
	}
	return nil
}
