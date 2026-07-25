package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// TransitionTerminal atomically transitions an experiment from `from` to `to` (EVICTED or
// REJECTED) and records the reason, in a single Postgres transaction — the RowsAffected guard
// means a concurrent status change (e.g. natural completion racing with eviction) can never
// double-transition.
//
// It deliberately does not write final usage to the metrics DB — that's a separate store with no
// shared transaction, so callers settle usage afterward via services/settlement.Settler, whose
// idempotent absolute-set writes can retry until quota_settled_at is set.
//
// Returns (false, nil) if the row was no longer in `from` status — the caller must skip
// settlement in that case.
func (s *Store) TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (updated bool, err error) {
	tag, err := s.ExperimentsStore.pool.pool.Exec(ctx, `
		UPDATE experiments SET status = $3, eviction_reason = $4, not_admitted_reason = NULL, updated_at = NOW()
		WHERE id = $1 AND status = $2`, id, string(from), string(to), reason)
	if err != nil {
		return false, fmt.Errorf("db.TransitionTerminal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
