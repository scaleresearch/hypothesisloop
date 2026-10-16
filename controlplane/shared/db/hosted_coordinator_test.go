package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func createTestPEWithCoordinator(t *testing.T, pool *Pool) string {
	t.Helper()
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "coordinated", BudgetAcceleratorHours: 10,
		MaxAgents: 10, StartsAt: now, DurationHours: 24, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
		Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
		Stages:    []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
		CreatedAt: now, UpdatedAt: now,
		CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Model: "model-1"}, MaxHosted: 50,
	}
	if err := NewPlatformExperimentsStore(pool).CreatePlatformExperiment(context.Background(), pe, nil, nil); err != nil {
		t.Fatalf("create: got = %v, want = nil", err)
	}
	return pe.ID
}

func TestHostedSaysWhoThePlatformRuns(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	coordinated := createTestPEWithCoordinator(t, pool)
	plain := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, plain, 10)
	agents := spawnTestAgents(t, pes, plain, domain.AgentSignup{Hosted: true}, domain.AgentSignup{})
	for _, tc := range []struct {
		name, pe, agent string
		want            bool
	}{
		{"a hosted coordinator", coordinated, "", true},
		{"a coordinator the platform does not run", plain, "", false},
		{"a hosted agent", plain, agents[0], true},
		{"an agent run elsewhere", plain, agents[1], false},
		{"an agent of another platform experiment", coordinated, agents[0], false},
		{"no platform experiment", "pe-missing", "", false},
	} {
		got, err := pes.Hosted(ctx, tc.pe, tc.agent)
		if err != nil || got != tc.want {
			t.Errorf("%s: got = %v, %v, want = %v", tc.name, got, err, tc.want)
		}
	}
}

func TestAHostedCoordinatorIsStoredReadAndListedOnlyWhereItIsOne(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe := createTestPEWithCoordinator(t, pool)
	plain := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, plain, 10)

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || !got.CoordinatorHosted || got.Coordinator == nil || got.Coordinator.Model != "model-1" || got.Coordinator.Instance.Name != "" {
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
		out, err := pes.ListLauncherCoordinators(ctx, windDown)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := listed(); len(out) != 1 || out[0].PlatformExperimentID != pe || out[0].Model != "model-1" || out[0].Ending {
		t.Fatalf("open, its baseline measured under the coordinator: listed = %+v", out)
	}
	if err := pes.AskInstance(ctx, pe, "", "c", "launch-1"); err != nil {
		t.Fatal(err)
	}
	if err := pes.TellCoordinator(ctx, pe, ""); err != nil {
		t.Fatal(err)
	}
	if out := listed(); len(out) != 1 {
		t.Fatalf("open, asked for and no longer waited for: listed = %+v, want it kept", out)
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

// A platform experiment created with a hosted coordinator waited for the coordinator to boot and
// add the agent that measures its baseline, where an accepted draft had that agent from the
// accept. It is created with it, in one write, and a create the signup refuses stores nothing.
func TestAPlatformExperimentIsCreatedWithItsAgentsOrNotAtAll(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	experiment := func(maxHosted int) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{Actor: testActor,
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "created", BudgetAcceleratorHours: 6,
			MaxAgents: 3, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
			Stages:    []domain.Stage{{LengthPct: 40, EvictPct: 75}, {LengthPct: 60, EvictPct: 0}},
			CreatedAt: now, UpdatedAt: now, DefaultQuotaTier: domain.QuotaTierGuaranteed,
			CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Model: "model-1"}, MaxHosted: maxHosted,
		}
	}
	measuring := func() ([]*domain.Agent, []domain.AgentSignup) {
		agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], CreatedAt: now}
		return []*domain.Agent{agent}, []domain.AgentSignup{{Actor: testActor, AgentID: agent.ID, Role: domain.SignupRoleBaseline, Hosted: true, AgentType: "baseline", Model: "model-1"}}
	}

	pe := experiment(4)
	agents, signups := measuring()
	if err := pes.CreatePlatformExperiment(ctx, pe, agents, signups); err != nil {
		t.Fatalf("create: %v", err)
	}
	if agents[0].Name != "Baseline 1" {
		t.Errorf("the baseline agent is named %q, want Baseline 1", agents[0].Name)
	}
	wanted, err := pes.ListLauncherAgents(ctx, time.Minute)
	if err != nil || len(wanted) != 1 || wanted[0].AgentID != agents[0].ID {
		t.Fatalf("the launcher is asked for %+v, %v, want the baseline agent's machine from the create on", wanted, err)
	}
	if quota, err := pes.GetAgentQuota(ctx, agents[0].ID, pe.ID); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 3 {
		t.Fatalf("the baseline agent's quota = %+v, %v, want half the budget, 3", quota, err)
	}
	if joined, err := pes.ListAgentSignups(ctx, pe.ID); err != nil || len(joined) != 1 || joined[0].Role != domain.SignupRoleBaseline {
		t.Fatalf("joined = %+v, %v, want the baseline agent alone", joined, err)
	}

	// The coordinator's machine is the only one this experiment may have, so none measures.
	refused := experiment(1)
	agents, signups = measuring()
	if err := pes.CreatePlatformExperiment(ctx, refused, agents, signups); err != ErrMaxHostedReached {
		t.Fatalf("no machine for the baseline agent: got = %v, want ErrMaxHostedReached", err)
	}
	if stored, err := pes.GetPlatformExperiment(ctx, refused.ID); err != nil || stored != nil {
		t.Fatalf("a refused create left %+v, %v, want nothing stored", stored, err)
	}
	var left int
	if err := pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE id = $1`, agents[0].ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("a refused create left %d agents, %v, want none", left, err)
	}
}
