package db

import (
	"context"
	"errors"
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
		CoordinatorHosted: true, Coordinator: &domain.HostedCoordinator{Launcher: launcher, Model: "model-1"}, MaxHosted: 50,
	}
	if err := NewPlatformExperimentsStore(pool).CreatePlatformExperimentWithAgents(context.Background(), pe, nil, nil); err != nil {
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

func TestAPlatformExperimentCreatedWithAgentsExistsWithAllOfThemOrNotAtAll(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	experiment := func(maxAgents int) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "with agents", BudgetAcceleratorHours: 10,
			MaxAgents: maxAgents, StartsAt: now, EndsAt: now.Add(24 * time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
			Stages:                 []domain.Stage{{LengthPct: 100, EvictPct: 0}},
			HypothesisSubmitPolicy: domain.SubmitterPolicyMixed, JobSubmitPolicy: domain.SubmitterPolicyMixed,
			CreatedAt: now, UpdatedAt: now, MaxHosted: 50,
		}
	}
	two := func() ([]*domain.Agent, []domain.AgentSignup) {
		agents, signups := make([]*domain.Agent, 2), make([]domain.AgentSignup, 2)
		for i := range agents {
			id := "ag-" + uuid.New().String()[:12]
			agents[i] = &domain.Agent{ID: id, Name: id, Kind: domain.AgentKindAgent, PerformanceScore: 0.5, CreatedAt: now}
			signups[i] = domain.AgentSignup{AgentID: id, Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: "main", Flavor: "generalist", Model: "model-1"}
		}
		return agents, signups
	}

	pe := experiment(5)
	agents, signups := two()
	if err := pes.CreatePlatformExperimentWithAgents(ctx, pe, agents, signups); err != nil {
		t.Fatalf("create with agents: got = %v, want = nil", err)
	}
	joined, err := pes.ListAgentSignups(ctx, pe.ID)
	if err != nil || len(joined) != 2 || !joined[0].Hosted || joined[0].Launcher != "main" {
		t.Fatalf("joined = %+v, %v, want the two hosted agents", joined, err)
	}

	// Two competitors do not fit a field of one: the experiment is not created either, and the
	// agents are in no org.
	tooSmall := experiment(1)
	agents, signups = two()
	if err := pes.CreatePlatformExperimentWithAgents(ctx, tooSmall, agents, signups); !errors.Is(err, ErrMaxAgentsReached) {
		t.Fatalf("agents that do not fit: got = %v, want ErrMaxAgentsReached", err)
	}
	if got, err := pes.GetPlatformExperiment(ctx, tooSmall.ID); err != nil || got != nil {
		t.Fatalf("after a refused write the platform experiment: got = %+v, %v, want none", got, err)
	}
	if agent, err := NewAgentsStore(pool).GetAgent(ctx, agents[0].ID); err != nil || agent != nil {
		t.Fatalf("after a refused write the agent: got = %+v, %v, want none", agent, err)
	}

	// An agent id already taken fails the second insert of the transaction.
	taken := experiment(5)
	agents, signups = two()
	agents[1].ID, signups[1].AgentID = joined[0].AgentID, joined[0].AgentID
	if err := pes.CreatePlatformExperimentWithAgents(ctx, taken, agents, signups); err == nil {
		t.Fatal("an agent id already taken: got = nil, want an error")
	}
	if got, err := pes.GetPlatformExperiment(ctx, taken.ID); err != nil || got != nil {
		t.Fatalf("after a failed write the platform experiment: got = %+v, %v, want none", got, err)
	}
}
