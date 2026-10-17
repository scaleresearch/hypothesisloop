package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// A token works only while the rows it names say so: the tokens of a run closed an hour ago are
// refused, and once it is reopened its coordinator's and the agents' the close ended are accepted
// again, while the token of one that finished or was cut before the close stays refused.
func TestAReopenedRunsTokensAreAcceptedByTheRowsAlone(t *testing.T) {
	pool := db.TestPool(t)
	ctx := context.Background()
	run := db.ClosedRunWithAgents(t, pool)
	const secret = "a-signing-secret-only-this-test-knows"
	authenticator, err := auth.NewAuthenticator(config.AuthConfig{SigningSecret: secret}, auth.NewStoreDirectory(pool))
	if err != nil {
		t.Fatal(err)
	}
	accepted := func(scope string) bool {
		t.Helper()
		token, err := auth.Mint(secret, scope)
		if err != nil {
			t.Fatal(err)
		}
		_, err = authenticator.Authenticate(ctx, token)
		return err == nil
	}
	live, coordinator, finished, cut := auth.AgentScope(run.ID, run.Live), auth.CoordinatorScope(run.ID), auth.AgentScope(run.ID, run.Finished), auth.AgentScope(run.ID, run.Cut)
	for _, scope := range []string{live, coordinator, finished, cut} {
		if accepted(scope) {
			t.Fatalf("closed an hour ago: %s accepted, want refused", scope)
		}
	}

	pes := db.NewPlatformExperimentsStore(pool)
	pe, err := pes.GetPlatformExperiment(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	endsAt := time.Now().UTC().Add(5 * time.Hour)
	if err := pes.ReopenPlatformExperiment(ctx, run.ID, endsAt, endsAt.Sub(pe.StartsAt).Hours(), ""); err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[string]bool{live: true, coordinator: true, finished: false, cut: false} {
		if got := accepted(scope); got != want {
			t.Errorf("reopened: %s accepted = %v, want %v", scope, got, want)
		}
	}
}
