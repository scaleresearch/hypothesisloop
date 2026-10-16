package db

import (
	"context"
	"fmt"
)

// SetPlatformExperimentShared records the platform's decision that every account may read a
// platform experiment, or takes it back. found is false when there is no such platform experiment,
// or it was removed.
func (s *PlatformExperimentsStore) SetPlatformExperimentShared(ctx context.Context, id string, shared bool) (found bool, err error) {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE platform_experiments SET shared = $2, updated_at = NOW() WHERE id = $1 AND removed_at IS NULL`, id, shared)
	if err != nil {
		return false, fmt.Errorf("platform_experiments_store.SetShared: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
