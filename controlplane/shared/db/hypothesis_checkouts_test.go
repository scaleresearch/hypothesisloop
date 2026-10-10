package db

import (
	"context"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A checkout says who works on a hypothesis before any job of theirs does: in working, in
// implementations and in the hypotheses in flight of the situation. A checkout older than
// domain.CheckoutClaim, or of an agent that finished, claims nothing; a job in flight still does.
func TestACheckoutClaimsTheHypothesisBeforeAJob(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store, pes, experiments := NewHypothesesStore(pool), NewPlatformExperimentsStore(pool), NewExperimentsStore(pool)
	first, peID, hypothesisID := seedRun(t, pool, "checkout", true)
	second, _, _ := seedRun(t, pool, "checkout-b", true)
	stale, _, _ := seedRun(t, pool, "checkout-c", true)
	finished, _, _ := seedRun(t, pool, "checkout-d", true)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, finished_at) VALUES ($1, $2, now())`, peID, finished)
	t.Cleanup(func() {
		exec(t, pool, `DELETE FROM experiment_signups WHERE agent_id = $1`, finished)
		exec(t, pool, `DELETE FROM experiments WHERE platform_experiment_id = $1`, peID)
		exec(t, pool, `DELETE FROM hypothesis_checkouts WHERE platform_experiment_id = $1`, peID)
	})

	others, err := store.CheckOutHypothesis(ctx, peID, hypothesisID, first, "hl/pe/first/idea")
	if err != nil || len(others) != 0 {
		t.Fatalf("first checkout: others = %+v, %v, want nobody else", others, err)
	}
	for _, agent := range []string{stale, finished} {
		if _, err := store.CheckOutHypothesis(ctx, peID, hypothesisID, agent, "hl/pe/"+agent+"/idea"); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, pool, `UPDATE hypothesis_checkouts SET checked_out_at = now() - interval '3 hours' WHERE agent_id = $1`, stale)
	others, err = store.CheckOutHypothesis(ctx, peID, hypothesisID, second, "hl/pe/second/idea")
	if want := []domain.HypothesisWorker{{AgentID: first, Name: first}}; err != nil || !reflect.DeepEqual(others, want) {
		t.Fatalf("second checkout: others = %+v, %v, want %+v", others, err, want)
	}
	// Checking out again renews the claim and keeps one row per agent.
	if _, err := store.CheckOutHypothesis(ctx, peID, hypothesisID, first, "hl/pe/first/idea"); err != nil {
		t.Fatal(err)
	}

	exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', 'checkout-job', $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', 'RUNNING', 1, now(), 'AK', 'SK')`, stale, peID, hypothesisID)

	working, err := store.Working(ctx, []string{hypothesisID, "hyp-nobody"})
	if want := []domain.HypothesisWorker{{AgentID: first, Name: first}, {AgentID: second, Name: second}, {AgentID: stale, Name: stale}}; err != nil || !reflect.DeepEqual(working[hypothesisID], want) || len(working) != 1 {
		t.Errorf("working = %+v, %v, want %+v alone", working, err, want)
	}

	implementations, err := experiments.Implementations(ctx, hypothesisID)
	if err != nil || len(implementations) != 4 {
		t.Fatalf("implementations = %+v, %v, want the four branches checked out", implementations, err)
	}
	for _, i := range implementations {
		if i.CodeRef != "" || i.CheckedOutAt == nil {
			t.Errorf("implementation %+v: want no code_ref before a job and when it was checked out", i)
		}
	}

	inFlight, err := pes.HypothesesInFlight(ctx, peID)
	if err != nil || len(inFlight) != 1 || inFlight[0].HypothesisID != hypothesisID || inFlight[0].Queued != 0 || inFlight[0].Running != 1 ||
		!reflect.DeepEqual(inFlight[0].AgentIDs, []string{first, second, stale}) {
		t.Errorf("in flight = %+v, %v, want the hypothesis with its running job and the agents of the claims and the job", inFlight, err)
	}

	if err := store.DeleteHypothesisComments(ctx, hypothesisID); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE experiments SET status = 'COMPLETED' WHERE id = 'checkout-job'`)
	if working, err := store.Working(ctx, []string{hypothesisID}); err != nil || len(working) != 0 {
		t.Errorf("working on a removed hypothesis = %+v, %v, want nobody", working, err)
	}
}
