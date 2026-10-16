package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ClosedRun is a run that was at work and closed an hour ago at staff's request, its machines
// since released: Live is a researcher whose run ended only with the close, Finished one that
// finished before it, Cut one cut before it and Baseline the baseline agent that finished.
type ClosedRun struct {
	ID, Launcher, Live, Finished, Cut, Baseline string
}

// closedRunWithAgents writes what a run and its close leave in the database.
func closedRunWithAgents(t *testing.T, pool *Pool) ClosedRun {
	t.Helper()
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := createTestPEWithCoordinator(t, pool, launcher)
	hosted := domain.AgentSignup{Hosted: true, Launcher: launcher, Flavor: "generalist", Model: "model-1"}
	baseline := hosted
	baseline.Role = domain.SignupRoleBaseline
	agents := spawnTestAgents(t, pes, pe, baseline, hosted, hosted, hosted)
	run := ClosedRun{ID: pe, Launcher: launcher, Baseline: agents[0], Live: agents[1], Finished: agents[2], Cut: agents[3]}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 3*time.Hour)
	for _, agent := range []string{run.Baseline, run.Finished} {
		if err := pes.FinishAgent(ctx, pe, agent, "nothing left to try"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE experiment_signups SET finished_at = now() - interval '2 hours' WHERE platform_experiment_id = $1 AND finished_at IS NOT NULL`, pe); err != nil {
		t.Fatal(err)
	}
	cutTestAgent(t, pool, pe, run.Cut, 2*time.Hour)
	for _, agent := range []string{run.Live, ""} {
		name := "machine-" + agent
		if err := pes.AskInstance(ctx, pe, agent, name, "launch-1"); err != nil {
			t.Fatal(err)
		}
		if err := pes.ObserveInstance(ctx, pe, agent, name, domain.InstanceRunning, nil, time.Hour, "", "m-"+name); err != nil {
			t.Fatal(err)
		}
	}

	if err := pes.RecordTop3(ctx, pe, run.Live, 0.9); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetCloseAgents(ctx, pe, "Agents:\n- at work at the close\n"); err != nil {
		t.Fatal(err)
	}
	if err := pes.ClosePlatformExperiment(ctx, pe, &domain.Actor{Kind: domain.ActorStaff}, "redeploying the platform"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET closed_at = now() - interval '1 hour' WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if err := pes.RecordClose(ctx, pe, closeRecordHeading+" it closed."); err != nil {
		t.Fatal(err)
	}
	if err := pes.ReleaseInstances(ctx, launcher, domain.AgentWindDown, domain.CoordinatorWindDown); err != nil {
		t.Fatal(err)
	}
	return run
}

const closeRecordHeading = "Recorded by the platform:"

// A run closed to redeploy the platform continues as the same run: the reopen takes back what the
// close wrote, and every agent whose run ended only with the close, and the coordinator, is
// wanted on a machine again, while one that finished or was cut before it stays as it ended.
func TestAReopenedRunReadsRunningAndWantsTheAgentsTheCloseEnded(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	run := closedRunWithAgents(t, pool)

	if agents, err := pes.ListLauncherAgents(ctx, run.Launcher, domain.AgentWindDown); err != nil || len(agents) != 0 {
		t.Fatalf("closed an hour ago: launcher agents = %+v, %v, want none", agents, err)
	}
	events, stop := listenForEvents(t, pool)
	defer stop()

	endsAt := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	before, err := pes.GetPlatformExperiment(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	duration := endsAt.Sub(before.StartsAt).Hours()
	if err := pes.ReopenPlatformExperiment(ctx, run.ID, endsAt, duration, "coordinator-2"); err != nil {
		t.Fatalf("reopen: got = %v, want = nil", err)
	}

	pe, err := pes.GetPlatformExperiment(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pe.Status != domain.PlatformExpRunning || pe.ClosedAt != nil || pe.CloseReason != "" || pe.CloseMessage != "" || pe.ClosedBy != nil ||
		pe.Summary != "" || !pe.EndsAt.Equal(endsAt) || pe.DurationHours != duration || pe.StartedAt.IsZero() || pe.Coordinator.Model != "coordinator-2" {
		t.Fatalf("reopened = %+v, want running until %s with nothing of the close and the coordinator on its new model", pe, endsAt)
	}
	if lines, err := pes.CloseAgents(ctx, run.ID); err != nil || lines != "" {
		t.Fatalf("agent lines of the close = %q, %v, want none", lines, err)
	}
	var placements int
	if err := pool.pool.QueryRow(ctx, `SELECT count(*) FROM experiment_top3 WHERE platform_experiment_id = $1`, run.ID).Scan(&placements); err != nil || placements != 0 {
		t.Fatalf("placements = %d, %v, want none", placements, err)
	}
	_, closedAt, err := NewAccountsStore(pool).PlatformExperimentTenancy(ctx, run.ID)
	if err != nil || closedAt != nil {
		t.Fatalf("closed_at the token check reads = %v, %v, want none", closedAt, err)
	}

	agents, err := pes.ListLauncherAgents(ctx, run.Launcher, domain.AgentWindDown)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].AgentID != run.Live || agents[0].Ended != "" || agents[0].Instance.Released || agents[0].Instance.Stopped || agents[0].Instance.Name != "" {
		t.Fatalf("launcher agents = %+v, want the researcher the close ended, at work and with no machine", agents)
	}
	coordinators, err := pes.ListLauncherCoordinators(ctx, run.Launcher, domain.CoordinatorWindDown)
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinators) != 1 || coordinators[0].Ending || coordinators[0].Instance.Released || coordinators[0].Instance.Name != "" || coordinators[0].Model != "coordinator-2" {
		t.Fatalf("launcher coordinators = %+v, want the coordinator at work with no machine", coordinators)
	}
	for agent, ended := range map[string]domain.RunEnd{run.Live: "", run.Finished: domain.RunEndFinished, run.Baseline: domain.RunEndFinished, run.Cut: domain.RunEndCut} {
		signup, err := pes.GetAgentSignup(ctx, run.ID, agent)
		if err != nil || signup.Ended != ended {
			t.Errorf("signup %s: got = %+v, %v, want ended %q", agent, signup, err, ended)
		}
	}

	deadline := time.After(5 * time.Second)
	for told := false; !told; {
		select {
		case e := <-events:
			told = e.Kind == domain.EventPlatformExperimentStatus && e.Subject == run.ID && e.Value == string(domain.PlatformExpRunning)
		case <-deadline:
			t.Fatal("no platform_experiment.status running was announced")
		}
	}

	if err := pes.ReopenPlatformExperiment(ctx, run.ID, endsAt, duration, ""); !errors.Is(err, ErrNotReopenable) {
		t.Fatalf("a running one: got = %v, want = ErrNotReopenable", err)
	}
	if err := pes.ReopenPlatformExperiment(ctx, "pe-none-"+uuid.New().String()[:8], endsAt, duration, ""); !errors.Is(err, ErrPlatformExperimentNotFound) {
		t.Fatalf("none: got = %v, want = ErrPlatformExperimentNotFound", err)
	}
}

func TestOnlyAClosedRunThatStartedAndNobodyContinuesIsReopened(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	endsAt := time.Now().UTC().Add(5 * time.Hour)

	neverStarted := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, neverStarted, 10)
	setPEStatus(t, pool, neverStarted, domain.PlatformExpClosed, time.Hour)
	if err := pes.ReopenPlatformExperiment(ctx, neverStarted, endsAt, 5, ""); !errors.Is(err, ErrNotReopenable) {
		t.Fatalf("closed before it started: got = %v, want = ErrNotReopenable", err)
	}

	continued := closedRunWithAgents(t, pool)
	later := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, later, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET continues = $2 WHERE id = $1`, later, continued.ID); err != nil {
		t.Fatal(err)
	}
	if err := pes.ReopenPlatformExperiment(ctx, continued.ID, endsAt, 5, ""); !errors.Is(err, ErrNotReopenable) {
		t.Fatalf("continued by a later run: got = %v, want = ErrNotReopenable", err)
	}

	plain := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, plain, 10)
	setPEStatus(t, pool, plain, domain.PlatformExpRunning, time.Hour)
	setPEStatus(t, pool, plain, domain.PlatformExpClosed, time.Minute)
	if err := pes.ReopenPlatformExperiment(ctx, plain, endsAt, 5, "coordinator-2"); !errors.Is(err, ErrNotReopenable) {
		t.Fatalf("a coordinator model with no hosted coordinator: got = %v, want = ErrNotReopenable", err)
	}
	if pe, err := pes.GetPlatformExperiment(ctx, plain); err != nil || pe.Status != domain.PlatformExpClosed {
		t.Fatalf("after a refused reopen: got = %+v, %v, want it closed", pe, err)
	}
}
