package db

import (
	"context"
	"slices"
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
		if err := store.ClosePlatformExperiment(ctx, id, by, ""); err != nil {
			t.Fatalf("ClosePlatformExperiment: %v", err)
		}
	}
	closed, err := store.GetPlatformExperiment(ctx, id)
	if err != nil || closed.Status != domain.PlatformExpClosed || closed.CloseReason != domain.CloseByRequest || closed.ClosedAt == nil || closed.ClosedBy == nil || closed.ClosedBy.ID != owner.ID {
		t.Fatalf("closed experiment: got = (%+v, %v), want it closed by request of %s", closed, err, owner.ID)
	}
}

// The owner said 20 minutes and the baseline got 10 of them, since the duration was counted from
// the accept. The run lasts its duration from its start, however long the baseline took.
func TestTheRunLastsItsDurationFromItsStart(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	created := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "an hour's run", BudgetAcceleratorHours: 10, MaxAgents: 2,
		StartsAt: created, BaselineDueAt: created.Add(45 * time.Minute), DurationHours: 1, Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: created, UpdatedAt: created,
	}
	if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
		t.Fatal(err)
	}
	open, err := pes.GetPlatformExperiment(ctx, pe.ID)
	if err != nil || !open.EndsAt.IsZero() || !open.StartedAt.IsZero() || !open.BaselineDueAt.Equal(pe.BaselineDueAt) || !open.ClosesAt().Equal(open.BaselineDueAt) {
		t.Fatalf("open: got = ends %v, started %v, baseline due %v, %v, want no end before the run and the baseline due as stored", open.EndsAt, open.StartedAt, open.BaselineDueAt, err)
	}

	// A researcher its owner runs is at work from the start, and the run ends an hour after it.
	spawnTestAgents(t, pes, pe.ID, domain.AgentSignup{})
	before := time.Now().Add(-time.Second)
	started, _, err := pes.StartPlatformExperimentTx(ctx, pe.ID, time.Hour, func([]StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) { return nil, oneStage, nil })
	if err != nil || !started {
		t.Fatalf("start: got = %v, %v, want started", started, err)
	}
	running, err := pes.GetPlatformExperiment(ctx, pe.ID)
	if err != nil || running.StartsAt.Before(before) || !running.StartedAt.Equal(running.StartsAt) || !running.EndsAt.Equal(running.StartsAt.Add(time.Hour)) || !running.BaselineDueAt.IsZero() {
		t.Fatalf("running: got = starts %v, started %v, ends %v, baseline due %v, %v, want it started now and ending an hour later", running.StartsAt, running.StartedAt, running.EndsAt, running.BaselineDueAt, err)
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
	if err := pes.ClosePlatformExperiment(ctx, pe.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if closed, err := pes.GetPlatformExperiment(ctx, pe.ID); err != nil || !closed.EndsAt.Equal(running.EndsAt) || closed.CloseReason != domain.CloseEndTimeReached {
		t.Fatalf("closed: got = ends %v, %s, %v, want the run's end kept and end_time_reached", closed.EndsAt, closed.CloseReason, err)
	}
}

// A run started while its launcher was unreachable: no researcher's machine came up and it
// waited with nobody at work. started_at is when the first researcher's machine is reported
// running, the wait is bounded and never passes the end, and one closed waiting says so.
func TestARunWaitsForItsFirstResearchersMachineWithinItsRun(t *testing.T) {
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
		if err := pes.ObserveInstance(ctx, pe, agent, "machine-"+agent, state, nil, time.Hour, "", "machine-1"); err != nil {
			t.Fatal(err)
		}
	}

	pe, researcher, baseline := start()
	waiting := read(pe)
	end := waiting.StartsAt.Add(time.Hour)
	if !waiting.AwaitsAgents() || !waiting.EndsAt.Equal(end) || time.Until(waiting.AgentsDueAt) > agentsDue+time.Second || time.Until(waiting.AgentsDueAt) < agentsDue-time.Minute {
		t.Fatalf("started with no machine up: got = started %v, ends %v, agents due %v, want no researcher at work, the end an hour after the start %v and the machines due in %s", waiting.StartedAt, waiting.EndsAt, waiting.AgentsDueAt, waiting.StartsAt, agentsDue)
	}
	agents, err := pes.ListLauncherAgents(ctx, waiting.Coordinator.Launcher, windDown)
	if err != nil || len(agents) == 0 || !agents[0].EndsBy.Equal(end.Add(windDown)) {
		t.Fatalf("machines rented while it waits: got = %+v, %v, want them bound by its end, %s", agents, err, end.Add(windDown))
	}
	observe(pe, baseline, domain.InstanceRunning)
	observe(pe, researcher, domain.InstanceStarting)
	if still := read(pe); !still.AwaitsAgents() {
		t.Fatalf("a baseline agent's machine running and a researcher's starting: started %v, want no clock yet", still.StartedAt)
	}
	before := time.Now().Add(-time.Second)
	observe(pe, researcher, domain.InstanceRunning)
	running := read(pe)
	if running.StartedAt.Before(before) || !running.EndsAt.Equal(end) || !running.AgentsDueAt.IsZero() {
		t.Fatalf("a researcher's machine running: got = started %v, ends %v, agents due %v, want started now and the end kept", running.StartedAt, running.EndsAt, running.AgentsDueAt)
	}
	observe(pe, researcher, domain.InstanceRunning)
	if again := read(pe); !again.StartedAt.Equal(running.StartedAt) {
		t.Fatalf("the machine reported again: started moved from %v to %v", running.StartedAt, again.StartedAt)
	}

	pe, _, _ = start()
	due := read(pe).AgentsDueAt
	if err := pes.ClosePlatformExperiment(ctx, pe, nil, ""); err != nil {
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

// The platform's record of a close is written once and kept beside the coordinator's summary,
// after it: a summary written before the close never stands alone as the record, and one
// written after it replaces only the coordinator's part. A record that was once written as the
// summary itself moves beside it when the schema is applied.
func TestTheRecordOfACloseIsKeptBesideTheSummary(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	summary := func() string {
		t.Helper()
		got, err := pes.GetPlatformExperiment(ctx, pe)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := pes.ListPlatformExperimentsByStatus(ctx, got.Status)
		if err != nil {
			t.Fatal(err)
		}
		for _, one := range listed {
			if one.ID == pe && one.Summary != got.Summary {
				t.Fatalf("listed with summary %q, read with %q", one.Summary, got.Summary)
			}
		}
		return got.Summary
	}
	if err := pes.RecordClose(ctx, pe, "too early"); err != nil {
		t.Fatal(err)
	}
	if got := summary(); got != "" {
		t.Fatalf("an open one: summary %q, want none", got)
	}
	if err := pes.SetPlatformExperimentSummary(ctx, pe, "Status: running."); err != nil {
		t.Fatal(err)
	}
	if err := pes.ClosePlatformExperiment(ctx, pe, nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{"the record", "a second record"} {
		if err := pes.RecordClose(ctx, pe, record); err != nil {
			t.Fatal(err)
		}
	}
	if got := summary(); got != "Status: running.\n\nthe record" {
		t.Fatalf("closed: summary %q, want the coordinator's and the first record after it", got)
	}
	if err := pes.SetPlatformExperimentSummary(ctx, pe, "The run found it."); err != nil {
		t.Fatal(err)
	}
	if got := summary(); got != "The run found it.\n\nthe record" {
		t.Fatalf("rewritten after the close: summary %q, want the new one and the record", got)
	}

	exec(t, pool, `UPDATE platform_experiments SET summary = 'Recorded by the platform: it closed.', close_record = '' WHERE id = $1`, pe)
	if err := applySchemaAt(ctx, testDSN()); err != nil {
		t.Fatal(err)
	}
	if got := summary(); got != "Recorded by the platform: it closed." {
		t.Fatalf("an old record after the schema is applied: summary %q", got)
	}
	if err := pes.SetPlatformExperimentSummary(ctx, pe, "Written after."); err != nil {
		t.Fatal(err)
	}
	if got := summary(); got != "Written after.\n\nRecorded by the platform: it closed." {
		t.Fatalf("an old record kept beside a summary: %q", got)
	}
}

func TestAListSaysWhyAPlatformExperimentClosed(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	if err := pes.ClosePlatformExperiment(ctx, pe, &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}, ""); err != nil {
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

// A baseline lost 17 of its 25 minutes to the platform building its environment. The time
// a baseline waits on the platform moves its baseline_due_at out, up to the bound given, and
// only while it is open with no baseline declared.
func TestTheTimeABaselineWaitsOnThePlatformMovesItsDeadline(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	created := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	due := created.Add(25 * time.Minute)
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "a held baseline", BudgetAcceleratorHours: 10, MaxAgents: 2,
		StartsAt: created, BaselineDueAt: due, DurationHours: 1, Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: created, UpdatedAt: created,
	}
	if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM platform_experiments WHERE id = $1`, pe.ID)
	})
	hold := func(held bool, most time.Duration) time.Time {
		t.Helper()
		if err := pes.HoldBaselineClock(ctx, pe.ID, held, most); err != nil {
			t.Fatal(err)
		}
		read, err := pes.GetPlatformExperiment(ctx, pe.ID)
		if err != nil {
			t.Fatal(err)
		}
		return read.BaselineDueAt
	}
	seen := func(ago time.Duration) {
		t.Helper()
		exec(t, pool, `UPDATE platform_experiments SET baseline_held_at = now() - $2 * interval '1 second' WHERE id = $1`, pe.ID, ago.Seconds())
	}

	// The first sweep that sees the wait credits nothing: it starts counting.
	if got := hold(true, time.Hour); !got.Equal(due) {
		t.Fatalf("first sight of a wait: due = %v, want %v", got, due)
	}
	// A minute later it is still waiting: a minute is credited.
	seen(time.Minute)
	if got := hold(true, time.Hour); got.Sub(due) < time.Minute-time.Second || got.Sub(due) > time.Minute+5*time.Second {
		t.Fatalf("a minute of waiting: due moved %v, want a minute", got.Sub(due))
	}
	// The wait ended: nothing more is credited, and the next one starts counting afresh.
	seen(10 * time.Minute)
	moved := hold(false, time.Hour)
	if moved.Sub(due) > time.Minute+5*time.Second {
		t.Fatalf("a wait that ended: due moved %v, want only the minute seen", moved.Sub(due))
	}
	if got := hold(true, time.Hour); !got.Equal(moved) {
		t.Fatalf("a new wait first seen: due = %v, want %v", got, moved)
	}
	// Never more than the bound in all.
	seen(3 * time.Hour)
	if got := hold(true, 2*time.Minute); got.Sub(due) < 2*time.Minute-time.Second || got.Sub(due) > 2*time.Minute+time.Second {
		t.Fatalf("a wait past the bound: due moved %v, want the bound, 2m", got.Sub(due))
	}

	// One with its baseline declared keeps its deadline.
	exec(t, pool, `UPDATE platform_experiments SET baseline_job_id = 'job-1' WHERE id = $1`, pe.ID)
	before := hold(true, 3*time.Hour)
	seen(time.Hour)
	if got := hold(true, 3*time.Hour); !got.Equal(before) {
		t.Errorf("declared baseline: due moved from %v to %v", before, got)
	}
}

// The record of a close states what the experiment spent, and a job still live at the close is
// billed only when it ends: the record waits for every job to end and be settled.
func TestACloseIsToRecordOnceEveryJobOfItIsSettled(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	agentID, peID, hypothesisID := seedRun(t, pool, "record", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'open' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ('agent', 'record-job', $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', 'SUBMITTED', 1, 'AK', 'SK')`, agentID, peID, hypothesisID)
	if err := pes.ClosePlatformExperiment(ctx, peID, nil, ""); err != nil {
		t.Fatal(err)
	}
	toRecord := func() bool {
		t.Helper()
		ids, err := pes.ClosesToRecord(ctx, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		settled, err := pes.JobsSettled(ctx, peID)
		if err != nil {
			t.Fatal(err)
		}
		listed := false
		for _, id := range ids {
			listed = listed || id == peID
		}
		if listed != settled {
			t.Fatalf("listed to record %v, every job settled %v: want the two to agree", listed, settled)
		}
		return listed
	}
	if toRecord() {
		t.Fatal("a close whose job is still placed is to record")
	}
	exec(t, pool, `UPDATE experiments SET status = 'REJECTED' WHERE id = 'record-job'`)
	if toRecord() {
		t.Fatal("a close whose job ended and is not yet settled is to record")
	}
	exec(t, pool, `UPDATE experiments SET quota_settled_at = now() WHERE id = 'record-job'`)
	if !toRecord() {
		t.Fatal("a close whose every job is settled is not to record")
	}
	if err := pes.RecordClose(ctx, peID, "the record"); err != nil {
		t.Fatal(err)
	}
	if ids, err := pes.ClosesToRecord(ctx, time.Hour); err != nil || slices.Contains(ids, peID) {
		t.Errorf("a close with its record written: to record = %v, %v, want it left out", ids, err)
	}
}
