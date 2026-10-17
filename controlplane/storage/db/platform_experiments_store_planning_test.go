package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func createTestDraft(t *testing.T, pool *Pool) (peID, plannerID string) {
	t.Helper()
	now := time.Now().UTC()
	by := domain.Actor{Kind: domain.ActorUser, ID: "u-planning"}
	pe := &domain.PlatformExperiment{DefaultQuotaTier: domain.QuotaTierGuaranteed,
		ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "a goal", Goal: "a goal, in a sentence", Actor: &by,
		MaxAgents: 1, PlanningEndsAt: now.Add(30 * time.Minute), Status: domain.PlatformExpDraft,
		Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30, Stages: []domain.Stage{{LengthPct: 100}},
		CreatedAt: now, UpdatedAt: now, MaxAgentSandboxes: 5, Repository: "acme/a-goal", RepositoryURL: "https://github.com/acme/a-goal.git",
	}
	planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", CreatedAt: now}
	signup := domain.AgentSignup{Actor: testActor, AgentID: planner.ID, Role: domain.SignupRolePlanner, QuotaTier: domain.QuotaTierGuaranteed,
		InAgentSandbox: true, AgentType: domain.PlannerAgentType, Model: "model-1"}
	if err := NewStore(pool, 3).CreateDraft(context.Background(), pe, planner, signup); err != nil {
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
	pes := NewStore(pool, 3)
	pe, planner := createTestDraft(t, pool)

	got, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || got.Status != domain.PlatformExpDraft || got.Goal != "a goal, in a sentence" ||
		got.Actor == nil || got.Actor.Kind != domain.ActorUser || got.Actor.ID != "u-planning" {
		t.Fatalf("got = %+v (created by %+v), %v", got, got.Actor, err)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup == nil || signup.Name != "Planner" || signup.AgentType != domain.PlannerAgentType || signup.Role != domain.SignupRolePlanner || !signup.InAgentSandbox {
		t.Fatalf("planner signed up as %+v, %v", signup, err)
	}
	// A planner runs no job, so it holds no quota, and no finding is filed for a job yet.
	if quota, err := pes.GetAgentQuota(ctx, planner, pe); err != nil || quota != nil {
		t.Fatalf("planner's quota = %+v, %v, want none", quota, err)
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
	if err := pes.SetProposal(ctx, pe, 1, []byte(`{"budget_accelerator_hours":6}`), Proposed{WaitUntil: wait}); err != nil {
		t.Fatal(err)
	}
	got, err = pes.GetPlatformExperiment(ctx, pe)
	if err != nil || got.ProposedAt == nil || string(got.Proposal) == "" || got.ProposalExpiresAt.Sub(wait).Abs() > time.Second || !got.EndsAt.IsZero() || got.Status != domain.PlatformExpDraft {
		t.Fatalf("proposed: got = %+v, %v", got, err)
	}
	if got.BaselineJobID != "" || got.JobEstimatedDurationHours != 0 {
		t.Fatalf("proposed with baseline %q and a trial's estimate of %g, want neither: a proposal measures nothing", got.BaselineJobID, got.JobEstimatedDurationHours)
	}
	open := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, open, 10)
	if err := pes.SetProposal(ctx, open, 1, []byte(`{}`), Proposed{WaitUntil: wait}); err != ErrNotDraft {
		t.Fatalf("proposing an open platform experiment: got = %v, want ErrNotDraft", err)
	}

	// Nobody joins a draft: joining is for an open platform experiment.
	other := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "x", CreatedAt: time.Now()}
	if err := pes.SpawnAgent(ctx, pe, other, domain.AgentSignup{Actor: testActor, AgentID: other.ID, Role: domain.SignupRoleCompetitor, AgentType: "generalist", Model: "m"}); err != ErrSignupClosed {
		t.Fatalf("joining a draft: got = %v, want ErrSignupClosed", err)
	}

	// One without a planner, or with something else in its place, is not created.
	now := time.Now().UTC()
	bad := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed, ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Status: domain.PlatformExpDraft, PlanningEndsAt: now, Stages: []domain.Stage{{LengthPct: 100}}}
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12]}
	if err := pes.CreateDraft(ctx, bad, agent, domain.AgentSignup{Actor: testActor, AgentID: agent.ID, AgentType: "generalist", InAgentSandbox: true}); err == nil {
		t.Fatal("a draft whose agent is no planner was created")
	}
	if got, _ := pes.GetPlatformExperiment(ctx, bad.ID); got != nil {
		t.Fatal("the refused draft exists")
	}
}

// A draft or experiment whose repository the platform creates is stored before it, and its
// machines are wanted only once the repository is recorded. One GitHub refuses closes with what
// GitHub answered.
func TestNoMachineIsWantedBeforeTheRepositoryIsRecorded(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	draft := func() (*domain.PlatformExperiment, string) {
		now := time.Now().UTC()
		pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed, ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "a goal", Goal: "a goal",
			MaxAgents: 1, PlanningEndsAt: now.Add(30 * time.Minute), Status: domain.PlatformExpDraft,
			Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30, Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now, MaxAgentSandboxes: 5, RepositoryAccount: "acme"}
		planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", CreatedAt: now}
		if err := pes.CreateDraft(ctx, pe, planner, domain.AgentSignup{Actor: testActor, AgentID: planner.ID, Role: domain.SignupRolePlanner, QuotaTier: domain.QuotaTierGuaranteed,
			InAgentSandbox: true, AgentType: domain.PlannerAgentType, Model: "model-1"}); err != nil {
			t.Fatal(err)
		}
		return pe, planner.ID
	}
	wanted := func() []string {
		agents, err := pes.ListLauncherAgents(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, agent := range agents {
			ids = append(ids, agent.AgentID)
		}
		return ids
	}
	awaiting := func(id string) bool {
		drafts, err := pes.RepositoriesToCreate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(drafts, func(d RepositoryToCreate) bool { return d.ID == id && d.Account == "acme" })
	}

	given, planner := draft()
	if !awaiting(given.ID) || len(wanted()) != 0 {
		t.Fatalf("a draft without its repository: awaiting %v, planners wanted %v, want it awaiting and none", awaiting(given.ID), wanted())
	}
	if err := pes.GiveRepository(ctx, given.ID, "acme/a-goal", "https://github.com/acme/a-goal.git", "main"); err != nil {
		t.Fatal(err)
	}
	if awaiting(given.ID) || !slices.Equal(wanted(), []string{planner}) {
		t.Fatalf("a draft given its repository: awaiting %v, planners wanted %v, want its planner", awaiting(given.ID), wanted())
	}
	if err := pes.GiveRepository(ctx, given.ID, "acme/other", "", "main"); !errors.Is(err, ErrRepositoryNotAwaited) {
		t.Fatalf("giving a second repository: %v, want ErrRepositoryNotAwaited", err)
	}

	// A whole experiment's coordinator waits for the repository the same way.
	now := time.Now().UTC()
	whole := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed, ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "whole", BudgetAcceleratorHours: 10,
		MaxAgents: 2, Status: domain.PlatformExpOpen, Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, ReportIntervalSeconds: 30,
		Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now, BaselineDueAt: now.Add(time.Hour), MaxAgentSandboxes: 5, RepositoryAccount: "acme",
		CoordinatorInAgentSandbox: true, Coordinator: &domain.AgentSandboxCoordinator{Model: "model-1"}}
	if err := pes.CreatePlatformExperiment(ctx, whole, nil, nil); err != nil {
		t.Fatal(err)
	}
	coordinators := func() int {
		listed, err := pes.ListLauncherCoordinators(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return len(listed)
	}
	if !awaiting(whole.ID) || coordinators() != 0 {
		t.Fatalf("a whole experiment without its repository: awaiting %v, %d coordinators wanted, want it awaiting and none", awaiting(whole.ID), coordinators())
	}
	if err := pes.TellCoordinator(ctx, whole.ID, "baseline_required"); err != nil {
		t.Fatal(err)
	}
	if err := pes.GiveRepository(ctx, whole.ID, "acme/whole", "https://github.com/acme/whole.git", "main"); err != nil || coordinators() != 1 {
		t.Fatalf("given its repository: %v, %d coordinators wanted, want its coordinator", err, coordinators())
	}

	refused, _ := draft()
	if err := pes.FailRepository(ctx, refused.ID, "GitHub said no"); err != nil {
		t.Fatal(err)
	}
	got, err := pes.GetPlatformExperiment(ctx, refused.ID)
	if err != nil || got.Status != domain.PlatformExpClosed || got.CloseReason != domain.CloseRepositoryNotCreated || got.RepositoryError != "GitHub said no" || awaiting(refused.ID) {
		t.Fatalf("a refused repository: %+v, %v, want the draft closed repository_not_created with the answer", got, err)
	}
}

// A planner's machine is kept while there is a plan to make, and at no other time: it must not
// idle beside a proposal a person may read for days, nor come back when the experiment runs.
func TestALauncherKeepsAPlannerOnlyWhileItsDraftIsBeingPlanned(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	pe, planner := createTestDraft(t, pool)
	const windDown = 2 * time.Minute

	listed := func() (found bool, ended domain.RunEnd) {
		t.Helper()
		agents, err := pes.ListLauncherAgents(ctx, windDown)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.AgentID == planner {
				return true, a.Ended
			}
		}
		return false, ""
	}
	// keptUntil moves the end of the time the planner is kept after its answer.
	keptUntil := func(fromNow time.Duration) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET planner_ends_at = now() + make_interval(secs => $2) WHERE id = $1 AND proposed_at IS NOT NULL`, pe, fromNow.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	// over says the planner's run has ended, which is what ends its token.
	over := func() bool {
		t.Helper()
		at, err := plannerAnsweredAt(ctx, pes, pe, planner)
		if err != nil {
			t.Fatal(err)
		}
		return at != nil
	}
	const kept = 10 * time.Minute
	wait := time.Now().UTC().Add(7 * 24 * time.Hour)
	until := time.Now().UTC().Add(30 * time.Minute)

	if found, ended := listed(); !found || ended != "" || over() {
		t.Fatalf("while planning: listed %v, ended %q, over %v, want kept and at work", found, ended, over())
	}
	// It answers, and is kept for a goal changed soon: its run goes on, its token works,
	// and a machine rented for it now ends when that time does.
	if err := pes.AskInstance(ctx, pe, planner, "machine-1", "launch-1"); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetProposal(ctx, pe, 1, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: kept}); err != nil {
		t.Fatal(err)
	}
	proposed, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || proposed.PlannerEndsAt == nil || time.Until(*proposed.PlannerEndsAt) < kept-time.Minute || time.Until(*proposed.PlannerEndsAt) > kept+time.Minute {
		t.Fatalf("proposed: planner kept until %v, %v, want %v from now", proposed.PlannerEndsAt, err, kept)
	}
	if found, ended := listed(); !found || ended != "" || over() {
		t.Fatalf("just proposed: listed %v, ended %q, over %v, want kept with its run going on", found, ended, over())
	}
	// An answer that replaces one leaves the moment alone: the machine was rented with it.
	if err := pes.Decline(ctx, pe, 1, "on second thought", wait, time.Hour); err != nil {
		t.Fatal(err)
	}
	if again, _ := pes.GetPlatformExperiment(ctx, pe); again.PlannerEndsAt == nil || !again.PlannerEndsAt.Equal(*proposed.PlannerEndsAt) {
		t.Fatalf("answered again: planner kept until %v, want %v still", again.PlannerEndsAt, proposed.PlannerEndsAt)
	}
	// A goal changed in that time reaches the planner on the machine it has, under the launch it
	// has, and an answer to the goal before is refused.
	if err := pes.SetGoal(ctx, pe, "it must fit one accelerator", until); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetProposal(ctx, pe, 1, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: kept}); err != ErrGoalChanged {
		t.Fatalf("proposing for the goal before: got = %v, want ErrGoalChanged", err)
	}
	revised, err := pes.GetPlatformExperiment(ctx, pe)
	if err != nil || revised.ProposedAt != nil || revised.DeclinedAt != nil || revised.PlannerEndsAt != nil || revised.Goal != "it must fit one accelerator" || revised.GoalVersion != 2 ||
		revised.PlanningEndsAt.Sub(until).Abs() > time.Second {
		t.Fatalf("goal changed: got = %+v, %v", revised, err)
	}
	if found, ended := listed(); !found || ended != "" || over() {
		t.Fatalf("planning again: listed %v, ended %q, over %v, want kept and at work again", found, ended, over())
	}
	if signup, err := pes.GetAgentSignup(ctx, pe, planner); err != nil || signup.Instance == nil || signup.Instance.Name != "machine-1" || signup.Instance.Launch != "launch-1" {
		t.Fatalf("after a change of the goal the kept planner's machine is %+v, %v, want the one it had under the launch it had", signup.Instance, err)
	}

	// It answers the new goal, and nobody changes it again: the time passes, its run is over and
	// its machine winds down, and past that it is no longer asked for.
	if err := pes.SetProposal(ctx, pe, 2, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: kept}); err != nil {
		t.Fatal(err)
	}
	keptUntil(-time.Second)
	if !over() {
		t.Fatal("a planner past the time it is kept has a run that goes on, so its token would never end")
	}
	if found, ended := listed(); !found || ended != domain.RunEndPlanned {
		t.Fatalf("past the time it is kept: listed %v, ended %q, want winding down as planned", found, ended)
	}
	keptUntil(-windDown - time.Second)
	if found, _ := listed(); found {
		t.Fatal("a planner is kept beside a proposal that waits for a person")
	}
	// A change of the goal then gives the draft back, and the planner has a plan to make again, on
	// a machine that owes nothing to the restarts of the one before.
	if err := pes.ReleaseInstances(ctx, windDown, windDown); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetGoal(ctx, pe, "it must fit one accelerator, with a second seed", until); err != nil {
		t.Fatal(err)
	}
	if found, ended := listed(); !found || ended != "" || over() {
		t.Fatalf("goal changed later: listed %v, ended %q, over %v, want asked for and at work again", found, ended, over())
	}
	if signup, err := pes.GetAgentSignup(ctx, pe, planner); err != nil || signup.Instance == nil || signup.Instance.Name != "" || signup.Instance.Launch != "" {
		t.Fatalf("after a change of the goal the released planner's instance is %+v, %v, want no launch, so that its next machine is a new one", signup.Instance, err)
	}

	// The time it is kept never passes the planning limit, which its machine was rented with.
	short, shortPlanner := createTestDraft(t, pool)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET ends_at = now() + interval '2 minutes' WHERE id = $1`, short); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetProposal(ctx, short, 1, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: kept}); err != nil {
		t.Fatal(err)
	}
	if got, _ := pes.GetPlatformExperiment(ctx, short); got.PlannerEndsAt == nil || time.Until(*got.PlannerEndsAt) > 2*time.Minute+time.Second || time.Until(*got.PlannerEndsAt) < time.Minute {
		t.Fatalf("proposed two minutes before the planning limit: planner %s kept until %v, want the limit", shortPlanner, got.PlannerEndsAt)
	}

	// Accepted, the experiment is open and then running, and the planner has nothing to do in it.
	if err := pes.SetProposal(ctx, pe, 3, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: kept}); err != nil {
		t.Fatal(err)
	}
	keptUntil(-windDown - time.Second)
	answered := over
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		setPEStatus(t, pool, pe, status, 0)
		if !answered() {
			t.Fatalf("the planner of a platform experiment that is %s has not answered", status)
		}
		if found, _ := listed(); found {
			t.Fatalf("a planner is kept in a platform experiment that is %s", status)
		}
	}
	setPEStatus(t, pool, pe, domain.PlatformExpClosed, 0)
	if found, ended := listed(); !found || ended != domain.RunEndClosed {
		t.Fatalf("just closed: listed %v, ended %q, want winding down like any agent", found, ended)
	}
	if err := pes.SetGoal(ctx, pe, "too late", until); err != ErrNotEditable {
		t.Fatalf("changing the goal of a closed draft: got = %v, want ErrNotEditable", err)
	}
	signup, err := pes.GetAgentSignup(ctx, pe, planner)
	if err != nil || signup.Instance == nil {
		t.Fatalf("signup %+v, %v", signup, err)
	}
}

func TestAcceptingADraftOpensItWithItsRosterOrChangesNothing(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id, planner := createTestDraft(t, pool)

	accepted := func(researchers int) (*domain.PlatformExperiment, []*domain.Agent, []domain.AgentSignup) {
		t.Helper()
		pe, err := pes.GetPlatformExperiment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents = "accepted", "the brief", 6, researchers
		pe.Metrics = []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}}
		pe.Stages = []domain.Stage{{LengthPct: 40, EvictPct: 50}, {LengthPct: 60}}
		pe.StartsAt, pe.DurationHours = time.Now().UTC(), 4
		pe.BaselineDueAt = pe.StartsAt.Add(time.Hour)
		pe.DefaultQuotaTier = domain.QuotaTierGuaranteed
		pe.CoordinatorInAgentSandbox, pe.Coordinator = true, &domain.AgentSandboxCoordinator{Model: "model-1"}
		acceptedAt := time.Now().UTC()
		pe.AcceptedAt, pe.AcceptedBy, pe.AcceptedAnswers = &acceptedAt, &domain.Actor{Kind: domain.ActorUser, ID: "u-accepting"}, []string{"32 only."}
		var agents []*domain.Agent
		var signups []domain.AgentSignup
		// The researchers, and last the agent that measures the baseline.
		for i := 0; i <= researchers; i++ {
			role := domain.SignupRoleCompetitor
			if i == researchers {
				role = domain.SignupRoleBaseline
			}
			agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], CreatedAt: time.Now().UTC()}
			agents = append(agents, agent)
			signups = append(signups, domain.AgentSignup{Actor: testActor, AgentID: agent.ID, Role: role, InAgentSandbox: true, Model: "model-1"})
		}
		return pe, agents, signups
	}

	{
		id, _ := createTestDraft(t, pool)
		// Past its limit a draft takes no answer, also in the minute before the sweep closes it, and
		// closed by the platform it says which limit ended it.
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET ends_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := pes.SetProposal(ctx, id, 1, []byte(`{}`), Proposed{WaitUntil: time.Now().Add(time.Hour)}); err != ErrPlanningOver {
			t.Fatalf("proposing past the limit: got = %v, want ErrPlanningOver", err)
		}
		if err := pes.Decline(ctx, id, 1, "late", time.Now().Add(time.Hour), 0); err != ErrPlanningOver {
			t.Fatalf("declining past the limit: got = %v, want ErrPlanningOver", err)
		}
		if err := pes.ClosePlatformExperiment(ctx, id, nil, ""); err != nil {
			t.Fatal(err)
		}
		if late, _ := pes.GetPlatformExperiment(ctx, id); late.CloseReason != domain.ClosePlanningLimitReached || late.ProposedAt != nil {
			t.Fatalf("a draft closed at its limit: got = %q proposed at %v, want planning_limit_reached and no proposal", late.CloseReason, late.ProposedAt)
		}
	}

	pe, agents, signups := accepted(2)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrNothingProposed {
		t.Fatalf("accepting before a proposal: got = %v, want ErrNothingProposed", err)
	}
	if err := pes.SetProposal(ctx, id, 1, []byte(`{}`), Proposed{WaitUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Declined, the draft is answered and has nothing to accept; a change of the goal gives it back
	// to the planner with the reason kept, and the next proposal clears it.
	if err := pes.Decline(ctx, id, 1, "nothing here can be measured", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	declined, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || declined.DeclinedReason != "nothing here can be measured" || declined.DeclinedAt == nil || declined.ProposedAt != nil || declined.ProposalExpiresAt.IsZero() || len(declined.Proposal) != 0 {
		t.Fatalf("declined: got = %+v, %v", declined, err)
	}
	if wanted, _ := pes.ListLauncherAgents(ctx, 0); len(wanted) != 0 {
		t.Fatalf("a planner that declined is still asked for: %+v", wanted)
	}
	pe, agents, signups = accepted(2)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrNothingProposed {
		t.Fatalf("accepting a declined draft: got = %v, want ErrNothingProposed", err)
	}
	if err := pes.SetGoal(ctx, id, "a goal, in a sentence, by validation loss", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("changing the goal of a declined draft: %v", err)
	}
	if revised, _ := pes.GetPlatformExperiment(ctx, id); revised.Answered() || revised.DeclinedReason == "" {
		t.Fatalf("goal changed after a decline: got = %+v, want it with the planner and the reason kept", revised)
	}
	if err := pes.SetProposal(ctx, id, 2, []byte(`{}`), Proposed{WaitUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if proposed, _ := pes.GetPlatformExperiment(ctx, id); proposed.DeclinedReason != "" || proposed.ProposedAt == nil || proposed.DeclinedAt != nil {
		t.Fatalf("proposed after a decline: got = %+v, want no reason", proposed)
	}
	if err := pes.Decline(ctx, "pe-none", 1, "r", time.Now(), 0); err != ErrNotDraft {
		t.Fatalf("declining what is not there: got = %v, want ErrNotDraft", err)
	}
	// More machines than the draft may have: nothing of the accept is kept. The draft's limit is
	// 5, its coordinator is one, its baseline agent another, and the planner is none of them.
	pe, agents, signups = accepted(4)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != ErrMaxAgentSandboxesReached {
		t.Fatalf("four researchers, a baseline agent and a coordinator in five machines: got = %v, want ErrMaxAgentSandboxesReached", err)
	}
	if got, _ := pes.GetPlatformExperiment(ctx, id); got.Status != domain.PlatformExpDraft || got.Name == "accepted" {
		t.Fatalf("a refused accept left %+v", got)
	}
	if joined, _ := pes.ListSignups(ctx, id); len(joined) != 1 {
		t.Fatalf("a refused accept left %d signups, want the planner's alone", len(joined))
	}

	pe, agents, signups = accepted(3)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.Status != domain.PlatformExpOpen || got.Name != "accepted" || got.Description != "the brief" || got.BudgetAcceleratorHours != 6 ||
		got.MaxAgents != 3 || len(got.Stages) != 2 || len(got.Metrics) != 1 || !got.CoordinatorInAgentSandbox || got.Coordinator == nil ||
		got.Goal != "a goal, in a sentence, by validation loss" {
		t.Fatalf("accepted: got = %+v (coordinator %+v), %v", got, got.Coordinator, err)
	}
	if got.StartsAt.IsZero() || time.Since(got.StartsAt) > time.Minute {
		t.Errorf("starts_at = %v, want now, when its baseline begins to be measured", got.StartsAt)
	}
	// The proposal is kept as proposed beside who accepted it, when and with which answers.
	if string(got.Proposal) != `{}` || got.ProposedAt == nil || got.AcceptedAt == nil || !got.AcceptedAt.Equal(pe.AcceptedAt.Truncate(time.Microsecond)) ||
		got.AcceptedBy == nil || *got.AcceptedBy != (domain.Actor{Kind: domain.ActorUser, ID: "u-accepting"}) || !slices.Equal(got.AcceptedAnswers, []string{"32 only."}) {
		t.Errorf("accepted: proposal %s proposed at %v, accepted at %v by %+v with %q, want the proposal kept and the accept recorded", got.Proposal, got.ProposedAt, got.AcceptedAt, got.AcceptedBy, got.AcceptedAnswers)
	}
	if err := pes.AcceptDraft(ctx, pe, nil, nil); err != ErrNotDraft {
		t.Fatalf("accepting twice: got = %v, want ErrNotDraft", err)
	}

	// The planner is no participant, though its signup stays in the roster.
	if taking, err := pes.ListParticipants(ctx, id); err != nil || len(taking) != 4 || slices.Contains(taking, planner) {
		t.Fatalf("participants after accept = %v, %v, want the 3 researchers and the baseline agent", taking, err)
	}

	// Accepted, nothing has run. The agent that measures the baseline joined with the accept, so
	// the launcher is asked for its machine, for no researcher's yet and no longer for the
	// planner's. It was named by its role, and holds half the budget of 6.
	baseline := agents[3]
	if baseline.Name != "Baseline 1" {
		t.Fatalf("the baseline agent is named %q, want Baseline 1", baseline.Name)
	}
	wanted, err := pes.ListLauncherAgents(ctx, time.Minute)
	if err != nil || len(wanted) != 1 || wanted[0].AgentID != baseline.ID || wanted[0].Ended != "" {
		t.Fatalf("the launcher is asked for %+v, %v, want the baseline agent alone", wanted, err)
	}
	if quota, err := pes.GetAgentQuota(ctx, baseline.ID, id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 3 || quota.BurstAcceleratorHours != 0 {
		t.Fatalf("the baseline agent's quota = %+v, %v, want 3 guaranteed accelerator-hours", quota, err)
	}
	// Every machine the draft may have is taken, so a second baseline agent has none while the
	// first holds its own, and takes its place once the first's machine stopped for good.
	second := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], CreatedAt: time.Now().UTC()}
	another := domain.AgentSignup{Actor: testActor, AgentID: second.ID, Role: domain.SignupRoleBaseline, InAgentSandbox: true, AgentType: "baseline", Model: "model-1"}
	if err := pes.SpawnAgent(ctx, id, second, another); err != ErrMaxAgentSandboxesReached {
		t.Fatalf("a second baseline agent beside the first: got = %v, want ErrMaxAgentSandboxesReached", err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO agentsandbox_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, true) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, id, baseline.ID); err != nil {
		t.Fatal(err)
	}
	if err := pes.SpawnAgent(ctx, id, second, another); err != nil || second.Name != "Baseline 2" {
		t.Fatalf("a second baseline agent once the first's machine stopped: got = %v, named %q, want it added as Baseline 2", err, second.Name)
	}
	if err := pes.RemoveAgent(ctx, id, second.ID, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `INSERT INTO agentsandbox_instances (platform_experiment_id, agent_id, stopped) VALUES ($1, $2, false) ON CONFLICT (platform_experiment_id, agent_id) DO UPDATE SET stopped = EXCLUDED.stopped`, id, baseline.ID); err != nil {
		t.Fatal(err)
	}

	// Started, the budget is shared among the researchers: the planner and the baseline agent
	// take no share, and the baseline agent keeps the quota it measured against.
	started, quotas, err := pes.StartPlatformExperimentTx(ctx, id, time.Hour, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		var out []*domain.AgentQuota
		for _, p := range participants {
			if p.AgentID == planner || p.AgentID == baseline.ID {
				t.Errorf("%s takes a share of the run", p.AgentID)
			}
			out = append(out, &domain.AgentQuota{ID: uuid.New().String(), AgentID: p.AgentID, PlatformExperimentID: id, GuaranteedAcceleratorHours: 1, CreatedAt: time.Now().UTC()})
		}
		return out, oneStage, nil
	})
	if err != nil || !started || len(quotas) != 3 {
		t.Fatalf("start: started %v with %d quotas, %v, want the 3 researchers'", started, len(quotas), err)
	}
	if quota, err := pes.GetAgentQuota(ctx, baseline.ID, id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 3 {
		t.Fatalf("the baseline agent's quota after the start = %+v, %v, want the 3 it joined with", quota, err)
	}
	if wanted, err = pes.ListLauncherAgents(ctx, time.Minute); err != nil || len(wanted) != 4 {
		t.Fatalf("the launcher of a running experiment is asked for %d agents, %v, want the 3 researchers and the baseline agent", len(wanted), err)
	}
}

// A baseline agent's quota is its place's: it lands in the column its tier names, a second
// signup of the same agent changes nothing, and it goes when the agent is removed.
func TestABaselineAgentHoldsItsQuotaFromSignupUntilItIsRemoved(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, AgentType: "baseline"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierBurstOnly, AgentType: "baseline"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleCompetitor, AgentType: "generalist"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleReviewer, AgentType: "generalist"})
	// Half the budget of 10.
	for i, want := range [][2]float64{{5, 0}, {0, 5}} {
		if quota, err := pes.GetAgentQuota(ctx, agents[i], id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != want[0] || quota.BurstAcceleratorHours != want[1] {
			t.Errorf("baseline agent %d: quota = %+v, %v, want %v guaranteed and %v burst", i, quota, err, want[0], want[1])
		}
	}
	for _, other := range agents[2:] {
		if quota, err := pes.GetAgentQuota(ctx, other, id); err != nil || quota != nil {
			t.Errorf("an agent of another role: quota = %+v, %v, want none before the start", quota, err)
		}
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE agent_quotas SET guaranteed_accelerator_hours = 0.1 WHERE platform_experiment_id = $1 AND agent_id = $2`, id, agents[0]); err != nil {
		t.Fatal(err)
	}
	if inserted, err := pes.Signup(ctx, id, domain.AgentSignup{Actor: testActor, AgentID: agents[0], Role: domain.SignupRoleBaseline, AgentType: "baseline"}); err != nil || inserted {
		t.Fatalf("signing up again: got = %v, %v, want nothing inserted", inserted, err)
	}
	if quota, _ := pes.GetAgentQuota(ctx, agents[0], id); quota == nil || quota.GuaranteedAcceleratorHours != 0.1 {
		t.Errorf("a second signup rewrote the quota to %+v", quota)
	}
	if err := pes.FinishAgent(ctx, id, agents[2], "too early"); !errors.Is(err, ErrSignupClosed) {
		t.Errorf("a researcher finishing before the start: got = %v, want ErrSignupClosed", err)
	}
	if err := pes.RemoveAgent(ctx, id, agents[0], nil); err != nil {
		t.Fatal(err)
	}
	if quota, err := pes.GetAgentQuota(ctx, agents[0], id); err != nil || quota != nil {
		t.Errorf("a removed baseline agent's quota = %+v, %v, want none", quota, err)
	}
}

// A baseline agent held what one researcher holds in the first stage, 1.6 of 8 core-hours, and
// could not run a failed measurement again while the researchers' budget sat unused. It holds
// half the budget of the open experiment as it stands, whatever it was when the agent joined.
func TestABaselineAgentsQuotaIsHalfTheBudgetOfAnOpenExperiment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, AgentType: "baseline"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierBurstOnly, AgentType: "baseline"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleCompetitor, AgentType: "generalist"})
	held := func(when string, want float64) {
		t.Helper()
		for i, columns := range [][2]float64{{want, 0}, {0, want}} {
			if quota, err := pes.GetAgentQuota(ctx, agents[i], id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != columns[0] || quota.BurstAcceleratorHours != columns[1] {
				t.Errorf("%s, baseline agent %d: quota = %+v, %v, want %v guaranteed and %v burst", when, i, quota, err, columns[0], columns[1])
			}
		}
		if quota, err := pes.GetAgentQuota(ctx, agents[2], id); err != nil || quota != nil {
			t.Errorf("%s, a researcher: quota = %+v, %v, want none before the start", when, quota, err)
		}
	}
	held("at signup", 5)
	pe, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pe.BudgetAcceleratorHours = 50
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen, nil); err != nil {
		t.Fatal(err)
	}
	held("with the budget raised to 50", 25)
	pe.MaxAgents = 4
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen, nil); err != nil {
		t.Fatal(err)
	}
	held("with max_agents lowered to 4", 25)

	// An update that is refused leaves the quota with the experiment it was refused for.
	pe.BudgetAcceleratorHours = 1
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil); err == nil {
		t.Fatal("an update checked for a running experiment was written to an open one")
	}
	held("after a refused update", 25)

	// Running, the shares are what the start and the stage ladder made them.
	setPEStatus(t, pool, id, domain.PlatformExpRunning, 0)
	pe.Name = "renamed"
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil); err != nil {
		t.Fatal(err)
	}
	held("renamed while running", 25)
}

// An agent's token is wound down from the moment it finished, which is read here.
func TestAnAgentsFinishIsReadWithItsMoment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, AgentType: "baseline"},
		domain.AgentSignup{Actor: testActor, Role: domain.SignupRoleBaseline, AgentType: "baseline"})
	if err := pes.FinishAgent(ctx, id, agents[0], "the baseline is measured"); err != nil {
		t.Fatal(err)
	}
	if at, err := agentFinishedAt(ctx, pes, id, agents[0]); err != nil || at == nil || time.Since(*at) > time.Minute {
		t.Errorf("the agent that finished: got = %v, %v, want the moment it did", at, err)
	}
	for name, agent := range map[string]string{"its neighbour at work": agents[1], "an agent that never joined": "ag-none"} {
		if at, err := agentFinishedAt(ctx, pes, id, agent); err != nil || at != nil {
			t.Errorf("%s: got = %v, %v, want no moment", name, at, err)
		}
	}
}

// Without a baseline an open platform experiment never starts. One the platform closes before
// its end time says why, and one that reached its end says that.
func TestAnOpenExperimentClosedWithoutABaselineSaysTheBaselineLimitEndedIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	closed := func(prepare string, by *domain.Actor) domain.CloseReason {
		t.Helper()
		id := "pe-" + uuid.New().String()[:8]
		createTestPE(t, pool, id, 10)
		if prepare != "" {
			if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET `+prepare+` WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := pes.ClosePlatformExperiment(ctx, id, by, ""); err != nil {
			t.Fatal(err)
		}
		pe, err := pes.GetPlatformExperiment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return pe.CloseReason
	}
	for name, c := range map[string]struct {
		prepare string
		by      *domain.Actor
		want    domain.CloseReason
	}{
		"open, no baseline, before its end":   {"", nil, domain.CloseBaselineLimitReached},
		"open, no baseline, at its deadline":  {"ends_at = now() - interval '1 second'", nil, domain.CloseBaselineLimitReached},
		"open with a baseline":                {"baseline_job_id = 'job-1', ends_at = now() - interval '1 second'", nil, domain.CloseBaselineLimitReached},
		"running":                             {"status = 'running', started_at = now() - interval '1 hour', ends_at = now() - interval '1 second'", nil, domain.CloseEndTimeReached},
		"running, no researcher's machine up": {"status = 'running', ends_at = now() - interval '1 second'", nil, domain.CloseAgentsNotStarted},
		"closed by a person":                  {"", &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}, domain.CloseByRequest},
	} {
		if got := closed(c.prepare, c.by); got != c.want {
			t.Errorf("%s: close_reason = %q, want %q", name, got, c.want)
		}
	}
}

// A declared baseline is written with the change that declares it, and only while the platform
// experiment is in the status the check was made for.
func TestADeclaredBaselineIsStoredWithTheUpdateThatDeclaresIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	pe, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || pe.BaselineJobID != "" || pe.JobEstimatedDurationHours != 0 {
		t.Fatalf("a new platform experiment: got = %+v, %v, want no baseline", pe, err)
	}
	pe.Description, pe.BaselineJobID, pe.JobEstimatedDurationHours = "BASELINE measured: job-1", "job-1", 0.3
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil); err == nil {
		t.Fatal("an update checked for a running experiment was written to an open one")
	}
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen, nil); err != nil {
		t.Fatal(err)
	}
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.BaselineJobID != "job-1" || got.JobEstimatedDurationHours != 0.3 || got.Description != "BASELINE measured: job-1" {
		t.Fatalf("got = baseline %q with an estimate of %g, %v, want job-1 and 0.3", got.BaselineJobID, got.JobEstimatedDurationHours, err)
	}
	open, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range open {
		if listed.ID == id && listed.BaselineJobID != "job-1" {
			t.Fatalf("listed with baseline %q, want job-1: the sweeps read it there", listed.BaselineJobID)
		}
	}
}

// A declined draft read proposed_at with no proposal, as if something were proposed, and waited
// a week in that state. It reads as declined, and the limit a proposal waits by closes it.
func TestADeclinedDraftSaysSoAndExpiresAsAProposalDoes(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id, _ := createTestDraft(t, pool)
	if err := pes.Decline(ctx, id, 1, "nothing here can be measured", time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	listed := func() *domain.PlatformExperiment {
		t.Helper()
		drafts, err := pes.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpDraft)
		if err != nil {
			t.Fatal(err)
		}
		for _, draft := range drafts {
			if draft.ID == id {
				return draft
			}
		}
		t.Fatalf("%s is not among the drafts listed", id)
		return nil
	}
	if draft := listed(); draft.DeclinedAt == nil || draft.ProposedAt != nil || draft.DeclinedReason == "" || draft.ProposalExpiresAt.IsZero() || !draft.PlanningEndsAt.IsZero() {
		t.Fatalf("a declined draft, listed: got = %+v, want declined_at, its reason and proposal_expires_at", draft)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET ends_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if draft := listed(); draft.ClosesAt().After(time.Now()) {
		t.Fatalf("a declined draft past its limit closes at %v, want a moment the sweep acts on", draft.ClosesAt())
	}
	if err := pes.ClosePlatformExperiment(ctx, id, nil, ""); err != nil {
		t.Fatal(err)
	}
	closed, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || closed.Status != domain.PlatformExpClosed || closed.CloseReason != domain.CloseProposalExpired || closed.DeclinedAt == nil || closed.ProposalExpiresAt.IsZero() {
		t.Fatalf("closed: got = %s %q declined at %v, %v, want closed proposal_expired and still declined", closed.Status, closed.CloseReason, closed.DeclinedAt, err)
	}
}

// A planner still on its machine after its answer hears of a changed goal as its next update: an
// event on the planner's own stream, once, that points at the draft with the new goal_version and
// carries no text.
func TestAChangedGoalIsAnEventOnThePlannersStream(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	draft, planner := createTestDraft(t, pool)
	if err := pes.SetProposal(ctx, draft, 1, []byte(`{}`), Proposed{WaitUntil: time.Now().UTC().Add(time.Hour), KeepPlanner: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	before := cursorNow(t, pool)

	if err := pes.SetGoal(ctx, draft, "it must fit one accelerator", time.Now().UTC().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	own := domain.EventFilter{PlatformExperimentID: draft, AgentID: planner}
	var goals []domain.Event
	for _, e := range changesAfter(t, pes, own, before) {
		if e.Kind == platformapi.EventPlatformExperimentGoal {
			goals = append(goals, e)
		}
	}
	if len(goals) != 1 {
		t.Fatalf("goal changes on the planner's stream: got = %+v, want one", goals)
	}
	live := goals[0]
	if live.Subject != draft || live.PlatformExperimentID != draft || live.Value != "2" || live.Detail != "" || live.AgentID != planner {
		t.Fatalf("goal change: got = %+v, want a pointer at the draft with goal_version 2, addressed to its planner", live)
	}
}

// A baseline machine came up 50 s after the accept that created its agent. The proposal
// signs the baseline agent up, and its machine is wanted while the platform is to accept the
// proposal by itself, so it is up by then. A person who holds the proposal to edit it lets the
// machine go, and the accept gives the agent its quota and its machine again.
func TestAProposalSignsUpTheBaselineAgentWhoseMachineRunsUntilTheDraftIsHeld(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewStore(pool, 3)
	id, planner := createTestDraft(t, pool)
	by := domain.Actor{Kind: domain.ActorUser, ID: "u-planning"}
	baselineAgent := func() (*domain.Agent, domain.AgentSignup) {
		agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], CreatedAt: time.Now().UTC()}
		return agent, domain.AgentSignup{AgentID: agent.ID, Role: domain.SignupRoleBaseline, InAgentSandbox: true, AgentType: "baseline", Model: "model-1", Actor: &by}
	}
	wait := time.Now().UTC().Add(7 * 24 * time.Hour)
	soon := time.Now().UTC().Add(30 * time.Second)
	agent, signup := baselineAgent()
	if err := pes.SetProposal(ctx, id, 1, []byte(`{}`), Proposed{WaitUntil: wait, KeepPlanner: time.Minute, AcceptsAt: &soon, Baseline: agent, BaselineSignup: signup}); err != nil {
		t.Fatal(err)
	}
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.AcceptsAt == nil || got.AcceptsAt.Sub(soon).Abs() > time.Second {
		t.Fatalf("proposed: accepts_at %v, %v, want %v", got.AcceptsAt, err, soon)
	}
	baselineWanted := func() *LauncherAgent {
		t.Helper()
		wanted, err := pes.ListLauncherAgents(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for i := range wanted {
			if wanted[i].AgentID == agent.ID {
				return &wanted[i]
			}
		}
		return nil
	}
	wanted := baselineWanted()
	if wanted == nil || wanted.Role != domain.SignupRoleBaseline || wanted.Ended != "" {
		t.Fatalf("while the draft counts down: baseline agent wanted as %+v, want it listed", wanted)
	}
	if agent.Name != "Baseline 1" {
		t.Errorf("the baseline agent is named %q, want Baseline 1", agent.Name)
	}
	// A second proposal keeps the agent it signed up.
	again, againSignup := baselineAgent()
	if err := pes.SetProposal(ctx, id, 1, []byte(`{}`), Proposed{WaitUntil: wait, AcceptsAt: &soon, Baseline: again, BaselineSignup: againSignup}); err != nil {
		t.Fatal(err)
	}
	if joined, _ := pes.ListSignups(ctx, id); len(joined) != 2 || !slices.Contains(joined, planner) || !slices.Contains(joined, agent.ID) {
		t.Fatalf("after a second proposal the draft has %v, want its planner and the one baseline agent", joined)
	}
	if due, err := pes.DraftsToAccept(ctx); err != nil || slices.Contains(due, id) {
		t.Fatalf("drafts to accept before accepts_at: %v, %v", due, err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET accepts_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if due, err := pes.DraftsToAccept(ctx); err != nil || !slices.Contains(due, id) {
		t.Fatalf("drafts to accept past accepts_at: %v, %v, want %s among them", due, err, id)
	}

	// Held, it waits for a person and its baseline agent has no machine.
	if err := pes.HoldAccept(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := pes.HoldAccept(ctx, id); err != nil {
		t.Fatalf("holding a held draft: %v", err)
	}
	if held, _ := pes.GetPlatformExperiment(ctx, id); held.AcceptsAt != nil {
		t.Fatalf("held: accepts_at %v, want none", held.AcceptsAt)
	}
	if due, _ := pes.DraftsToAccept(ctx); slices.Contains(due, id) {
		t.Fatal("a held draft is among the drafts to accept")
	}
	if wanted := baselineWanted(); wanted != nil {
		t.Fatalf("a held draft's baseline agent is wanted: %+v", wanted)
	}

	// Accepted, its baseline agent takes its quota from the budget stated and its machine again,
	// and no second baseline agent is signed up.
	pe, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pe.Name, pe.Description, pe.BudgetAcceleratorHours, pe.MaxAgents = "accepted", "the brief", 6, 1
	pe.Metrics = []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}}
	pe.StartsAt, pe.DurationHours = time.Now().UTC(), 4
	pe.BaselineDueAt = pe.StartsAt.Add(time.Hour)
	pe.DefaultQuotaTier = domain.QuotaTierGuaranteed
	pe.CoordinatorInAgentSandbox, pe.Coordinator = true, &domain.AgentSandboxCoordinator{Model: "model-1"}
	acceptedAt := time.Now().UTC()
	pe.AcceptedAt = &acceptedAt
	researcher := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], CreatedAt: time.Now().UTC()}
	if err := pes.AcceptDraft(ctx, pe, []*domain.Agent{researcher}, []domain.AgentSignup{{Actor: testActor, AgentID: researcher.ID, Role: domain.SignupRoleCompetitor, InAgentSandbox: true, AgentType: "generalist", Model: "model-1"}}); err != nil {
		t.Fatal(err)
	}
	if quota, err := pes.GetAgentQuota(ctx, agent.ID, id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 3 {
		t.Fatalf("the baseline agent's quota once accepted = %+v, %v, want half the budget of 6", quota, err)
	}
	if wanted := baselineWanted(); wanted == nil {
		t.Fatalf("the accepted draft's baseline agent is wanted as %+v, want it listed", wanted)
	}
	if got, err := pes.GetPlatformExperiment(ctx, id); err != nil || got.AcceptedAt == nil || got.AcceptedBy != nil || got.AcceptedAnswers != nil || len(got.Proposal) == 0 {
		t.Fatalf("accepted by the platform: got = %+v, %v, want the proposal kept, accepted_at and no accepted_by", got, err)
	}
	if err := pes.HoldAccept(ctx, id); err != ErrNotDraft {
		t.Fatalf("holding what is no draft: got = %v, want ErrNotDraft", err)
	}

	// A change of the goal gives the draft back to its planner, and nothing is accepted by itself
	// until it proposes again.
	revising, _ := createTestDraft(t, pool)
	if err := pes.HoldAccept(ctx, revising); err != ErrNotProposed {
		t.Fatalf("holding a draft with nothing proposed: got = %v, want ErrNotProposed", err)
	}
	if err := pes.SetProposal(ctx, revising, 1, []byte(`{}`), Proposed{WaitUntil: wait, AcceptsAt: &soon}); err != nil {
		t.Fatal(err)
	}
	if err := pes.SetGoal(ctx, revising, "smaller", time.Now().UTC().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if revised, _ := pes.GetPlatformExperiment(ctx, revising); revised.AcceptsAt != nil {
		t.Fatalf("goal changed: accepts_at %v, want none", revised.AcceptsAt)
	}
}

func plannerAnsweredAt(ctx context.Context, pes *Store, pe, agent string) (*time.Time, error) {
	standing, err := pes.AgentStanding(ctx, pe, agent)
	return standing.PlannerAnsweredAt, err
}

func agentFinishedAt(ctx context.Context, pes *Store, pe, agent string) (*time.Time, error) {
	standing, err := pes.AgentStanding(ctx, pe, agent)
	return standing.FinishedAt, err
}
