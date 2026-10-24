package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// idleSinceSQL is when the stretch of signup s of platform experiment pe with nothing of its own
// on the platform began: the start of the run, its joining, or the last moment a job of its was
// created or seen running, whichever is latest. It says nothing of whether a job is in flight now.
const idleSinceSQL = `GREATEST(pe.started_at, s.signed_up_at,
                    (SELECT max(GREATEST(e.created_at, e.run_reported_at)) FROM experiments e
                     WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id))`

// IdleExecutor is an executor of a running platform experiment with nothing of its own on the
// platform for at least the notice, and due to be told so (agent.idle).
type IdleExecutor struct {
	PlatformExperimentID string
	AgentID              string
	// Since is when the stretch began (idleSinceSQL).
	Since time.Time
	// Repeat is 0 for the first notice of the stretch, and counts the notices of it told before.
	Repeat int
	// NoticedAt is when it was last told, nil for never. The notice stands only while it still is.
	NoticedAt *time.Time
}

// IdleExecutors is every executor of a running platform experiment that had no job of its
// own created or running for after, since the run started or it joined, has none queued or
// running, and was not told so in the last after of the stretch: due a notice. An executor that
// finished or was cut is never due one.
func (s *Store) IdleExecutors(ctx context.Context, after time.Duration) ([]IdleExecutor, error) {
	rows, err := s.pool.pool.Query(ctx, `
WITH idle AS (
    SELECT s.platform_experiment_id, s.agent_id, s.idle_noticed_at, s.idle_repeat, `+idleSinceSQL+` AS since
    FROM experiment_signups s JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
    WHERE pe.status = 'running' AND pe.started_at IS NOT NULL AND s.role = $2 AND s.finished_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM platform_experiment_cuts c
                      WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id)
      AND NOT EXISTS (SELECT 1 FROM experiments e
                      WHERE e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id
                        AND e.status IN `+unfinishedSQL+`)
)
SELECT platform_experiment_id, agent_id, since,
       CASE WHEN idle_noticed_at IS NULL OR idle_noticed_at < since THEN 0 ELSE idle_repeat + 1 END,
       idle_noticed_at
FROM idle
WHERE since <= now() - make_interval(secs => $1)
  AND (idle_noticed_at IS NULL OR idle_noticed_at < GREATEST(since, now() - make_interval(secs => $1)))
ORDER BY platform_experiment_id, agent_id`, after.Seconds(), string(domain.SignupRoleExecutor))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.IdleExecutors: %w", err)
	}
	idle, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IdleExecutor, error) {
		var r IdleExecutor
		err := row.Scan(&r.PlatformExperimentID, &r.AgentID, &r.Since, &r.Repeat, &r.NoticedAt)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.IdleExecutors: %w", err)
	}
	return idle, nil
}

// NoticeIdle tells r that it is idle, with detail: agent.idle, new for its first notice of the
// stretch and continues after. A notice told since r was read stands, and this one writes nothing.
func (s *Store) NoticeIdle(ctx context.Context, r IdleExecutor, detail string) error {
	return s.write(ctx, func(w *Write) error {
		tag, err := w.Exec(ctx, `
UPDATE experiment_signups
SET idle_noticed_at = now(), idle_repeat = $3, idle_detail = $4
WHERE platform_experiment_id = $1 AND agent_id = $2 AND idle_noticed_at IS NOT DISTINCT FROM $5`,
			r.PlatformExperimentID, r.AgentID, r.Repeat, detail, r.NoticedAt)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.NoticeIdle: %w", err)
		}
		if tag.RowsAffected() == 1 {
			w.Change(domain.Change{Kind: platformapi.EventAgentIdle, Subject: r.AgentID, Value: domain.IdleValue(r.Repeat), Detail: detail,
				PlatformExperimentID: r.PlatformExperimentID, AgentID: r.AgentID})
		}
		return nil
	})
}

// IdleCapacity is a running platform experiment whose capacity has held places beyond its queued
// jobs for at least the notice, and due to be told so (capacity.idle).
type IdleCapacity struct {
	PlatformExperimentID string
	// Since is when the sweep first saw places free in this stretch.
	Since time.Time
	// Repeat is 0 for the first notice of the stretch, and counts the notices of it told before.
	Repeat int
	// NoticedAt is when it was last told, nil for never. The notice stands only while it still is.
	NoticedAt *time.Time
}

// MarkCapacityFree records whether the capacity of the running platform experiment id holds
// places beyond its queued jobs now: the first pass that sees some starts a stretch, and one that
// sees none ends it. With free it answers the stretch when it lasted after and was not told so in
// the last after of it: due a notice. Otherwise it answers nil.
func (s *Store) MarkCapacityFree(ctx context.Context, id string, free bool, after time.Duration) (*IdleCapacity, error) {
	if !free {
		if _, err := s.pool.pool.Exec(ctx, `UPDATE platform_experiments SET capacity_free_since = NULL WHERE id = $1 AND capacity_free_since IS NOT NULL`, id); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.MarkCapacityFree: %w", err)
		}
		return nil, nil
	}
	if _, err := s.pool.pool.Exec(ctx, `UPDATE platform_experiments SET capacity_free_since = now() WHERE id = $1 AND status = 'running' AND capacity_free_since IS NULL`, id); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.MarkCapacityFree: %w", err)
	}
	r := IdleCapacity{PlatformExperimentID: id}
	err := s.pool.pool.QueryRow(ctx, `
SELECT capacity_free_since,
       CASE WHEN capacity_noticed_at IS NULL OR capacity_noticed_at < capacity_free_since THEN 0 ELSE capacity_repeat + 1 END,
       capacity_noticed_at
FROM platform_experiments
WHERE id = $1 AND status = 'running' AND capacity_free_since <= now() - make_interval(secs => $2)
  AND (capacity_noticed_at IS NULL OR capacity_noticed_at < GREATEST(capacity_free_since, now() - make_interval(secs => $2)))`,
		id, after.Seconds()).Scan(&r.Since, &r.Repeat, &r.NoticedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.MarkCapacityFree: %w", err)
	}
	return &r, nil
}

// NoticeCapacityIdle tells whoever steers the platform experiment of r that its capacity holds
// places beyond its queued jobs, with detail: capacity.idle, new for the first notice of the
// stretch and continues after. A notice told since r was read stands, and this one writes nothing.
func (s *Store) NoticeCapacityIdle(ctx context.Context, r IdleCapacity, detail string) error {
	return s.write(ctx, func(w *Write) error {
		tag, err := w.Exec(ctx, `
UPDATE platform_experiments
SET capacity_noticed_at = now(), capacity_repeat = $2, capacity_detail = $3
WHERE id = $1 AND capacity_noticed_at IS NOT DISTINCT FROM $4`,
			r.PlatformExperimentID, r.Repeat, detail, r.NoticedAt)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.NoticeCapacityIdle: %w", err)
		}
		if tag.RowsAffected() == 1 {
			w.runChange(r.PlatformExperimentID, platformapi.EventCapacityIdle, domain.IdleValue(r.Repeat), detail)
		}
		return nil
	})
}
