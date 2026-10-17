package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// HoldBaselineClock moves baseline_due_at of the open platform experiment id, which has no
// baseline declared, out by the time its baseline waits on the platform: held says it waits now.
// The time since the last call that saw it waiting is credited, up to most in all, so a wait is
// counted from the first call that sees it and is short by at most the interval between calls.
func (s *PlatformExperimentsStore) HoldBaselineClock(ctx context.Context, id string, held bool, most time.Duration) error {
	const q = `
UPDATE platform_experiments pe
SET ends_at = pe.ends_at + make_interval(secs => c.credit),
    baseline_held_seconds = pe.baseline_held_seconds + c.credit,
    baseline_held_at = CASE WHEN $2::boolean THEN now() END
FROM (SELECT id, CASE WHEN $2::boolean AND baseline_held_at IS NOT NULL
                      THEN LEAST(GREATEST(EXTRACT(EPOCH FROM now() - baseline_held_at)::double precision, 0),
                                 GREATEST($3::double precision - baseline_held_seconds, 0))
                      ELSE 0 END AS credit
      FROM platform_experiments WHERE id = $1) c
WHERE pe.id = c.id AND pe.status = 'open' AND pe.baseline_job_id = '' AND ($2::boolean OR pe.baseline_held_at IS NOT NULL)`
	if _, err := s.pool.pool.Exec(ctx, q, id, held, most.Seconds()); err != nil {
		return fmt.Errorf("platform_experiments_store.HoldBaselineClock: %w", err)
	}
	return nil
}

// RaiseBaseline writes the running pe as patched, its baseline raised from raise.From to
// raise.To, and records the raise, or does neither: one whose status or baseline moved since it
// was read is an error.
func (s *PlatformExperimentsStore) RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error {
	metrics, err := json.Marshal(pe.Metrics)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: marshal metrics: %w", err)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := writePlatformExperiment(ctx, tx, pe, metrics, domain.PlatformExpRunning, raise.From); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO baseline_raises (platform_experiment_id, from_job_id, to_job_id, reason, detail, actor_kind, actor_id, created_xid)
VALUES ($1, $2, $3, $4, $5, $6, $7, pg_current_xact_id()::text::bigint)`,
		pe.ID, raise.From, raise.To, raise.Reason, raise.Detail, string(raise.Actor.Kind), raise.Actor.ID); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform_experiments_store.RaiseBaseline: commit: %w", err)
	}
	return nil
}

func (s *PlatformExperimentsStore) baselineRaises(ctx context.Context, id string) ([]domain.BaselineRaise, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT from_job_id, to_job_id, reason, detail, actor_kind, actor_id, created_at FROM baseline_raises WHERE platform_experiment_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.baselineRaises: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.BaselineRaise, error) {
		var raise domain.BaselineRaise
		err := row.Scan(&raise.From, &raise.To, &raise.Reason, &raise.Detail, &raise.Actor.Kind, &raise.Actor.ID, &raise.At)
		raise.At = raise.At.UTC()
		return raise, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.baselineRaises: %w", err)
	}
	return out, nil
}

// reallocateBaselineQuotas writes the quota of every agent of role baseline of pe again, from
// pe's budget.
func reallocateBaselineQuotas(ctx context.Context, tx pgx.Tx, pe *domain.PlatformExperiment) error {
	rows, err := tx.Query(ctx, `
SELECT agent_id, quota_tier FROM experiment_signups
WHERE platform_experiment_id = $1 AND role = $2`, pe.ID, string(domain.SignupRoleBaseline))
	if err != nil {
		return fmt.Errorf("list baseline agents: %w", err)
	}
	type holder struct {
		AgentID string
		Tier    domain.QuotaTier
	}
	holders, err := pgx.CollectRows(rows, pgx.RowToStructByPos[holder])
	if err != nil {
		return fmt.Errorf("list baseline agents: %w", err)
	}
	for _, h := range holders {
		if err := allocateBaselineQuota(ctx, tx, pe.ID, h.AgentID, h.Tier, pe.BudgetAcceleratorHours); err != nil {
			return err
		}
	}
	return nil
}
