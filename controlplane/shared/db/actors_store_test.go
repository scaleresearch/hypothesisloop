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
	key := &domain.APIKey{ID: "key-" + uuid.New().String(), OrgID: orgID, CreatedBy: userID, Name: "laptop"}
	if err := accounts.CreateAPIKey(ctx, key, "hash-"+key.ID); err != nil {
		t.Fatalf("create key: %v", err)
	}
	agentID := "agent-" + uuid.New().String()
	createTestAgent(t, pool, agentID, domain.AgentKindAgent)
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
	createTestAgent(t, pool, agentID, domain.AgentKindAgent)
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
		if err != nil || c.AgentID != w.owner || c.Actor != w.actor || c.Source != domain.SourceOf(w.owner) {
			t.Fatalf("comment of %q by %+v: got = %+v, %v", w.owner, w.actor, c, err)
		}
	}
	// Rows from before the credential was kept: an agent's, and a person's under a typed name.
	for _, old := range [][3]string{{agentID, "agent", ""}, {"", "human", "Grace"}} {
		if _, err := pool.pool.Exec(ctx, `
INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, source, author, actor_kind, text, created_at)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, 'unverified', 'old', now() + interval '1 minute')`, hypothesisID, peID, old[0], old[1], old[2]); err != nil {
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
