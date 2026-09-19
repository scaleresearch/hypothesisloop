package db

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The notification triggers are the one part of this package whose behaviour is defined by the
// database and not by Go, so the tests below run against a real PostgreSQL or not at all: a fake
// cannot show that a NOTIFY inherits its transaction, which is the entire reason the events are
// emitted from a trigger rather than from a Go write path.
// eventsTestPrefix namespaces the rows these tests insert.
const eventsTestPrefix = "watch-test-"

// eventsTestDB connects to the run's scratch database and skips the test when no database is
// reachable.
func eventsTestDB(t *testing.T) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: testDSN(), MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Skipf("no database at %s: %v", testDSN(), err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// listenForEvents starts a listener and returns the channel it delivers on, already listening by
// the time this returns — a listener started after the write it is meant to observe proves
// nothing.
func listenForEvents(t *testing.T, pool *Pool) (<-chan Event, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, 16)
	ready := make(chan struct{})
	go func() {
		conn, err := pool.pool.Acquire(ctx)
		if err != nil {
			close(ready)
			return
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "LISTEN "+eventsChannel); err != nil {
			close(ready)
			return
		}
		close(ready)
		for {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				return
			}
			var e Event
			if err := json.Unmarshal([]byte(notification.Payload), &e); err != nil {
				return
			}
			select {
			case events <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	<-ready
	return events, cancel
}

// seedRun inserts an agent, a platform experiment and one comment on a hypothesis, inside one
// transaction, committing or rolling back as asked. The comment insert is what fires a trigger;
// the hypothesis itself lives in the graph store, so here it is only an id.
func seedRun(t *testing.T, pool *Pool, suffix string, commit bool) (agentID, peID, hypothesisID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: got = %v, want = nil", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	agentID = "watch-test-agent-" + suffix
	peID = "watch-test-pe-" + suffix
	hypothesisID = "watch-test-hypothesis-" + suffix
	if _, err := tx.Exec(ctx, `INSERT INTO agents (id, name) VALUES ($1, $1)`, agentID); err != nil {
		t.Fatalf("insert agent: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_experiments (id, name, budget_accelerator_hours, starts_at, ends_at)
VALUES ($1, $1, 1, now(), now() + interval '1 hour')`, peID); err != nil {
		t.Fatalf("insert platform experiment: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, text)
VALUES ($1, $2, $3, 'watch test comment')`, hypothesisID, peID, agentID); err != nil {
		t.Fatalf("insert comment: got = %v, want = nil", err)
	}
	if !commit {
		return agentID, peID, hypothesisID
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: got = %v, want = nil", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, q := range []string{
			`DELETE FROM hypothesis_findings WHERE platform_experiment_id = $1`,
			`DELETE FROM hypothesis_comments WHERE platform_experiment_id = $1`,
			`DELETE FROM experiments WHERE platform_experiment_id = $1`,
			`DELETE FROM platform_experiment_cuts WHERE platform_experiment_id = $1`,
			`DELETE FROM platform_experiments WHERE id = $1`,
		} {
			if _, err := pool.pool.Exec(cleanupCtx, q, peID); err != nil {
				t.Errorf("cleanup: got = %v, want = nil", err)
			}
		}
		if _, err := pool.pool.Exec(cleanupCtx, `DELETE FROM agents WHERE id = $1`, agentID); err != nil {
			t.Errorf("cleanup agent: got = %v, want = nil", err)
		}
	})
	return agentID, peID, hypothesisID
}

// noHypothesisEvents stands in for the graph store where a test exercises only Postgres-derived
// replay.
type noHypothesisEvents struct{}

func (noHypothesisEvents) ReplayEvents(context.Context, string, int64) ([]Event, error) {
	return nil, nil
}

func (noHypothesisEvents) ConcernsAgent(_ context.Context, _ string, events []Event) ([]bool, error) {
	return make([]bool, len(events)), nil
}

// This is the whole reason the events are emitted by a trigger inside the writing transaction. A
// stream that can announce a state the database then threw away is worse than no stream at all:
// an agent would act on a job it believes exists, and no later event would ever correct it,
// because a rollback leaves nothing behind to notify about.
func TestAChangeInARolledBackTransactionProducesNoEvent(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()

	_, peID, _ := seedRun(t, pool, "rollback", false)

	// Scoped to this test's own write, the same way the committed case is. The listener sees
	// every event on the database, and the development database it runs against is shared with a
	// control-service and with every other test in this package — "no event at all" is a claim
	// about the whole system, not about the rollback.
	deadline := time.After(time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == EventCommentNew && e.PlatformExperimentID == peID {
				t.Fatalf("event after a rolled-back write: got = %v, want = %v", e.Kind, "no event")
			}
		case <-deadline:
			return
		}
	}
}

// The guard on the test above: it would pass just as happily against a trigger that never fires
// at all. This one proves the same write, committed, does reach a listener — so "nothing arrived"
// above means the rollback suppressed it, not that nothing was ever wired up.
func TestTheSameChangeCommittedDoesReachAListener(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()

	seedRun(t, pool, "commit", true)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind != EventCommentNew {
				continue
			}
			if e.PlatformExperimentID != "watch-test-pe-commit" {
				continue
			}
			if e.Position <= 0 || e.Cursor <= 0 || e.Cursor > e.Position {
				t.Errorf("event position and cursor: got = %v and %v, want = a transaction id and a cursor at or below it", e.Position, e.Cursor)
			}
			return
		case <-deadline:
			t.Fatalf("comment.new after a committed write: got = %v, want = one event", "no event")
		}
	}
}

// cursorNow is the cursor a client would hold had it seen everything committed so far.
func cursorNow(t *testing.T, pool *Pool) int64 {
	t.Helper()
	var cursor int64
	if err := pool.pool.QueryRow(context.Background(), `SELECT pg_snapshot_xmin(pg_current_snapshot())::text::bigint`).Scan(&cursor); err != nil {
		t.Fatalf("current cursor: got = %v, want = nil", err)
	}
	return cursor
}

// insertJob writes one experiment row and walks it QUEUED -> SUBMITTED -> RUNNING, the way the
// scheduler does, returning a cursor from just before the first transition.
func insertJob(t *testing.T, pool *Pool, agentID, peID, hypothesisID, jobID string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := cursorNow(t, pool)
	if _, err := pool.pool.Exec(ctx, `INSERT INTO experiments
(id, agent_id, project_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ($1, $2, 'p', $3, 'r@0', 'h', 'd', $4, 'claim', 'objective', 'test-accelerator',
        'QUEUED', 'capacity_unavailable', 1, now(), 'AK', 'SK')`,
		jobID, agentID, peID, hypothesisID); err != nil {
		t.Fatalf("insert experiment: got = %v, want = nil", err)
	}
	for _, q := range []string{
		`UPDATE experiments SET status = 'SUBMITTED', not_admitted_reason = NULL, submitted_at = now(), updated_at = now() WHERE id = $1`,
		`UPDATE experiments SET status = 'RUNNING', updated_at = now() WHERE id = $1`,
	} {
		if _, err := pool.pool.Exec(ctx, q, jobID); err != nil {
			t.Fatalf("advance experiment: got = %v, want = nil", err)
		}
	}
	return before
}

// Replay is what makes a dropped connection a delay rather than a gap, and it is derived from the
// experiment row's own columns rather than from any stored event log — the whole argument for
// not writing the same state twice. If those columns cannot reproduce the sequence, the argument
// fails and this stream would need a log after all.
func TestReplayReconstructsAJobsStatusSequenceFromTheRowsOwnColumns(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := seedRun(t, pool, "replay", true)
	before := insertJob(t, pool, agentID, peID, hypothesisID, "watch-test-job-replay")

	events, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(),
		EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentStatus: true}}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	var values []string
	for _, e := range events {
		values = append(values, e.Value)
	}
	want := "QUEUED SUBMITTED RUNNING"
	if got := strings.Join(values, " "); got != want {
		t.Errorf("replayed status sequence: got = %v, want = %v", got, want)
	}
}

// A reconnecting client drops a replayed event only when it is identical to one it printed, so a
// replayed event must equal its live twin, including after a later write to the row that changed
// neither the status nor the queue reason. (A queue reason already cleared is not replayed at all.)
func TestReplayedJobEventsEqualTheirLiveTwins(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "twins", true)
	const jobID = "watch-test-job-twins"
	before := insertJob(t, pool, agentID, peID, hypothesisID, jobID)
	exec(t, pool, `UPDATE experiments SET quota_settled_at = now(), updated_at = now() WHERE id = $1`, jobID)

	var live []Event
	deadline := time.After(5 * time.Second)
	for len(live) < 4 {
		select {
		case e := <-events:
			if e.Subject == jobID {
				live = append(live, e)
			}
		case <-deadline:
			t.Fatalf("live events for %s: got = %+v, want = QUEUED, blocked, SUBMITTED, RUNNING", jobID, live)
		}
	}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(),
		EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentStatus: true, EventExperimentBlocked: true}}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	twin := map[[3]string]Event{}
	for _, e := range live {
		twin[[3]string{e.Kind, e.Subject, e.Value}] = e.Identity()
	}
	statuses := 0
	for _, e := range replayed {
		want, ok := twin[[3]string{e.Kind, e.Subject, e.Value}]
		if !ok {
			t.Errorf("replayed event %+v: got = no live twin, want = one", e)
			continue
		}
		if e.Identity() != want {
			t.Errorf("replayed event: got = %+v, want = its live twin %+v", e.Identity(), want)
		}
		statuses++
	}
	if statuses != 3 {
		t.Errorf("replayed statuses: got = %v, want = QUEUED, SUBMITTED, RUNNING", statuses)
	}
}

// A job nobody has judged yet has nothing to report, and hl job submit already says summary_gate.
func TestAJobAwaitingAdmissionOrASummaryIsNotAnnouncedAsBlocked(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "unjudged", true)
	const jobID = "watch-test-job-unjudged"
	before := cursorNow(t, pool)
	exec(t, pool, `INSERT INTO experiments
(id, agent_id, project_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ($1, $2, 'p', $3, 'r@0', 'h', 'd', $4, 'claim', 'objective', 'test-accelerator',
        'QUEUED', 'awaiting_admission', 1, now(), 'AK', 'SK')`, jobID, agentID, peID, hypothesisID)
	store := NewEventsStore(pool, noHypothesisEvents{})
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentBlocked: true}}
	for _, reason := range []string{"awaiting_admission", "summary_gate"} {
		exec(t, pool, `UPDATE experiments SET not_admitted_reason = $2, updated_at = now() WHERE id = $1`, jobID, reason)
		replayed, err := store.Replay(context.Background(), filter, before)
		if err != nil || len(replayed) != 0 {
			t.Errorf("replay of a job held as %s: got = %+v, %v, want = no blocked event", reason, replayed, err)
		}
	}
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = 'capacity_unavailable', updated_at = now() WHERE id = $1`, jobID)
	if got := awaitEvent(t, events, EventExperimentBlocked, jobID).Value; got != "capacity_unavailable" {
		t.Errorf("first live blocked event: got = %v, want = capacity_unavailable", got)
	}
}

// A client hands back the cursor of the last event it saw. It must be told everything from that
// event's transaction on, which may repeat that event, and nothing from before it.
func TestReplayFromAnEventsCursorReturnsThatEventAndWhatCameAfterIt(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := seedRun(t, pool, "cursor", true)
	before := insertJob(t, pool, agentID, peID, hypothesisID, "watch-test-job-cursor")

	store := NewEventsStore(pool, noHypothesisEvents{})
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentStatus: true}}
	all, err := store.Replay(context.Background(), filter, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	if len(all) < 2 {
		t.Fatalf("replayed events: got = %v, want = at least 2", len(all))
	}
	rest, err := store.Replay(context.Background(), filter, all[len(all)-2].Cursor)
	if err != nil {
		t.Fatalf("replay from the second-to-last cursor: got = %v, want = nil", err)
	}
	if len(rest) != 2 || rest[0].Identity() != all[len(all)-2].Identity() || rest[1].Identity() != all[len(all)-1].Identity() {
		t.Errorf("events from the second-to-last cursor: got = %+v, want = the last two of %+v", rest, all)
	}
}

// awaitEvent returns the first event of the given kind and subject, ignoring everything else on
// the channel — the tests share one database, so a stream carrying another test's rows is the
// ordinary case and not a failure.
func awaitEvent(t *testing.T, events <-chan Event, kind, subject string) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == kind && e.Subject == subject {
				return e
			}
		case <-deadline:
			t.Fatalf("%s for %s: got = %v, want = one event", kind, subject, "no event")
		}
	}
}

func exec(t *testing.T, pool *Pool, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: got = %v, want = nil", query, err)
	}
}

// The description is the question every agent in the run is working on, and it is edited mid-run.
// The event says only that it changed: a description is unbounded, and copying it onto the stream
// would make the event a second copy of a row anyone can already GET — the one thing this stream
// is built never to be.
func TestEditingTheDescriptionAnnouncesThatItChangedAndCarriesNoneOfItsText(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, _ := seedRun(t, pool, "brief", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `UPDATE platform_experiments SET description = $2 WHERE id = $1`, peID,
		"the question, restated at some length after the coordinator resolved it")

	e := awaitEvent(t, events, EventPlatformExperimentDescription, peID)
	if e.Value != "" || e.Detail != "" {
		t.Errorf("description event payload: got = %v/%v, want = empty, a pointer only", e.Value, e.Detail)
	}
}

// An agent is told to stop when the run closes, and nothing told it the run had closed: a closed
// platform experiment looked exactly like a quiet one, so the agent kept spending on a competition
// that had already finished.
func TestClosingThePlatformExperimentAnnouncesItsNewStatus(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, _ := seedRun(t, pool, "closed", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)

	if got := awaitEvent(t, events, EventPlatformExperimentStatus, peID).Value; got != "closed" {
		t.Errorf("announced run status: got = %v, want = %v", got, "closed")
	}
}

// One cut row says two different things to two different audiences: the ladder moved, which is
// news for everyone, and this agent is done, which only that agent can act on. Emitting only the
// boundary left every agent GETting to find out whether the cut was its own — the poll this event
// exists to remove.
func TestACutAnnouncesBothTheLadderBoundaryAndTheCutAgentsOwnStopCondition(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, _ := seedRun(t, pool, "cut", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index)
VALUES ($1, $2, 1)`, peID, agentID)

	if got := awaitEvent(t, events, EventStageBoundary, peID).Detail; got != "cut" {
		t.Errorf("boundary detail: got = %v, want = %v", got, "cut")
	}
	if got := awaitEvent(t, events, EventAgentCut, agentID).PlatformExperimentID; got != peID {
		t.Errorf("agent.cut platform experiment: got = %v, want = %v", got, peID)
	}
}

// The rollback guarantee has to hold for every kind, not only the one it was first written for.
// These are emitted by triggers added later, and a trigger added later is exactly where a
// notify outside the writing transaction would creep in — announcing a verdict, a closure or a cut
// the database then threw away, with no later event to correct it.
func TestTheNewKindsEmitNothingWhenTheirTransactionRollsBack(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, _ := seedRun(t, pool, "newkinds-rollback", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: got = %v, want = nil", err)
	}
	for _, q := range []string{
		`UPDATE platform_experiments SET status = 'closed', description = 'rewritten' WHERE id = '` + peID + `'`,
		`INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index)
VALUES ('` + peID + `', '` + agentID + `', 1)`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("exec in transaction: got = %v, want = nil", err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: got = %v, want = nil", err)
	}

	deadline := time.After(time.Second)
	for {
		select {
		case e := <-events:
			if e.PlatformExperimentID == peID {
				t.Errorf("event after a rolled-back write: got = %v, want = %v", e.Kind, "no event")
			}
		case <-deadline:
			return
		}
	}
}

// Replay is what makes a dropped connection a delay rather than a gap, and it has to cover the
// whole vocabulary: a kind that is only ever live is a kind an agent silently misses across every
// reconnect, which puts it straight back to polling for exactly that thing. These are derived
// from the changed row's own columns, with no event log to fall back on.
func TestReplayReturnsEachOfTheNewKindsFromTheChangedRowsOwnColumns(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	agentID, peID, hypothesisID := seedRun(t, pool, "newkinds-replay", true)

	exec(t, pool, `UPDATE platform_experiments SET status = 'closed', description = 'rewritten' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index)
VALUES ($1, $2, 1)`, peID, agentID)

	events, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(), EventFilter{PlatformExperimentID: peID}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	seen := map[string]string{}
	for _, e := range events {
		seen[e.Kind] = e.Subject
	}
	for kind, wantSubject := range map[string]string{
		EventCommentNew:                    hypothesisID,
		EventPlatformExperimentStatus:      peID,
		EventPlatformExperimentDescription: peID,
		EventAgentCut:                      agentID,
	} {
		if seen[kind] != wantSubject {
			t.Errorf("replayed subject for %v: got = %v, want = %v", kind, seen[kind], wantSubject)
		}
	}
}

// The cursor is the whole contract of a reconnect: be told what happened from it on, nothing
// before. A run-wide kind derived from a column rather than from a row per event is where an
// off-by-one shows up as an agent being told the run closed every single time it reconnects.
func TestReplayDoesNotReturnARunWideChangeThatHappenedBeforeTheGivenCursor(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	_, peID, _ := seedRun(t, pool, "newkinds-cursor", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)

	store := NewEventsStore(pool, noHypothesisEvents{})
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventPlatformExperimentStatus: true}}
	all, err := store.Replay(context.Background(), filter, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	if len(all) != 1 {
		t.Fatalf("replayed run status changes: got = %v, want = %v", len(all), 1)
	}
	rest, err := store.Replay(context.Background(), filter, cursorNow(t, pool))
	if err != nil {
		t.Fatalf("replay from a cursor taken after the change: got = %v, want = nil", err)
	}
	if len(rest) != 0 {
		t.Errorf("events from a cursor taken after the change: got = %v, want = %v", len(rest), 0)
	}
}

// The resume bug this cursor exists to fix. A transaction that writes first and commits last
// carries the smaller position, and a client that saw the later one's event before disconnecting
// must still be given it. A timestamp cursor (transaction start, replayed with >) skipped it.
func TestAnEventCommittedAfterALaterOneIsReplayedFromTheLaterOnesCursor(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := seedRun(t, pool, "out-of-order", true)
	events, stop := listenForEvents(t, pool)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insert := `INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, text) VALUES ($1, $2, $3, $4)`

	early, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: got = %v, want = nil", err)
	}
	defer early.Rollback(ctx) //nolint:errcheck
	if _, err := early.Exec(ctx, insert, hypothesisID+"-early", peID, agentID, "written first, committed last"); err != nil {
		t.Fatalf("insert in the open transaction: got = %v, want = nil", err)
	}
	exec(t, pool, insert, hypothesisID+"-late", peID, agentID, "written second, committed first")
	seen := awaitEvent(t, events, EventCommentNew, hypothesisID+"-late")
	if err := early.Commit(ctx); err != nil {
		t.Fatalf("commit: got = %v, want = nil", err)
	}
	committedLast := awaitEvent(t, events, EventCommentNew, hypothesisID+"-early")
	if seen.Cursor > committedLast.Position {
		t.Errorf("cursor of the event committed first: got = %v, want = at most %v, the open transaction's position", seen.Cursor, committedLast.Position)
	}

	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx,
		EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventCommentNew: true}}, seen.Cursor)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	for _, e := range replayed {
		if e.Identity() == committedLast.Identity() {
			return
		}
	}
	t.Errorf("replay from the cursor of the event committed first: got = %+v, want = it to include %+v", replayed, committedLast)
}

// One transaction writes several events that a timestamp cursor gave the same value: a cut is a
// stage.boundary and an agent.cut. A client that saw only the first before disconnecting must be
// given the rest.
func TestEveryEventOfATransactionIsReplayedFromTheCursorOfAnyOfThem(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, _ := seedRun(t, pool, "same-tx", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index) VALUES ($1, $2, 1)`, peID, agentID)
	first := awaitEvent(t, events, EventStageBoundary, peID)

	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(),
		EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventStageBoundary: true, EventAgentCut: true}}, first.Cursor)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	kinds := map[string]bool{}
	for _, e := range replayed {
		if e.Position == first.Position {
			kinds[e.Kind] = true
		}
	}
	if !kinds[EventStageBoundary] || !kinds[EventAgentCut] {
		t.Errorf("replayed kinds of the cut's transaction: got = %v, want = stage.boundary and agent.cut", kinds)
	}
}

// A write to another store is fenced by the Postgres transaction Emit holds open: while it runs,
// no cursor can pass its position, so a client cannot resume past a graph write still in flight.
func TestNoCursorPassesAWriteEmitIsStillMaking(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, hypothesisID := seedRun(t, pool, "emit", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	var position, during int64
	err := NewEventsStore(pool, noHypothesisEvents{}).Emit(context.Background(), func(stamp Stamp) ([]Event, error) {
		position, during = stamp.Position, cursorNow(t, pool)
		return []Event{{Kind: EventHypothesisNew, Subject: hypothesisID, Value: "agent", PlatformExperimentID: peID}}, nil
	})
	if err != nil {
		t.Fatalf("Emit: got = %v, want = nil", err)
	}
	if during > position {
		t.Errorf("cursor while the write was in flight: got = %v, want = at most its position %v", during, position)
	}
	if got := awaitEvent(t, events, EventHypothesisNew, hypothesisID); got.Position != position || got.Cursor > position {
		t.Errorf("emitted event: got = position %v cursor %v, want = position %v and a cursor at or below it", got.Position, got.Cursor, position)
	}
}

// A graph write commits before Emit's transaction does, so that transaction can abort after the
// graph holds its position. A later Emit has to tell that apart from a position whose events were
// sent, and from one still being written.
func TestEmitTellsAnAbortedPositionFromACommittedOrRunningOne(t *testing.T) {
	pool := eventsTestDB(t)
	store := NewEventsStore(pool, noHypothesisEvents{})
	ctx := context.Background()
	var committed, aborted int64
	if err := store.Emit(ctx, func(stamp Stamp) ([]Event, error) {
		committed = stamp.Position
		return nil, nil
	}); err != nil {
		t.Fatalf("Emit: got = %v, want = nil", err)
	}
	failed := errors.New("notify failed")
	if err := store.Emit(ctx, func(stamp Stamp) ([]Event, error) {
		aborted = stamp.Position
		return nil, failed
	}); !errors.Is(err, failed) {
		t.Fatalf("Emit: got = %v, want = %v", err, failed)
	}
	err := store.Emit(ctx, func(stamp Stamp) ([]Event, error) {
		for position, want := range map[int64]bool{committed: false, aborted: true, stamp.Position: false, 0: false} {
			if got, err := stamp.Aborted(position); err != nil || got != want {
				t.Errorf("Aborted(%d): got = (%v, %v), want = (%v, nil)", position, got, err, want)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Emit: got = %v, want = nil", err)
	}
}

// A cursor from before cursors were transaction ids is a microsecond timestamp, far above any
// transaction id. Answering it with an empty replay would silently skip everything.
func TestReplayRefusesACursorThisDatabaseCannotHaveIssued(t *testing.T) {
	pool := eventsTestDB(t)
	store := NewEventsStore(pool, noHypothesisEvents{})
	for _, since := range []int64{time.Now().UnixMicro(), -1} {
		if _, err := store.Replay(context.Background(), EventFilter{PlatformExperimentID: "pe"}, since); !errors.Is(err, ErrUnknownCursor) {
			t.Errorf("Replay(since=%d): got = %v, want = %v", since, err, ErrUnknownCursor)
		}
	}
}

// The default subscription exists so an agent does not have to know the vocabulary to stop polling.
// It therefore has to carry every signal its loop would otherwise poll for — and the loop polls for
// all of them: its jobs, why they are stuck, its allocation, its two stop conditions, the ladder,
// the brief and the pool. Only the metric pointer is left out, and that exclusion is the one thing
// this has to keep honest, because a firehose in the default would cost every subscriber its buffer
// and be the one kind a reconnect could not replay.
func TestTheDefaultKindSetIsEveryKindOnTheStreamExceptTheMetricPointer(t *testing.T) {
	kinds := DefaultKinds()
	if kinds[EventMetricPoint] {
		t.Errorf("metric.point in the default set: got = %v, want = %v", true, false)
	}
	for _, kind := range EventKinds {
		if kind.Kind == EventMetricPoint {
			continue
		}
		if !kinds[kind.Kind] {
			t.Errorf("%v in the default set: got = %v, want = %v", kind.Kind, false, true)
		}
	}
}

// The advertised vocabulary is the only list of the stream, and that has to include which kinds a
// subscription naming none receives. A default flag served by GET /watch/kinds that disagreed with
// the set the server applies is the same failure a hand-kept second list produces: an agent told it
// will be woken by something that never reaches it.
func TestTheAdvertisedDefaultFlagIsTheSetTheServerWouldApply(t *testing.T) {
	kinds := DefaultKinds()
	for _, kind := range EventKinds {
		if kind.Default != kinds[kind.Kind] {
			t.Errorf("advertised default for %v: got = %v, want = %v", kind.Kind, kind.Default, kinds[kind.Kind])
		}
	}
}

// An agent scope keeps what the agent must act on: its own jobs' outcomes, the queue reasons that
// waiting will not clear, its allocation and cut. A rival's are private state and would wake it
// for nothing, and a job moving through QUEUED, SUBMITTED or RUNNING, or waiting in the queue for
// capacity, asks nothing of it.
func TestAnAgentScopeAdmitsItsOwnOutcomesAndNoRivalsOrIntermediateStatuses(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: DefaultKinds()}
	admitted := []Event{
		{Kind: EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "EVICTED", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "exceeds_accelerator_share: cpu 64 above 8", AgentID: "agent-a"},
		{Kind: EventQuotaChanged, Value: "4", AgentID: "agent-a"},
		{Kind: EventAgentCut, Value: "1", AgentID: "agent-a"},
		{Kind: EventStageBoundary, Value: "1", Detail: "advanced"},
		{Kind: EventPlatformExperimentStatus, Value: "closed"},
		{Kind: EventPlatformExperimentDescription},
		{Kind: EventCommentNew, Value: "agent", AgentID: "agent-b"},
		{Kind: EventCommentNew, Value: "human"},
	}
	refused := []Event{
		{Kind: EventExperimentStatus, Value: "QUEUED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "SUBMITTED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "RUNNING", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "capacity_unavailable: short {accelerator:nvidia.com/gpu.product=nvidia-l40=1}", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "outranked", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "concurrency_cap", AgentID: "agent-a"},
		{Kind: EventQuotaChanged, Value: "4", AgentID: "agent-b"},
		{Kind: EventAgentCut, Value: "1", AgentID: "agent-b"},
		{Kind: EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-a"},
		{Kind: EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-b"},
		{Kind: EventMetricPoint, Value: "loss", AgentID: "agent-a"},
	}
	for _, e := range admitted {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
	for _, e := range refused {
		e.PlatformExperimentID = "pe-1"
		if filter.Matches(e) {
			t.Errorf("Matches(%+v): got = true, want = false", e)
		}
	}
}

// The agent's own writes to the pool are echoes of what it just did; waking it for them is spam.
func TestAnAgentScopeDropsTheEchoOfItsOwnHypothesisActivity(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: DefaultKinds()}
	for _, kind := range []string{EventHypothesisNew, EventHypothesisStatus, EventHypothesisLink, EventFindingNew, EventCommentNew} {
		if filter.Matches(Event{Kind: kind, Subject: "hyp-1", PlatformExperimentID: "pe-1", AgentID: "agent-a"}) {
			t.Errorf("own %v on an agent scope: got = true, want = false", kind)
		}
	}
}

// Without an agent the stream is the whole platform experiment, whoever wrote each row.
func TestASubscriptionWithoutAnAgentCarriesEveryAgentsEvents(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", Kinds: DefaultKinds()}
	for _, e := range []Event{
		{Kind: EventExperimentStatus, Value: "RUNNING", AgentID: "agent-b"},
		{Kind: EventHypothesisNew, AgentID: "agent-b"},
		{Kind: EventStageBoundary, Detail: "cut", AgentID: "agent-b"},
	} {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
}

type ownedHypotheses struct {
	owner map[string]string
	reads *int
}

func (ownedHypotheses) ReplayEvents(context.Context, string, int64) ([]Event, error) { return nil, nil }

func (o ownedHypotheses) ConcernsAgent(_ context.Context, agentID string, events []Event) ([]bool, error) {
	*o.reads++
	out := make([]bool, len(events))
	for i, e := range events {
		out[i] = o.owner[e.Subject] == agentID
	}
	return out, nil
}

// Concerns asks the graph only about hypothesis events on an agent scope, once for all of them;
// everything else Matches admitted stands.
func TestConcernsAsksTheGraphOnceAndOnlyAboutHypothesisEventsOnAnAgentScope(t *testing.T) {
	reads := 0
	store := NewEventsStore(nil, ownedHypotheses{owner: map[string]string{"hyp-a": "agent-a"}, reads: &reads})
	mine := Event{Kind: EventCommentNew, Subject: "hyp-a", AgentID: "agent-b"}
	theirs := Event{Kind: EventCommentNew, Subject: "hyp-b", AgentID: "agent-b"}
	job := Event{Kind: EventExperimentStatus, Subject: "exp-1", Value: "COMPLETED", AgentID: "agent-a"}
	events := []Event{theirs, job, mine, theirs}

	got, err := store.Concerns(context.Background(), EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a"}, events)
	if err != nil || !reflect.DeepEqual(got, []Event{job, mine}) {
		t.Errorf("Concerns on an agent scope: got = %+v, %v, want = %+v, nil", got, err, []Event{job, mine})
	}
	if reads != 1 {
		t.Errorf("graph reads for four events: got = %d, want = 1", reads)
	}

	got, err = store.Concerns(context.Background(), EventFilter{PlatformExperimentID: "pe-1"}, events)
	if err != nil || !reflect.DeepEqual(got, events) || reads != 1 {
		t.Errorf("Concerns without an agent: got = %+v, %v after %d reads, want every event and no further read", got, err, reads)
	}
}

// A default that could not be overridden would put the excluded kind out of reach. Naming kinds has
// to mean exactly those kinds — including naming the one the default leaves out, which is how an
// agent watching a single job's progress sees its samples arrive at all.
func TestAnExplicitKindSetMeansExactlyWhatItNamesEvenWhenItNamesTheExcludedKind(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", Kinds: map[string]bool{EventMetricPoint: true}}
	if !filter.Matches(Event{Kind: EventMetricPoint, Subject: "exp-1", PlatformExperimentID: "pe-1"}) {
		t.Errorf("metric.point on a subscription that named it: got = %v, want = %v", false, true)
	}
	if filter.Matches(Event{Kind: EventExperimentStatus, Subject: "exp-1", PlatformExperimentID: "pe-1"}) {
		t.Errorf("experiment.status on a subscription that named only metric.point: got = %v, want = %v", true, false)
	}
}

type fixedHypothesisEvents []Event

func (f fixedHypothesisEvents) ReplayEvents(context.Context, string, int64) ([]Event, error) {
	return f, nil
}

func (fixedHypothesisEvents) ConcernsAgent(_ context.Context, _ string, events []Event) ([]bool, error) {
	return make([]bool, len(events)), nil
}

// Hypothesis events come from the graph store, everything else from Postgres, and a reconnecting
// client must still get one stream: both halves filtered by the same subscription and interleaved
// in position order.
func TestReplayInterleavesTheGraphsHypothesisEventsWithPostgresEventsInPositionOrder(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	_, peID, hypothesisID := seedRun(t, pool, "graph-merge", true)
	var commentPosition int64
	if err := pool.pool.QueryRow(context.Background(), `SELECT created_xid FROM hypothesis_comments WHERE platform_experiment_id = $1`, peID).Scan(&commentPosition); err != nil {
		t.Fatalf("comment position: got = %v, want = nil", err)
	}
	graph := fixedHypothesisEvents{
		{Kind: EventHypothesisNew, Subject: hypothesisID, Value: "agent", PlatformExperimentID: peID, Position: commentPosition + 1},
		{Kind: EventHypothesisLink, Subject: hypothesisID, Value: "related", PlatformExperimentID: peID, Position: commentPosition + 1000},
		{Kind: EventHypothesisStatus, Subject: hypothesisID, Value: "refuted", PlatformExperimentID: peID, Position: before - 1},
	}

	events, err := NewEventsStore(pool, graph).Replay(context.Background(), EventFilter{PlatformExperimentID: peID,
		Kinds: map[string]bool{EventHypothesisNew: true, EventHypothesisLink: true, EventHypothesisStatus: true, EventCommentNew: true}}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := "comment.new hypothesis.new hypothesis.link"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("replayed kinds: got = %v, want = %v", got, want)
	}
}
