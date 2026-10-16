package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestActorsAreNamedByWhatTheyCarryNow(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	accounts := NewAccountsStore(pool)
	agents := NewAgentsStore(pool)
	userID, orgID := newTestUserHome(t, accounts)
	quiet, _ := newTestUserHome(t, accounts)
	if _, err := pool.pool.Exec(ctx, `UPDATE users SET name = '' WHERE id = $1`, quiet); err != nil {
		t.Fatalf("clear a name: %v", err)
	}
	key := &domain.APIKey{ID: "key-" + uuid.New().String(), OrgID: orgID, Actor: domain.Actor{Kind: domain.ActorUser, ID: userID}, Name: "laptop"}
	if err := accounts.CreateAPIKey(ctx, key, "hash-"+key.ID); err != nil {
		t.Fatalf("create key: %v", err)
	}
	agentID := "agent-" + uuid.New().String()
	createTestAgent(t, pool, agentID)
	if _, err := pool.pool.Exec(ctx, `UPDATE agents SET name = 'Rosalind' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("name an agent: %v", err)
	}

	cases := []struct {
		actor domain.Actor
		want  string
	}{
		{domain.Actor{Kind: domain.ActorUser, ID: userID}, "name of " + userID},
		{domain.Actor{Kind: domain.ActorUser, ID: userID, Name: "a name the caller brought"}, "name of " + userID},
		{domain.Actor{Kind: domain.ActorUser, ID: quiet}, quiet},
		{domain.Actor{Kind: domain.ActorUser, ID: "user-who-is-gone"}, "user-who-is-gone"},
		{domain.Actor{Kind: domain.ActorKey, ID: key.ID}, "laptop"},
		{domain.Actor{Kind: domain.ActorKey, ID: userID}, userID},
		{domain.Actor{Kind: domain.ActorAgent, ID: agentID}, "Rosalind"},
		{domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-1"}, "coordinator"},
		{domain.Actor{Kind: domain.ActorStaff}, "staff"},
		{domain.Actor{Kind: domain.ActorUnverified, Name: "Grace, as typed"}, "Grace, as typed"},
	}
	actors := make([]*domain.Actor, len(cases))
	for i := range cases {
		actors[i] = &cases[i].actor
	}
	if err := agents.NameActors(ctx, actors); err != nil {
		t.Fatalf("NameActors: got = %v, want = nil", err)
	}
	for _, c := range cases {
		if c.actor.Name != c.want {
			t.Errorf("%s %q: got name = %q, want = %q", c.actor.Kind, c.actor.ID, c.actor.Name, c.want)
		}
	}
	if err := agents.NameActors(ctx, nil); err != nil {
		t.Errorf("NameActors of none: got = %v, want = nil", err)
	}
	if err := agents.NameActors(ctx, []*domain.Actor{{Kind: "robot", ID: "r"}}); err == nil {
		t.Error("NameActors of an unknown kind: got = nil, want an error")
	}
}

func TestACommentKeepsWhoseItIsAndWhoWroteIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	hyps := NewHypothesesStore(pool)
	peID, agentID := "pe-"+uuid.New().String(), "agent-"+uuid.New().String()
	createTestPE(t, pool, peID, 10)
	createTestAgent(t, pool, agentID)
	hypothesisID := "hyp-" + uuid.New().String()

	written := []struct {
		owner string
		actor domain.Actor
	}{
		{agentID, domain.Actor{Kind: domain.ActorAgent, ID: agentID}},
		{agentID, domain.Actor{Kind: domain.ActorUser, ID: "user-ada"}},
		{"", domain.Actor{Kind: domain.ActorKey, ID: "key-1"}},
		{"", domain.Actor{Kind: domain.ActorStaff}},
	}
	for _, w := range written {
		c, err := hyps.CreateHypothesisComment(ctx, hypothesisID, peID, w.owner, w.actor, "note")
		if err != nil || c.AgentID != w.owner || c.Actor != w.actor {
			t.Fatalf("comment of %q by %+v: got = %+v, %v", w.owner, w.actor, c, err)
		}
	}
	// Rows from before the credential was kept: an agent's, and a person's under a typed name.
	for _, old := range [][3]string{{agentID, "agent", ""}, {"", "human", "Grace"}} {
		if _, err := pool.pool.Exec(ctx, `
INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, author, actor_kind, text, created_at)
VALUES ($1, $2, NULLIF($3, ''), $4, 'unverified', 'old', now() + interval '1 minute')`, hypothesisID, peID, old[0], old[2]); err != nil {
			t.Fatalf("insert an old comment: %v", err)
		}
	}

	read, err := hyps.ListCommentsByHypothesis(ctx, hypothesisID, 50, 0)
	if err != nil || len(read) != len(written)+2 {
		t.Fatalf("read comments: got = %d, %v, want = %d", len(read), err, len(written)+2)
	}
	for i, w := range written {
		if read[i].AgentID != w.owner || read[i].Actor != w.actor {
			t.Errorf("comment %d: got owner %q actor %+v, want owner %q actor %+v", i, read[i].AgentID, read[i].Actor, w.owner, w.actor)
		}
	}
	if got, want := read[len(written)].Actor, (domain.Actor{Kind: domain.ActorUnverified, Name: agentID}); got != want {
		t.Errorf("an old agent comment: got actor %+v, want %+v", got, want)
	}
	if got, want := read[len(written)+1].Actor, (domain.Actor{Kind: domain.ActorUnverified, Name: "Grace"}); got != want {
		t.Errorf("an old human comment: got actor %+v, want %+v", got, want)
	}
}

func TestAJobAndItsFindingKeepWhoseTheyAreAndWhoMadeThem(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	experiments, hyps := NewExperimentsStore(pool), NewHypothesesStore(pool)
	peID, agentID := "pe-"+uuid.New().String(), "agent-"+uuid.New().String()
	createTestPE(t, pool, peID, 10)
	createTestAgent(t, pool, agentID)
	key := domain.Actor{Kind: domain.ActorKey, ID: "key-1"}
	person := domain.Actor{Kind: domain.ActorUser, ID: "user-ada"}

	job := func(id string, actor domain.Actor) *domain.Experiment {
		return &domain.Experiment{
			ID: id, AgentID: agentID, Actor: actor, PlatformExperimentID: peID,
			HypothesisID: "hyp-" + peID, Hypothesis: "h",
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40", AcceleratorCount: 1,
			EstimatedDurationHours: 1, EstimatedCostAccH: 1, Status: domain.StatusQueued,
			Job:     domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-L40", AcceleratorCount: 1},
			DataKey: domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}
	}
	byKey, old := "job-by-key-"+peID, "job-old-"+peID
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM hypothesis_findings WHERE platform_experiment_id = $1`, peID)
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiments WHERE platform_experiment_id = $1`, peID)
	})
	if err := createExperiment(ctx, pool.pool, job("job-nobodys-"+peID, domain.Actor{})); err == nil {
		t.Fatal("a job with no actor was stored")
	}
	for id, actor := range map[string]domain.Actor{byKey: key, old: person} {
		if err := createExperiment(ctx, pool.pool, job(id, actor)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// A job from before the credential was kept.
	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET actor_kind = 'unverified', actor_id = '' WHERE id = $1`, old); err != nil {
		t.Fatalf("age a job: %v", err)
	}
	for id, want := range map[string]domain.Actor{byKey: key, old: {Kind: domain.ActorUnverified, Name: agentID}} {
		got, err := experiments.GetExperiment(ctx, id)
		if err != nil || got.AgentID != agentID || got.Actor != want {
			t.Errorf("job %s: got = %+v by %+v, %v, want the agent's by %+v", id, got.AgentID, got.Actor, err, want)
		}
	}

	filed, err := hyps.CreateHypothesisFinding(ctx, "hyp-"+peID, peID, byKey, agentID, person, "it held")
	if err != nil || filed.AgentID != agentID || filed.Actor != person {
		t.Fatalf("file a finding: got = %+v, %v, want the agent's by %+v", filed, err, person)
	}
	if _, err := pool.pool.Exec(ctx, `
INSERT INTO hypothesis_findings (hypothesis_id, platform_experiment_id, experiment_id, agent_id, actor_kind, summary, created_at)
VALUES ($1, $2, $3, $4, 'unverified', 'an old one', now() - interval '1 hour')`, "hyp-"+peID, peID, old, agentID); err != nil {
		t.Fatalf("insert an old finding: %v", err)
	}
	listed, err := hyps.ListFindingsByHypothesis(ctx, "hyp-"+peID, 50, 0)
	if err != nil || len(listed) != 2 {
		t.Fatalf("findings: got = %d, %v, want 2", len(listed), err)
	}
	if want := (domain.Actor{Kind: domain.ActorUnverified, Name: agentID}); listed[0].Actor != want || listed[1].Actor != person {
		t.Errorf("findings: got actors %+v then %+v, want %+v then %+v", listed[0].Actor, listed[1].Actor, want, person)
	}
	newest, err := hyps.NewestFindings(ctx, []string{"hyp-" + peID})
	if err != nil || newest["hyp-"+peID] == nil || newest["hyp-"+peID].Actor != person {
		t.Errorf("newest finding: got = %+v, %v, want the one by %+v", newest["hyp-"+peID], err, person)
	}
}
