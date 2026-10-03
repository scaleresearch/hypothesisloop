package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func createTestPEWithCoordinator(t *testing.T, pool *Pool, launcher string) string {
	t.Helper()
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "coordinated", BudgetAcceleratorHours: 10,
		MaxAgents: 10, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now,
		CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Launcher: launcher, Model: "model-1"}, MaxHosted: 50,
	}
	if err := NewPlatformExperimentsStore(pool).CreatePlatformExperiment(context.Background(), pe); err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	return pe.ID
}

// An open platform experiment can still start as late as its baseline deadline and then runs its
// whole length, so that is what bounds a machine rented while it is open. A running one ends at
// its end time.
func TestAMachineIsBoundByTheLatestEndItsPlatformExperimentCanStillHave(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := createTestPEWithCoordinator(t, pool, launcher)
	spawnTestAgents(t, pes, pe, domain.AgentSignup{Hosted: true, Launcher: launcher, Flavor: "generalist", Role: domain.SignupRoleBaseline})
	const windDown = 2 * time.Minute
	deadline := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET ends_at = $2, duration_hours = 5 WHERE id = $1`, pe, deadline); err != nil {
		t.Fatal(err)
	}
	bounds := func() (agent, coordinator time.Time) {
		t.Helper()
		agents, err := pes.ListLauncherAgents(ctx, launcher, windDown)
		if err != nil || len(agents) != 1 {
			t.Fatalf("agents = %+v, %v", agents, err)
		}
		coordinators, err := pes.ListLauncherCoordinators(ctx, launcher, windDown)
		if err != nil || len(coordinators) != 1 {
			t.Fatalf("coordinators = %+v, %v", coordinators, err)
		}
		return agents[0].EndsBy, coordinators[0].EndsBy
	}
	if agent, coordinator := bounds(); !agent.Equal(deadline.Add(5*time.Hour+windDown)) || !coordinator.Equal(agent) {
		t.Fatalf("open: bounds = %s and %s, want the baseline deadline %s, 5h of run and the wind-down", agent, coordinator, deadline)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if agent, coordinator := bounds(); !agent.Equal(deadline.Add(windDown)) || !coordinator.Equal(agent) {
		t.Fatalf("running: bounds = %s and %s, want the end time %s and the wind-down", agent, coordinator, deadline)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, time.Minute)
	if agent, coordinator := bounds(); !coordinator.Equal(agent) || time.Until(agent) > windDown-time.Minute || time.Until(agent) < windDown-2*time.Minute {
		t.Fatalf("closed a minute ago: bounds = %s and %s, want the wind-down after the close", agent, coordinator)
	}
}

func TestAHostedCoordinatorIsStoredReadAndListedOnlyWhereItIsOne(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := createTestPEWithCoordinator(t, pool, launcher)
	plain := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, plain, 10)

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || !got.CoordinatorHosted || got.Coordinator == nil || got.Coordinator.Launcher != launcher || got.Coordinator.Model != "model-1" || got.Coordinator.Instance.Name != "" {
		t.Fatalf("got = %+v (coordinator %+v), %v", got, got.Coordinator, err)
	}
	if other, err := pes.GetPlatformExperiment(ctx, plain); err != nil || other.CoordinatorHosted || other.Coordinator != nil {
		t.Fatalf("a platform experiment without one: got = %+v, %v", other, err)
	}
	if err := pes.AskInstance(ctx, plain, "", "x", "launch-1"); err != nil {
		t.Fatal(err)
	}
	if _, recorded, err := pes.EndInstance(ctx, plain, "", "x", InstanceExited, "boom", time.Minute, 3); err != nil || recorded {
		t.Fatalf("an exit where no coordinator is hosted: got = %v, %v, want = false", recorded, err)
	}
	listedAll, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range listedAll {
		if one.ID == pe && (!one.CoordinatorHosted || one.Coordinator != nil) {
			t.Errorf("listed = %+v, want coordinator_hosted without the coordinator's detail", one)
		}
	}

	const windDown = 2 * time.Minute
	listed := func() []LauncherCoordinator {
		t.Helper()
		out, err := pes.ListLauncherCoordinators(ctx, launcher, windDown)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := listed(); len(out) != 1 || out[0].PlatformExperimentID != pe || out[0].Model != "model-1" || out[0].Ending {
		t.Fatalf("open: listed = %+v", out)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpRunning, 0)
	if out := listed(); len(out) != 1 || out[0].Ending {
		t.Fatalf("running: listed = %+v", out)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, windDown-10*time.Second)
	if out := listed(); len(out) != 1 || !out[0].Ending {
		t.Fatalf("just closed: listed = %+v, want it ending", out)
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, windDown+10*time.Second)
	if out := listed(); len(out) != 0 {
		t.Fatalf("closed past the wind-down: listed = %+v, want none", out)
	}
}
