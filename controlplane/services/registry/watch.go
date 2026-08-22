package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

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
type subscriber struct {
	filter db.EventFilter
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

func (h *hub) subscribe(filter db.EventFilter) *subscriber {
	sub := &subscriber{filter: filter, events: make(chan db.Event, subscriberBuffer), lagged: make(chan struct{})}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
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
// This socket is one-way by design. Frames arriving from the client are read only to notice a
// close or answer a ping, and are never interpreted. Do not add a command here: the control plane
// decides, and every decision it makes is written as desired state that the runtime fetches. A
// job or an agent that could be told something down this socket would be a second path to an
// effect that already has one, and the two would disagree the first time this connection dropped.
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

	sub := h.hub.subscribe(filter)
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

	peerGone := make(chan struct{})
	go func() {
		defer close(peerGone)
		for {
			if _, _, err := conn.ReadFrame(); err != nil {
				return
			}
		}
	}()

	lastCursor := since
	for _, e := range replayed {
		if err := writeEvent(conn, e); err != nil {
			return
		}
		lastCursor = e.Cursor
	}

	ping := time.NewTicker(watchPingInterval)
	defer ping.Stop()
	for {
		select {
		case e := <-sub.events:
			// Replay and the live stream overlap by construction — the subscription was open
			// while the replay ran. The cursor is what removes the overlap.
			if e.Cursor <= lastCursor {
				continue
			}
			if err := writeEvent(conn, e); err != nil {
				return
			}
			lastCursor = e.Cursor
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

// knownKinds is the closed set a subscription may name. An unrecognised one is rejected rather
// than ignored: a client filtering on a kind that does not exist would wait forever on a stream
// that looks healthy, which is exactly the failure /watch exists to end.
var knownKinds = map[string]bool{
	db.EventExperimentStatus:  true,
	db.EventExperimentBlocked: true,
	db.EventQuotaChanged:      true,
	db.EventStageBoundary:     true,
	db.EventHypothesisNew:     true,
	db.EventFindingNew:        true,
	db.EventCommentNew:        true,
	db.EventMetricPoint:       true,
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
	if raw := q.Get("kinds"); raw != "" {
		filter.Kinds = map[string]bool{}
		for _, kind := range strings.Split(raw, ",") {
			kind = strings.TrimSpace(kind)
			if !knownKinds[kind] {
				return filter, 0, errWatch("unknown kind " + kind)
			}
			filter.Kinds[kind] = true
		}
	}
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
