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

// Event is one change, described as small as it can be described: what kind of thing changed,
// which thing, its new value, one qualifying detail, the two ids a subscription is scoped by,
// a position and a cursor. Never a copy of the row — a client that wants detail follows with a normal GET,
// so this stream duplicates no read path and no state.
type Event struct {
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
	// Position is the id of the transaction that made the change, the same on a live event and
	// on its replayed twin. With the fields above it identifies the event; transaction ids are
	// not commit ordered, so it does not order the stream.
	Position int64 `json:"position"`
	// OccurredAt is when the transaction at Position began, the same on a live event and on its
	// replayed twin. It is absent on the replay of a change made before positions kept a time.
	OccurredAt time.Time `json:"occurred_at,omitzero"`
	// Cursor is where to resume: every transaction below it had finished when this event was
	// written, so its events were already delivered. A client hands the largest cursor it has
	// seen back as `since`, and Replay returns every event at or above it. That can repeat an
	// event the client already has (same Position), and never skips one.
	Cursor int64 `json:"cursor"`
}

// Identity is the event without its cursor and its time: a live event and its replayed twin carry
// different cursors, so a client dropping repeats compares identities, and the position already
// says which transaction it is.
func (e Event) Identity() Event {
	e.Cursor = 0
	e.OccurredAt = time.Time{}
	return e
}

// ErrUnknownCursor is a since this database cannot have issued, such as a cursor from before
// cursors were transaction ids.
var ErrUnknownCursor = errors.New("since is not a cursor this platform issued")

// Event kinds. Every event on the stream carries one of these.
const (
	EventExperimentStatus  = "experiment.status"
	EventExperimentBlocked = "experiment.blocked"
	EventQuotaChanged      = "quota.changed"
	EventStageBoundary     = "stage.boundary"
	EventHypothesisNew     = "hypothesis.new"
	EventHypothesisStatus  = "hypothesis.status"
	EventHypothesisLink    = "hypothesis.link"
	EventFindingNew        = "finding.new"
	EventCommentNew        = "comment.new"
	// EventAgentCut says the run ended for one agent. stage.boundary already says the ladder
	// moved, but it names the platform experiment, so an agent still had to GET to learn whether
	// the cut fell on it. This is the same row addressed to the only party it stops.
	EventAgentCut = "agent.cut"
	// EventAgentFinished says an agent told the platform it has nothing left to do. It is the
	// agent's own, so no rival is woken by it; whoever steers the run sees every one.
	EventAgentFinished = "agent.finished"
	// EventPlatformExperimentStatus and EventPlatformExperimentDescription describe the run
	// itself: whether it is still open, and whether the brief still says what it said. The
	// description event deliberately carries no text — see EventKinds.
	EventPlatformExperimentStatus      = "platform_experiment.status"
	EventPlatformExperimentDescription = "platform_experiment.description"
	// EventMetricPoint is a pointer, never a value: experiment id, metric name, fraction
	// complete. Metrics live solely in the metrics store, and this event says only that one
	// arrived — a subscriber that wants the number reads it from there. It is the one kind
	// Replay cannot return, because Postgres never saw it (see Replay).
	EventMetricPoint = "metric.point"
	// EventJobUnderperforming says only that this job trailed its cohort on the platform
	// experiment's ranking metric by enough margin to be worth a look: an opinion, never an
	// instruction. Trailing is recomputed from the metrics store when a sample makes it true; the
	// job's row records the metric its owner was told about, once, and that write is the event,
	// live and replayed. The agent decides; the existing cancel-experiment call is the only path
	// that ever ends the job.
	EventJobUnderperforming = "job.underperforming"
	// EventJobHeld says a job has stayed QUEUED for longer than it was to run, and why. A queue
	// reason that waiting clears is kept from an agent's stream (experiment.blocked), so without
	// this an agent whose job never starts learns nothing. The job's row records the reason its
	// owner was told, once, and that write is the event, live and replayed.
	EventJobHeld = "job.held"
)

// EventKindDoc describes one kind well enough to subscribe to it without reading this file: what
// its subject and value are, whether an `agent` filter narrows it, and what it is for.
type EventKindDoc struct {
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	Value       string `json:"value"`
	Description string `json:"description"`
	// AgentScope says what an `agent` filter keeps of this kind. It is filled in from agentScope,
	// written beside the rule Matches applies, so the documented scope and the applied one sit together.
	AgentScope string `json:"agent_scope"`
	// Default is true when a subscription that names no kinds carries this one. Like AgentScope it
	// is filled in from the predicate the server actually consults (isDefaultKind), so the default
	// set cannot be advertised here and be a different set on the wire.
	Default bool `json:"default"`
}

// EventKinds is the vocabulary of the stream, and the only list of it. The server validates a
// subscription's `kinds` against exactly this slice and GET /watch/kinds serves exactly this
// slice, so a kind cannot be documented but unaccepted, or accepted but undocumented — the two
// failures a hand-kept second list produces the first time a kind is added.
var EventKinds = []EventKindDoc{
	{Kind: EventExperimentStatus, Subject: "experiment id", Value: "QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED, CANCELLED",
		Description: "Your job moved. detail carries why, when the platform knows, as a reason code, or the code, a colon and a sentence (code: sentence): the reason a job was evicted, rejected or requeued (no_offer: no machine to rent for shape rtx4090 in 3m0s of asking), or how a FAILED job's process ended (container_failed: command exited with code 1: the last lines it said, the most recent last), which a FAILED job always has. The code is the text before the first colon, and the same split as eviction_reason on the job. A job requeued for a fault of the platform's (code_unreachable, no_offer, stuck_pending and the like) is QUEUED with that detail and runs again at no cost. The queue reason is experiment.blocked."},
	{Kind: EventExperimentBlocked, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason)",
		Description: "Why a job is still waiting. detail is the sentence that goes with the code (not_admitted_message). It changes while the status does not, so a stuck job has something to wait on. awaiting_admission and summary_gate are not sent, and an agent scope leaves out every reason that waiting clears, such as capacity_unavailable and outranked: GET /experiments has them."},
	{Kind: EventQuotaChanged, Subject: "agent id", Value: "share_accelerator_hours of GET /quota: all the agent may spend",
		Description: "A grant or a stage move landed. What is used of the share comes from GET /quota."},
	{Kind: EventStageBoundary, Subject: "platform experiment id", Value: "stage index",
		Description: "The elimination ladder advanced. detail is 'advanced' or 'cut'; agent_id names who was cut."},
	{Kind: EventAgentCut, Subject: "agent id", Value: "stage index",
		Description: "This agent was cut at a stage boundary. Its own stop condition, without a GET to find out whether the cut was its."},
	{Kind: EventAgentFinished, Subject: "agent id", Value: "empty by design",
		Description: "This agent said it has nothing left to do, and its machine stops. detail is the reason it gave. When every agent is finished or cut the run has nobody left to wait for."},
	{Kind: EventHypothesisNew, Subject: "hypothesis id", Value: "empty by design",
		Description: "An idea entered the shared pool. agent_id is whose it is, empty on one no agent owns. The claim itself and who registered it come from GET /hypotheses/{id}."},
	{Kind: EventHypothesisStatus, Subject: "hypothesis id", Value: "open, confirmed, refuted, inconclusive",
		Description: "A claim was settled. A settled claim is to read, not to retest."},
	{Kind: EventHypothesisLink, Subject: "hypothesis id (the link's source)", Value: "builds_on, contradicts or related",
		Description: "A link was added to an existing hypothesis. detail is the target: a hypothesis id, or the platform experiment id for the objective. Links given at registration ride on hypothesis.new instead."},
	{Kind: EventFindingNew, Subject: "hypothesis id", Value: "the experiment id that produced it",
		Description: "A result was attached to a claim — someone's number is now readable instead of yours being re-derived."},
	{Kind: EventCommentNew, Subject: "hypothesis id", Value: "empty by design",
		Description: "Someone said something on a claim, people included. agent_id is whose note it is, empty on one no agent owns."},
	{Kind: EventPlatformExperimentStatus, Subject: "platform experiment id", Value: "draft, open, running, closed",
		Description: "The run itself moved. 'closed' is a stop condition for every agent in it."},
	{Kind: EventPlatformExperimentDescription, Subject: "platform experiment id", Value: "empty by design",
		Description: "The brief changed. A description is unbounded and is never copied into an event: re-read it with GET /platform-experiments/{id}."},
	{Kind: EventMetricPoint, Subject: "experiment id", Value: "metric name and fraction complete",
		Description: "A sample arrived. The number lives only in the metrics store — read it with GET /experiments/{id}/metrics. The one kind replay cannot return."},
	{Kind: EventJobHeld, Subject: "experiment id", Value: "the code of the queue reason (not_admitted_reason)",
		Description: "This job is held in the queue. For summary_gate it is sent as soon as the scheduler holds the job: detail names the COMPLETED jobs whose summary its agent owes and the command that files one, and the job starts once none is owed. For any other reason it is sent when the job has waited longer than its estimated duration, and for at least the platform's scheduler.held_job_notice_seconds: detail is the sentence that says why (not_admitted_message), never empty, for capacity_unavailable the cluster, what it is short of and how many jobs hold that. Nothing is done to the job: it stays queued, and its owner decides whether to wait, cancel it or ask for less. Sent once per job for each of the two, with the reason it had then."},
	{Kind: EventJobUnderperforming, Subject: "experiment id", Value: "the metric name it trails on",
		Description: "This job trails its cohort on the ranking metric by a real margin. An opinion, not a command: nothing stops it but you. Consider POST /experiments/{id}/cancel if it is not worth its allocation. Sent once per job, and not for a job that has already ended."},
}

func init() {
	for i := range EventKinds {
		EventKinds[i].AgentScope = agentScope(EventKinds[i].Kind)
		EventKinds[i].Default = isDefaultKind(EventKinds[i].Kind)
	}
}

// isDefaultKind reports what a subscription naming no kinds carries: everything an agent's loop
// would otherwise poll for — its jobs and why they are stuck, its allocation, its two stop
// conditions, the ladder, the brief, and the shared pool.
//
// metric.point is the one exclusion, and it is deliberate. It is by far the highest-volume kind,
// it fires for every sample of every job in the run rather than for the subscriber's own, and it
// carries no value — only a pointer to the metrics store. On a run-wide subscription that is a
// firehose whose only effect is to push the connection past its buffer and get it dropped, and a
// stream that drops is worse than one that says less. It is also the single kind Replay cannot
// return, so defaulting it on would make the default subscription the only one with a hole in its
// reconnect. An agent watching one job's progress asks for it by name, which costs one flag and
// says exactly what it means.
func isDefaultKind(kind string) bool { return kind != EventMetricPoint }

// DefaultKinds is the kind set of a subscription that names none, derived from EventKinds so that
// the served vocabulary and the applied default are one list.
func DefaultKinds() map[string]bool {
	kinds := make(map[string]bool, len(EventKinds))
	for _, k := range EventKinds {
		if k.Default {
			kinds[k.Kind] = true
		}
	}
	return kinds
}

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
	// Decisions narrows a stream of a whole platform experiment to what whoever steers it may
	// have to act on: of a job's statuses the terminal ones, and of its queue reasons those
	// waiting will not clear. An agent filter is that narrow already.
	Decisions bool
}

// isExperimentSubject reports the kinds whose Subject is an experiment id, and therefore the only
// ones an ExperimentID filter can meaningfully narrow to.
func isExperimentSubject(kind string) bool {
	switch kind {
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint, EventJobUnderperforming, EventJobHeld:
		return true
	default:
		return false
	}
}

// isAgentOwned reports the kinds that describe one agent's own property: its jobs and its
// allocation. agent.cut is here and stage.boundary is not, though one row emits both: the cut is the
// one agent's stop condition, the boundary is about the ladder.
func isAgentOwned(kind string) bool {
	switch kind {
	case EventExperimentStatus, EventExperimentBlocked, EventMetricPoint, EventQuotaChanged, EventAgentCut, EventAgentFinished, EventJobUnderperforming, EventJobHeld:
		return true
	default:
		return false
	}
}

// isHypothesisActivity reports the kinds whose subject is a hypothesis.
func isHypothesisActivity(kind string) bool {
	switch kind {
	case EventHypothesisNew, EventHypothesisStatus, EventHypothesisLink, EventFindingNew, EventCommentNew:
		return true
	default:
		return false
	}
}

func agentScope(kind string) string {
	switch {
	case kind == EventExperimentStatus:
		return "terminal statuses of your own jobs"
	case kind == EventExperimentBlocked:
		return "your own jobs, and only a reason that waiting will not clear"
	case isAgentOwned(kind):
		return "your own"
	case kind == EventHypothesisNew:
		return "someone else's hypothesis linking to one you registered"
	case isHypothesisActivity(kind):
		return "someone else's, on a hypothesis you registered"
	case kind == EventStageBoundary:
		return "advances only; your own cut is agent.cut"
	default:
		return "every one"
	}
}

// Matches decides whether one event belongs on one subscription. Live fan-out and replay both
// go through it, so a reconnecting client cannot receive a differently-scoped set than it was
// receiving before the connection broke.
//
// An agent filter scopes the stream to what that agent has to act on, so it never needs to poll
// its own resources: its jobs' terminal statuses and the queue reasons waiting will not clear,
// its allocation and cut, ladder advances, the run's status and brief, and other people's
// activity on hypotheses it registered. A job waiting its turn for capacity is no news.
// Whether a hypothesis event concerns one of the agent's hypotheses is a graph question, answered
// by EventsStore.Concerns; Matches only drops the agent's own echo. Without an agent the stream is
// the whole platform experiment, or with Decisions what of it may need a decision: a job that
// was queued, admitted or started is on its way and asks nothing of anyone.
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
	if f.AgentID == "" {
		switch {
		case !f.Decisions:
			return true
		case e.Kind == EventExperimentStatus:
			return domain.ExperimentStatus(e.Value).IsTerminal()
		case e.Kind == EventExperimentBlocked:
			return !domain.QueueReasonClearsByWaiting(e.Value)
		default:
			return true
		}
	}
	switch {
	case e.Kind == EventExperimentStatus:
		return e.AgentID == f.AgentID && domain.ExperimentStatus(e.Value).IsTerminal()
	case e.Kind == EventExperimentBlocked:
		return e.AgentID == f.AgentID && !domain.QueueReasonClearsByWaiting(e.Value)
	case isAgentOwned(e.Kind):
		return e.AgentID == f.AgentID
	case isHypothesisActivity(e.Kind):
		return e.AgentID != f.AgentID
	case e.Kind == EventStageBoundary:
		return e.AgentID == ""
	default:
		return true
	}
}

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
	ReplayEvents(ctx context.Context, platformExperimentID string, since int64) ([]Event, error)
	ConcernsAgent(ctx context.Context, agentID string, events []Event) ([]bool, error)
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
func (s *EventsStore) ListenEvents(ctx context.Context, out chan<- Event, listening func()) error {
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
		var e Event
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
func answered(e Event) Event {
	if e.Kind == EventExperimentStatus {
		e.Value = string(domain.AnsweredStatus(domain.ExperimentStatus(e.Value), e.Detail))
	}
	return e
}

// Concerns completes Matches for an agent filter: of events, a hypothesis event stays only when it
// concerns a hypothesis that agent registered. Every other event that Matches admits stays. The
// graph is read once for all of them, so a snapshot replay over a large pool costs one read.
func (s *EventsStore) Concerns(ctx context.Context, filter EventFilter, events []Event) ([]Event, error) {
	if filter.AgentID == "" {
		return events, nil
	}
	var asked []Event
	for _, e := range events {
		if isHypothesisActivity(e.Kind) {
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
	kept := make([]Event, 0, len(events))
	next := 0
	for _, e := range events {
		if isHypothesisActivity(e.Kind) {
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
func (s *EventsStore) NotifyEvent(ctx context.Context, e Event) error {
	if _, err := s.pool.pool.Exec(ctx, notifySQL, e.Kind, e.Subject, e.Value, e.Detail, e.PlatformExperimentID, e.AgentID); err != nil {
		return fmt.Errorf("events_store.NotifyEvent: %w", err)
	}
	return nil
}

// Stamp is what Emit hands a write to another store: the position to record on what it writes,
// and a way to tell whether an earlier write's events were sent.
type Stamp struct {
	// Position is the id of Emit's transaction.
	Position int64
	// Aborted reports whether the transaction with id position aborted. The events it emitted
	// were then never sent, though its write to the other store may have committed: a write that
	// finds such a position on what it would have written records its own and emits the events
	// again.
	Aborted func(position int64) (bool, error)
}

// Emit runs a write to another store inside a Postgres transaction and emits the events it
// returns from that transaction. write stamps the transaction's id on what it writes: until
// this transaction ends no snapshot's xmin, and so no cursor, can pass it, which is what keeps a
// resuming watcher from skipping a graph write. Position and cursor of the returned events are
// set by Postgres.
func (s *EventsStore) Emit(ctx context.Context, write func(stamp Stamp) ([]Event, error)) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("events_store.Emit: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	stamp := Stamp{Aborted: func(position int64) (bool, error) {
		var status *string
		if err := tx.QueryRow(ctx, "SELECT pg_xact_status($1::bigint::text::xid8)", position).Scan(&status); err != nil {
			return false, fmt.Errorf("events_store.Emit: status of %d: %w", position, err)
		}
		return status != nil && *status == "aborted", nil
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
func (s *EventsStore) Replay(ctx context.Context, filter EventFilter, since int64) ([]Event, error) {
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

	var events []Event
	for _, derive := range []func(context.Context, pgx.Tx, string, int64) ([]Event, error){
		replayExperiments,
		replayQuotas,
		replayEvidence,
		replayStageBoundaries,
		replayAgentCuts,
		replayAgentsFinished,
		replayPlatformExperiment,
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
func occurredAt(ctx context.Context, tx pgx.Tx, events []Event) error {
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

func replayExperiments(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	const q = `SELECT id, agent_id, status::text, COALESCE(eviction_reason, ''), COALESCE(not_admitted_reason, ''),
	queued_xid, submitted_xid, status_xid, not_admitted_xid, COALESCE(underperforming_metric, ''), underperforming_xid,
	COALESCE(held_reason, ''), held_xid
FROM experiments
WHERE platform_experiment_id = $1
  AND (status_xid >= $2 OR not_admitted_xid >= $2 OR queued_xid >= $2 OR submitted_xid >= $2 OR underperforming_xid >= $2 OR held_xid >= $2)`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayExperiments: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var id, agentID, status, evictionReason, notAdmitted, underperformingMetric, heldReason string
		var queuedXid, submittedXid, underperformingXid, heldXid *int64
		var statusXid, notAdmittedXid int64
		if err := rows.Scan(&id, &agentID, &status, &evictionReason, &notAdmitted, &queuedXid, &submittedXid, &statusXid, &notAdmittedXid, &underperformingMetric, &underperformingXid, &heldReason, &heldXid); err != nil {
			return nil, fmt.Errorf("events_store.replayExperiments: scan: %w", err)
		}
		if queuedXid != nil {
			events = append(events, Event{Kind: EventExperimentStatus, Subject: id, Value: "QUEUED",
				PlatformExperimentID: peID, AgentID: agentID, Position: *queuedXid})
		}
		if submittedXid != nil {
			events = append(events, Event{Kind: EventExperimentStatus, Subject: id, Value: "SUBMITTED",
				PlatformExperimentID: peID, AgentID: agentID, Position: *submittedXid})
		}
		events = append(events, answered(Event{Kind: EventExperimentStatus, Subject: id, Value: status, Detail: evictionReason,
			PlatformExperimentID: peID, AgentID: agentID, Position: statusXid}))
		if reason, message := domain.SplitQueueReason(notAdmitted); reason != "" && reason != domain.NotAdmittedAwaitingAdmission && reason != domain.NotAdmittedSummaryGate {
			events = append(events, Event{Kind: EventExperimentBlocked, Subject: id, Value: reason, Detail: message,
				PlatformExperimentID: peID, AgentID: agentID, Position: notAdmittedXid})
		}
		// A job that left the queue is held no longer.
		if heldXid != nil && status == string(domain.StatusQueued) {
			reason, message := domain.SplitQueueReason(heldReason)
			events = append(events, Event{Kind: EventJobHeld, Subject: id, Value: reason, Detail: message,
				PlatformExperimentID: peID, AgentID: agentID, Position: *heldXid})
		}
		// An advisory to cancel a job that has ended has nothing left to advise.
		if underperformingXid != nil && !domain.ExperimentStatus(status).IsTerminal() {
			events = append(events, Event{Kind: EventJobUnderperforming, Subject: id, Value: underperformingMetric,
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

func replayQuotas(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	const q = `SELECT agent_id, guaranteed_accelerator_hours, burst_accelerator_hours, updated_xid
FROM agent_quotas WHERE platform_experiment_id = $1 AND updated_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var agentID string
		var guaranteed, burst float64
		var position int64
		if err := rows.Scan(&agentID, &guaranteed, &burst, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayQuotas: scan: %w", err)
		}
		share := (&domain.AgentQuota{GuaranteedAcceleratorHours: guaranteed, BurstAcceleratorHours: burst}).Share()
		events = append(events, Event{Kind: EventQuotaChanged, Subject: agentID, Value: formatHours(share),
			PlatformExperimentID: peID, AgentID: agentID, Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayQuotas: %w", err)
	}
	return events, nil
}

func replayEvidence(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
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

	var events []Event
	for rows.Next() {
		var e Event
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

func replayStageBoundaries(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	const q = `
SELECT a.stage_index, 'advanced', '', a.advanced_xid
FROM platform_experiment_stage_advances a WHERE a.platform_experiment_id = $1 AND a.advanced_xid >= $2
UNION ALL
SELECT c.stage_index, 'cut', c.agent_id, c.cut_xid
FROM platform_experiment_cuts c WHERE c.platform_experiment_id = $1 AND c.cut_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayStageBoundaries: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var stageIndex int
		var detail, agentID string
		var position int64
		if err := rows.Scan(&stageIndex, &detail, &agentID, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayStageBoundaries: scan: %w", err)
		}
		events = append(events, Event{Kind: EventStageBoundary, Subject: peID,
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
func replayAgentCuts(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	const q = `SELECT agent_id, stage_index, cut_xid FROM platform_experiment_cuts
WHERE platform_experiment_id = $1 AND cut_xid >= $2`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentCuts: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var agentID string
		var stageIndex int
		var position int64
		if err := rows.Scan(&agentID, &stageIndex, &position); err != nil {
			return nil, fmt.Errorf("events_store.replayAgentCuts: scan: %w", err)
		}
		events = append(events, Event{Kind: EventAgentCut, Subject: agentID,
			Value: fmt.Sprintf("%d", stageIndex), PlatformExperimentID: peID, AgentID: agentID,
			Position: position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayAgentCuts: %w", err)
	}
	return events, nil
}

// replayAgentsFinished derives agent.finished from the signup, which records it once.
func replayAgentsFinished(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	rows, err := tx.Query(ctx, `SELECT agent_id, finished_reason, finished_xid FROM experiment_signups
WHERE platform_experiment_id = $1 AND finished_xid >= $2`, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayAgentsFinished: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		event := Event{Kind: EventAgentFinished, PlatformExperimentID: peID}
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

// replayPlatformExperiment derives the run's own two events from the two columns that record when
// each of its fields last moved. They are separate columns for the reason replay exists: one
// updated_at cannot say which of the two changed, and answering "the description changed" because
// the status did would send every agent back to re-read a brief that still says what it said.
//
// A NULL column is a field that has never moved since the row was created, and produces nothing.
func replayPlatformExperiment(ctx context.Context, tx pgx.Tx, peID string, since int64) ([]Event, error) {
	const q = `SELECT status::text, status_changed_xid, description_changed_xid
FROM platform_experiments WHERE id = $1 AND (status_changed_xid >= $2 OR description_changed_xid >= $2)`
	rows, err := tx.Query(ctx, q, peID, since)
	if err != nil {
		return nil, fmt.Errorf("events_store.replayPlatformExperiment: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var status string
		var statusXid, descriptionXid *int64
		if err := rows.Scan(&status, &statusXid, &descriptionXid); err != nil {
			return nil, fmt.Errorf("events_store.replayPlatformExperiment: scan: %w", err)
		}
		if statusXid != nil {
			events = append(events, Event{Kind: EventPlatformExperimentStatus, Subject: peID,
				Value: status, PlatformExperimentID: peID, Position: *statusXid})
		}
		if descriptionXid != nil {
			events = append(events, Event{Kind: EventPlatformExperimentDescription, Subject: peID,
				PlatformExperimentID: peID, Position: *descriptionXid})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events_store.replayPlatformExperiment: %w", err)
	}
	return events, nil
}

// formatHours renders an hour figure the same way the notify trigger's ::text cast does, so a
// live quota.changed event and its replayed twin carry the same value rather than two spellings
// of one number.
func formatHours(h float64) string {
	return fmt.Sprintf("%g", h)
}
