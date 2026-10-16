package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrUnknownCursor is a since this database cannot have issued, such as a cursor from before
// cursors were transaction ids.
var ErrUnknownCursor = errors.New("since is not a cursor this platform issued")

// eventsChannel is the single Postgres NOTIFY channel every event is emitted on, by the AFTER
// triggers the migrations create. One channel, so one LISTEN per process carries the whole stream.
const eventsChannel = "hypothesisloop_events"

// EventsStore is the only path to the change stream: it listens for live events and derives the
// replay of missed ones. Like every other store here it owns its queries — nothing outside this
// package writes SQL against these tables to build an event. Hypothesis events are derived by the
// graph store, which holds the hypotheses.
type EventsStore struct {
	pool       *Pool
	hypotheses HypothesisEvents
}

// HypothesisEvents derives the hypothesis.new, hypothesis.status and hypothesis.link events of one
// platform experiment at a position of at least since (see graphdb.Store.ReplayEvents), and answers
// per event whether a hypothesis event concerns a hypothesis an agent registered
// (graphdb.Store.ConcernsAgent).
type HypothesisEvents interface {
	ReplayEvents(ctx context.Context, platformExperimentID string, since int64) ([]domain.Event, error)
	ConcernsAgent(ctx context.Context, agentID string, events []domain.Event) ([]bool, error)
}

// NewEventsStore returns an EventsStore backed by pool and the graph store's hypothesis events.
func NewEventsStore(pool *Pool, hypotheses HypothesisEvents) *EventsStore {
	return &EventsStore{pool: pool, hypotheses: hypotheses}
}

// ListenEvents holds one dedicated connection on LISTEN and forwards every notification to out
// until ctx ends or the connection breaks, returning why. It never reconnects itself: the caller
// owns the retry policy, and a caller that reconnects must also replay from its last cursor,
// which only the caller knows. listening is called once LISTEN is in effect: every event
// committed after it returns is forwarded, and none committed before it is.
func (s *EventsStore) ListenEvents(ctx context.Context, out chan<- domain.Event, listening func()) error {
	conn, err := s.pool.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("events_store.ListenEvents: acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+eventsChannel); err != nil {
		return fmt.Errorf("events_store.ListenEvents: listen: %w", err)
	}
	listening()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("events_store.ListenEvents: wait: %w", err)
		}
		var e domain.Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err != nil {
			return fmt.Errorf("events_store.ListenEvents: decode %q: %w", n.Payload, err)
		}
		select {
		case out <- answered(e):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// answered gives a job's status event the status a read of the job answers
// (domain.AnsweredStatus). The trigger and the row say the stored one.
func answered(e domain.Event) domain.Event {
	if e.Kind == domain.EventExperimentStatus {
		e.Value = string(domain.AnsweredStatus(domain.ExperimentStatus(e.Value), e.Detail))
	}
	return e
}

// Concerns completes Matches for an agent filter: of events, a hypothesis event stays only when it
// concerns a hypothesis that agent registered. Every other event that Matches admits stays. The
// graph is read once for all of them, so a snapshot replay over a large pool costs one read.
func (s *EventsStore) Concerns(ctx context.Context, filter domain.EventFilter, events []domain.Event) ([]domain.Event, error) {
	if filter.AgentID == "" {
		return events, nil
	}
	var asked []domain.Event
	for _, e := range events {
		if domain.IsHypothesisActivity(e.Kind) {
			asked = append(asked, e)
		}
	}
	if len(asked) == 0 {
		return events, nil
	}
	concerns, err := s.hypotheses.ConcernsAgent(ctx, filter.AgentID, asked)
	if err != nil {
		return nil, err
	}
	kept := make([]domain.Event, 0, len(events))
	next := 0
	for _, e := range events {
		if domain.IsHypothesisActivity(e.Kind) {
			next++
			if !concerns[next-1] {
				continue
			}
		}
		kept = append(kept, e)
	}
	return kept, nil
}

const notifySQL = "SELECT hypothesisloop_notify_event($1, $2, $3, $4, $5, $6)"

// NotifyEvent emits the one event that no row produced: a metric pointer, which the metrics store
// holds and Replay cannot reconstruct. Position and cursor are set by Postgres; e's are ignored.
func (s *EventsStore) NotifyEvent(ctx context.Context, e domain.Event) error {
	if _, err := s.pool.pool.Exec(ctx, notifySQL, e.Kind, e.Subject, e.Value, e.Detail, e.PlatformExperimentID, e.AgentID); err != nil {
		return fmt.Errorf("events_store.NotifyEvent: %w", err)
	}
	return nil
}

// Emit runs a write to another store inside a Postgres transaction and emits the events it
// returns from that transaction. write stamps the transaction's id on what it writes: until
// this transaction ends no snapshot's xmin, and so no cursor, can pass it, which is what keeps a
// resuming watcher from skipping a graph write. Position and cursor of the returned events are
// set by Postgres.
func (s *EventsStore) Emit(ctx context.Context, write func(stamp domain.Stamp) ([]domain.Event, error)) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("events_store.Emit: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	stamp := domain.Stamp{Aborted: func(position int64) (bool, error) {
		var status *string
		if err := tx.QueryRow(ctx, "SELECT pg_xact_status($1::bigint::text::xid8)", position).Scan(&status); err != nil {
			return false, fmt.Errorf("events_store.Emit: status of %d: %w", position, err)
		}
		return status != nil && *status == "aborted", nil
	}, RequireOpen: func(platformExperimentID string) error {
		return holdOpenPlatformExperiment(ctx, tx, platformExperimentID)
	}}
	if err := tx.QueryRow(ctx, "SELECT pg_current_xact_id()::text::bigint").Scan(&stamp.Position); err != nil {
		return fmt.Errorf("events_store.Emit: position: %w", err)
	}
	events, err := write(stamp)
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err := tx.Exec(ctx, notifySQL, e.Kind, e.Subject, e.Value, e.Detail, e.PlatformExperimentID, e.AgentID); err != nil {
			return fmt.Errorf("events_store.Emit: notify: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("events_store.Emit: commit: %w", err)
	}
	return nil
}

// Replay returns, in position order, every event at a position of at least since that matches
// filter: everything a client holding cursor `since` may not have, possibly with some it has.
//
// All of it is read in one snapshot, taken before the graph is read. Each event's cursor is the
// smaller of its position and that snapshot's xmin: a transaction below the xmin has finished and
// is in this replay, one at or above it may still commit. A graph write runs inside a Postgres
// transaction (see Emit), so one below the xmin is already in the graph.
//
// Nothing is replayed from a stored event log, because there is no event log: every event this
// stream carries is a change to a row Postgres already holds, and every one of those rows already
// carries the timestamps that change happened at. Replay re-derives the events from those
// columns. A persisted copy of them beside the rows themselves would be the same state written
// twice, and would drift the first time a row was written by a path that forgot to append to it.
// The position of each event is the transaction id recorded beside each of those columns.
//
// What that costs, stated plainly: the rows record when each state was reached, not every state
// ever passed through. A status the experiment row records (QUEUED via queued_xid, SUBMITTED
// via submitted_xid, and whatever it holds now via status_xid) replays exactly; a status it passed
// through and left inside the missed window, without its own column, is collapsed into the one
// the row now holds. So a reconnecting client is never told something false and never misses a
// subject that changed — it may only be told the latest of two values instead of both. That is
// the honest limit of deriving history from state, and it is the trade important.md asks for.
//
// since = 0 means the caller has no cursor at all, not that there is nothing to catch up on:
// it replays the current state of every row this filter admits, which is
// exactly the snapshot a first-time watcher needs (including one connecting to an already-terminal
// job, which would otherwise see nothing but pings until its own timeout). The result is still one
// event per row, not one per historical transition, so its size is bounded by how many rows this
// platform experiment has, never by how long it has been running.
func (s *EventsStore) Replay(ctx context.Context, filter domain.EventFilter, since int64) ([]domain.Event, error) {
	if since < 0 {
		return nil, fmt.Errorf("events_store.Replay: %w: %d", ErrUnknownCursor, since)
	}
	if filter.PlatformExperimentID == "" {
		return nil, fmt.Errorf("events_store.Replay: platform_experiment_id is required")
	}
	tx, err := s.pool.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("events_store.Replay: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var horizon, next int64
	if err := tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(s)::text::bigint, pg_snapshot_xmax(s)::text::bigint
FROM pg_current_snapshot() s`).Scan(&horizon, &next); err != nil {
		return nil, fmt.Errorf("events_store.Replay: snapshot: %w", err)
	}
	if since > next {
		return nil, fmt.Errorf("events_store.Replay: %w: %d", ErrUnknownCursor, since)
	}

	var events []domain.Event
	for _, derive := range []func(context.Context, pgx.Tx, string, int64) ([]domain.Event, error){
		replayExperiments,
		replayQuotas,
		replayEvidence,
		replayStageBoundaries,
		replayAgentCuts,
		replayAgentsFinished,
		replayAgentsReplaced,
		replayAgentsIdle,
		replayAgentInstructions,
		replayCapacityIdle,
		replayPlatformExperiment,
		replayRevisions,
		replayBaselineRaises,
		replayAgentMessages,
		replayEnvironmentBuilds,
	} {
		part, err := derive(ctx, tx, filter.PlatformExperimentID, since)
		if err != nil {
			return nil, err
		}
		events = append(events, part...)
	}
	hypotheses, err := s.hypotheses.ReplayEvents(ctx, filter.PlatformExperimentID, since)
	if err != nil {
		return nil, err
	}
	events = append(events, hypotheses...)

	kept := events[:0]
	for _, e := range events {
		if e.Position >= since && filter.Matches(e) {
			e.Cursor = min(e.Position, horizon)
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Position < kept[j].Position })
	if err := occurredAt(ctx, tx, kept); err != nil {
		return nil, err
	}
	return kept, nil
}

// occurredAt sets on each event the time its position's transaction began (event_times).
func occurredAt(ctx context.Context, tx pgx.Tx, events []domain.Event) error {
	positions := make([]int64, len(events))
	for i, e := range events {
		positions[i] = e.Position
	}
	rows, err := tx.Query(ctx, `SELECT xid, occurred_at FROM event_times WHERE xid = ANY($1)`, positions)
	if err != nil {
		return fmt.Errorf("events_store.Replay: event times: %w", err)
	}
	times := map[int64]time.Time{}
	var xid int64
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&xid, &at}, func() error {
		times[xid] = at.UTC()
		return nil
	}); err != nil {
		return fmt.Errorf("events_store.Replay: event times: %w", err)
	}
	for i := range events {
		events[i].OccurredAt = times[events[i].Position]
	}
	return nil
}

func replayExperiments(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `SELECT id, agent_id, status::text, COALESCE(eviction_reason, ''), COALESCE(not_admitted_reason, ''),
	queued_xid, submitted_xid, status_xid, not_admitted_xid, COALESCE(underperforming_metric, ''), underperforming_xid,
	COALESCE(held_reason, ''), held_xid, hypothesis_id, COALESCE(same_hypothesis, ''), same_hypothesis_xid
FROM experiments
WHERE platform_experiment_id = $1
  AND (status_xid >= $2 OR not_admitted_xid >= $2 OR queued_xid >= $2 OR submitted_xid >= $2 OR underperforming_xid >= $2 OR held_xid >= $2
       OR same_hypothesis_xid >= $2)`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayExperiments: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var id, agentID, status, evictionReason, notAdmitted, underperformingMetric, heldReason, hypothesisID, sameHypothesis string
		var queuedXid, submittedXid, underperformingXid, heldXid, sameHypothesisXid *int64
		var statusXid, notAdmittedXid int64
		if err := rows.Scan(&id, &agentID, &status, &evictionReason, &notAdmitted, &queuedXid, &submittedXid, &statusXid, &notAdmittedXid, &underperformingMetric, &underperformingXid, &heldReason, &heldXid,
			&hypothesisID, &sameHypothesis, &sameHypothesisXid); err != nil {
			return nil, fmt.Errorf("events_store.replayExperiments: scan: %w", err)
		}
		if queuedXid != nil {
			events = append(events, domain.Event{Kind: domain.EventExperimentStatus, Subject: id, Value: "QUEUED",
				PlatformExperimentID: peID, AgentID: agentID, Position: *queuedXid})
		}
		if submittedXid != nil {
			events = append(events, domain.Event{Kind: domain.EventExperimentStatus, Subject: id, Value: "SUBMITTED",
				PlatformExperimentID: peID, AgentID: agentID, Position: *submittedXid})
		}
		events = append(events, answered(domain.Event{Kind: domain.EventExperimentStatus, Subject: id, Value: status, Detail: evictionReason,
			PlatformExperimentID: peID, AgentID: agentID, Position: statusXid}))
		if reason, message := domain.SplitQueueReason(notAdmitted); reason != "" && reason != domain.NotAdmittedAwaitingAdmission {
			events = append(events, domain.Event{Kind: domain.EventExperimentBlocked, Subject: id, Value: reason, Detail: message,
				PlatformExperimentID: peID, AgentID: agentID, Position: notAdmittedXid})
		}
		// A job that left the queue is held no longer.
		if heldXid != nil && status == string(domain.StatusQueued) {
			reason, message := domain.SplitQueueReason(heldReason)
			events = append(events, domain.Event{Kind: domain.EventJobHeld, Subject: id, Value: reason, Detail: message,
				PlatformExperimentID: peID, AgentID: agentID, Position: *heldXid})
		}
		if sameHypothesisXid != nil {
			events = append(events, domain.Event{Kind: domain.EventJobDuplicate, Subject: id, Value: hypothesisID, Detail: sameHypothesis,
				PlatformExperimentID: peID, AgentID: agentID, Position: *sameHypothesisXid})
		}
		// An advisory to cancel a job that has ended has nothing left to advise.
		if underperformingXid != nil && !domain.ExperimentStatus(status).IsTerminal() {
			events = append(events, domain.Event{Kind: domain.EventJobUnderperforming, Subject: id, Value: underperformingMetric,
				PlatformExperimentID: peID, AgentID: agentID, Position: *underperformingXid})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayExperiments: %w", err)
	}
	return dedupe(events), nil
}

// dedupe drops an event that is identical to one already derived at the same position. The
// waypoint columns and status_xid name the same transition whenever the row has not moved since:
// without this a job sitting in QUEUED would replay QUEUED twice.
func dedupe(events []domain.Event) []domain.Event {
	seen := make(map[domain.Event]bool, len(events))
	kept := events[:0]
	for _, e := range events {
		if seen[e] {
			continue
		}
		seen[e] = true
		kept = append(kept, e)
	}
	return kept
}

func replayQuotas(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `SELECT agent_id, guaranteed_accelerator_hours, burst_accelerator_hours, updated_xid
FROM agent_quotas WHERE platform_experiment_id = $1 AND updated_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var agentID string
		var guaranteed, burst float64
		var position int64
		if err := rows.Scan(&agentID, &guaranteed, &burst, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayQuotas: scan: %w", err)
		}
		share := (&domain.AgentQuota{GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst}).Share()
		events = append(events, domain.Event{Kind: domain.EventQuotaChanged, Subject: agentID, Value: formatHours(share),
			PlatformExperimentID: peID, AgentID: agentID, Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	return events, nil
}

func replayEvidence(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `
SELECT 'finding.new', f.hypothesis_id, f.experiment_id, '', f.agent_id, f.created_xid
FROM hypothesis_findings f WHERE f.platform_experiment_id = $1 AND f.created_xid >= $2
UNION ALL
SELECT 'comment.new', c.hypothesis_id, '', '', COALESCE(c.agent_id, ''), c.created_xid
FROM hypothesis_comments c WHERE c.platform_experiment_id = $1 AND c.created_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayEvidence: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var e domain.Event
		if err := rows.Scan(&e.Kind, &e.Subject, &e.Value, &e.Detail, &e.AgentID, &e.Position); err != nil {
			return nil, fmt.Errorf("events_store.replayEvidence: scan: %w", err)
		}
		e.PlatformExperimentID = peID
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayEvidence: %w", err)
	}
	return events, nil
}

func replayStageBoundaries(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `
SELECT a.stage_index, 'advanced', '', a.advanced_xid
FROM platform_experiment_stage_advances a WHERE a.platform_experiment_id = $1 AND a.advanced_xid >= $2
UNION ALL
SELECT c.stage_index, 'cut', c.agent_id, c.cut_xid
FROM platform_experiment_cuts c WHERE c.platform_experiment_id = $1 AND c.cut_xid >= $2 AND c.reason = ''`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayStageBoundaries: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var stageIndex int
		var detail, agentID string
		var position int64
		if err := rows.Scan(&stageIndex, &detail, &agentID, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayStageBoundaries: scan: %w", err)
		}
		events = append(events, domain.Event{Kind: domain.EventStageBoundary, Subject: peID,
			Value: fmt.Sprintf("%d", stageIndex), Detail: detail,
			PlatformExperimentID: peID, AgentID: agentID, Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayStageBoundaries: %w", err)
	}
	return events, nil
}

// replayAgentCuts derives the cut of each agent from the cut row. A cut is written once and never
// revised, so this replays exactly, with no collapsing of intermediate states.
func replayAgentCuts(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `SELECT agent_id, stage_index, reason, cut_xid FROM platform_experiment_cuts
WHERE platform_experiment_id = $1 AND cut_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentCuts: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var agentID, reason string
		var stageIndex int
		var position int64
		if err := rows.Scan(&agentID, &stageIndex, &reason, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayAgentCuts: scan: %w", err)
		}
		events = append(events, domain.Event{Kind: domain.EventAgentCut, Subject: agentID,
			Value: fmt.Sprintf("%d", stageIndex), Detail: reason, PlatformExperimentID: peID, AgentID: agentID,
			Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayAgentCuts: %w", err)
	}
	return events, nil
}

// replayAgentsFinished derives agent.finished from the signup, which records it once.
func replayAgentsFinished(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT agent_id, finished_reason, finished_xid FROM experiment_signups
WHERE platform_experiment_id = $1 AND finished_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsFinished: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		event := domain.Event{Kind: domain.EventAgentFinished, PlatformExperimentID: peID}
		if err := rows.Scan(&event.AgentID, &event.Detail, &event.Position); err != nil {
			return nil, fmt.Errorf("events_store.replayAgentsFinished: scan: %w", err)
		}
		event.Subject = event.AgentID
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsFinished: %w", err)
	}
	return events, nil
}

// replayAgentsReplaced derives agent.replaced from the signup of the researcher added, which
// records whose place it took once, and from that researcher's why, as the trigger reads it.
func replayAgentsReplaced(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, s.replaces,
       CASE WHEN old.finished_at IS NOT NULL THEN 'it finished: ' || old.finished_reason ELSE 'its machine stopped with no restart left' END,
       s.replaces_xid
FROM experiment_signups s
JOIN experiment_signups old ON old.platform_experiment_id = s.platform_experiment_id AND old.agent_id = s.replaces
WHERE s.platform_experiment_id = $1 AND s.replaces <> '' AND s.replaces_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsReplaced: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventAgentReplaced, PlatformExperimentID: peID}
		err := row.Scan(&event.AgentID, &event.Value, &event.Detail, &event.Position)
		event.Subject = event.AgentID
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsReplaced: %w", err)
	}
	return events, nil
}

// IdleNew and IdleContinues are the values of agent.idle and capacity.idle: the first notice of a
// stretch, and each after it.
const (
	IdleNew       = "new"
	IdleContinues = "continues"
)

// replayAgentsIdle derives agent.idle from the signup, which records the last one told.
func replayAgentsIdle(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT agent_id, CASE WHEN idle_repeat = 0 THEN $3 ELSE $4 END, idle_detail, idle_xid FROM experiment_signups
WHERE platform_experiment_id = $1 AND idle_xid >= $2`, peID, since, IdleNew, IdleContinues)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsIdle: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventAgentIdle, PlatformExperimentID: peID}
		err := row.Scan(&event.AgentID, &event.Value, &event.Detail, &event.Position)
		event.Subject = event.AgentID
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsIdle: %w", err)
	}
	return events, nil
}

// replayAgentInstructions derives agent.instructions from the signups and the coordinator, which
// record the version an edit last moved them to.
func replayAgentInstructions(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `
SELECT agent_id, agent_type_version::text, flavor, instructions_xid FROM experiment_signups
WHERE platform_experiment_id = $1 AND instructions_xid >= $2
UNION ALL
SELECT $3, coordinator_agent_type_version::text, coordinator_agent_type, coordinator_instructions_xid FROM platform_experiments
WHERE id = $1 AND coordinator_instructions_xid >= $2`, peID, since, domain.CoordinatorRecipient)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentInstructions: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventAgentInstructions, PlatformExperimentID: peID}
		err := row.Scan(&event.AgentID, &event.Value, &event.Detail, &event.Position)
		event.Subject = event.AgentID
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentInstructions: %w", err)
	}
	return events, nil
}

// replayCapacityIdle derives capacity.idle from the platform experiment, which records the last
// one told.
func replayCapacityIdle(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT CASE WHEN capacity_repeat = 0 THEN $3 ELSE $4 END, capacity_detail, capacity_xid FROM platform_experiments
WHERE id = $1 AND capacity_xid >= $2`, peID, since, IdleNew, IdleContinues)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayCapacityIdle: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventCapacityIdle, Subject: peID, PlatformExperimentID: peID}
		err := row.Scan(&event.Value, &event.Detail, &event.Position)
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayCapacityIdle: %w", err)
	}
	return events, nil
}

// replayPlatformExperiment derives the run's own two events from the two columns that record when
// each of its fields last moved, and the status event of a status_reason its coordinator was told
// from the column that holds it. They are separate columns for the reason replay exists: one
// updated_at cannot say which of the two changed, and answering "the description changed" because
// the status did would send every agent back to re-read a brief that still says what it said.
//
// A NULL column is a field that has never moved since the row was created, and produces nothing.
func replayPlatformExperiment(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	const q = `SELECT status::text, close_message, status_changed_xid, description_changed_xid, coordinator_reason, coordinator_reason_xid
FROM platform_experiments WHERE id = $1 AND (status_changed_xid >= $2 OR description_changed_xid >= $2 OR coordinator_reason_xid >= $2)`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayPlatformExperiment: %w", err)
	}
	defer rows.Close()

	var events []domain.Event
	for rows.Next() {
		var status, closeMessage string
		var coordinatorReason *string
		var statusXid, descriptionXid, coordinatorReasonXid *int64
		if err := rows.Scan(&status, &closeMessage, &statusXid, &descriptionXid, &coordinatorReason, &coordinatorReasonXid); err != nil {
			return nil, fmt.Errorf("events_store.replayPlatformExperiment: scan: %w", err)
		}
		if statusXid != nil {
			detail := ""
			if status == string(domain.PlatformExpClosed) {
				detail = closeMessage
			}
			events = append(events, domain.Event{Kind: domain.EventPlatformExperimentStatus, Subject: peID,
				Value: status, Detail: detail, PlatformExperimentID: peID, Position: *statusXid})
		}
		if coordinatorReason != nil {
			events = append(events, domain.Event{Kind: domain.EventPlatformExperimentStatus, Subject: peID,
				Value: status, Detail: *coordinatorReason, PlatformExperimentID: peID, Position: *coordinatorReasonXid})
		}
		if descriptionXid != nil {
			events = append(events, domain.Event{Kind: domain.EventPlatformExperimentDescription, Subject: peID,
				PlatformExperimentID: peID, Position: *descriptionXid})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayPlatformExperiment: %w", err)
	}
	return events, nil
}

// replayRevisions derives platform_experiment.revision from the requests a draft keeps, one row
// each.
func replayRevisions(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT created_xid FROM planning_requests WHERE platform_experiment_id = $1 AND created_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayRevisions: %w", err)
	}
	positions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("events_store.replayRevisions: %w", err)
	}
	events := make([]domain.Event, len(positions))
	for i, position := range positions {
		events[i] = domain.Event{Kind: domain.EventPlatformExperimentRevision, Subject: peID, PlatformExperimentID: peID, Position: position}
	}
	return events, nil
}

// replayBaselineRaises derives platform_experiment.baseline from the raises a running platform
// experiment keeps, one row each.
func replayBaselineRaises(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT to_job_id, detail, created_xid FROM baseline_raises WHERE platform_experiment_id = $1 AND created_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayBaselineRaises: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventPlatformExperimentBaseline, Subject: peID, PlatformExperimentID: peID}
		err := row.Scan(&event.Value, &event.Detail, &event.Position)
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayBaselineRaises: %w", err)
	}
	return events, nil
}

// replayAgentMessages derives agent.message from the messages a platform experiment keeps, one row
// each.
func replayAgentMessages(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT agent_id, sender, text, created_xid FROM agent_messages WHERE platform_experiment_id = $1 AND created_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentMessages: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventAgentMessage, PlatformExperimentID: peID}
		err := row.Scan(&event.AgentID, &event.Value, &event.Detail, &event.Position)
		event.Subject = event.AgentID
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentMessages: %w", err)
	}
	return events, nil
}

// replayEnvironmentBuilds derives environment.status from the agents that waited for a build,
// one row each, which keeps what each was told: loaded, then how it ended unless it was ready.
func replayEnvironmentBuilds(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT environment_key, agent_id, 'loaded', loaded_detail, loaded_xid FROM environment_waiters
WHERE platform_experiment_id = $1 AND loaded_xid >= $2
UNION ALL
SELECT environment_key, agent_id, state, detail, ended_xid FROM environment_waiters
WHERE platform_experiment_id = $1 AND ended_xid >= $2 AND NOT (state = 'ready' AND loaded_xid IS NOT NULL)`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayEnvironmentBuilds: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		event := domain.Event{Kind: domain.EventEnvironmentStatus, PlatformExperimentID: peID}
		err := row.Scan(&event.Subject, &event.AgentID, &event.Value, &event.Detail, &event.Position)
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("events_store.replayEnvironmentBuilds: %w", err)
	}
	return events, nil
}

// formatHours renders an hour figure the same way the notify trigger's ::text cast does, so a
// live quota.changed event and its replayed twin carry the same value rather than two spellings
// of one number.
func formatHours(h float64) string {
	return fmt.Sprintf("%g", h)
}
