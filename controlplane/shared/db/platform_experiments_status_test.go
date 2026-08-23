package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Against a real PostgreSQL, because the defect this covers is invisible to anything else: a
// statement that compiles, type-checks and passes every Go test can still be rejected by the
// planner. A version of UpdatePlatformExperimentStatus once wrote
//
//	SET status = $2 ... CASE WHEN $2 = 'running' THEN ...
//
// which asks Postgres to deduce $2 as the status enum and as text at the same time. Every close
// failed with SQLSTATE 42P08, and it reached a deployed cluster because no test ever executed the
// statement — the e2e suite caught it only as 29 scenarios exiting 22 with all their assertions
// passing, which is a long way from naming the cause.
func TestClosingAPlatformExperimentExecutesAgainstARealDatabase(t *testing.T) {
	pool := eventsTestDB(t)
	store := &PlatformExperimentsStore{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := eventsTestPrefix + "pe-status-" + time.Now().UTC().Format("150405.000000")
	if _, err := pool.pool.Exec(ctx, `INSERT INTO platform_experiments
		(id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds,
		 starts_at, ends_at, status, stages, current_stage, created_at, updated_at)
		VALUES ($1, $1, '', 1, 1, '[]'::jsonb, 10, NOW(), NOW() + interval '1 hour', 'open',
		        '[]'::jsonb, 1, NOW(), NOW())`, id); err != nil {
		t.Fatalf("seed platform experiment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM platform_experiments WHERE id = $1`, id)
	})

	// Every status this function is asked to write, executed for real.
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpRunning, domain.PlatformExpClosed} {
		if err := store.UpdatePlatformExperimentStatus(ctx, id, status); err != nil {
			t.Fatalf("UpdatePlatformExperimentStatus(%s): %v", status, err)
		}
	}
}
