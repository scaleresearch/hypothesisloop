package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func createTestDraft(t *testing.T, pool *Pool, launcher string) (peID, plannerID string) {
	t.Helper()
	now := time.Now().UTC()
	by := domain.Actor{Kind: domain.ActorUser, ID: "u-planning"}
	pe := &domain.PlatformExperiment{
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "a goal", Goal: "a goal, in a sentence", CreatedBy: &by,
		BudgetAcceleratorHours: 0.5, MaxAgents: 1, EndsAt: now.Add(30 * time.Minute), Status: domain.PlatformExpDraft,
		Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30, Stages: []domain.Stage{{LengthPct: 100}},
		HypothesisSubmitPolicy: domain.SubmitterPolicyMixed, JobSubmitPolicy: domain.SubmitterPolicyMixed,
		CreatedAt: now, UpdatedAt: now, MaxHosted: 5,
	}
	planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", Kind: domain.AgentKindAgent, PerformanceScore: 0.5, CreatedAt: now}
	signup := domain.AgentSignup{AgentID: planner.ID, Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, Launcher: launcher, Flavor: domain.PlannerFlavor, Model: "model-1"}
	if err := NewPlatformExperimentsStore(pool).CreateDraft(context.Background(), pe, planner, signup); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if planner.OrgID != "default" {
		t.Fatalf("the planner is of org %q, want the project's", planner.OrgID)
	}
	return pe.ID, planner.ID
}

func TestADraftIsStoredWithItsGoalItsCreatorAndItsPlanner(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe, planner := createTestDraft(t, pool, "launcher-"+uuid.New().String()[:8])

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || got.Status != domain.PlatformExpDraft || got.Goal != "a goal, in a sentence" ||
		got.CreatedBy == nil || got.CreatedBy.Kind != domain.ActorUser || got.CreatedBy.ID != "u-planning" {
		t.Fatalf("got = %+v (created by %+v), %v", got, got.CreatedBy, err)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup == nil || signup.Name != "Planner" || signup.Flavor != domain.PlannerFlavor || signup.Role != domain.SignupRoleBaseline || !signup.Hosted {
		t.Fatalf("planner signed up as %+v, %v", signup, err)
	}
	drafts, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpDraft)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, d := range drafts {
		listed = listed || (d.ID == pe && d.Goal == "a goal, in a sentence" && d.SignupCount == 1)
	}
	if !listed {
		t.Fatalf("the draft is not listed by its status with its goal and its one signup")
	}

	// A proposal is stored beside the draft, marks it proposed and moves its end.
	wait := time.Now().UTC().Add(7 * 24 * time.Hour)
	if err := pes.SetProposal(ctx, pe, []byte(`{"budget_accelerator_hours":6}`), wait); err != nil {
		t.Fatal(err)
	}
	got, err = pes.GetPlatformExperiment(ctx, pe)
	if err != nil || got.ProposedAt == nil || string(got.Proposal) == "" || got.EndsAt.Sub(wait).Abs() > time.Second || got.Status != domain.PlatformExpDraft {
		t.Fatalf("proposed: got = %+v, %v", got, err)
	}
	open := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, open, 10)
	if err := pes.SetProposal(ctx, open, []byte(`{}`), wait); err != ErrNotDraft {
		t.Fatalf("proposing an open platform experiment: got = %v, want ErrNotDraft", err)
	}

	// Nobody joins a draft: joining is for an open platform experiment.
	other := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "x", Kind: domain.AgentKindAgent, CreatedAt: time.Now()}
	if err := pes.SpawnAgent(ctx, pe, other, domain.AgentSignup{AgentID: other.ID, Role: domain.SignupRoleCompetitor, Flavor: "generalist", Model: "m"}); err != ErrSignupClosed {
		t.Fatalf("joining a draft: got = %v, want ErrSignupClosed", err)
	}

	// One without a planner, or with something else in its place, is not created.
	now := time.Now().UTC()
	bad := &domain.PlatformExperiment{ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Status: domain.PlatformExpDraft, EndsAt: now, Stages: []domain.Stage{{LengthPct: 100}}}
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Kind: domain.AgentKindAgent}
	if err := pes.CreateDraft(ctx, bad, agent, domain.AgentSignup{AgentID: agent.ID, Flavor: "generalist", Hosted: true}); err == nil {
		t.Fatal("a draft whose agent is no planner was created")
	}
	if got, _ := pes.GetPlatformExperiment(ctx, bad.ID); got != nil {
		t.Fatal("the refused draft exists")
	}
}

// A planner's machine is kept while there is a plan to make, and at no other time: it must not
// idle beside a proposal a person may read for days, nor come back when the experiment runs.
func TestALauncherKeepsAPlannerOnlyWhileItsDraftIsBeingPlanned(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	pe, planner := createTestDraft(t, pool, launcher)
	const windDown = 2 * time.Minute

	listed := func() (found, ending bool) {
		t.Helper()
		agents, err := pes.ListLauncherAgents(ctx, launcher, windDown)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.AgentID == planner {
				return true, a.Ending
			}
		}
		return false, false
	}
	propose := func(ago time.Duration) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET proposed_at = now() - make_interval(secs => $2) WHERE id = $1`, pe, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	if found, ending := listed(); !found || ending {
		t.Fatalf("while planning: listed %v, ending %v, want kept", found, ending)
	}
	propose(0)
	if found, ending := listed(); !found || !ending {
		t.Fatalf("just proposed: listed %v, ending %v, want winding down", found, ending)
	}
	propose(windDown + time.Second)
	if found, _ := listed(); found {
		t.Fatal("a planner is kept beside a proposal that waits for a person")
	}
	// A revision clears the proposal, and the planner has a plan to make again.
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET proposed_at = NULL WHERE id = $1`, pe); err != nil {
		t.Fatal(err)
	}
	if found, ending := listed(); !found || ending {
		t.Fatalf("revising: listed %v, ending %v, want kept", found, ending)
	}
	// Accepted, the experiment is open and then running, and the planner has nothing to do in it.
	propose(windDown + time.Second)
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		setPEStatus(t, pool, pe, status, 0)
		if found, _ := listed(); found {
			t.Fatalf("a planner is kept in a platform experiment that is %s", status)
		}
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, 0)
	if found, ending := listed(); !found || !ending {
		t.Fatalf("just closed: listed %v, ending %v, want winding down like any agent", found, ending)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup.Instance == nil {
		t.Fatalf("signup %+v, %v", signup, err)
	}
}
