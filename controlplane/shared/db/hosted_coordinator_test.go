package db

import (
	"context"
	"strings"
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
		Stages:                 []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		HypothesisSubmitPolicy: domain.SubmitterPolicyMixed, JobSubmitPolicy: domain.SubmitterPolicyMixed,
		CreatedAt: now, UpdatedAt: now,
		CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Launcher: launcher, Model: "model-1"},
	}
	if err := NewPlatformExperimentsStore(pool).CreatePlatformExperiment(context.Background(), pe); err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	return pe.ID
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
	if err := pes.SetCoordinatorInstanceName(ctx, plain, "x"); err != nil {
		t.Fatal(err)
	}
	if recorded, err := pes.RecordCoordinatorInstanceExit(ctx, plain, "x", "boom", 3); err != nil || recorded {
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
		has, err := pes.LauncherHasCoordinators(ctx, launcher, windDown)
		if err != nil || has != (len(out) > 0) {
			t.Fatalf("LauncherHasCoordinators = %v, %v with %d listed", has, err, len(out))
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

func TestACoordinatorsMachineThatExitsOrNeverStartsIsRecordedAndFinallyStopped(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe := createTestPEWithCoordinator(t, pool, launcher)
	const maxRestarts, bootLimit = 2, 5 * time.Minute
	instance := func() domain.AgentInstance {
		t.Helper()
		got, err := pes.GetPlatformExperiment(ctx, pe)
		if err != nil {
			t.Fatal(err)
		}
		return got.Coordinator.Instance
	}

	if err := pes.SetCoordinatorInstanceName(ctx, pe, "name-0"); err != nil {
		t.Fatal(err)
	}
	if recorded, err := pes.RecordCoordinatorInstanceStuck(ctx, pe, "name-0", bootLimit, maxRestarts); err != nil || recorded {
		t.Fatalf("just asked for: got = %v, %v, want = false", recorded, err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET coordinator_instance_asked_at = now() - make_interval(secs => $2) WHERE id = $1`, pe, (bootLimit + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}
	if recorded, err := pes.RecordCoordinatorInstanceStuck(ctx, pe, "name-0", bootLimit, maxRestarts); err != nil || !recorded {
		t.Fatalf("past the limit: got = %v, %v, want = true", recorded, err)
	}
	if got := instance(); got.Restarts != 1 || got.Name != "" || !strings.Contains(got.ExitLog, "5m0s") {
		t.Fatalf("after the boot limit: got = %+v", got)
	}

	for exit := 2; exit <= maxRestarts+1; exit++ {
		name := "name-" + string(rune('0'+exit))
		if err := pes.SetCoordinatorInstanceName(ctx, pe, name); err != nil {
			t.Fatal(err)
		}
		if recorded, err := pes.RecordCoordinatorInstanceExit(ctx, pe, "another-name", "boom", maxRestarts); err != nil || recorded {
			t.Fatalf("exit %d under a stale name: got = %v, %v, want = false", exit, recorded, err)
		}
		if recorded, err := pes.RecordCoordinatorInstanceExit(ctx, pe, name, "log of "+name, maxRestarts); err != nil || !recorded {
			t.Fatalf("exit %d: got = %v, %v, want = true", exit, recorded, err)
		}
		if recorded, err := pes.RecordCoordinatorInstanceExit(ctx, pe, name, "said twice", maxRestarts); err != nil || recorded {
			t.Fatalf("exit %d reported again: got = %v, %v, want = false", exit, recorded, err)
		}
		got := instance()
		wantRestarts, wantStopped := exit, false
		if exit > maxRestarts {
			wantRestarts, wantStopped = maxRestarts, true
		}
		if got.Name != "" || got.Restarts != wantRestarts || got.Stopped != wantStopped || got.ExitLog != "log of "+name || got.ExitedAt == nil {
			t.Fatalf("after exit %d: got = %+v, want restarts %d stopped %v", exit, got, wantRestarts, wantStopped)
		}
	}
	if listed, err := pes.ListLauncherCoordinators(ctx, launcher, time.Minute); err != nil || len(listed) != 0 {
		t.Fatalf("a stopped coordinator: listed = %+v, %v, want none", listed, err)
	}
	if has, err := pes.LauncherHasCoordinators(ctx, launcher, time.Minute); err != nil || has {
		t.Errorf("a launcher whose only coordinator stopped: got = %v, %v, want = false", has, err)
	}
}
