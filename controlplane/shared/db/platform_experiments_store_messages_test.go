package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A message reaches its agent alone, live and replayed, with who it is from and its text, is
// kept for the run, and is refused to an agent whose run ended and past the hourly limit.
func TestAMessageIsToldToItsAgentAloneAndKept(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	agentID, peID, _ := seedRun(t, pool, "message", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id) VALUES ($1, $2)`, peID, agentID)
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()

	coordinator := domain.Actor{Kind: domain.ActorCoordinator, ID: peID}
	sent, err := pes.SendAgentMessages(ctx, peID, []string{agentID}, "leave the lr line, try warmup", coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].AgentID != agentID || sent[0].From != "coordinator" || sent[0].At.IsZero() {
		t.Fatalf("sent: got = %+v, want one message from the coordinator", sent)
	}
	live := awaitEvent(t, events, EventAgentMessage, agentID)
	if live.Value != "coordinator" || live.Detail != "leave the lr line, try warmup" || live.AgentID != agentID {
		t.Fatalf("live: got = %+v, want the text from the coordinator", live)
	}
	if own := (EventFilter{PlatformExperimentID: peID, AgentID: agentID}); !own.Matches(live) {
		t.Error("the agent's own subscription does not carry its message")
	}
	if rival := (EventFilter{PlatformExperimentID: peID, AgentID: "another"}); rival.Matches(live) {
		t.Error("a rival's subscription carries the message")
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, EventFilter{PlatformExperimentID: peID, AgentID: agentID}, before)
	if err != nil {
		t.Fatal(err)
	}
	var told []Event
	for _, e := range replayed {
		if e.Kind == EventAgentMessage {
			told = append(told, e)
		}
	}
	if len(told) != 1 || told[0].Identity() != live.Identity() {
		t.Fatalf("replayed: got = %+v, want the live event %+v once", told, live)
	}

	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	for range domain.MaxAgentMessagesPerHour - 1 {
		if _, err := pes.SendAgentMessages(ctx, peID, nil, "to every researcher", owner); err != nil {
			t.Fatal(err)
		}
	}
	var limited *MessagesRateLimited
	if _, err := pes.SendAgentMessages(ctx, peID, []string{agentID}, "one too many", coordinator); !errors.As(err, &limited) || limited.AgentID != agentID || limited.Next.IsZero() {
		t.Fatalf("past the hourly limit: got = %v, want MessagesRateLimited for %s", err, agentID)
	}
	kept, err := pes.ListAgentMessages(ctx, peID, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != domain.MaxAgentMessagesPerHour || kept[0].Text != "leave the lr line, try warmup" || kept[1].From != "owner" || kept[1].Actor.ID != "user-1" {
		t.Fatalf("kept: got = %+v, want the %d sent, oldest first", kept, domain.MaxAgentMessagesPerHour)
	}

	if _, err := pes.SendAgentMessages(ctx, peID, []string{"nobody"}, "hello", coordinator); !errors.Is(err, ErrNotSignedUp) {
		t.Errorf("to an agent not signed up: got = %v, want = %v", err, ErrNotSignedUp)
	}
	exec(t, pool, `UPDATE experiment_signups SET finished_at = now() WHERE platform_experiment_id = $1`, peID)
	if _, err := pes.SendAgentMessages(ctx, peID, []string{agentID}, "hello", coordinator); !errors.Is(err, ErrAgentEnded) {
		t.Errorf("to a finished agent: got = %v, want = %v", err, ErrAgentEnded)
	}
	if _, err := pes.SendAgentMessages(ctx, peID, nil, "hello", coordinator); !errors.Is(err, ErrNoResearcherAtWork) {
		t.Errorf("to every researcher with none at work: got = %v, want = %v", err, ErrNoResearcherAtWork)
	}
	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)
	if _, err := pes.SendAgentMessages(ctx, peID, []string{agentID}, "hello", coordinator); !errors.Is(err, ErrPlatformExperimentClosed) {
		t.Errorf("once closed: got = %v, want = %v", err, ErrPlatformExperimentClosed)
	}
}

// What an agent may still be sent this hour is counted as a send refuses: the messages of the
// last hour, by everyone together, and it grows again when the oldest of them leaves the hour.
func TestTheMessageAllowanceCountsTheLastHour(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	agentID, peID, _ := seedRun(t, pool, "allowance", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id) VALUES ($1, $2)`, peID, agentID)
	allowance := func() domain.MessageAllowance {
		t.Helper()
		all, err := pes.MessageAllowance(ctx, peID)
		if err != nil {
			t.Fatal(err)
		}
		a, ok := all[agentID]
		if !ok || len(all) != 1 {
			t.Fatalf("allowance %+v, want the one agent", all)
		}
		return a
	}
	if got := allowance(); got.LeftThisHour != domain.MaxAgentMessagesPerHour || got.NextAt != nil {
		t.Fatalf("none sent: %+v, want the whole hour's and no next_at", got)
	}
	coordinator := domain.Actor{Kind: domain.ActorCoordinator, ID: peID}
	for range 2 {
		if _, err := pes.SendAgentMessages(ctx, peID, []string{agentID}, "direction", coordinator); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, pool, `UPDATE agent_messages SET created_at = now() - interval '50 minutes' WHERE id = (SELECT min(id) FROM agent_messages WHERE platform_experiment_id = $1)`, peID)
	got := allowance()
	if got.LeftThisHour != domain.MaxAgentMessagesPerHour-2 || got.NextAt == nil || time.Until(*got.NextAt) < 9*time.Minute || time.Until(*got.NextAt) > 11*time.Minute {
		t.Fatalf("two sent, the oldest 50 minutes ago: %+v, want %d left and the next in 10 minutes", got, domain.MaxAgentMessagesPerHour-2)
	}
	exec(t, pool, `UPDATE agent_messages SET created_at = now() - interval '61 minutes' WHERE platform_experiment_id = $1`, peID)
	if got := allowance(); got.LeftThisHour != domain.MaxAgentMessagesPerHour || got.NextAt != nil {
		t.Fatalf("both older than an hour: %+v, want the whole hour's", got)
	}
}

// The agent lines of a close record are kept as taken before the close, and one closed keeps
// them; the summary's time moves with each write of it.
func TestCloseAgentsAreKeptAsTakenBeforeTheClose(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	_, peID, _ := seedRun(t, pool, "close-agents", true)
	if lines, err := pes.CloseAgents(ctx, peID); err != nil || lines != "" {
		t.Fatalf("none taken: %q, %v", lines, err)
	}
	if err := pes.SetCloseAgents(ctx, peID, "Agents:\n- a: at work.\n"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE platform_experiments SET status = 'closed', closed_at = now() WHERE id = $1`, peID)
	if err := pes.SetCloseAgents(ctx, peID, "Agents:\n- a: released.\n"); err != nil {
		t.Fatal(err)
	}
	if lines, err := pes.CloseAgents(ctx, peID); err != nil || lines != "Agents:\n- a: at work.\n" {
		t.Fatalf("after the close: %q, %v, want the lines taken before it", lines, err)
	}

	if at, err := pes.SummaryWrittenAt(ctx, peID); err != nil || at != nil {
		t.Fatalf("no summary: %v, %v", at, err)
	}
	if err := pes.SetPlatformExperimentSummary(ctx, peID, "what the run found"); err != nil {
		t.Fatal(err)
	}
	if at, err := pes.SummaryWrittenAt(ctx, peID); err != nil || at == nil || time.Since(*at) > time.Minute {
		t.Fatalf("summary written: %v, %v, want now", at, err)
	}
}
