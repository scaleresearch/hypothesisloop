package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// EventSource is the change stream this service serves. Both halves come from the one store that
// owns them (db.EventsStore): the live one from Postgres LISTEN/NOTIFY, the catch-up one derived
// from the rows the events describe.
type EventSource interface {
	ListenEvents(ctx context.Context, out chan<- db.Event) error
	Replay(ctx context.Context, filter db.EventFilter, since int64) ([]db.Event, error)
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
	filter db.EventFilter
	// since is the cursor this connection opened at. A widened filter replays the newly-added
	// kinds from here (or from where they were last delivered), which is what stops a widen from
	// being a silent gap.
	since  int64
	events chan db.Event
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

func (h *hub) subscribe(filter db.EventFilter, since int64) *subscriber {
	sub := &subscriber{filter: filter, since: since, events: make(chan db.Event, subscriberBuffer), lagged: make(chan struct{})}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

// setFilter re-scopes one live connection. It takes the same lock broadcast does, so an event is
// matched against either the old filter or the new one and never against a half-written one.
func (h *hub) setFilter(sub *subscriber, filter db.EventFilter) {
	h.mu.Lock()
	sub.filter = filter
	h.mu.Unlock()
}

func (h *hub) filterOf(sub *subscriber) db.EventFilter {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sub.filter
}

// list describes every connection open on this process right now. It is a read of the live sockets
// themselves — the map is exactly the set of open connections, and an entry disappears the instant
// its socket does — so it adds no state and cannot be written to. Nothing about a client that is
// not connected exists here to be listed.
func (h *hub) list(platformExperimentID, agentID string) []WatchSubscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []WatchSubscription{}
	for sub := range h.subs {
		if platformExperimentID != "" && sub.filter.PlatformExperimentID != platformExperimentID {
			continue
		}
		if agentID != "" && sub.filter.AgentID != agentID {
			continue
		}
		out = append(out, describe(sub.filter, sub.since))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlatformExperimentID != out[j].PlatformExperimentID {
			return out[i].PlatformExperimentID < out[j].PlatformExperimentID
		}
		if out[i].AgentID != out[j].AgentID {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].ExperimentID < out[j].ExperimentID
	})
	return out
}

// WatchSubscription is what one live connection is subscribed to. It is derived from the filter
// the server is actually matching against, never from what the client asked for, so it cannot
// report a subscription the server is not serving.
type WatchSubscription struct {
	PlatformExperimentID string   `json:"platform_experiment_id"`
	ExperimentID         string   `json:"experiment_id"`
	AgentID              string   `json:"agent_id"`
	Kinds                []string `json:"kinds"`
	// Since is the cursor this connection opened at, and the floor a widened filter replays from.
	Since int64 `json:"since"`
}

func describe(filter db.EventFilter, since int64) WatchSubscription {
	kinds := make([]string, 0, len(filter.Kinds))
	for kind := range filter.Kinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return WatchSubscription{
		PlatformExperimentID: filter.PlatformExperimentID,
		ExperimentID:         filter.ExperimentID,
		AgentID:              filter.AgentID,
		Kinds:                kinds,
		Since:                since,
	}
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

func (h *hub) broadcast(e db.Event) {
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
// events is worse than one that visibly ends.
func (h *hub) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		sub.markLagged()
	}
}

// WatchHandler serves GET /watch. It is registered on the chi router directly rather than through
// apidocs/Huma: a WebSocket upgrade needs the raw connection, and Huma owns the response for
// every operation registered with it. The honest place for it is therefore beside the router, and
// the contract agents actually read for it is the system prompt's hl-watch section.
type WatchHandler struct {
	source EventSource
	// experiments resolves the platform experiment a job belongs to, so a caller that knows
	// only its own job id — which is all a running job is told — can subscribe without first
	// having to look that up itself.
	experiments experimentLookup
	hub         *hub
	logger      *zap.Logger
}

type experimentLookup interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
}

// NewWatchHandler returns a handler over source. Start must be called once to begin listening.
func NewWatchHandler(source EventSource, experiments experimentLookup, logger *zap.Logger) *WatchHandler {
	return &WatchHandler{source: source, experiments: experiments, hub: newHub(), logger: logger}
}

// Start runs the single LISTEN connection for this process until ctx ends, re-establishing it
// when it breaks.
func (h *WatchHandler) Start(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			events := make(chan db.Event, listenerBuffer)
			done := make(chan error, 1)
			listenCtx, cancel := context.WithCancel(ctx)
			go func() { done <- h.source.ListenEvents(listenCtx, events) }()

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

// ServeHTTP upgrades to a WebSocket and streams events: first everything missed since the
// caller's cursor, then everything as it happens. The subscription is opened before the replay
// runs, so an event committed between the two is buffered rather than lost — which is what makes
// a dropped connection a delay and never a gap.
//
// Exactly one thing may be sent up this socket, and it is not a command to the platform: a client
// may inspect and re-scope ITS OWN subscription — see watchControl. That touches nothing but the
// filter of this one connection, which lives and dies with the socket, and it writes nothing
// anywhere. Do not widen it into a command. The control plane decides, and every decision it makes
// is written as desired state the runtime fetches; anything a job or an agent could be *told* down
// this socket would be a second path to an effect that already has one, and the two would disagree
// the first time this connection dropped.
func (h *WatchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	filter, since, err := parseWatchQuery(r)
	if err != nil {
		writeWatchError(w, http.StatusBadRequest, err.Error())
		return
	}
	if filter.PlatformExperimentID == "" {
		exp, err := h.experiments.GetExperiment(r.Context(), filter.ExperimentID)
		if err != nil {
			h.logger.Error("watch: resolve experiment", zap.String("id", filter.ExperimentID), zap.Error(err))
			writeWatchError(w, http.StatusInternalServerError, "could not resolve experiment")
			return
		}
		if exp == nil {
			writeWatchError(w, http.StatusNotFound, "no such experiment "+filter.ExperimentID)
			return
		}
		filter.PlatformExperimentID = exp.PlatformExperimentID
	}

	sub := h.hub.subscribe(filter, since)
	defer h.hub.unsubscribe(sub)

	replayed, err := h.source.Replay(r.Context(), filter, since)
	if err != nil {
		h.logger.Error("watch: replay", zap.Error(err))
		writeWatchError(w, http.StatusInternalServerError, "replay failed")
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

	// delivered is the per-kind floor: for each kind this connection carries, the cursor up to
	// which it has been served. It replaces a single stream-wide cursor because a filter change
	// makes one number a lie — a kind added later has been served up to a different point than a
	// kind carried since the handshake, and one floor for both either repeats or skips.
	//
	// It is stack-local to this connection and vanishes with it, like the subscription itself.
	delivered := map[string]int64{}
	for kind := range filter.Kinds {
		delivered[kind] = since
	}
	for _, e := range replayed {
		if err := deliver(conn, filter.Annotate(e), delivered); err != nil {
			return
		}
	}

	ping := time.NewTicker(watchPingInterval)
	defer ping.Stop()
	for {
		select {
		case e := <-sub.events:
			// Replay and the live stream overlap by construction — the subscription was open
			// while the replay ran. The floor for that kind is what removes the overlap.
			if err := deliver(conn, filter.Annotate(e), delivered); err != nil {
				return
			}
		case payload := <-controls:
			if err := h.applyControl(r.Context(), conn, sub, payload, delivered); err != nil {
				return
			}
		case <-ping.C:
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

func writeEvent(conn *wsConn, e db.Event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return conn.WriteText(payload, watchWriteTimeout)
}

// deliver writes one event unless this connection has already been served that kind up to that
// cursor, and records how far it has now been served. A kind with no floor cannot reach here — the
// filter only admits kinds the connection carries, and every one of those is given a floor when it
// is added.
func deliver(conn *wsConn, e db.Event, delivered map[string]int64) error {
	if floor, carried := delivered[e.Kind]; carried && e.Cursor <= floor {
		return nil
	}
	if err := writeEvent(conn, e); err != nil {
		return err
	}
	if e.Cursor > delivered[e.Kind] {
		delivered[e.Kind] = e.Cursor
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
	Subscription *WatchSubscription `json:"subscription,omitempty"`
	Error        string             `json:"error,omitempty"`
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
// overlap between that replay and what the widened filter has been buffering is removed by the
// same per-kind floor that removes it at handshake time.
//
// The honest limits, and they are the ones already true of this stream: a connection opened with
// no cursor (`since=0`) claimed no history, so widening it starts the added kinds live; and
// metric.point has no rows behind it, so widening onto it can only ever start live.
func (h *WatchHandler) applyControl(ctx context.Context, conn *wsConn, sub *subscriber, payload []byte, delivered map[string]int64) error {
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
			floor, carried := delivered[kind]
			if !carried {
				floor = sub.since
			}
			if added[floor] == nil {
				added[floor] = map[string]bool{}
			}
			added[floor][kind] = true
			delivered[kind] = floor
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
				return writeFrame(conn, watchFrame{Error: "could not replay the kinds you added; reconnect with since=<your last cursor>"})
			}
			for _, e := range events {
				if err := deliver(conn, replayFilter.Annotate(e), delivered); err != nil {
					return err
				}
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
// It is built from db.EventKinds, which is also what GET /watch/kinds serves, so the set the
// server accepts and the set it advertises are one set. A second list written by hand here would
// be wrong the first time a kind was added, and wrong in the worst direction: a caller told a kind
// exists, then refused when it asks for it.
var knownKinds = func() map[string]bool {
	kinds := make(map[string]bool, len(db.EventKinds))
	for _, k := range db.EventKinds {
		kinds[k.Kind] = true
	}
	return kinds
}()

// RegisterWatchHuma documents the change stream in the one place an agent already looks. The
// socket itself cannot be a Huma operation — an upgrade needs the raw connection, and Huma owns
// the response of everything registered with it — so its vocabulary is served as an ordinary GET
// beside it. That makes it discoverable from /explore, which an agent loads into its own prompt at
// startup, instead of folklore an agent could only learn by being told.
//
// The body is db.EventKinds itself, the same slice parseWatchQuery validates against, so this
// cannot describe a stream the server does not serve.
func RegisterWatchHuma(doc *apidocs.Doc, watch *WatchHandler) {
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-watch-kinds", Method: "GET", Path: "/watch/kinds",
		Summary: "List the event kinds GET /watch can carry", Tags: []string{"watch"},
		Description: "GET /watch is a WebSocket that pushes small typed records — kind, subject, value, detail, cursor — " +
			"so you can wait on a change instead of polling for it. Every event is a pointer, never a copy: follow one " +
			"with the ordinary GET when you want detail.\n\n" +
			"Scope a subscription with `platform_experiment_id` or `experiment_id` (one is required), narrow it with " +
			"`agent` and a comma-separated `kinds`, and resume it with `since=<cursor of the last event you saw>`. " +
			"An `agent` filter narrows only the kinds marked agent_owned below; shared kinds still reach you whoever " +
			"wrote them. An unknown kind is refused rather than ignored.\n\n" +
			"Omit `kinds` and you get every kind marked `default` below — everything an agent's loop would otherwise " +
			"poll for. Only `metric.point` is left out of it: it is the highest-volume kind, it fires for every sample " +
			"of every job in the run, and it is the one kind a reconnect cannot replay. Name it in `kinds` when you " +
			"are watching one job's progress. A `kinds` you write out means exactly what it says and nothing else.\n\n" +
			"A connected client can inspect and re-scope itself without reconnecting: send a text frame `{}` to be " +
			"told the filter actually in force, or `{\"kinds\": \"a,b\"}` to change it (`{\"kinds\": \"\"}` restores the " +
			"default). The answer is a `{\"subscription\": {...}}` frame — distinguishable from an event, which always " +
			"has a `kind`. Widening replays the kinds you added from where you last saw them, so a change costs you " +
			"nothing you were entitled to; narrowing simply stops delivery. Nothing else may be sent up the socket.\n\n" +
			"Hold the socket with `hl-watch`, which exits on a condition or a timeout — a tool call cannot keep one open.",
	}, func(context.Context, *struct{}) (*struct{ Body []db.EventKindDoc }, error) {
		return &struct{ Body []db.EventKindDoc }{Body: db.EventKinds}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-watch-subscriptions", Method: "GET", Path: "/watch/subscriptions",
		Summary: "List the /watch connections open right now", Tags: []string{"watch"},
		Description: "What every /watch socket open on this process is subscribed to: its scope, the kinds in force, " +
			"and the cursor it opened at. Narrow it with `platform_experiment_id` and `agent`.\n\n" +
			"This is a read of the live connections themselves and of nothing else. There is no stored subscription " +
			"here and there cannot be one: a subscription that outlived its socket would have nowhere to deliver to, " +
			"and `since=<cursor>` already makes a reconnect lossless. So an entry appears when a socket opens and is " +
			"gone the moment it closes — if your watcher died, it is not listed, and that is the answer.",
	}, func(_ context.Context, in *listWatchSubscriptionsInput) (*struct{ Body []WatchSubscription }, error) {
		return &struct{ Body []WatchSubscription }{Body: watch.hub.list(in.PlatformExperimentID, in.AgentID)}, nil
	})
}

type listWatchSubscriptionsInput struct {
	PlatformExperimentID string `query:"platform_experiment_id" doc:"only connections scoped to this platform experiment"`
	AgentID              string `query:"agent" doc:"only connections narrowed to this agent"`
}

func parseWatchQuery(r *http.Request) (db.EventFilter, int64, error) {
	q := r.URL.Query()
	filter := db.EventFilter{
		PlatformExperimentID: q.Get("platform_experiment_id"),
		ExperimentID:         q.Get("experiment_id"),
		AgentID:              q.Get("agent"),
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
		if err != nil {
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
		return db.DefaultKinds(), nil
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
