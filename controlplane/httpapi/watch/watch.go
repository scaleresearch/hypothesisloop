package watch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// EventSource is the change stream this service serves. Both halves come from the one store that
// owns them (db.EventsStore): the live one from Postgres LISTEN/NOTIFY, the catch-up one derived
// from the rows the events describe. Concerns completes domain.EventFilter.Matches for an agent filter.
type EventSource interface {
	ListenEvents(ctx context.Context, out chan<- domain.Event, listening func()) error
	Replay(ctx context.Context, filter domain.EventFilter, since int64) ([]domain.Event, error)
	Concerns(ctx context.Context, filter domain.EventFilter, events []domain.Event) ([]domain.Event, error)
}

const (
	// subscriberBuffer is how far behind a single connection may fall before it is dropped
	// rather than served a gap. Dropping is the correct answer here and not a failure: the
	// client reconnects with its last cursor and replays what it missed, which is the same
	// path a broken network takes. Silently discarding events would be the failure.
	subscriberBuffer = 256
	// listenerBuffer holds events between the one LISTEN connection and the fan-out.
	listenerBuffer = 1024
	// watchPingInterval keeps an idle stream honest — a job can sit RUNNING for an hour with
	// nothing to say, and without this neither end would notice the path between them died.
	watchPingInterval = 20 * time.Second
	watchWriteTimeout = 10 * time.Second
	// listenRetryDelay is how long the fan-out waits before re-establishing LISTEN after the
	// connection to Postgres breaks. Subscribers are dropped when that happens, so each one
	// reconnects and replays; nothing is lost, the stream is only delayed.
	listenRetryDelay = 2 * time.Second
)

// subscriber is one live connection's view of the stream. This — and only this — is what the
// control plane holds in memory for /watch: no subscriber registry outlives the sockets, and
// nothing here is state anyone could read back. Drop the connection and it is gone.
//
// filter is mutable because a connected client may re-scope its own subscription without dropping
// the socket, so it is read and written under the hub's lock — the same lock broadcast holds.
type subscriber struct {
	filter domain.EventFilter
	// since is the cursor this connection opened at. A widened filter replays the newly-added
	// kinds from here (or from where they were last delivered), which is what stops a widen from
	// being a silent gap.
	since  int64
	events chan domain.Event
	// lagged is closed when this subscriber fell further behind than subscriberBuffer. The
	// connection is then closed, deliberately, so the client reconnects and replays.
	lagged chan struct{}
	once   sync.Once
}

func (s *subscriber) markLagged() { s.once.Do(func() { close(s.lagged) }) }

// hub fans one process-wide LISTEN out to every live connection.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

func (h *hub) subscribe(filter domain.EventFilter, since int64) *subscriber {
	sub := &subscriber{filter: filter, since: since, events: make(chan domain.Event, subscriberBuffer), lagged: make(chan struct{})}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

// setFilter re-scopes one live connection. It takes the same lock broadcast does, so an event is
// matched against either the old filter or the new one and never against a half-written one.
func (h *hub) setFilter(sub *subscriber, filter domain.EventFilter) {
	h.mu.Lock()
	sub.filter = filter
	h.mu.Unlock()
}

func (h *hub) filterOf(sub *subscriber) domain.EventFilter {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sub.filter
}

// Subscription is what one live connection is subscribed to. It is derived from the filter
// the server is actually matching against, never from what the client asked for, so it cannot
// report a subscription the server is not serving.
type Subscription struct {
	PlatformExperimentID string   `json:"platform_experiment_id"`
	ExperimentID         string   `json:"experiment_id"`
	AgentID              string   `json:"agent_id"`
	Kinds                []string `json:"kinds"`
	// Decisions is set on a stream narrowed to what may need a decision (domain.EventFilter).
	Decisions bool `json:"decisions,omitempty"`
	// Since is the cursor this connection opened at, and the floor a widened filter replays from.
	Since int64 `json:"since"`
}

func describe(filter domain.EventFilter, since int64) Subscription {
	kinds := make([]string, 0, len(filter.Kinds))
	for kind := range filter.Kinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return Subscription{
		PlatformExperimentID: filter.PlatformExperimentID,
		ExperimentID:         filter.ExperimentID,
		AgentID:              filter.AgentID,
		Kinds:                kinds,
		Decisions:            filter.Decisions,
		Since:                since,
	}
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

func (h *hub) broadcast(e domain.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if !sub.filter.Matches(e) {
			continue
		}
		select {
		case sub.events <- e:
		default:
			sub.markLagged()
		}
	}
}

// dropAll ends every live connection. Called when the LISTEN connection breaks: from that moment
// this process cannot know what it is not being told, and a stream that silently stops carrying
// events is worse than one that visibly ends. Called again when LISTEN is back, see Start.
func (h *hub) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.markLagged()
	}
}

// Handler serves GET /watch. It is registered on the chi router directly rather than through
// apidocs/Huma: a WebSocket upgrade needs the raw connection, and Huma owns the response for
// every operation registered with it. The honest place for it is therefore beside the router, and
// the contract a caller reads for it is GET /watch/kinds (httpapi.RegisterWatchKinds) and hl watch --help.
type Handler struct {
	source EventSource
	// experiments resolves the platform experiment a job belongs to, so a caller that knows
	// only its own job id — which is all a running job is told — can subscribe without first
	// having to look that up itself.
	experiments experimentLookup
	access      *auth.Access
	hub         *hub
	logger      *zap.Logger
	// recheckEvery is how often an open stream is pinged and its token asked again.
	recheckEvery time.Duration
}

type experimentLookup interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
}

// NewHandler returns a handler over source. Start must be called once to begin listening.
// Its requests must pass auth.Gate first, which reads the token from the subprotocol
// (Subprotocol) before the upgrade.
func NewHandler(source EventSource, experiments experimentLookup, access *auth.Access, logger *zap.Logger) *Handler {
	return &Handler{source: source, experiments: experiments, access: access, hub: newHub(), logger: logger, recheckEvery: watchPingInterval}
}

// Start runs the single LISTEN connection for this process until ctx ends, re-establishing it
// when it breaks. A client that connected while nothing was listening replayed before LISTEN took
// effect, so events committed in between reached neither its replay nor its stream: every
// connection is dropped again once LISTEN is in effect, and replays.
func (h *Handler) Start(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			events := make(chan domain.Event, listenerBuffer)
			done := make(chan error, 1)
			listenCtx, cancel := context.WithCancel(ctx)
			go func() { done <- h.source.ListenEvents(listenCtx, events, h.hub.dropAll) }()

			for draining := true; draining; {
				select {
				case e := <-events:
					h.hub.broadcast(e)
				case err := <-done:
					if ctx.Err() == nil {
						h.logger.Warn("watch: change stream ended, reconnecting", zap.Error(err))
					}
					draining = false
				case <-ctx.Done():
					draining = false
				}
			}
			cancel()
			h.hub.dropAll()
			select {
			case <-ctx.Done():
			case <-time.After(listenRetryDelay):
			}
		}
	}()
}

func (h *Handler) admit(ctx context.Context, principal auth.Principal) (context.Context, error) {
	if !apidocs.AudienceAgent.Admits(principal, "") {
		return nil, auth.Forbidden("forbidden: a " + string(principal.Kind) + " token may not watch")
	}
	// A stream only reads. The platform experiment it is narrowed to is admitted by the access
	// check on its own, so none is named for a listing here.
	return auth.WithRead(auth.WithPrincipal(ctx, principal), ""), nil
}

// stillAdmitted asks again what the upgrade asked: a stream outlives the request that opened
// it, and the token may have died or lost the platform experiment since.
func (h *Handler) stillAdmitted(ctx context.Context, platformExperimentID string) error {
	principal, err := auth.Recheck(ctx)
	if err != nil {
		return err
	}
	ctx, err = h.admit(ctx, principal)
	if err != nil {
		return err
	}
	return h.access.PlatformExperiment(ctx, platformExperimentID)
}

// ServeHTTP upgrades to a WebSocket and streams events: first everything missed since the
// caller's cursor, then everything as it happens. A caller with no cursor (`since` omitted or 0)
// gets the current snapshot of everything its filter admits, so watching a job that is already
// terminal answers immediately instead of waiting on a live event that will never come. The
// subscription is opened before the replay runs, so an event committed between the two is
// buffered rather than lost — which is what makes a dropped connection a delay and never a gap.
//
// Exactly one thing may be sent up this socket, and it is not a command to the platform: a client
// may inspect and re-scope ITS OWN subscription — see watchControl. That touches nothing but the
// filter of this one connection, which lives and dies with the socket, and it writes nothing
// anywhere. Do not widen it into a command. The control plane decides, and every decision it makes
// is written as desired state the runtime fetches; anything a job or an agent could be *told* down
// this socket would be a second path to an effect that already has one, and the two would disagree
// the first time this connection dropped.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: reached without passing auth.Gate", errors.New("no principal")))
		return
	}
	ctx, err := h.admit(r.Context(), principal)
	if err != nil {
		auth.WriteError(w, h.logger, err)
		return
	}
	r = r.WithContext(ctx)
	filter, since, err := parseWatchQuery(r)
	if err != nil {
		writeWatchError(w, http.StatusBadRequest, err.Error())
		return
	}
	if principal.Kind == auth.KindAgent {
		filter.AgentID = principal.AgentID
	}
	if filter.ExperimentID != "" {
		if err := h.access.Experiment(ctx, filter.ExperimentID); err != nil {
			auth.WriteError(w, h.logger, err)
			return
		}
	}
	if filter.PlatformExperimentID == "" {
		exp, err := h.experiments.GetExperiment(r.Context(), filter.ExperimentID)
		if err != nil {
			auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: resolve experiment", err, zap.String("id", filter.ExperimentID)))
			return
		}
		if exp == nil {
			writeWatchError(w, http.StatusNotFound, "no such experiment "+filter.ExperimentID)
			return
		}
		filter.PlatformExperimentID = exp.PlatformExperimentID
	}
	if err := h.access.PlatformExperiment(ctx, filter.PlatformExperimentID); err != nil {
		auth.WriteError(w, h.logger, err)
		return
	}

	sub := h.hub.subscribe(filter, since)
	defer h.hub.unsubscribe(sub)

	replayed, err := h.source.Replay(r.Context(), filter, since)
	if errors.Is(err, db.ErrUnknownCursor) {
		writeWatchError(w, http.StatusGone, err.Error()+"; reconnect without since to start from the current state")
		return
	}
	if err != nil {
		auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: replay", err))
		return
	}

	conn, err := acceptWebSocket(w, r)
	if err != nil {
		writeWatchError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer conn.Close()

	controls := make(chan []byte, 4)
	peerGone := make(chan struct{})
	go func() {
		defer close(peerGone)
		for {
			opcode, payload, err := conn.ReadFrame()
			if err != nil {
				return
			}
			if opcode != wsOpText {
				continue
			}
			select {
			case controls <- payload:
			case <-r.Context().Done():
				return
			}
		}
	}()

	// It is stack-local to this connection and vanishes with it, like the subscription itself.
	delivered := &served{ctx: r.Context(), source: h.source, logger: h.logger, floor: map[string]int64{}, sent: map[domain.Event]bool{}}
	for kind := range filter.Kinds {
		delivered.floor[kind] = since
	}
	if err := delivered.send(conn, filter, replayed...); err != nil {
		return
	}

	ping := time.NewTicker(h.recheckEvery)
	defer ping.Stop()
	for {
		select {
		case e := <-sub.events:
			// Replay and the live stream overlap by construction — the subscription was open
			// while the replay ran. send drops what the replay already carried.
			if err := delivered.send(conn, filter, e); err != nil {
				return
			}
		case payload := <-controls:
			if err := h.applyControl(r.Context(), conn, sub, payload, delivered); err != nil {
				return
			}
		case <-ping.C:
			if err := h.stillAdmitted(r.Context(), filter.PlatformExperimentID); err != nil {
				var refusal *auth.Error
				if errors.As(err, &refusal) {
					h.logger.Info("watch: closing a stream whose token is no longer admitted",
						zap.String("platform_experiment_id", filter.PlatformExperimentID), zap.String("reason", refusal.Message))
					return
				}
				// The platform's own failure says nothing about the caller: ask again next tick.
				h.logger.Warn("watch: could not re-check a stream's token", zap.Error(err))
			}
			if err := conn.WritePing(watchWriteTimeout); err != nil {
				return
			}
		case <-sub.lagged:
			return
		case <-peerGone:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeEvent(conn *wsConn, e domain.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return conn.WriteText(payload, watchWriteTimeout)
}

// served is what one connection has been sent. floor is, per kind, the largest cursor delivered: a
// kind is carried from its own floor because after a filter change one stream-wide cursor would
// skip. A replay resends what is at its cursor and the live stream repeats what committed while a
// replay ran. An event below its kind's floor is such a repeat: its transaction had finished
// before an event already sent was written, so it came earlier on this stream. At or above the
// floor only a repeat of the same identity is dropped, which sent holds: one transaction writes
// several events (a stage cut per agent), and each is news. Every event
// this connection writes passes through send, so it is where source.Concerns completes the
// filter, for replay and live alike.
type served struct {
	ctx    context.Context
	source EventSource
	logger *zap.Logger
	floor  map[string]int64
	sent   map[domain.Event]bool
}

func (s *served) send(conn *wsConn, filter domain.EventFilter, events ...domain.Event) error {
	events, err := s.source.Concerns(s.ctx, filter, events)
	if err != nil {
		s.logger.Error("watch: scope events to their agent", zap.Error(err))
		return err
	}
	for _, e := range events {
		if e.Position < s.floor[e.Kind] || s.sent[e.Identity()] {
			continue
		}
		if err := writeEvent(conn, e); err != nil {
			return err
		}
		s.sent[e.Identity()] = true
		if e.Cursor > s.floor[e.Kind] {
			s.floor[e.Kind] = e.Cursor
			for sent := range s.sent {
				if sent.Kind == e.Kind && sent.Position < e.Cursor {
					delete(s.sent, sent)
				}
			}
		}
	}
	return nil
}

// watchControl is the only frame a client may send, and everything it can do is to this one
// connection's own filter. `kinds` absent asks a question and changes nothing; `kinds` present
// re-scopes the subscription, and the empty string restores the default set.
type watchControl struct {
	Kinds *string `json:"kinds"`
}

// watchFrame is what the server writes in answer to a control frame. It is a distinct shape from
// an event — an event has a `kind`, this has a `subscription` or an `error` — so a client reading
// one stream of text frames can tell an answer from news without a mode flag.
type watchFrame struct {
	Subscription *Subscription `json:"subscription,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// applyControl answers, and if asked re-scopes, on the writer's own goroutine — so the filter
// change is ordered against the events around it rather than racing them.
//
// Narrowing can never open a gap: the client asked to stop being told, and every floor stays where
// it was, so re-adding a kind resumes from the last event of that kind it actually received.
//
// Widening would open one, and this is where that is paid for. A kind added now was not matched a
// moment ago, so the live stream will never carry what it missed, and the client's cursor would
// claim otherwise. The filter is therefore widened FIRST — from that instant the added kinds are
// buffered for this connection — and only then are they replayed from their own floor: where this
// connection last saw that kind, or, for a kind it never carried, the cursor it opened at. The
// overlap between that replay and what the widened filter has been buffering is removed the same
// way as at handshake time (see served).
//
// The honest limit here is metric.point: it has no row behind it, so widening onto it can only
// ever start live. Every other kind widens the same way a fresh
// connection opens -- a connection that carried no floor for it yet is treated as `since=0`,
// which Replay answers with that kind's current snapshot, not silence.
func (h *Handler) applyControl(ctx context.Context, conn *wsConn, sub *subscriber, payload []byte, delivered *served) error {
	var control watchControl
	if err := json.Unmarshal(payload, &control); err != nil {
		return writeFrame(conn, watchFrame{Error: "a control frame must be a JSON object, optionally carrying \"kinds\""})
	}
	if control.Kinds != nil {
		kinds, err := parseKinds(*control.Kinds)
		if err != nil {
			return writeFrame(conn, watchFrame{Error: err.Error()})
		}
		filter := h.hub.filterOf(sub)
		added := map[int64]map[string]bool{}
		for kind := range kinds {
			if filter.Kinds[kind] {
				continue
			}
			floor, carried := delivered.floor[kind]
			if !carried {
				floor = sub.since
			}
			if added[floor] == nil {
				added[floor] = map[string]bool{}
			}
			added[floor][kind] = true
			delivered.floor[kind] = floor
		}
		filter.Kinds = kinds
		h.hub.setFilter(sub, filter)

		floors := make([]int64, 0, len(added))
		for floor := range added {
			floors = append(floors, floor)
		}
		sort.Slice(floors, func(i, j int) bool { return floors[i] < floors[j] })
		for _, floor := range floors {
			replayFilter := filter
			replayFilter.Kinds = added[floor]
			events, err := h.source.Replay(ctx, replayFilter, floor)
			if err != nil {
				h.logger.Error("watch: replay after widening a subscription", zap.Error(err))
				return writeFrame(conn, watchFrame{Error: "could not replay the kinds you added; reconnect with since=<largest cursor you saw>"})
			}
			if err := delivered.send(conn, replayFilter, events...); err != nil {
				return err
			}
		}
	}
	subscription := describe(h.hub.filterOf(sub), sub.since)
	return writeFrame(conn, watchFrame{Subscription: &subscription})
}

func writeFrame(conn *wsConn, frame watchFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return conn.WriteText(payload, watchWriteTimeout)
}

// knownKinds is the closed set a subscription may name. An unrecognised one is rejected rather
// than ignored: a client filtering on a kind that does not exist would wait forever on a stream
// that looks healthy, which is exactly the failure /watch exists to end.
//
// It is built from domain.EventKinds, which is also what GET /watch/kinds serves, so the set the
// server accepts and the set it advertises are one set. A second list written by hand here would
// be wrong the first time a kind was added, and wrong in the worst direction: a caller told a kind
// exists, then refused when it asks for it.
var knownKinds = func() map[string]bool {
	kinds := make(map[string]bool, len(domain.EventKinds))
	for _, k := range domain.EventKinds {
		kinds[k.Kind] = true
	}
	return kinds
}()

func parseWatchQuery(r *http.Request) (domain.EventFilter, int64, error) {
	q := r.URL.Query()
	filter := domain.EventFilter{
		PlatformExperimentID: q.Get("platform_experiment_id"),
		ExperimentID:         q.Get("experiment_id"),
		AgentID:              q.Get("agent_id"),
	}
	if q.Has("agent") {
		return filter, 0, errWatch("agent is no parameter of a watch: the agent is agent_id")
	}
	if raw := q.Get("decisions"); raw != "" {
		decisions, err := strconv.ParseBool(raw)
		if err != nil {
			return filter, 0, errWatch("decisions must be true or false")
		}
		filter.Decisions = decisions
	}
	if filter.PlatformExperimentID == "" && filter.ExperimentID == "" {
		return filter, 0, errWatch("one of platform_experiment_id or experiment_id is required")
	}
	kinds, err := parseKinds(q.Get("kinds"))
	if err != nil {
		return filter, 0, err
	}
	filter.Kinds = kinds
	var since int64
	if raw := q.Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return filter, 0, errWatch("since must be a cursor returned by a previous event")
		}
		since = parsed
	}
	return filter, since, nil
}

// parseKinds turns a comma-separated list into the set a filter admits. An empty list is the
// default subscription — everything an agent's loop would otherwise poll for — and a list that
// names kinds means exactly those and nothing more, including when what it names is one kind the
// default leaves out.
func parseKinds(raw string) (map[string]bool, error) {
	if strings.TrimSpace(raw) == "" {
		return domain.DefaultKinds(), nil
	}
	kinds := map[string]bool{}
	for _, kind := range strings.Split(raw, ",") {
		kind = strings.TrimSpace(kind)
		if !knownKinds[kind] {
			return nil, errWatch("unknown kind " + kind)
		}
		kinds[kind] = true
	}
	return kinds, nil
}

type errWatch string

func (e errWatch) Error() string { return string(e) }

// writeWatchError answers in the same {"error": "..."} envelope every other operation on this
// API uses, so a caller parses one error shape whether or not the upgrade ever happened.
func writeWatchError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
