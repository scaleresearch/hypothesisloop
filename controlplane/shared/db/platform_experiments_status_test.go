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

	// A researcher its owner runs is at work from the start, so the clock starts with it.
	spawnTestAgents(t, pes, pe.ID, domain.AgentSignup{})
	before := time.Now().Add(-time.Second)
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe.ID, time.Hour, func([]StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) { return nil, oneStage, nil })
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

// A run started while its launcher was unreachable: no researcher's machine came up, its 45
// minutes passed with nobody at work and it closed end_time_reached. The clock of a run whose
// researchers the platform runs starts when the first one's machine is reported running, the wait
// is bounded, and one closed waiting says so.
func TestARunsLengthCountsFromTheFirstResearchersMachineRunning(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	const agentsDue, windDown = 20 * time.Minute, 2 * time.Minute
	start := func() (pe, researcher, baseline string) {
		t.Helper()
		launcher := "launcher-" + uuid.New().String()[:8]
		pe = createTestPEWithCoordinator(t, pool, launcher)
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET duration_hours = 1 WHERE id = $1`, pe); err != nil {
			t.Fatal(err)
		}
		ids := spawnTestAgents(t, pes, pe,
			domain.AgentSignup{Hosted: true, Launcher: launcher, Flavor: "generalist"},
			domain.AgentSignup{Hosted: true, Launcher: launcher, Flavor: "generalist", Role: domain.SignupRoleBaseline})
		started, _, err := pes.StartPlatformExperimentTx(ctx, pe, agentsDue, func([]StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) { return nil, oneStage, nil })
		if err != nil || !started {
			t.Fatalf("start: got = %v, %v, want started", started, err)
		}
		return pe, ids[0], ids[1]
	}
	read := func(pe string) *domain.PlatformExperiment {
		t.Helper()
		got, err := pes.GetPlatformExperiment(ctx, pe)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	observe := func(pe, agent, state string) {
		t.Helper()
		if err := pes.AskInstance(ctx, pe, agent, "machine-"+agent, "launch-1"); err != nil {
			t.Fatal(err)
		}
		if err := pes.ObserveInstance(ctx, pe, agent, "machine-"+agent, state, nil, time.Hour, ""); err != nil {
			t.Fatal(err)
		}
	}

	pe, researcher, baseline := start()
	waiting := read(pe)
	if !waiting.AwaitsAgents() || !waiting.EndsAt.IsZero() || time.Until(waiting.AgentsDueAt) > agentsDue || time.Until(waiting.AgentsDueAt) < agentsDue-time.Minute {
		t.Fatalf("started with no machine up: got = started %v, ends %v, agents due %v, want no clock and the machines due in %s", waiting.StartedAt, waiting.EndsAt, waiting.AgentsDueAt, agentsDue)
	}
	agents, err := pes.ListLauncherAgents(ctx, waiting.Coordinator.Launcher, windDown)
	if err != nil || len(agents) == 0 || !agents[0].EndsBy.Equal(waiting.AgentsDueAt.Add(time.Hour+windDown)) {
		t.Fatalf("machines rented while it waits: got = %+v, %v, want them bound by the latest end the run can have, %s", agents, err, waiting.AgentsDueAt.Add(time.Hour+windDown))
	}
	observe(pe, baseline, domain.InstanceRunning)
	observe(pe, researcher, domain.InstanceStarting)
	if still := read(pe); !still.AwaitsAgents() {
		t.Fatalf("a baseline agent's machine running and a researcher's starting: started %v, want no clock yet", still.StartedAt)
	}
	before := time.Now().Add(-time.Second)
	observe(pe, researcher, domain.InstanceRunning)
	running := read(pe)
	if running.StartedAt.Before(before) || !running.EndsAt.Equal(running.StartedAt.Add(time.Hour)) || !running.AgentsDueAt.IsZero() {
		t.Fatalf("a researcher's machine running: got = started %v, ends %v, agents due %v, want the clock started now for one hour", running.StartedAt, running.EndsAt, running.AgentsDueAt)
	}
	observe(pe, researcher, domain.InstanceRunning)
	if again := read(pe); !again.StartedAt.Equal(running.StartedAt) {
		t.Fatalf("the machine reported again: started moved from %v to %v", running.StartedAt, again.StartedAt)
	}

	pe, _, _ = start()
	due := read(pe).AgentsDueAt
	if err := pes.ClosePlatformExperiment(ctx, pe, nil); err != nil {
		t.Fatal(err)
	}
	if closed := read(pe); closed.CloseReason != domain.CloseAgentsNotStarted || !closed.AgentsDueAt.Equal(due) || !closed.StartedAt.IsZero() || !closed.EndsAt.IsZero() {
		t.Fatalf("closed waiting: got = %s, agents due %v, started %v, ends %v, want agents_not_started with its deadline kept and no run", closed.CloseReason, closed.AgentsDueAt, closed.StartedAt, closed.EndsAt)
	}
	// Applying the schema again leaves a run with no clock as it is: only a row from before
	// started_at was kept is given one.
	if err := applySchemaAt(ctx, testDSN()); err != nil {
		t.Fatal(err)
	}
	if closed := read(pe); !closed.StartedAt.IsZero() {
		t.Fatalf("after the schema is applied again: started %v, want none", closed.StartedAt)
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
