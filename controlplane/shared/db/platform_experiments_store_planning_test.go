package db

import (
	"context"
	"errors"
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
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "a goal", Goal: "a goal, in a sentence", Actor: &by,
		BudgetAcceleratorHours: 0.5, MaxAgents: 1, EndsAt: now.Add(30 * time.Minute), Status: domain.PlatformExpDraft,
		Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30, Stages: []domain.Stage{{LengthPct: 100}},
		CreatedAt: now, UpdatedAt: now, MaxHosted: 5,
	}
	planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", PerformanceScore: 0.5, CreatedAt: now}
	signup := domain.AgentSignup{AgentID: planner.ID, Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, Launcher: launcher, Flavor: domain.PlannerFlavor, Model: "model-1"}
	if err := NewPlatformExperimentsStore(pool).CreateDraft(context.Background(), pe, planner, signup, DraftLimits{Concurrent: 1000, PerDay: 1000}); err != nil {
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
		got.Actor == nil || got.Actor.Kind != domain.ActorUser || got.Actor.ID != "u-planning" {
		t.Fatalf("got = %+v (created by %+v), %v", got, got.Actor, err)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup == nil || signup.Name != "Planner" || signup.Flavor != domain.PlannerFlavor || signup.Role != domain.SignupRoleBaseline || !signup.Hosted {
		t.Fatalf("planner signed up as %+v, %v", signup, err)
	}
	// The planner's quota is the planning allowance, and no finding is filed for a job yet.
	if quota, err := pes.GetAgentQuota(ctx, planner, pe); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 0.5 || quota.BurstAcceleratorHours != 0 {
		t.Fatalf("planner's quota = %+v, %v, want the draft's 0.5 accelerator-hours", quota, err)
	}
	if found, err := pes.JobHasFinding(ctx, "job-"+uuid.New().String()[:8]); err != nil || found {
		t.Fatalf("a job nobody filed for: got = %v, %v", found, err)
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
	other := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "x", CreatedAt: time.Now()}
	if err := pes.SpawnAgent(ctx, pe, other, domain.AgentSignup{AgentID: other.ID, Role: domain.SignupRoleCompetitor, Flavor: "generalist", Model: "m"}); err != ErrSignupClosed {
		t.Fatalf("joining a draft: got = %v, want ErrSignupClosed", err)
	}

	// One without a planner, or with something else in its place, is not created.
	now := time.Now().UTC()
	bad := &domain.PlatformExperiment{ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Status: domain.PlatformExpDraft, EndsAt: now, Stages: []domain.Stage{{LengthPct: 100}}}
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12]}
	if err := pes.CreateDraft(ctx, bad, agent, domain.AgentSignup{AgentID: agent.ID, Flavor: "generalist", Hosted: true}, DraftLimits{Concurrent: 1000, PerDay: 1000}); err == nil {
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
	// A revision gives the draft back, and the planner has a plan to make again, on a machine
	// that owes nothing to the restarts of the one before.
	if err := pes.AskInstance(ctx, pe, planner, "machine-1", "launch-1"); err != nil {
		t.Fatal(err)
	}
	by := domain.Actor{Kind: domain.ActorUser, ID: "u-planning"}
	until := time.Now().UTC().Add(30 * time.Minute)
	if err := pes.Revise(ctx, pe, "it must fit one accelerator", by, until); err != nil {
		t.Fatal(err)
	}
	if err := pes.Revise(ctx, pe, "again", by, until); err != ErrNothingProposed {
		t.Fatalf("revising a draft that is being planned: got = %v, want ErrNothingProposed", err)
	}
	revised, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || revised.ProposedAt != nil || len(revised.Revisions) != 1 || revised.Revisions[0].Message != "it must fit one accelerator" ||
		revised.Revisions[0].Actor.Kind != domain.ActorUser || revised.Revisions[0].Actor.ID != "u-planning" || revised.EndsAt.Sub(until).Abs() > time.Second {
		t.Fatalf("revised: got = %+v, %v", revised, err)
	}
	if found, ending := listed(); !found || ending {
		t.Fatalf("revising: listed %v, ending %v, want kept", found, ending)
	}
	if signup, err := pes.GetAgentSignup(ctx, pe, planner); err != nil || signup.Instance == nil || signup.Instance.Launch != "" {
		t.Fatalf("after a revision the planner's launch is %+v, %v, want none, so that its next machine is a new launch", signup.Instance, err)
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
	if err := pes.Revise(ctx, pe, "too late", by, until); err != ErrNotDraft {
		t.Fatalf("revising a closed draft: got = %v, want ErrNotDraft", err)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup.Instance == nil {
		t.Fatalf("signup %+v, %v", signup, err)
	}
}

func TestAcceptingADraftOpensItWithItsRosterOrChangesNothing(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	launcher := "launcher-" + uuid.New().String()[:8]
	id, planner := createTestDraft(t, pool, launcher)

	accepted := func(researchers int) (*domain.PlatformExperiment, []*domain.Agent, []domain.AgentSignup) {
		t.Helper()
		pe, err := pes.GetPlatformExperiment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents = "accepted", "the brief", 6, researchers
		pe.Metrics = []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}}
		pe.Stages = []domain.Stage{{LengthPct: 40, EvictPct: 50}, {LengthPct: 60}}
		pe.EndsAt = time.Now().UTC().Add(4 * time.Hour)
		pe.DefaultQuotaTier = domain.QuotaTierGuaranteed
		pe.CoordinatorHosted, pe.Coordinator = true, &domain.HostedCoordinator{Launcher: launcher, Model: "model-1"}
		var agents []*domain.Agent
		var signups []domain.AgentSignup
		for i := 0; i < researchers; i++ {
			agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Researcher", PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
			agents = append(agents, agent)
			signups = append(signups, domain.AgentSignup{AgentID: agent.ID, Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: launcher, Flavor: "generalist", Model: "model-1"})
		}
		return pe, agents, signups
	}

	pe, agents, signups := accepted(2)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrNothingProposed {
		t.Fatalf("accepting before a proposal: got = %v, want ErrNothingProposed", err)
	}
	if err := pes.SetProposal(ctx, id, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Declined, the draft is answered and has nothing to accept; a revision gives it back to the
	// planner with the reason kept, and the next proposal clears it.
	if err := pes.Decline(ctx, id, "nothing here can be measured", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	declined, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || declined.DeclinedReason != "nothing here can be measured" || declined.ProposedAt == nil || len(declined.Proposal) != 0 {
		t.Fatalf("declined: got = %+v, %v", declined, err)
	}
	if wanted, _ := pes.ListLauncherAgents(ctx, launcher, 0); len(wanted) != 0 {
		t.Fatalf("a planner that declined is still asked for: %+v", wanted)
	}
	pe, agents, signups = accepted(2)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrNothingProposed {
		t.Fatalf("accepting a declined draft: got = %v, want ErrNothingProposed", err)
	}
	if err := pes.Revise(ctx, id, "measure validation loss", domain.Actor{Kind: domain.ActorUser, ID: "u-planning"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("revising a declined draft: %v", err)
	}
	if revised, _ := pes.GetPlatformExperiment(ctx, id); revised.ProposedAt != nil || revised.DeclinedReason == "" {
		t.Fatalf("revised after a decline: got = %+v, want it with the planner and the reason kept", revised)
	}
	if err := pes.SetProposal(ctx, id, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if proposed, _ := pes.GetPlatformExperiment(ctx, id); proposed.DeclinedReason != "" || proposed.ProposedAt == nil {
		t.Fatalf("proposed after a decline: got = %+v, want no reason", proposed)
	}
	if err := pes.Decline(ctx, "pe-none", "r", time.Now()); err != ErrNotDraft {
		t.Fatalf("declining what is not there: got = %v, want ErrNotDraft", err)
	}
	// More machines than the draft may have: nothing of the accept is kept. The draft's limit is
	// 5, its coordinator is one, and the planner is none of them.
	pe, agents, signups = accepted(5)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrMaxHostedReached {
		t.Fatalf("five researchers and a coordinator in five machines: got = %v, want ErrMaxHostedReached", err)
	}
	if got, _ := pes.GetPlatformExperiment(ctx, id); got.Status != domain.PlatformExpDraft || got.Name == "accepted" {
		t.Fatalf("a refused accept left %+v", got)
	}
	if joined, _ := pes.ListSignups(ctx, id); len(joined) != 1 {
		t.Fatalf("a refused accept left %d signups, want the planner's alone", len(joined))
	}

	pe, agents, signups = accepted(4)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.Status != domain.PlatformExpOpen || got.Name != "accepted" || got.Description != "the brief" || got.BudgetAcceleratorHours != 6 ||
		got.MaxAgents != 4 || len(got.Stages) != 2 || len(got.Metrics) != 1 || !got.CoordinatorHosted || got.Coordinator == nil || got.Coordinator.Launcher != launcher ||
		got.Goal != "a goal, in a sentence" {
		t.Fatalf("accepted: got = %+v (coordinator %+v), %v", got, got.Coordinator, err)
	}
	if got.StartsAt.IsZero() || time.Since(got.StartsAt) > time.Minute {
		t.Errorf("starts_at = %v, want now, so that the platform starts it if nobody does", got.StartsAt)
	}
	if err := pes.AcceptDraft(ctx, pe, nil, nil); err != ErrNotDraft {
		t.Fatalf("accepting twice: got = %v, want ErrNotDraft", err)
	}

	// The launcher runs the coordinator and the researchers, and no longer the planner.
	wanted, err := pes.ListLauncherAgents(ctx, launcher, time.Minute)
	if err != nil || len(wanted) != 4 {
		t.Fatalf("the launcher is asked for %d agents, %v, want the 4 researchers", len(wanted), err)
	}
	coordinators, err := pes.ListLauncherCoordinators(ctx, launcher, time.Minute)
	if err != nil || len(coordinators) != 1 || coordinators[0].PlatformExperimentID != id {
		t.Fatalf("coordinators = %+v, %v", coordinators, err)
	}

	// Started, the budget is shared among the researchers: the planner's work is over.
	started, quotas, err := pes.StartPlatformExperimentTx(ctx, id, func(participants []StartParticipant) ([]*domain.AgentQuota, error) {
		var out []*domain.AgentQuota
		for _, p := range participants {
			if p.AgentID == planner {
				t.Errorf("the planner takes a share of the run")
			}
			out = append(out, &domain.AgentQuota{ID: uuid.New().String(), AgentID: p.AgentID, PlatformExperimentID: id, GuaranteedAcceleratorHours: 1, CreatedAt: time.Now().UTC()})
		}
		return out, nil
	})
	if err != nil || !started || len(quotas) != 4 {
		t.Fatalf("start: started %v with %d quotas, %v, want the 4 researchers'", started, len(quotas), err)
	}
}

// The platform pays for planning, so an org's drafts are bounded where they are counted.
func TestAnOrgsDraftsAreBoundedAtOnceAndPerDay(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	org := "org-" + uuid.New().String()[:8]
	project := "prj-" + uuid.New().String()[:8]
	if _, err := pool.pool.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $1)`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO projects (id, org_id, name) VALUES ($1, $2, $1)`, project, org); err != nil {
		t.Fatal(err)
	}
	create := func(limits DraftLimits) (string, error) {
		now := time.Now().UTC()
		pe := &domain.PlatformExperiment{
			ID: "pe-" + uuid.New().String()[:8], ProjectID: project, Name: "g", Goal: "g", BudgetAcceleratorHours: 0.5, MaxAgents: 1,
			EndsAt: now.Add(time.Hour), Status: domain.PlatformExpDraft, Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30,
			Stages:    []domain.Stage{{LengthPct: 100}},
			CreatedAt: now, UpdatedAt: now, MaxHosted: 5,
		}
		planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", CreatedAt: now}
		return pe.ID, pes.CreateDraft(ctx, pe, planner, domain.AgentSignup{AgentID: planner.ID, Role: domain.SignupRoleBaseline, Hosted: true, Launcher: "l", Flavor: domain.PlannerFlavor, Model: "m"}, limits)
	}
	limits := DraftLimits{Concurrent: 2, PerDay: 3}
	first, err := create(limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create(limits); err != nil {
		t.Fatal(err)
	}
	var over *PlanningLimitError
	if _, err := create(limits); !errors.As(err, &over) || over.PerDay || over.Limit != 2 {
		t.Fatalf("a third draft at once: got = %v, want the concurrent limit", err)
	}
	// Closing one makes room at once, and the day's count still holds what was created.
	setPEStatus(t, pool, first, domain.PlatformExpClosed, 0)
	if _, err := create(limits); err != nil {
		t.Fatalf("a draft after one closed: %v", err)
	}
	setPEStatus(t, pool, first, domain.PlatformExpClosed, 0)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET status = 'closed' WHERE project_id = $1`, project); err != nil {
		t.Fatal(err)
	}
	if _, err := create(limits); !errors.As(err, &over) || !over.PerDay || over.Limit != 3 {
		t.Fatalf("a fourth draft in a day: got = %v, want the daily limit", err)
	}
	// An experiment created the ordinary way is no draft of the day.
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET goal = '' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := create(limits); err != nil {
		t.Fatalf("a draft once one of the day's is no draft: %v", err)
	}
}
