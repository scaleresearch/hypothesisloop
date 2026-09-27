package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Against a real PostgreSQL, because the defect this covers is invisible to anything else: a
// statement that compiles, type-checks and passes every Go test can still be rejected by the
// planner. A version of the close once wrote
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
		(id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds,
		 starts_at, ends_at, status, stages, current_stage, created_at, updated_at)
		VALUES ($1, 'default', $1, '', 1, 1, '[]'::jsonb, 10, NOW(), NOW() + interval '1 hour', 'open',
		        '[]'::jsonb, 1, NOW(), NOW())`, id); err != nil {
		t.Fatalf("seed platform experiment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM platform_experiments WHERE id = $1`, id)
	})

	// The close records who asked, and a second close leaves it as it is.
	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	for _, by := range []*domain.Actor{&owner, nil} {
		if err := store.ClosePlatformExperiment(ctx, id, by); err != nil {
			t.Fatalf("ClosePlatformExperiment: %v", err)
		}
	}
	closed, err := store.GetPlatformExperiment(ctx, id)
	if err != nil || closed.Status != domain.PlatformExpClosed || closed.CloseReason != domain.CloseByRequest || closed.ClosedAt == nil || closed.ClosedBy == nil || closed.ClosedBy.ID != owner.ID {
		t.Fatalf("closed experiment: got = (%+v, %v), want it closed by request of %s", closed, err, owner.ID)
	}
}

// A run of one hour once closed end_time_reached while its baseline was still being prepared:
// its hour counted from the moment it was accepted. The hour counts from the start of the run.
func TestARunsLengthCountsFromTheMomentItStarts(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	created := time.Now().UTC().Add(-3 * time.Hour)
	due := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "an hour's run", BudgetAcceleratorHours: 10, MaxAgents: 2,
		DurationHours: 1, BaselineDueAt: due, Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: created, UpdatedAt: created,
	}
	if err := pes.CreatePlatformExperiment(ctx, pe); err != nil {
		t.Fatal(err)
	}
	open, err := pes.GetPlatformExperiment(ctx, pe.ID)
	if err != nil || !open.EndsAt.IsZero() || !open.StartedAt.IsZero() || !open.BaselineDueAt.Equal(due) || open.DurationHours != 1 {
		t.Fatalf("open: got = ends %v, started %v, baseline due %v, %v hours, %v, want no end, no start, its baseline deadline and one hour", open.EndsAt, open.StartedAt, open.BaselineDueAt, open.DurationHours, err)
	}

	before := time.Now().Add(-time.Second)
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe.ID, func([]StartParticipant) ([]*domain.AgentQuota, error) { return nil, nil })
	if err != nil || !started {
		t.Fatalf("start: got = %v, %v, want started", started, err)
	}
	running, err := pes.GetPlatformExperiment(ctx, pe.ID)
	if err != nil || running.StartedAt.Before(before) || !running.EndsAt.Equal(running.StartedAt.Add(time.Hour)) || !running.BaselineDueAt.IsZero() {
		t.Fatalf("running: got = started %v, ends %v, baseline due %v, %v, want it started now and ending one hour later", running.StartedAt, running.EndsAt, running.BaselineDueAt, err)
	}
	listed, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpRunning)
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range listed {
		if one.ID == pe.ID && !one.ClosesAt().Equal(running.EndsAt) {
			t.Fatalf("listed: closes at %v, want the run's end %v", one.ClosesAt(), running.EndsAt)
		}
	}
	if err := pes.ClosePlatformExperiment(ctx, pe.ID, nil); err != nil {
		t.Fatal(err)
	}
	if closed, err := pes.GetPlatformExperiment(ctx, pe.ID); err != nil || !closed.EndsAt.Equal(running.EndsAt) || closed.CloseReason != domain.CloseEndTimeReached {
		t.Fatalf("closed: got = ends %v, %s, %v, want the run's end kept and end_time_reached", closed.EndsAt, closed.CloseReason, err)
	}
}

func TestAListSaysWhyAPlatformExperimentClosed(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	if err := pes.ClosePlatformExperiment(ctx, pe, &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	listed, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpClosed)
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range listed {
		if one.ID == pe {
			if one.CloseReason != domain.CloseByRequest || one.ClosedAt == nil {
				t.Fatalf("listed: close_reason %q at %v, want closed_by_request and when", one.CloseReason, one.ClosedAt)
			}
			return
		}
	}
	t.Fatalf("%s is not among the closed ones listed", pe)
}
