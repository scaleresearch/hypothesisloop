package db

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	if _, err := tx.Exec(ctx, `INSERT INTO agents (id, name, org_id) VALUES ($1, $1, 'default')`, agentID); err != nil {
		t.Fatalf("insert agent: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_experiments (id, project_id, name, budget_accelerator_hours, starts_at, ends_at)
VALUES ($1, 'default', $1, 1, now(), now() + interval '1 hour')`, peID); err != nil {
		t.Fatalf("insert platform experiment: got = %v, want = nil", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, actor_kind, actor_id, text)
VALUES ($1, $2, $3, 'agent', $3, 'watch test comment')`, hypothesisID, peID, agentID); err != nil {
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
			`DELETE FROM experiment_signups WHERE platform_experiment_id = $1`,
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
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', $1, $2, $3, 'r@0', 'h', 'd', $4, 'claim', 'test-accelerator',
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
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', $1, $2, $3, 'r@0', 'h', 'd', $4, 'claim', 'test-accelerator',
        'QUEUED', 'awaiting_admission', 1, now(), 'AK', 'SK')`, jobID, agentID, peID, hypothesisID)
	store := NewEventsStore(pool, noHypothesisEvents{})
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentBlocked: true}}
	for _, reason := range []string{domain.QueueReasonAwaitingAdmission, "summary_gate: job job-a of this agent is COMPLETED and has no summary"} {
		exec(t, pool, `UPDATE experiments SET not_admitted_reason = $2, updated_at = now() WHERE id = $1`, jobID, reason)
		replayed, err := store.Replay(context.Background(), filter, before)
		if err != nil || len(replayed) != 0 {
			t.Errorf("replay of a job held as %s: got = %+v, %v, want = no blocked event", reason, replayed, err)
		}
	}
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = 'capacity_unavailable', updated_at = now() WHERE id = $1`, jobID)
	if got := awaitEvent(t, events, EventExperimentBlocked, jobID); got.Value != "capacity_unavailable" || got.Detail != "" {
		t.Errorf("first live blocked event: got = %q with detail %q, want = capacity_unavailable and none", got.Value, got.Detail)
	}

	// A reason with more to say is its code and its sentence, live, replayed and read.
	const sentence = "cluster box-b is short of 16 cores, held by 2 jobs: 1 of this platform experiment and 1 of other platform experiments"
	before = cursorNow(t, pool)
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = $2, updated_at = now() WHERE id = $1`, jobID, "capacity_unavailable: "+sentence)
	if got := awaitEvent(t, events, EventExperimentBlocked, jobID); got.Value != "capacity_unavailable" || got.Detail != sentence {
		t.Errorf("live blocked event: got = %q with detail %q, want = the code and the sentence", got.Value, got.Detail)
	}
	replayed, err := store.Replay(context.Background(), filter, before)
	if err != nil || len(replayed) != 1 || replayed[0].Value != "capacity_unavailable" || replayed[0].Detail != sentence {
		t.Errorf("replayed blocked event: got = %+v, %v, want = the code and the sentence", replayed, err)
	}
	job, err := NewExperimentsStore(pool).GetExperiment(context.Background(), jobID)
	if err != nil || job.NotAdmittedReason != "capacity_unavailable" || job.NotAdmittedMessage != sentence {
		t.Errorf("the job read: got = %q, %q, %v, want = the code and the sentence apart", job.NotAdmittedReason, job.NotAdmittedMessage, err)
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

// The owner may lengthen or shorten a run while it goes on, and every agent plans its work by
// the time left: a new duration_hours is told as the brief is, live and replayed.
func TestChangingTheDurationIsAnnouncedAsTheBriefIs(t *testing.T) {
	pool := eventsTestDB(t)
	_, peID, _ := seedRun(t, pool, "length", true)
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()

	exec(t, pool, `UPDATE platform_experiments SET duration_hours = duration_hours + 2 WHERE id = $1`, peID)

	awaitEvent(t, events, EventPlatformExperimentDescription, peID)
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(), EventFilter{PlatformExperimentID: peID}, before)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(replayed, func(e Event) bool { return e.Kind == EventPlatformExperimentDescription }) {
		t.Fatalf("replayed %+v, want the change among them", replayed)
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

// The reason an owner gave to close the run is its close event's detail, live and replayed, and
// the platform experiment carries it.
func TestTheCloseEventCarriesTheReasonGiven(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, _ := seedRun(t, pool, "closed-for", true)
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()
	store := NewPlatformExperimentsStore(pool)

	if err := store.ClosePlatformExperiment(ctx, peID, &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}, "the baseline measured the old split"); err != nil {
		t.Fatal(err)
	}
	live := awaitEvent(t, events, EventPlatformExperimentStatus, peID)
	if live.Value != "closed" || live.Detail != "the baseline measured the old split" {
		t.Fatalf("live event: got = %q %q, want = closed with the reason", live.Value, live.Detail)
	}
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventPlatformExperimentStatus: true}}
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, filter, before)
	if err != nil || len(replayed) != 1 || replayed[0].Identity() != live.Identity() || replayed[0].Detail != live.Detail {
		t.Fatalf("replay: got = %+v, %v, want = the live event %+v alone", replayed, err, live)
	}
	pe, err := store.GetPlatformExperiment(ctx, peID)
	if err != nil || pe.CloseMessage != "the baseline measured the old split" {
		t.Fatalf("close_message: got = %+v, %v", pe, err)
	}
}

// A status_reason is derived on every read, so a run whose last researcher's machine stopped for
// good read researchers_done and wrote no row: nothing woke the coordinator that was to close it.
// The reason that waits for the coordinator is recorded, and that write is the status event, sent
// once for each change, live and replayed, and never for a reason of a status the row has left.
func TestAReasonThatWaitsForTheCoordinatorIsAnnouncedOnceAsAStatusEventAndReplayedAsItsLiveTwin(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, _ := seedRun(t, pool, "coordinator-reason", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()
	store := NewPlatformExperimentsStore(pool)
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventPlatformExperimentStatus: true}}
	replay := func() []Event {
		t.Helper()
		replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, filter, before)
		if err != nil {
			t.Fatalf("replay: got = %v, want = nil", err)
		}
		return replayed
	}

	for _, reason := range []string{"", "researchers_done", "researchers_done"} {
		if err := store.TellCoordinator(ctx, peID, reason); err != nil {
			t.Fatalf("tell %q: got = %v, want = nil", reason, err)
		}
	}
	live := awaitEvent(t, events, EventPlatformExperimentStatus, peID)
	if live.Value != "running" || live.Detail != "researchers_done" {
		t.Fatalf("live event: got = %q %q, want = running researchers_done", live.Value, live.Detail)
	}
	if replayed := replay(); len(replayed) != 1 || replayed[0].Identity() != live.Identity() {
		t.Fatalf("replay: got = %+v, want = the live event %+v alone", replayed, live)
	}

	// A researcher whose machine is asked for again leaves the run waiting for nobody, and the
	// next time none is left is told again.
	for _, reason := range []string{"", "researchers_done"} {
		if err := store.TellCoordinator(ctx, peID, reason); err != nil {
			t.Fatalf("tell %q: got = %v, want = nil", reason, err)
		}
	}
	again := awaitEvent(t, events, EventPlatformExperimentStatus, peID)
	if again.Detail != "researchers_done" || again.Position == live.Position {
		t.Fatalf("told again: got = %+v, want = researchers_done at a position past %d", again, live.Position)
	}

	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)
	if closed := awaitEvent(t, events, EventPlatformExperimentStatus, peID); closed.Value != "closed" || closed.Detail != "" {
		t.Fatalf("closing: got = %q %q, want = closed with no detail", closed.Value, closed.Detail)
	}
	if err := store.TellCoordinator(ctx, peID, "researchers_done"); err != nil {
		t.Fatalf("tell a closed one: got = %v, want = nil", err)
	}
	if replayed := replay(); len(replayed) != 1 || replayed[0].Value != "closed" || replayed[0].Detail != "" {
		t.Fatalf("replay of a closed one: got = %+v, want = its close alone", replayed)
	}
}

// A grant was announced as its guaranteed and burst hours added up: 8.96 where the agent could
// spend 2.24, of a budget of 6. It is announced as the share GET /quota answers, live and
// replayed, and a connection drops a repeat only when the two are one.
func TestAQuotaGrantIsAnnouncedAsTheShareTheAgentMaySpendAndReplayedAsItsLiveTwin(t *testing.T) {
	pool := eventsTestDB(t)
	for _, tc := range []struct {
		name              string
		guaranteed, burst float64
	}{{"quota-share", 2.24, 6.72}, {"quota-burst-only", 0, 4.5}} {
		agentID, peID, _ := seedRun(t, pool, tc.name, true)
		before := cursorNow(t, pool)
		events, stop := listenForEvents(t, pool)

		exec(t, pool, `INSERT INTO agent_quotas (id, agent_id, platform_experiment_id, guaranteed_accelerator_hours, burst_accelerator_hours, created_at)
VALUES ($1, $1, $2, $3, $4, now())`, agentID, peID, tc.guaranteed, tc.burst)
		t.Cleanup(func() { exec(t, pool, `DELETE FROM agent_quotas WHERE id = $1`, agentID) })

		want := formatHours((&domain.AgentQuota{GuaranteedAcceleratorHours: tc.guaranteed, BurstAcceleratorHours: tc.burst}).Share())
		live := awaitEvent(t, events, EventQuotaChanged, agentID)
		stop()
		if live.Value != want || live.Detail != "" {
			t.Fatalf("%s: live event: got = %q %q, want = the share %s and no detail", tc.name, live.Value, live.Detail, want)
		}
		replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(),
			EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventQuotaChanged: true}}, before)
		if err != nil || len(replayed) != 1 || replayed[0].Identity() != live.Identity() {
			t.Fatalf("%s: replay: got = (%+v, %v), want = the live event %+v", tc.name, replayed, err, live)
		}
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

	// The agent's other way out is its own: it says it has nothing left to do.
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id) VALUES ($1, $2)`, peID, agentID)
	if err := NewPlatformExperimentsStore(pool).FinishAgent(context.Background(), peID, agentID, "quota spent"); err != nil {
		t.Fatalf("finish: got = %v, want = nil", err)
	}
	if got := awaitEvent(t, events, EventAgentFinished, agentID); got.Detail != "quota spent" || got.PlatformExperimentID != peID {
		t.Errorf("agent.finished: got = %+v, want = the reason in %s", got, peID)
	}

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

	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id) VALUES ($1, $2)`, peID, agentID)
	if err := NewPlatformExperimentsStore(pool).FinishAgent(context.Background(), peID, agentID, "nothing left"); err != nil {
		t.Fatalf("finish: got = %v, want = nil", err)
	}
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
		EventAgentFinished:                 agentID,
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
	insert := `INSERT INTO hypothesis_comments (hypothesis_id, platform_experiment_id, agent_id, actor_kind, actor_id, text) VALUES ($1, $2, $3, 'agent', $3, $4)`

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
		return []Event{{Kind: EventHypothesisNew, Subject: hypothesisID, PlatformExperimentID: peID}}, nil
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
		{Kind: EventExperimentStatus, Value: "QUEUED", Detail: "stuck_pending: admitted and not running within 5m0s", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "exceeds_accelerator_share: cpu 64 above 8", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "exceeds_cluster_capacity: needs {accelerator:h200=3}, more than any connected cluster has installed", AgentID: "agent-a"},
		{Kind: EventQuotaChanged, Value: "4", AgentID: "agent-a"},
		{Kind: EventAgentCut, Value: "1", AgentID: "agent-a"},
		{Kind: EventStageBoundary, Value: "1", Detail: "advanced"},
		{Kind: EventPlatformExperimentStatus, Value: "closed"},
		{Kind: EventPlatformExperimentDescription},
		{Kind: EventCommentNew, AgentID: "agent-b"},
		{Kind: EventCommentNew},
	}
	refused := []Event{
		{Kind: EventExperimentStatus, Value: "QUEUED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "SUBMITTED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "RUNNING", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "capacity_unavailable", Detail: "cluster c is short of 1 x nvidia.com/gpu.product=nvidia-l40", AgentID: "agent-a"},
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
		{Kind: EventHypothesisNew, Subject: hypothesisID, PlatformExperimentID: peID, Position: commentPosition + 1},
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

// A hosted coordinator was woken for every job being queued, admitted and started, each wake a
// reread of its whole context that ended in "nothing to act on". Whoever steers an experiment
// decides on outcomes and on jobs that are stuck, across every agent.
func TestTheDecisionsStreamOfAnExperimentLeavesOutJobsThatAreOnTheirWay(t *testing.T) {
	filter := EventFilter{PlatformExperimentID: "pe-1", Kinds: DefaultKinds(), Decisions: true}
	whole := EventFilter{PlatformExperimentID: "pe-1", Kinds: DefaultKinds()}
	admitted := []Event{
		{Kind: EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "FAILED", AgentID: "agent-b"},
		{Kind: EventExperimentStatus, Value: "EVICTED", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-a"},
		{Kind: EventHypothesisNew, AgentID: "agent-a"},
		{Kind: EventHypothesisStatus, Value: "confirmed", AgentID: "agent-a"},
		{Kind: EventFindingNew, Value: "job-1", AgentID: "agent-b"},
		{Kind: EventCommentNew},
		{Kind: EventQuotaChanged, Value: "4", AgentID: "agent-b"},
		{Kind: EventAgentCut, Value: "1", AgentID: "agent-b"},
		{Kind: EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-b"},
		{Kind: EventPlatformExperimentStatus, Value: "closed"},
	}
	onTheirWay := []Event{
		{Kind: EventExperimentStatus, Value: "QUEUED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "SUBMITTED", AgentID: "agent-a"},
		{Kind: EventExperimentStatus, Value: "RUNNING", AgentID: "agent-b"},
		{Kind: EventExperimentBlocked, Value: "capacity_unavailable", Detail: "cluster c is short of 1 x nvidia.com/gpu.product=nvidia-l40", AgentID: "agent-a"},
		{Kind: EventExperimentBlocked, Value: "outranked", AgentID: "agent-b"},
	}
	for _, e := range admitted {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
	for _, e := range onTheirWay {
		e.PlatformExperimentID = "pe-1"
		if filter.Matches(e) {
			t.Errorf("Matches(%+v): got = true, want = false", e)
		}
		if !whole.Matches(e) {
			t.Errorf("the whole stream lost %+v, which only the decisions stream leaves out", e)
		}
	}
}

// A job that ends by running is announced with the cause the write that ended it stored, live and
// replayed alike: a failure with how its process ended, a completion with nothing.
func TestAFinishedJobIsAnnouncedWithItsCause(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "cause", true)
	exps := NewExperimentsStore(pool)
	ctx := context.Background()
	const cause = "container_failed: command exited with code 1"
	for jobID, end := range map[string][2]string{
		"watch-test-job-failed":    {"FAILED", cause},
		"watch-test-job-completed": {"COMPLETED", ""},
	} {
		before := insertJob(t, pool, agentID, peID, hypothesisID, jobID)
		if ok, err := exps.TransitionStatus(ctx, jobID, domain.StatusRunning, domain.ExperimentStatus(end[0]), end[1]); err != nil || !ok {
			t.Fatalf("TransitionStatus(%s, %s): got = %v, %v, want true", jobID, end[0], ok, err)
		}
		deadline := time.After(5 * time.Second)
		for announced := false; !announced; {
			select {
			case e := <-events:
				if e.Subject == jobID && e.Value == end[0] {
					if e.Detail != end[1] {
						t.Errorf("live %s of %s: detail = %q, want %q", end[0], jobID, e.Detail, end[1])
					}
					announced = true
				}
			case <-deadline:
				t.Fatalf("no live %s for %s", end[0], jobID)
			}
		}
		replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx,
			EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentStatus: true}}, before)
		if err != nil {
			t.Fatalf("replay: got = %v, want = nil", err)
		}
		last := replayed[len(replayed)-1]
		if last.Subject != jobID || last.Value != end[0] || last.Detail != end[1] {
			t.Errorf("replayed end of %s: got = %+v, want %s with detail %q", jobID, last, end[0], end[1])
		}
	}
	if _, err := exps.TransitionStatus(ctx, "watch-test-job-completed", domain.StatusCompleted, domain.StatusCompleted, cause); err == nil || !strings.Contains(err.Error(), "experiments_reason_of_attempt") {
		t.Errorf("a completion written with a reason: err = %v, want experiments_reason_of_attempt", err)
	}
}

// A watcher and a reader are told the same status. A cancelled job is stored EVICTED with its
// reason and answered CANCELLED, and its event says CANCELLED too, live and replayed.
func TestACancelledJobIsAnnouncedAsAReadAnswersIt(t *testing.T) {
	pool := eventsTestDB(t)
	store := NewEventsStore(pool, noHypothesisEvents{})
	agentID, peID, hypothesisID := seedRun(t, pool, "cancelled", true)
	const jobID = "watch-test-job-cancelled"
	before := insertJob(t, pool, agentID, peID, hypothesisID, jobID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, listening := make(chan Event, 16), make(chan struct{})
	go store.ListenEvents(ctx, events, func() { close(listening) }) //nolint:errcheck
	<-listening
	exec(t, pool, `UPDATE experiments SET status = 'EVICTED', eviction_reason = 'cancelled', updated_at = now() WHERE id = $1`, jobID)

	var live Event
	for live.Subject != jobID {
		select {
		case live = <-events:
		case <-ctx.Done():
			t.Fatal("no live event for the cancelled job")
		}
	}
	if live.Value != "CANCELLED" || live.Detail != "cancelled" {
		t.Errorf("live: got = %s %q, want CANCELLED \"cancelled\"", live.Value, live.Detail)
	}
	replayed, err := store.Replay(ctx, EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentStatus: true}}, before)
	if err != nil {
		t.Fatalf("replay: got = %v, want = nil", err)
	}
	if last := replayed[len(replayed)-1]; last.Identity() != live.Identity() {
		t.Errorf("replayed: got = %+v, want the live event %+v", last.Identity(), live.Identity())
	}
}

// A watcher prints when a change was made. A replayed event said nothing of it, so a client could
// print only when it read the event, and a replay of an hour-old change looked new.
func TestAnEventCarriesWhenItsChangeWasMadeLiveAndReplayed(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "times", true)
	before := cursorNow(t, pool)
	started := time.Now().Add(-time.Second)
	exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', 'times-job', $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator',
        'QUEUED', 'capacity_unavailable: no room', 1, now(), 'AK', 'SK')`, agentID, peID, hypothesisID)
	live := awaitEvent(t, events, EventExperimentBlocked, "times-job")
	if live.OccurredAt.Before(started) || live.OccurredAt.After(time.Now().Add(time.Second)) || live.OccurredAt.Location() != time.UTC {
		t.Fatalf("live event occurred_at = %v, want the moment of the write, in UTC", live.OccurredAt)
	}

	time.Sleep(20 * time.Millisecond)
	replayed, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(), EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentBlocked: true}}, before)
	if err != nil || len(replayed) != 1 {
		t.Fatalf("replay: got = %+v, %v, want the one blocked event", replayed, err)
	}
	if !replayed[0].OccurredAt.Equal(live.OccurredAt) || replayed[0].Identity() != live.Identity() {
		t.Errorf("replayed event = %+v, want the live event's time %v and identity", replayed[0], live.OccurredAt)
	}
	payload, err := json.Marshal(replayed[0])
	if err != nil || !strings.Contains(string(payload), `"occurred_at":"`+live.OccurredAt.Format(time.RFC3339Nano)+`"`) {
		t.Errorf("delivered event = %s, %v, want occurred_at in RFC3339 UTC", payload, err)
	}

	// A change from before positions kept a time replays without one, and says so by leaving
	// the field out.
	exec(t, pool, `DELETE FROM event_times WHERE xid = $1`, live.Position)
	replayed, err = NewEventsStore(pool, noHypothesisEvents{}).Replay(context.Background(), EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventExperimentBlocked: true}}, before)
	if err != nil || len(replayed) != 1 || !replayed[0].OccurredAt.IsZero() {
		t.Fatalf("replay of a change with no kept time: got = %+v, %v", replayed, err)
	}
	if payload, _ := json.Marshal(replayed[0]); strings.Contains(string(payload), "occurred_at") {
		t.Errorf("delivered event = %s, want no occurred_at", payload)
	}
}

// The summary gate is the one hold the job's owner lifts itself, so it is told as soon as the
// scheduler holds the job, with the jobs it owes a summary for. The same job is announced once
// more when it has then waited past its estimate for something else.
func TestAJobHeldByTheSummaryGateIsAnnouncedAtOnce(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "gate", true)
	experiments := NewExperimentsStore(pool)
	const gate = "job gate-job-done of this agent is COMPLETED and has no summary: hl job summary gate-job-done --text T files one"
	const short = "cluster box-b is short of 2 cores"
	exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', 'gate-job', $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator',
        'QUEUED', $4, 1, now(), 'AK', 'SK')`, agentID, peID, hypothesisID, "summary_gate: "+gate)
	mark := func() {
		t.Helper()
		if err := experiments.MarkHeldJobs(context.Background(), 5*time.Minute); err != nil {
			t.Fatalf("MarkHeldJobs: %v", err)
		}
	}
	held := func() string {
		t.Helper()
		var reason *string
		if err := pool.pool.QueryRow(context.Background(), `SELECT held_reason FROM experiments WHERE id = 'gate-job'`).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if reason == nil {
			return ""
		}
		return *reason
	}

	mark()
	if got := awaitEvent(t, events, EventJobHeld, "gate-job"); got.Value != "summary_gate" || got.Detail != gate || got.AgentID != agentID {
		t.Fatalf("held event of a job queued a moment ago behind a summary: got = %+v, want the code and the sentence", got)
	}

	// The gate lifts and the job waits for room: nothing more until it has waited past its
	// estimate, and then once.
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = $1 WHERE id = 'gate-job'`, "capacity_unavailable: "+short)
	mark()
	if got := held(); got != "summary_gate: "+gate {
		t.Fatalf("held_reason of a job that has not waited past its estimate = %q, want the gate's still", got)
	}
	exec(t, pool, `UPDATE experiments SET queued_at = now() - interval '2 hours' WHERE id = 'gate-job'`)
	mark()
	if got := awaitEvent(t, events, EventJobHeld, "gate-job"); got.Value != "capacity_unavailable" || got.Detail != short {
		t.Fatalf("held event once it waited past its estimate: got = %+v", got)
	}
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = 'outranked: jobs ahead took the room' WHERE id = 'gate-job'`)
	mark()
	if got := held(); got != "capacity_unavailable: "+short {
		t.Errorf("held_reason after a third pass = %q, want the second notice to be the last", got)
	}
}

// An agent's stream leaves out a queue reason that waiting clears, so an agent whose job never
// started learned nothing. A job that has waited longer than it was to run, and longer than the
// platform's minimum, is announced once with the reason it has then.
func TestAJobThatWaitsLongerThanItWasToRunIsAnnouncedAsHeldOnce(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "held", true)
	experiments := NewExperimentsStore(pool)
	store := NewEventsStore(pool, noHypothesisEvents{})
	const sentence = "cluster box-b is short of 1 x tenstorrent.com/chiparch=blackhole, held by 1 job of other platform experiments"
	queue := func(id, reason string, estimatedHours float64, waited time.Duration) {
		t.Helper()
		exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', $1, $2, $3, 'r@0', 'h', 'd', $4, 'claim', 'test-accelerator',
        'QUEUED', $5, $6, now() - $7 * interval '1 second', 'AK', 'SK')`, id, agentID, peID, hypothesisID, reason, estimatedHours, waited.Seconds())
	}
	queue("held-job-long-wait", "capacity_unavailable: "+sentence, 0.08, 10*time.Minute)
	queue("held-job-short-wait", "capacity_unavailable: "+sentence, 0.08, 4*time.Minute)
	queue("held-job-long-run", "capacity_unavailable: "+sentence, 1, 10*time.Minute)
	queue("held-job-unjudged", domain.QueueReasonAwaitingAdmission, 0.08, 10*time.Minute)
	before := cursorNow(t, pool)

	if err := experiments.MarkHeldJobs(context.Background(), 5*time.Minute); err != nil {
		t.Fatalf("MarkHeldJobs: %v", err)
	}
	if got := awaitEvent(t, events, EventJobHeld, "held-job-long-wait"); got.Value != "capacity_unavailable" || got.Detail != sentence || got.AgentID != agentID {
		t.Errorf("live held event: got = %+v, want = the code, the sentence and the job's agent", got)
	}

	// A second pass, and a reason that changed since, announce nothing more.
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = 'outranked' WHERE id = 'held-job-long-wait'`)
	if err := experiments.MarkHeldJobs(context.Background(), 5*time.Minute); err != nil {
		t.Fatalf("MarkHeldJobs: %v", err)
	}
	filter := EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{EventJobHeld: true}}
	replayed, err := store.Replay(context.Background(), filter, before)
	if err != nil || len(replayed) != 1 || replayed[0].Subject != "held-job-long-wait" || replayed[0].Value != "capacity_unavailable" || replayed[0].Detail != sentence {
		t.Fatalf("replayed held events: got = %+v, %v, want = the one job that waited past its estimate and the minimum, once", replayed, err)
	}

	// The job's own agent and whoever steers the run are told, and no other agent.
	held := replayed[0]
	if !(EventFilter{PlatformExperimentID: peID, AgentID: agentID}).Matches(held) || !(EventFilter{PlatformExperimentID: peID, Decisions: true}).Matches(held) {
		t.Errorf("a held job is kept from its agent or from its coordinator: %+v", held)
	}
	if (EventFilter{PlatformExperimentID: peID, AgentID: "another-agent"}).Matches(held) {
		t.Errorf("a held job is announced to another agent: %+v", held)
	}

	// A job that left the queue is held no longer.
	exec(t, pool, `UPDATE experiments SET status = 'REJECTED', not_admitted_reason = NULL WHERE id = 'held-job-long-wait'`)
	if replayed, err := store.Replay(context.Background(), filter, before); err != nil || len(replayed) != 0 {
		t.Errorf("replay once the job left the queue: got = %+v, %v, want = no held event", replayed, err)
	}
}

// Run 5's baseline job sat building its environment past the point it could end by
// baseline_due_at, and its agent, whose stream leaves out a queue reason that waiting clears,
// slept through it. Such a job is announced to its agent once, as job.held baseline_late.
func TestABaselineJobThatCannotEndByItsDeadlineIsAnnouncedOnce(t *testing.T) {
	pool := eventsTestDB(t)
	events, stop := listenForEvents(t, pool)
	defer stop()
	agentID, peID, hypothesisID := seedRun(t, pool, "late", true)
	experiments := NewExperimentsStore(pool)
	exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, not_admitted_reason,
 estimated_duration_hours, queued_at, data_access_key_id, data_secret_access_key)
VALUES ('agent', 'late-job', $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator',
        'QUEUED', 'environment_building: the platform builds it', 1, now(), 'AK', 'SK')`, agentID, peID, hypothesisID)
	const first = "job late-job is QUEUED (environment_building): it cannot end by baseline_due_at"
	if err := experiments.MarkBaselineLate(context.Background(), "late-job", domain.HeldBaselineLate+": "+first); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, events, EventJobHeld, "late-job"); got.Value != domain.HeldBaselineLate || got.Detail != first || got.AgentID != agentID {
		t.Fatalf("held event of a late baseline job: got = %+v, want baseline_late with its sentence to its agent", got)
	}
	if err := experiments.MarkBaselineLate(context.Background(), "late-job", domain.HeldBaselineLate+": a minute later, still late"); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := pool.pool.QueryRow(context.Background(), `SELECT held_reason FROM experiments WHERE id = 'late-job'`).Scan(&reason); err != nil || reason != domain.HeldBaselineLate+": "+first {
		t.Errorf("held_reason after a second notice = %q, %v, want the first one kept", reason, err)
	}
	if err := experiments.MarkBaselineLate(context.Background(), "late-job", "no sentence"); err == nil {
		t.Error("a reason with no sentence was taken")
	}
}

// Run 6: a researcher worked 28 minutes on its own machine with nothing on the platform, and
// neither it nor the coordinator was told. A researcher with no job submitted for the notice and
// none in flight is told so once for each such stretch, live and on replay.
func TestAResearcherWithNoJobForTheNoticeIsToldOnceForEachStretch(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	agentID, peID, hypothesisID := seedRun(t, pool, "idle", true)
	store := NewPlatformExperimentsStore(pool)
	ctx := context.Background()
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '20 minutes' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at) VALUES ($1, $2, now() - interval '30 minutes')`, peID, agentID)
	told := func() int {
		t.Helper()
		events, err := NewEventsStore(pool, noHypothesisEvents{}).Replay(ctx, EventFilter{PlatformExperimentID: peID, AgentID: agentID, Kinds: map[string]bool{EventAgentIdle: true}}, before)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	job := func(id, status string) {
		t.Helper()
		exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ('agent', $4, $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', $5, 1, 'AK', 'SK')`, agentID, peID, hypothesisID, id, status)
	}

	if err := store.NoticeIdleResearchers(ctx, 30*time.Minute); err != nil || told() != 0 {
		t.Fatalf("idle 20 minutes, notice at 30: told %d, %v, want none", told(), err)
	}
	events, stop := listenForEvents(t, pool)
	if err := store.NoticeIdleResearchers(ctx, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	live := awaitEvent(t, events, EventAgentIdle, agentID)
	stop()
	if live.Detail != "no job or check of its own ran on the platform for 20 minutes, and it has none queued or running" || live.PlatformExperimentID != peID {
		t.Fatalf("live: got = %+v, want the 20 minutes since the run started", live)
	}
	if err := store.NoticeIdleResearchers(ctx, 10*time.Minute); err != nil || told() != 1 {
		t.Fatalf("the same stretch again: told %d, %v, want once", told(), err)
	}

	job("idle-job-1", "RUNNING")
	if err := store.NoticeIdleResearchers(ctx, 0); err != nil || told() != 1 {
		t.Fatalf("a job running: told %d, %v, want nothing more", told(), err)
	}
	exec(t, pool, `UPDATE experiments SET status = 'COMPLETED' WHERE id = 'idle-job-1'`)
	if err := store.NoticeIdleResearchers(ctx, 10*time.Minute); err != nil || told() != 1 {
		t.Fatalf("its job just ended: told %d, %v, want nothing more", told(), err)
	}
	if err := store.NoticeIdleResearchers(ctx, 0); err != nil || told() != 1 {
		t.Fatalf("a new stretch: told %d, %v, want the last notice replayed alone", told(), err)
	}
	var detail string
	if err := pool.pool.QueryRow(ctx, `SELECT idle_detail FROM experiment_signups WHERE platform_experiment_id = $1 AND agent_id = $2`, peID, agentID).Scan(&detail); err != nil || detail != "no job or check of its own ran on the platform for 0 minutes, and it has none queued or running" {
		t.Fatalf("a new stretch: detail %q, %v, want it told again", detail, err)
	}

	exec(t, pool, `UPDATE experiment_signups SET finished_at = now() WHERE platform_experiment_id = $1`, peID)
	exec(t, pool, `UPDATE experiment_signups SET idle_noticed_at = NULL WHERE platform_experiment_id = $1`, peID)
	if err := store.NoticeIdleResearchers(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var untold bool
	if err := pool.pool.QueryRow(ctx, `SELECT idle_noticed_at IS NULL FROM experiment_signups WHERE platform_experiment_id = $1`, peID).Scan(&untold); err != nil || !untold {
		t.Fatalf("a finished researcher: told %v, want not", !untold)
	}
}

// A researcher ran hl env run checks every minute or two and was told it had submitted no
// job for 10 minutes. A check created or running is activity, and the stretch starts when the
// last one ended.
func TestAResearcherRunningChecksIsNotIdle(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := seedRun(t, pool, "checking", true)
	store := NewPlatformExperimentsStore(pool)
	ctx := context.Background()
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '40 minutes' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, signed_up_at) VALUES ($1, $2, now() - interval '40 minutes')`, peID, agentID)
	check := func(id, status, created, reported string) {
		t.Helper()
		exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, data_access_key_id, data_secret_access_key,
 is_check, created_at, run_reported_at)
VALUES ('agent', $4, $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', $5, 1, 'AK', 'SK',
        true, now() - $6::interval, now() - $7::interval)`, agentID, peID, hypothesisID, id, status, created, reported)
	}
	detail := func() string {
		t.Helper()
		var detail string
		if err := pool.pool.QueryRow(ctx, `SELECT idle_detail FROM experiment_signups WHERE platform_experiment_id = $1 AND agent_id = $2`, peID, agentID).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}

	check("check-running", "RUNNING", "30 minutes", "30 minutes")
	if err := store.NoticeIdleResearchers(ctx, 0); err != nil || detail() != "" {
		t.Fatalf("a check running: detail %q, %v, want no notice", detail(), err)
	}
	exec(t, pool, `UPDATE experiments SET status = 'COMPLETED', run_reported_at = now() - interval '3 minutes' WHERE id = 'check-running'`)
	if err := store.NoticeIdleResearchers(ctx, 10*time.Minute); err != nil || detail() != "" {
		t.Fatalf("a check that ended 3 minutes ago, notice at 10: detail %q, %v, want no notice", detail(), err)
	}
	if err := store.NoticeIdleResearchers(ctx, 2*time.Minute); err != nil || detail() != "no job or check of its own ran on the platform for 3 minutes, and it has none queued or running" {
		t.Fatalf("notice at 2: detail %q, %v, want the 3 minutes since the check ended", detail(), err)
	}
}
