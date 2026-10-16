package db

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A raise of the baseline writes the experiment and its history together, is told to every agent
// with its sentence, live and replayed, and is refused once the baseline it raises from moved.
func TestARaisedBaselineIsKeptInTheHistoryAndToldToEveryAgent(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	agentID, peID, _ := seedRun(t, pool, "raise", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', baseline_job_id = 'job-1' WHERE id = $1`, peID)
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()

	pe, err := pes.GetPlatformExperiment(ctx, peID)
	if err != nil {
		t.Fatal(err)
	}
	pe.BaselineJobID, pe.Description = "job-2", "the brief, with job-2 measured"
	raise := domain.BaselineRaise{From: "job-1", To: "job-2", Reason: "the control's other coder", Detail: "the baseline was raised to score = 2 by job job-2",
		Actor: domain.Actor{Kind: domain.ActorCoordinator, ID: peID}}
	if err := pes.RaiseBaseline(ctx, pe, raise); err != nil {
		t.Fatal(err)
	}
	live := awaitEvent(t, events, domain.EventPlatformExperimentBaseline, peID)
	if live.Value != "job-2" || live.Detail != raise.Detail {
		t.Fatalf("live: got = %+v, want job-2 with the sentence", live)
	}
	if own := (domain.EventFilter{PlatformExperimentID: peID, AgentID: agentID}); !own.Matches(live) {
		t.Fatal("an agent's own subscription does not carry the raise")
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: peID, AgentID: agentID}, before)
	if err != nil {
		t.Fatal(err)
	}
	var raises []domain.Event
	for _, e := range replayed {
		if e.Kind == domain.EventPlatformExperimentBaseline {
			raises = append(raises, e)
		}
	}
	if len(raises) != 1 || raises[0].Identity() != live.Identity() || raises[0].Detail != live.Detail {
		t.Fatalf("replayed: got = %+v, want the live event %+v once", raises, live)
	}

	read, err := pes.GetPlatformExperiment(ctx, peID)
	if err != nil {
		t.Fatal(err)
	}
	if read.BaselineJobID != "job-2" || read.Description != "the brief, with job-2 measured" || len(read.BaselineRaises) != 1 {
		t.Fatalf("read: baseline %q, brief %q, raises %+v, want job-2, its brief and one raise", read.BaselineJobID, read.Description, read.BaselineRaises)
	}
	if got := read.BaselineRaises[0]; got.From != "job-1" || got.To != "job-2" || got.Reason != raise.Reason || got.Actor.Kind != domain.ActorCoordinator || got.Actor.ID != peID || got.At.IsZero() {
		t.Errorf("raise read back: got = %+v, want %+v with its time", got, raise)
	}

	// Raised again from the baseline it had before, it is refused and records nothing.
	pe.BaselineJobID = "job-3"
	raise.From, raise.To = "job-1", "job-3"
	if err := pes.RaiseBaseline(ctx, pe, raise); err == nil {
		t.Fatal("a raise from a baseline that moved: got = nil, want an error")
	}
	if read, _ := pes.GetPlatformExperiment(ctx, peID); read.BaselineJobID != "job-2" || len(read.BaselineRaises) != 1 {
		t.Errorf("after the refused raise: baseline %q, %d raises, want job-2 and one", read.BaselineJobID, len(read.BaselineRaises))
	}
}
