package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
	return s.pool.write(ctx, func(tx *Write) error {
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
		return nil
	})
}
