package db

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
		MaxAgents: 1, PlanningEndsAt: now.Add(30 * time.Minute), Status: domain.PlatformExpDraft,
		Metrics: []domain.MetricDefinition{}, ReportIntervalSeconds: 30, Stages: []domain.Stage{{LengthPct: 100}},
		CreatedAt: now, UpdatedAt: now, MaxHosted: 5,
	}
	planner := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Planner", PerformanceScore: 0.5, CreatedAt: now}
	signup := domain.AgentSignup{AgentID: planner.ID, Role: domain.SignupRolePlanner, QuotaTier: domain.QuotaTierGuaranteed,
		Hosted: true, Launcher: launcher, Flavor: domain.PlannerFlavor, Model: "model-1"}
	if err := NewPlatformExperimentsStore(pool).CreateDraft(context.Background(), pe, planner, signup, nil); err != nil {
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
	if err != nil || signup == nil || signup.Name != "Planner" || signup.Flavor != domain.PlannerFlavor || signup.Role != domain.SignupRolePlanner || !signup.Hosted {
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
	if err := pes.SetProposal(ctx, pe, []byte(`{"budget_accelerator_hours":6}`), wait); err != nil {
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
	bad := &domain.PlatformExperiment{ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Status: domain.PlatformExpDraft, PlanningEndsAt: now, Stages: []domain.Stage{{LengthPct: 100}}}
	agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12]}
	if err := pes.CreateDraft(ctx, bad, agent, domain.AgentSignup{AgentID: agent.ID, Flavor: "generalist", Hosted: true}, nil); err == nil {
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

	listed := func() (found bool, ended domain.RunEnd) {
		t.Helper()
		agents, err := pes.ListLauncherAgents(ctx, launcher, windDown)
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
	propose := func(ago time.Duration) {
		t.Helper()
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET proposed_at = now() - make_interval(secs => $2) WHERE id = $1`, pe, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	answered := func() bool {
		t.Helper()
		at, err := pes.PlannerAnsweredAt(ctx, pe, planner)
		if err != nil {
			t.Fatal(err)
		}
		return at != nil
	}

	if found, ended := listed(); !found || ended != "" || answered() {
		t.Fatalf("while planning: listed %v, ended %q, answered %v, want kept and at work", found, ended, answered())
	}
	propose(0)
	if !answered() {
		t.Fatal("a planner whose proposal is stored has not answered, so its token would never end")
	}
	if found, ended := listed(); !found || ended != domain.RunEndPlanned {
		t.Fatalf("just proposed: listed %v, ended %q, want winding down as planned", found, ended)
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
		revised.Revisions[0].Actor.Kind != domain.ActorUser || revised.Revisions[0].Actor.ID != "u-planning" || revised.PlanningEndsAt.Sub(until).Abs() > time.Second {
		t.Fatalf("revised: got = %+v, %v", revised, err)
	}
	if found, ended := listed(); !found || ended != "" || answered() {
		t.Fatalf("revising: listed %v, ended %q, answered %v, want kept and at work again", found, ended, answered())
	}
	if signup, err := pes.GetAgentSignup(ctx, pe, planner); err != nil || signup.Instance == nil || signup.Instance.Launch != "" {
		t.Fatalf("after a revision the planner's launch is %+v, %v, want none, so that its next machine is a new launch", signup.Instance, err)
	}
	// Accepted, the experiment is open and then running, and the planner has nothing to do in it.
	propose(windDown + time.Second)
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
			agent := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
			agents = append(agents, agent)
			signups = append(signups, domain.AgentSignup{AgentID: agent.ID, Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: launcher, Flavor: "generalist", Model: "model-1"})
		}
		return pe, agents, signups
	}

	{
		id, _ := createTestDraft(t, pool, launcher+"-late")
		// Past its limit a draft takes no answer, also in the minute before the sweep closes it, and
		// closed by the platform it says which limit ended it.
		if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET ends_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := pes.SetProposal(ctx, id, []byte(`{}`), time.Now().Add(time.Hour)); err != ErrPlanningOver {
			t.Fatalf("proposing past the limit: got = %v, want ErrPlanningOver", err)
		}
		if err := pes.Decline(ctx, id, "late", time.Now().Add(time.Hour)); err != ErrPlanningOver {
			t.Fatalf("declining past the limit: got = %v, want ErrPlanningOver", err)
		}
		if err := pes.ClosePlatformExperiment(ctx, id, nil); err != nil {
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
	if err := pes.SetProposal(ctx, id, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Declined, the draft is answered and has nothing to accept; a revision gives it back to the
	// planner with the reason kept, and the next proposal clears it.
	if err := pes.Decline(ctx, id, "nothing here can be measured", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	declined, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || declined.DeclinedReason != "nothing here can be measured" || declined.DeclinedAt == nil || declined.ProposedAt != nil || declined.ProposalExpiresAt.IsZero() || len(declined.Proposal) != 0 {
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
	if revised, _ := pes.GetPlatformExperiment(ctx, id); revised.Answered() || revised.DeclinedReason == "" {
		t.Fatalf("revised after a decline: got = %+v, want it with the planner and the reason kept", revised)
	}
	if err := pes.SetProposal(ctx, id, []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if proposed, _ := pes.GetPlatformExperiment(ctx, id); proposed.DeclinedReason != "" || proposed.ProposedAt == nil || proposed.DeclinedAt != nil {
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

	pe, agents, signups = accepted(3)
	if err := pes.AcceptDraft(ctx, pe, agents, signups); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.Status != domain.PlatformExpOpen || got.Name != "accepted" || got.Description != "the brief" || got.BudgetAcceleratorHours != 6 ||
		got.MaxAgents != 3 || len(got.Stages) != 2 || len(got.Metrics) != 1 || !got.CoordinatorHosted || got.Coordinator == nil || got.Coordinator.Launcher != launcher ||
		got.Goal != "a goal, in a sentence" {
		t.Fatalf("accepted: got = %+v (coordinator %+v), %v", got, got.Coordinator, err)
	}
	if got.StartsAt.IsZero() || time.Since(got.StartsAt) > time.Minute {
		t.Errorf("starts_at = %v, want now, so that the platform starts it if nobody does", got.StartsAt)
	}
	if err := pes.AcceptDraft(ctx, pe, nil, nil); err != ErrNotDraft {
		t.Fatalf("accepting twice: got = %v, want ErrNotDraft", err)
	}

	// The planner is no participant, though its signup stays in the roster.
	if taking, err := pes.ListParticipants(ctx, id); err != nil || len(taking) != 3 || slices.Contains(taking, planner) {
		t.Fatalf("participants after accept = %v, %v, want the 3 researchers", taking, err)
	}

	// Accepted, nothing has run. The launcher runs the coordinator, which has the baseline
	// measured, and no researcher yet, and no longer the planner.
	wanted, err := pes.ListLauncherAgents(ctx, launcher, time.Minute)
	if err != nil || len(wanted) != 0 {
		t.Fatalf("the launcher is asked for %d agents, %v, want none before there is a baseline", len(wanted), err)
	}
	coordinators, err := pes.ListLauncherCoordinators(ctx, launcher, time.Minute)
	if err != nil || len(coordinators) != 1 || coordinators[0].PlatformExperimentID != id {
		t.Fatalf("coordinators = %+v, %v", coordinators, err)
	}
	// The agent the coordinator adds to measure the baseline holds, from that moment, what one
	// researcher holds in the first stage: 40 percent of 6 over 3 places, in the one machine the draft has left.
	baseline := &domain.Agent{ID: "ag-" + uuid.New().String()[:12], Name: "Baseline", PerformanceScore: 0.5, CreatedAt: time.Now().UTC()}
	if err := pes.SpawnAgent(ctx, id, baseline, domain.AgentSignup{AgentID: baseline.ID, Role: domain.SignupRoleBaseline, Hosted: true, Launcher: launcher, Flavor: "generalist", Model: "model-1"}); err != nil {
		t.Fatalf("add the baseline agent: %v", err)
	}
	if quota, err := pes.GetAgentQuota(ctx, baseline.ID, id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 0.8 || quota.BurstAcceleratorHours != 0 {
		t.Fatalf("the baseline agent's quota = %+v, %v, want 0.8 guaranteed accelerator-hours", quota, err)
	}
	if wanted, err = pes.ListLauncherAgents(ctx, launcher, time.Minute); err != nil || len(wanted) != 1 || wanted[0].AgentID != baseline.ID {
		t.Fatalf("the launcher is asked for %+v, %v, want the baseline agent alone", wanted, err)
	}

	// Started, the budget is shared among the researchers: the planner and the baseline agent
	// take no share, and the baseline agent keeps the quota it measured against.
	started, quotas, err := pes.StartPlatformExperimentTx(ctx, id, func(participants []StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
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
	if quota, err := pes.GetAgentQuota(ctx, baseline.ID, id); err != nil || quota == nil || quota.GuaranteedAcceleratorHours != 0.8 {
		t.Fatalf("the baseline agent's quota after the start = %+v, %v, want the 0.8 it joined with", quota, err)
	}
	if wanted, err = pes.ListLauncherAgents(ctx, launcher, time.Minute); err != nil || len(wanted) != 4 {
		t.Fatalf("the launcher of a running experiment is asked for %d agents, %v, want the 3 researchers and the baseline agent", len(wanted), err)
	}
}

// A baseline agent's quota is its place's: it lands in the column its tier names, a second
// signup of the same agent changes nothing, and it goes when the agent is removed.
func TestABaselineAgentHoldsItsQuotaFromSignupUntilItIsRemoved(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Role: domain.SignupRoleBaseline, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierBurstOnly, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleCompetitor, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleReviewer, Flavor: "generalist"})
	// 40 percent of 10 over 10 places.
	for i, want := range [][2]float64{{0.4, 0}, {0, 0.4}} {
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
	if inserted, err := pes.Signup(ctx, id, domain.AgentSignup{AgentID: agents[0], Role: domain.SignupRoleBaseline, Flavor: "generalist"}); err != nil || inserted {
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

// A baseline agent's quota is a share of the budget over max_agents, so it is the share of the
// open experiment as it stands, whatever it was when the agent joined.
func TestABaselineAgentsQuotaFollowsTheBudgetAndTheFieldOfAnOpenExperiment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Role: domain.SignupRoleBaseline, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleBaseline, QuotaTier: domain.QuotaTierBurstOnly, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleCompetitor, Flavor: "generalist"})
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
	// 40 percent of 10 over 10 places.
	held("at signup", 0.4)
	pe, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pe.BudgetAcceleratorHours = 50
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen); err != nil {
		t.Fatal(err)
	}
	held("with the budget raised to 50", 2)
	pe.MaxAgents = 4
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen); err != nil {
		t.Fatal(err)
	}
	held("with max_agents lowered to 4", 5)

	// An update that is refused leaves the quota with the experiment it was refused for.
	pe.BudgetAcceleratorHours = 1
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning); err == nil {
		t.Fatal("an update checked for a running experiment was written to an open one")
	}
	held("after a refused update", 5)

	// Running, the shares are what the start and the stage ladder made them.
	setPEStatus(t, pool, id, domain.PlatformExpRunning, 0)
	pe.Name = "renamed"
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning); err != nil {
		t.Fatal(err)
	}
	held("renamed while running", 5)
}

// An agent's token is wound down from the moment it finished, which is read here.
func TestAnAgentsFinishIsReadWithItsMoment(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	agents := spawnTestAgents(t, pes, id,
		domain.AgentSignup{Role: domain.SignupRoleBaseline, Flavor: "generalist"},
		domain.AgentSignup{Role: domain.SignupRoleBaseline, Flavor: "generalist"})
	if err := pes.FinishAgent(ctx, id, agents[0], "the baseline is measured"); err != nil {
		t.Fatal(err)
	}
	if at, err := pes.AgentFinishedAt(ctx, id, agents[0]); err != nil || at == nil || time.Since(*at) > time.Minute {
		t.Errorf("the agent that finished: got = %v, %v, want the moment it did", at, err)
	}
	for name, agent := range map[string]string{"its neighbour at work": agents[1], "an agent that never joined": "ag-none"} {
		if at, err := pes.AgentFinishedAt(ctx, id, agent); err != nil || at != nil {
			t.Errorf("%s: got = %v, %v, want no moment", name, at, err)
		}
	}
}

// Without a baseline an open platform experiment never starts. One the platform closes before
// its end time says why, and one that reached its end says that.
func TestAnOpenExperimentClosedWithoutABaselineSaysTheBaselineLimitEndedIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	closed := func(prepare string, by *domain.Actor) domain.CloseReason {
		t.Helper()
		id := "pe-" + uuid.New().String()[:8]
		createTestPE(t, pool, id, 10)
		if prepare != "" {
			if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET `+prepare+` WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := pes.ClosePlatformExperiment(ctx, id, by); err != nil {
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
		"open, no baseline, before its end":  {"", nil, domain.CloseBaselineLimitReached},
		"open, no baseline, at its deadline": {"ends_at = now() - interval '1 second'", nil, domain.CloseBaselineLimitReached},
		"open with a baseline":               {"baseline_job_id = 'job-1', ends_at = now() - interval '1 second'", nil, domain.CloseBaselineLimitReached},
		"running":                            {"status = 'running', ends_at = now() - interval '1 second'", nil, domain.CloseEndTimeReached},
		"closed by a person":                 {"", &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}, domain.CloseByRequest},
	} {
		if got := closed(c.prepare, c.by); got != c.want {
			t.Errorf("%s: close_reason = %q, want %q", name, got, c.want)
		}
	}
}

// A planner once held a quota, the allowance its baseline job ran against. It runs no job now,
// and a row left from then is removed where the schema is applied.
func TestAQuotaLeftWithAPlannerIsRemoved(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	pe, planner := createTestDraft(t, pool, "launcher-"+uuid.New().String()[:8])
	if _, err := pool.pool.Exec(ctx, upsertAgentQuotaSQL, agentQuotaUpsertArgs(&domain.AgentQuota{
		ID: uuid.New().String(), AgentID: planner, PlatformExperimentID: pe, GuaranteedAcceleratorHours: 2, CreatedAt: time.Now().UTC(),
	})...); err != nil {
		t.Fatal(err)
	}
	applyTestSchema(t, pool)
	if quota, err := pes.GetAgentQuota(ctx, planner, pe); err != nil || quota != nil {
		t.Fatalf("the planner's quota = %+v, %v, want none", quota, err)
	}
}

// A proposal stored before the baseline had its own columns carried the job and the estimate a
// trial takes from it. Rows from then keep both, and their proposals no longer state them.
func TestABaselineStoredInAProposalMovesToItsColumns(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	if _, err := pool.pool.Exec(ctx, `UPDATE platform_experiments SET proposal = $2 WHERE id = $1`, id,
		`{"budget_accelerator_hours":6,"baseline_job_id":"job-old","accelerator_type":"rtx-4090","job_estimated_duration_hours":0.3}`); err != nil {
		t.Fatal(err)
	}
	applyTestSchema(t, pool)
	applyTestSchema(t, pool)
	got, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || got.BaselineJobID != "job-old" || got.JobEstimatedDurationHours != 0.3 {
		t.Fatalf("got = baseline %q with an estimate of %g, %v, want job-old and 0.3", got.BaselineJobID, got.JobEstimatedDurationHours, err)
	}
	var left map[string]any
	if err := json.Unmarshal(got.Proposal, &left); err != nil || len(left) != 2 || left["accelerator_type"] != "rtx-4090" {
		t.Fatalf("the proposal keeps %v, %v, want its budget and its type alone", left, err)
	}
}

// A declared baseline is written with the change that declares it, and only while the platform
// experiment is in the status the check was made for.
func TestADeclaredBaselineIsStoredWithTheUpdateThatDeclaresIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	id := "pe-" + uuid.New().String()[:8]
	createTestPE(t, pool, id, 10)
	pe, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || pe.BaselineJobID != "" || pe.JobEstimatedDurationHours != 0 {
		t.Fatalf("a new platform experiment: got = %+v, %v, want no baseline", pe, err)
	}
	pe.Description, pe.BaselineJobID, pe.JobEstimatedDurationHours = "BASELINE measured: job-1", "job-1", 0.3
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning); err == nil {
		t.Fatal("an update checked for a running experiment was written to an open one")
	}
	if err := pes.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpOpen); err != nil {
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
	pes := NewPlatformExperimentsStore(pool)
	id, _ := createTestDraft(t, pool, "launcher-"+uuid.New().String()[:8])
	if err := pes.Decline(ctx, id, "nothing here can be measured", time.Now().Add(time.Hour)); err != nil {
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
	if err := pes.ClosePlatformExperiment(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	closed, err := pes.GetPlatformExperiment(ctx, id)
	if err != nil || closed.Status != domain.PlatformExpClosed || closed.CloseReason != domain.CloseProposalExpired || closed.DeclinedAt == nil || closed.ProposalExpiresAt.IsZero() {
		t.Fatalf("closed: got = %s %q declined at %v, %v, want closed proposal_expired and still declined", closed.Status, closed.CloseReason, closed.DeclinedAt, err)
	}
}
