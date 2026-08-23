package db

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The notification triggers are the one part of this package whose behaviour is defined by the
// database and not by Go, so the tests below run against a real PostgreSQL or not at all: a fake
// cannot show that a NOTIFY inherits its transaction, which is the entire reason the events are
// emitted from a trigger rather than from a Go write path.
const devDSN = "postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"

// eventsTestPrefix namespaces every row these tests insert, so a developer looking at the shared
// development database can tell test debris from real data — and so a cleanup that has to be done
// by hand has something to match on.
const eventsTestPrefix = "watch-test-"

func testDSN() string {
	if dsn := os.Getenv("HYPOTHESISLOOP_TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return devDSN
}

// notificationDDL returns the part of schema.sql these tests need installed: every
// ADD COLUMN IF NOT EXISTS the schema declares, followed by the whole change-notification
// section. It is read from the shipped schema rather than restated here, so these tests can only
// ever exercise the triggers the platform actually installs. Every statement it returns is
// idempotent — ADD COLUMN IF NOT EXISTS, CREATE OR REPLACE, DROP TRIGGER IF EXISTS — which is
// what makes it safe to run against a development database that is simply a few columns behind.
func notificationDDL(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: got = %v, want = nil", err)
	}
	var ddl strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "ADD COLUMN IF NOT EXISTS") {
			ddl.WriteString(line + "\n")
		}
	}
	const marker = "CREATE OR REPLACE FUNCTION hypothesisloop_notify_event("
	start := strings.Index(string(raw), marker)
	if start < 0 {
		t.Fatalf("locate the notification section in schema.sql: got = %v, want = a %q statement", -1, marker)
	}
	ddl.WriteString(strings.TrimSuffix(strings.TrimSpace(string(raw)[start:]), "COMMIT;"))
	return ddl.String()
}

// eventsTestDB connects to a real database, makes sure the notification triggers are installed,
// and skips the test when no database is reachable.
func eventsTestDB(t *testing.T) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: testDSN(), MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Skipf("no database at %s: %v", testDSN(), err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: got = %v, want = nil", err)
	}
	defer conn.Release()
	// The simple protocol, because this is several statements in one string and the extended
	// protocol refuses those.
	if _, err := conn.Conn().PgConn().Exec(ctx, notificationDDL(t)).ReadAll(); err != nil {
		t.Fatalf("install notification triggers: got = %v, want = nil", err)
	}
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

// registerHypothesis inserts the rows a hypothesis needs and the hypothesis itself, inside one
// transaction, committing or rolling back as asked. The hypothesis insert is what fires the
// trigger; the rest exists only because the row cannot exist without it.
func registerHypothesis(t *testing.T, pool *Pool, suffix string, commit bool) (agentID, peID, hypothesisID string) {
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
	if _, err := tx.Exec(ctx, `INSERT INTO agents (id, name) VALUES ($1, $1)`, agentID); err != nil {
		t.Fatalf("insert agent: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_experiments (id, name, budget_accelerator_hours, starts_at, ends_at)
VALUES ($1, $1, 1, now(), now() + interval '1 hour')`, peID); err != nil {
		t.Fatalf("insert platform experiment: got = %v, want = nil", err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO hypotheses (agent_id, platform_experiment_id, text, normalized_text)
VALUES ($1, $2, $3, $3) RETURNING id`, agentID, peID, "watch test claim "+suffix).Scan(&hypothesisID); err != nil {
		t.Fatalf("insert hypothesis: got = %v, want = nil", err)
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
			`DELETE FROM experiments WHERE platform_experiment_id = $1`,
			`DELETE FROM platform_experiment_cuts WHERE platform_experiment_id = $1`,
			`DELETE FROM hypotheses WHERE platform_experiment_id = $1`,
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

// This is the whole reason the events are emitted by a trigger inside the writing transaction. A
// stream that can announce a state the database then threw away is worse than no stream at all:
// an agent would act on a job it believes exists, and no later event would ever correct it,
// because a rollback leaves nothing behind to notify about.
func TestAChangeInARolledBackTransactionProducesNoEvent(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()

	_, peID, _ := registerHypothesis(t, pool, "rollback", false)

	// Scoped to this test's own write, the same way the committed case is. The listener sees
	// every event on the database, and the development database it runs against is shared with a
	// control-service and with every other test in this package — "no event at all" is a claim
	// about the whole system, not about the rollback.
	deadline := time.After(time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == EventHypothesisNew && e.PlatformExperimentID == peID {
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

	registerHypothesis(t, pool, "commit", true)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind != EventHypothesisNew {
				continue
			}
			if e.PlatformExperimentID != "watch-test-pe-commit" {
				continue
			}
			if e.Cursor <= 0 {
				t.Errorf("event cursor: got = %v, want = a positive microsecond timestamp", e.Cursor)
			}
			return
		case <-deadline:
			t.Fatalf("hypothesis.new after a committed write: got = %v, want = one event", "no event")
		}
	}
}

// insertJob writes one experiment row and walks it QUEUED -> SUBMITTED -> RUNNING, the way the
// scheduler does, returning a cursor from just before the first transition.
func insertJob(t *testing.T, pool *Pool, agentID, peID, hypothesisID, jobID string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := NewCursor(time.Now().UTC().Add(-time.Second))
	if _, err := pool.pool.Exec(ctx, `INSERT INTO experiments
(id, agent_id, project_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, objective, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at)
VALUES ($1, $2, 'p', $3, 'r@0', 'h', 'd', $4, 'claim', 'objective', 'test-accelerator',
        'QUEUED', 'capacity_unavailable', 1, now())`,
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
// experiment row's own timestamps rather than from any stored event log — the whole argument for
// not writing the same state twice. If those columns cannot reproduce the sequence, the argument
// fails and this stream would need a log after all.
func TestReplayReconstructsAJobsStatusSequenceFromTheRowsOwnTimestamps(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := registerHypothesis(t, pool, "replay", true)
	before := insertJob(t, pool, agentID, peID, hypothesisID, "watch-test-job-replay")

	events, err := NewEventsStore(pool).Replay(context.Background(),
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

// A client hands back the cursor of the last event it saw, and must be told what happened after
// that and nothing else. Re-delivering what it already has would make an agent count the same
// transition twice; delivering less would put it back to polling.
func TestReplayReturnsOnlyWhatHappenedAfterTheGivenCursor(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := registerHypothesis(t, pool, "cursor", true)
	before := insertJob(t, pool, agentID, peID, hypothesisID, "watch-test-job-cursor")

	store := NewEventsStore(pool)
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
	if len(rest) != 1 {
		t.Fatalf("events after the second-to-last cursor: got = %v, want = %v", len(rest), 1)
	}
	if rest[0].Value != all[len(all)-1].Value {
		t.Errorf("event after the second-to-last cursor: got = %v, want = %v", rest[0].Value, all[len(all)-1].Value)
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

// A settled claim is the one pool change nothing announced: hypothesis.new carried the idea, and
// the verdict on it arrived silently. Without this event an agent spends real device time
// re-testing what a peer already confirmed or refuted, which is the most expensive mistake its own
// prompt warns it about.
func TestSettlingAHypothesisAnnouncesItsNewVerdictToThePool(t *testing.T) {
	pool := eventsTestDB(t)
	_, _, hypothesisID := registerHypothesis(t, pool, "verdict", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `UPDATE hypotheses SET status = 'refuted' WHERE id = $1`, hypothesisID)

	if got := awaitEvent(t, events, EventHypothesisStatus, hypothesisID).Value; got != "refuted" {
		t.Errorf("announced verdict: got = %v, want = %v", got, "refuted")
	}
}

// The description is the question every agent in the run is working on, and it is edited mid-run.
// The event says only that it changed: a description is unbounded, and copying it onto the stream
// would make the event a second copy of a row anyone can already GET — the one thing this stream
// is built never to be.
func TestEditingTheDescriptionAnnouncesThatItChangedAndCarriesNoneOfItsText(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, _ := registerHypothesis(t, pool, "brief", true)
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
	_, peID, _ := registerHypothesis(t, pool, "closed", true)
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
	agentID, peID, _ := registerHypothesis(t, pool, "cut", true)
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
// These four are emitted by triggers added later, and a trigger added later is exactly where a
// notify outside the writing transaction would creep in — announcing a verdict, a closure or a cut
// the database then threw away, with no later event to correct it.
func TestTheNewKindsEmitNothingWhenTheirTransactionRollsBack(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := registerHypothesis(t, pool, "newkinds-rollback", true)
	events, stop := listenForEvents(t, pool)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: got = %v, want = nil", err)
	}
	for _, q := range []string{
		`UPDATE hypotheses SET status = 'confirmed' WHERE id = '` + hypothesisID + `'`,
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
// reconnect, which puts it straight back to polling for exactly that thing. These four are derived
// from the changed row's own timestamps, with no event log to fall back on.
func TestReplayReturnsEachOfTheNewKindsFromTheChangedRowsOwnTimestamps(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := registerHypothesis(t, pool, "newkinds-replay", true)
	before := NewCursor(time.Now().UTC().Add(-time.Second))

	exec(t, pool, `UPDATE hypotheses SET status = 'confirmed' WHERE id = $1`, hypothesisID)
	exec(t, pool, `UPDATE platform_experiments SET status = 'closed', description = 'rewritten' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO platform_experiment_cuts (platform_experiment_id, agent_id, stage_index)
VALUES ($1, $2, 1)`, peID, agentID)

	events, err := NewEventsStore(pool).Replay(context.Background(), EventFilter{PlatformExperimentID: peID}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	seen := map[string]string{}
	for _, e := range events {
		seen[e.Kind] = e.Subject
	}
	for kind, wantSubject := range map[string]string{
		EventHypothesisStatus:              hypothesisID,
		EventPlatformExperimentStatus:      peID,
		EventPlatformExperimentDescription: peID,
		EventAgentCut:                      agentID,
	} {
		if seen[kind] != wantSubject {
			t.Errorf("replayed subject for %v: got = %v, want = %v", kind, seen[kind], wantSubject)
		}
	}
}

// The cursor is the whole contract of a reconnect: hand back the last one seen and be told what
// happened after it, nothing before. A run-wide kind derived from a column rather than from a row
// per event is where an off-by-one shows up as an agent being told the run closed every single
// time it reconnects.
func TestReplayDoesNotReturnARunWideChangeThatHappenedBeforeTheGivenCursor(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, _ := registerHypothesis(t, pool, "newkinds-cursor", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)

	store := NewEventsStore(pool)
	all, err := store.Replay(context.Background(), EventFilter{PlatformExperimentID: peID,
		Kinds: map[string]bool{EventPlatformExperimentStatus: true}}, NewCursor(time.Now().UTC().Add(-time.Second)))
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	if len(all) != 1 {
		t.Fatalf("replayed run status changes: got = %v, want = %v", len(all), 1)
	}
	rest, err := store.Replay(context.Background(), EventFilter{PlatformExperimentID: peID,
		Kinds: map[string]bool{EventPlatformExperimentStatus: true}}, all[0].Cursor)
	if err != nil {
		t.Fatalf("replay from the last cursor: got = %v, want = nil", err)
	}
	if len(rest) != 0 {
		t.Errorf("events after the last cursor: got = %v, want = %v", len(rest), 0)
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

// The default set is wide, and wide is exactly where the agent scoping rule is easiest to lose: it
// admits every agent-owned kind at once. An agent's default subscription reading a rival's job
// timeline or allocation would hand it a competitor's private state, and being woken by a rival's
// cut would fire its own stop condition on someone else's elimination.
func TestADefaultSubscriptionScopedToAnAgentAdmitsItsOwnAgentOwnedEventsAndNoRivals(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: DefaultKinds()}
	for _, kind := range []string{EventExperimentStatus, EventExperimentBlocked, EventQuotaChanged, EventAgentCut} {
		mine := Event{Kind: kind, Subject: "s", PlatformExperimentID: "pe-1", AgentID: "agent-a"}
		if !filter.Matches(mine) {
			t.Errorf("own %v on a default subscription: got = %v, want = %v", kind, false, true)
		}
		theirs := Event{Kind: kind, Subject: "s", PlatformExperimentID: "pe-1", AgentID: "agent-b"}
		if filter.Matches(theirs) {
			t.Errorf("another agent's %v on a default subscription: got = %v, want = %v", kind, true, false)
		}
	}
}

// The shared half of the same filter. A default subscription that hid the pool because the rows
// carry someone else's name would leave an agent watching a competition it can no longer see —
// the settled claim it must not retest, and the closure it must stop on, are both written by others.
func TestADefaultSubscriptionScopedToAnAgentStillCarriesTheSharedKindsWhoeverWroteThem(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: DefaultKinds()}
	for _, kind := range []string{EventHypothesisNew, EventHypothesisStatus, EventFindingNew,
		EventCommentNew, EventStageBoundary, EventPlatformExperimentStatus, EventPlatformExperimentDescription} {
		e := Event{Kind: kind, Subject: "s", PlatformExperimentID: "pe-1", AgentID: "agent-b"}
		if !filter.Matches(e) {
			t.Errorf("another agent's %v on a default subscription: got = %v, want = %v", kind, false, true)
		}
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

// An agent's own event never gets flagged as ambient, whether it is agent-owned (its job status)
// or shared but authored by it (a comment it left on the pool).
func TestAnnotateLeavesAnAgentsOwnEventUnmarked(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a"}
	for _, e := range []Event{
		{Kind: EventExperimentStatus, Subject: "exp-1", Detail: "evicted for OOM", AgentID: "agent-a"},
		{Kind: EventCommentNew, Subject: "hyp-1", Detail: "left a comment", AgentID: "agent-a"},
	} {
		got := filter.Annotate(e)
		if got.Detail != e.Detail {
			t.Errorf("Annotate(%+v).Detail = %q, want unchanged %q", e, got.Detail, e.Detail)
		}
	}
}

// A shared event authored by a different agent's job is ambient context, not this subscription's
// own concern, and Annotate says so with the one word an agent's loop can grep for.
func TestAnnotateFlagsAnotherAgentsSharedEventAsFYI(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a"}
	e := Event{Kind: EventStageBoundary, Subject: "pe-1", Value: "2", Detail: "cut", AgentID: "agent-b"}
	got := filter.Annotate(e)
	if got.Detail != "FYI: cut" {
		t.Errorf("Annotate(%+v).Detail = %q, want %q", e, got.Detail, "FYI: cut")
	}
}

// A subscription with no agent scope — watching a whole platform experiment or one experiment id
// rather than one agent's slice of it — has no "own" to compare against, so Annotate leaves every
// Detail as it found it rather than guessing.
func TestAnnotateLeavesEverythingUnmarkedWithNoAgentScope(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1"}
	e := Event{Kind: EventStageBoundary, Subject: "pe-1", Detail: "cut", AgentID: "agent-b"}
	got := filter.Annotate(e)
	if got.Detail != "cut" {
		t.Errorf("Annotate(%+v).Detail = %q, want unchanged %q", e, got.Detail, "cut")
	}
}

// hypothesis.new and most other kinds never carry Detail text at all (they're pointers, not
// copies — see the package doc). An agent-scoped subscriber must still be able to tell another
// agent's hypothesis.new apart from its own without a prefix to strip a non-existent string onto,
// so an empty Detail becomes the bare word "FYI" rather than staying invisible.
func TestAnnotateFlagsAnotherAgentsEmptyDetailEventAsFYI(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a"}
	e := Event{Kind: EventHypothesisNew, Subject: "hyp-1", AgentID: "agent-b"}
	got := filter.Annotate(e)
	if got.Detail != "FYI" {
		t.Errorf("Annotate(%+v).Detail = %q, want %q", e, got.Detail, "FYI")
	}
}

// The agent's own empty-Detail event is left exactly as it is — nothing to flag about your own
// work.
func TestAnnotateLeavesAnAgentsOwnEmptyDetailEmpty(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a"}
	e := Event{Kind: EventHypothesisNew, Subject: "hyp-1", AgentID: "agent-a"}
	got := filter.Annotate(e)
	if got.Detail != "" {
		t.Errorf("Annotate(%+v).Detail = %q, want empty", e, got.Detail)
	}
}
