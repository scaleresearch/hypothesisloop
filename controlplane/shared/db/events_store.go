package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Event is one change, described as small as it can be described: what kind of thing changed,
// which thing, its new value, one qualifying detail, the two ids a subscription is scoped by,
// and a cursor. Never a copy of the row — a client that wants detail follows with a normal GET,
// so this stream duplicates no read path and no state.
type Event struct {
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
	// Cursor is the changed row's own timestamp in microseconds since the epoch. It is
	// monotonic per subject and orders the stream; a client hands its last one back as `since`
	// on reconnect and Replay returns exactly what it missed.
	Cursor int64 `json:"cursor"`
}

// Event kinds. Every event on the stream carries one of these.
const (
	EventExperimentStatus  = "experiment.status"
	EventExperimentBlocked = "experiment.blocked"
	EventQuotaChanged      = "quota.changed"
	EventStageBoundary     = "stage.boundary"
	EventHypothesisNew     = "hypothesis.new"
	EventFindingNew        = "finding.new"
	EventCommentNew        = "comment.new"
	// EventMetricPoint is a pointer, never a value: experiment id, metric name, fraction
	// complete. Metrics live solely in the metrics store, and this event says only that one
	// arrived — a subscriber that wants the number reads it from there. It is the one kind
	// Replay cannot return, because Postgres never saw it (see Replay).
	EventMetricPoint = "metric.point"
)

// eventsChannel is the single Postgres NOTIFY channel every event is emitted on, by the AFTER
// triggers in schema.sql. One channel, so one LISTEN per process carries the whole stream.
const eventsChannel = "hypothesisloop_events"

// EventFilter scopes a subscription. Empty fields do not filter.
type EventFilter struct {
	PlatformExperimentID string
	ExperimentID         string
	AgentID              string
	// Kinds, when non-empty, admits only these kinds.
	Kinds map[string]bool
}

// isExperimentSubject reports the kinds whose Subject is an experiment id, and therefore the only
// ones an ExperimentID filter can meaningfully narrow to.
func isExperimentSubject(kind string) bool {
	switch kind {
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint:
		return true
	default:
		return false
	}
}

// isAgentOwned reports the kinds that describe one agent's own property — its jobs and its
// allocation. Everything else on the stream is shared: the idea pool and the stage ladder belong
// to the platform experiment, not to whoever happened to write the row.
func isAgentOwned(kind string) bool {
	switch kind {
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint, EventQuotaChanged:
		return true
	default:
		return false
	}
}

// Matches decides whether one event belongs on one subscription. Live fan-out and replay both
// go through it, so a reconnecting client cannot receive a differently-scoped set than it was
// receiving before the connection broke.
//
// The agent rule narrows only the kinds that describe that agent's own property. An agent asking
// for its own jobs still receives pool activity and stage boundaries whoever wrote them, because
// those are the shared notebook and the shared ladder — filtering them by author would leave an
// agent watching a competition it can no longer see.
func (f EventFilter) Matches(e Event) bool {
	if len(f.Kinds) > 0 && !f.Kinds[e.Kind] {
		return false
	}
	if f.PlatformExperimentID != "" && e.PlatformExperimentID != f.PlatformExperimentID {
		return false
	}
	if f.ExperimentID != "" && (!isExperimentSubject(e.Kind) || e.Subject != f.ExperimentID) {
		return false
	}
	if f.AgentID != "" && isAgentOwned(e.Kind) && e.AgentID != f.AgentID {
		return false
	}
	return true
}

// EventsStore is the only path to the change stream: it listens for live events and derives the
// replay of missed ones. Like every other store here it owns its queries — nothing outside this
// package writes SQL against these tables to build an event.
type EventsStore struct {
	pool *Pool
}

// NewEventsStore returns an EventsStore backed by pool.
func NewEventsStore(pool *Pool) *EventsStore {
	return &EventsStore{pool: pool}
}

// ListenEvents holds one dedicated connection on LISTEN and forwards every notification to out
// until ctx ends or the connection breaks, returning why. It never reconnects itself: the caller
// owns the retry policy, and a caller that reconnects must also replay from its last cursor,
// which only the caller knows.
func (s *EventsStore) ListenEvents(ctx context.Context, out chan<- Event) error {
	conn, err := s.pool.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("events_store.ListenEvents: acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+eventsChannel); err != nil {
		return fmt.Errorf("events_store.ListenEvents: listen: %w", err)
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("events_store.ListenEvents: wait: %w", err)
		}
		var e Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err != nil {
			return fmt.Errorf("events_store.ListenEvents: decode %q: %w", n.Payload, err)
		}
		select {
		case out <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// NotifyEvent emits one event that no row change produced. The metric pointer is the only such
// event: a metric is written to the metrics store, not to Postgres, so there is no row and no
// transaction to hang a trigger on. It is therefore also the only kind Replay cannot reconstruct.
func (s *EventsStore) NotifyEvent(ctx context.Context, e Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("events_store.NotifyEvent: encode: %w", err)
	}
	if _, err := s.pool.pool.Exec(ctx, "SELECT pg_notify($1, $2)", eventsChannel, string(payload)); err != nil {
		return fmt.Errorf("events_store.NotifyEvent: %w", err)
	}
	return nil
}

// NewCursor returns the cursor value for a moment in time.
func NewCursor(t time.Time) int64 { return t.UnixMicro() }

func cursorTime(cursor int64) time.Time { return time.UnixMicro(cursor).UTC() }

// Replay returns, in cursor order, every event with a cursor greater than since that matches
// filter — exactly what a client that was disconnected at `since` missed.
//
// Nothing is replayed from a stored event log, because there is no event log: every event this
// stream carries is a change to a row Postgres already holds, and every one of those rows already
// carries the timestamps that change happened at. Replay re-derives the events from those
// columns. A persisted copy of them beside the rows themselves would be the same state written
// twice, and would drift the first time a row was written by a path that forgot to append to it.
//
// What that costs, stated plainly: the rows record when each state was reached, not every state
// ever passed through. A status the experiment row timestamps (QUEUED via queued_at, SUBMITTED
// via submitted_at, and whatever it holds now via updated_at) replays exactly; a status it passed
// through and left inside the missed window, without its own column, is collapsed into the one
// the row now holds. So a reconnecting client is never told something false and never misses a
// subject that changed — it may only be told the latest of two values instead of both. That is
// the honest limit of deriving history from state, and it is the trade important.md asks for.
//
// since <= 0 means "start live": there is nothing to catch up on.
func (s *EventsStore) Replay(ctx context.Context, filter EventFilter, since int64) ([]Event, error) {
	if since <= 0 {
		return nil, nil
	}
	if filter.PlatformExperimentID == "" {
		return nil, fmt.Errorf("events_store.Replay: platform_experiment_id is required")
	}
	at := cursorTime(since)

	var events []Event
	for _, derive := range []func(context.Context, string, time.Time) ([]Event, error){
		s.replayExperiments,
		s.replayQuotas,
		s.replayPool,
		s.replayStageBoundaries,
	} {
		part, err := derive(ctx, filter.PlatformExperimentID, at)
		if err != nil {
			return nil, err
		}
		events = append(events, part...)
	}

	kept := events[:0]
	for _, e := range events {
		if e.Cursor > since && filter.Matches(e) {
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Cursor < kept[j].Cursor })
	return kept, nil
}

func (s *EventsStore) replayExperiments(ctx context.Context, peID string, at time.Time) ([]Event, error) {
	const q = `SELECT id, agent_id, status::text, COALESCE(eviction_reason, ''), COALESCE(not_admitted_reason, ''),
	queued_at, submitted_at, updated_at
FROM experiments
WHERE platform_experiment_id = $1 AND (updated_at > $2 OR queued_at > $2 OR submitted_at > $2)`
	rows, err := s.pool.pool.Query(ctx, q, peID, at)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayExperiments: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var id, agentID, status, evictionReason, notAdmitted string
		var queuedAt, submittedAt *time.Time
		var updatedAt time.Time
		if err := rows.Scan(&id, &agentID, &status, &evictionReason, &notAdmitted, &queuedAt, &submittedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("events_store.replayExperiments: scan: %w", err)
		}
		if queuedAt != nil {
			events = append(events, Event{Kind: EventExperimentStatus, Subject: id, Value: "QUEUED",
				PlatformExperimentID: peID, AgentID: agentID, Cursor: NewCursor(*queuedAt)})
		}
		if submittedAt != nil {
			events = append(events, Event{Kind: EventExperimentStatus, Subject: id, Value: "SUBMITTED",
				PlatformExperimentID: peID, AgentID: agentID, Cursor: NewCursor(*submittedAt)})
		}
		current := Event{Kind: EventExperimentStatus, Subject: id, Value: status, Detail: evictionReason,
			PlatformExperimentID: peID, AgentID: agentID, Cursor: NewCursor(updatedAt)}
		events = append(events, current)
		if notAdmitted != "" {
			events = append(events, Event{Kind: EventExperimentBlocked, Subject: id, Value: notAdmitted,
				PlatformExperimentID: peID, AgentID: agentID, Cursor: NewCursor(updatedAt)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayExperiments: %w", err)
	}
	return dedupe(events), nil
}

// dedupe drops an event that is identical to one already derived at the same cursor. The waypoint
// columns and updated_at name the same transition whenever the row has not moved since — without
// this a job sitting in QUEUED would replay QUEUED twice.
func dedupe(events []Event) []Event {
	seen := make(map[Event]bool, len(events))
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

func (s *EventsStore) replayQuotas(ctx context.Context, peID string, at time.Time) ([]Event, error) {
	const q = `SELECT agent_id, guaranteed_accelerator_hours + burst_accelerator_hours, updated_at
FROM agent_quotas WHERE platform_experiment_id = $1 AND updated_at > $2`
	rows, err := s.pool.pool.Query(ctx, q, peID, at)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var agentID string
		var hours float64
		var updatedAt time.Time
		if err := rows.Scan(&agentID, &hours, &updatedAt); err != nil {
			return nil, fmt.Errorf("events_store.replayQuotas: scan: %w", err)
		}
		events = append(events, Event{Kind: EventQuotaChanged, Subject: agentID,
			Value: formatHours(hours), PlatformExperimentID: peID, AgentID: agentID,
			Cursor: NewCursor(updatedAt)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	return events, nil
}

func (s *EventsStore) replayPool(ctx context.Context, peID string, at time.Time) ([]Event, error) {
	const q = `
SELECT 'hypothesis.new', h.id, h.source, h.text, COALESCE(h.agent_id, ''), h.created_at
FROM hypotheses h WHERE h.platform_experiment_id = $1 AND h.created_at > $2
UNION ALL
SELECT 'finding.new', f.hypothesis_id, f.experiment_id, '', f.agent_id, f.created_at
FROM hypothesis_findings f JOIN hypotheses h ON h.id = f.hypothesis_id
WHERE h.platform_experiment_id = $1 AND f.created_at > $2
UNION ALL
SELECT 'comment.new', c.hypothesis_id, c.source, '', COALESCE(c.agent_id, ''), c.created_at
FROM hypothesis_comments c JOIN hypotheses h ON h.id = c.hypothesis_id
WHERE h.platform_experiment_id = $1 AND c.created_at > $2`
	rows, err := s.pool.pool.Query(ctx, q, peID, at)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayPool: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt time.Time
		if err := rows.Scan(&e.Kind, &e.Subject, &e.Value, &e.Detail, &e.AgentID, &createdAt); err != nil {
			return nil, fmt.Errorf("events_store.replayPool: scan: %w", err)
		}
		e.PlatformExperimentID = peID
		e.Cursor = NewCursor(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayPool: %w", err)
	}
	return events, nil
}

func (s *EventsStore) replayStageBoundaries(ctx context.Context, peID string, at time.Time) ([]Event, error) {
	const q = `
SELECT a.stage_index, 'advanced', '', a.advanced_at
FROM platform_experiment_stage_advances a WHERE a.platform_experiment_id = $1 AND a.advanced_at > $2
UNION ALL
SELECT c.stage_index, 'cut', c.agent_id, c.cut_at
FROM platform_experiment_cuts c WHERE c.platform_experiment_id = $1 AND c.cut_at > $2`
	rows, err := s.pool.pool.Query(ctx, q, peID, at)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayStageBoundaries: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var stageIndex int
		var detail, agentID string
		var ts time.Time
		if err := rows.Scan(&stageIndex, &detail, &agentID, &ts); err != nil {
			return nil, fmt.Errorf("events_store.replayStageBoundaries: scan: %w", err)
		}
		events = append(events, Event{Kind: EventStageBoundary, Subject: peID,
			Value: fmt.Sprintf("%d", stageIndex), Detail: detail,
			PlatformExperimentID: peID, AgentID: agentID, Cursor: NewCursor(ts)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayStageBoundaries: %w", err)
	}
	return events, nil
}

// formatHours renders an hour figure the same way the notify trigger's ::text cast does, so a
// live quota.changed event and its replayed twin carry the same value rather than two spellings
// of one number.
func formatHours(h float64) string {
	return fmt.Sprintf("%g", h)
}
