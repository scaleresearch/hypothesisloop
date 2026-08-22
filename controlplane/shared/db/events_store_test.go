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

	registerHypothesis(t, pool, "rollback", false)

	select {
	case e := <-events:
		t.Errorf("event after a rolled-back write: got = %v, want = %v", e.Kind, "no event")
	case <-time.After(time.Second):
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
