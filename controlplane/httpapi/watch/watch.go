package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// Changes is the change stream as the store holds it (db.Store): the changes of a platform
// experiment, or of the platform experiments a project's listing holds, after an id, the newest
// id, and a wake-up on every append.
type Changes interface {
	After(ctx context.Context, platformExperimentID string, after int64, limit int) ([]domain.Event, int64, bool, error)
	ProjectAfter(ctx context.Context, scope domain.TenantScope, withShared bool, after int64, limit int) ([]domain.Event, int64, bool, error)
	Head(ctx context.Context) (int64, error)
	Listen(ctx context.Context, woke func()) error
}

// Hypotheses answers, per hypothesis change, whether it concerns a hypothesis an agent registered
// (graphdb.Store.ConcernsAgent).
type Hypotheses interface {
	ConcernsAgent(ctx context.Context, agentID string, events []domain.Event) ([]bool, error)
}

const (
	// pageSize is how many changes one read takes.
	pageSize = 500
	// watchPingInterval keeps an idle stream honest: a job can sit RUNNING for an hour with
	// nothing to say, and without this neither end would notice the path between them died. Each
	// tick writes the last progress frame again: a browser sees no ping.
	watchPingInterval = 20 * time.Second
	watchWriteTimeout = 10 * time.Second
	// listenRetryDelay is how long the one LISTEN waits before it is established again after its
	// connection broke. Every stream reads again once it is back.
	listenRetryDelay = 2 * time.Second
)

// hub wakes every open stream when a change is appended. It holds wake-ups only: what a stream
// has read is its own cursor, and what there is to read is the store's.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

func (h *hub) subscribe() chan struct{} {
	woke := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[woke] = struct{}{}
	h.mu.Unlock()
	return woke
}

func (h *hub) unsubscribe(woke chan struct{}) {
	h.mu.Lock()
	delete(h.subs, woke)
	h.mu.Unlock()
}

func (h *hub) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for woke := range h.subs {
		select {
		case woke <- struct{}{}:
		default:
		}
	}
}

func describe(filter domain.EventFilter, after int64) platformapi.Subscription {
	kinds := make([]string, 0, len(filter.Kinds))
	for kind := range filter.Kinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return platformapi.Subscription{
		ProjectID:            filter.ProjectID,
		PlatformExperimentID: filter.PlatformExperimentID,
		ExperimentID:         filter.ExperimentID,
		AgentID:              filter.AgentID,
		Kinds:                kinds,
		Decisions:            filter.Decisions,
		After:                after,
	}
}

// Page is one read of the changes a subscription admits: next is where to read on from, and
// caughtUp says nothing more was held when it was read.
type Page struct {
	Changes  []domain.Event `json:"changes"`
	Next     int64          `json:"next"`
	CaughtUp bool           `json:"caught_up"`
}

// Handler serves GET /watch and GET /changes. They are registered on the chi router directly
// rather than through the httpapi document: a WebSocket upgrade needs the raw connection, and
// Huma owns the response for every operation registered with it. Their vocabulary is GET
// /watch/kinds (registered by httpapi) and hl watch --help.
type Handler struct {
	changes    Changes
	hypotheses Hypotheses
	// experiments resolves the platform experiment a job belongs to, so a caller that knows
	// only its own job id can subscribe without looking it up itself.
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

// NewHandler returns a handler over changes. Start must be called once to begin listening. Its
// requests must pass auth.Gate first, which reads a stream's token from the subprotocol
// (Subprotocol) before the upgrade.
func NewHandler(changes Changes, hypotheses Hypotheses, experiments experimentLookup, access *auth.Access, logger *zap.Logger) *Handler {
	return &Handler{changes: changes, hypotheses: hypotheses, experiments: experiments, access: access, hub: newHub(), logger: logger, recheckEvery: watchPingInterval}
}

// Start holds the one LISTEN of this process until ctx ends, establishing it again after
// listenRetryDelay when it breaks. Every open stream is woken once it is back, so a change
// appended while nothing listened is read then.
func (h *Handler) Start(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			err := h.changes.Listen(ctx, h.hub.wake)
			if ctx.Err() != nil {
				return
			}
			h.logger.Warn("watch: change stream ended, listening again", zap.Error(err))
			select {
			case <-ctx.Done():
			case <-time.After(listenRetryDelay):
			}
		}
	}()
}

func (h *Handler) admit(ctx context.Context, principal auth.Principal) (context.Context, error) {
	if !auth.AudienceAgent.Admits(principal, "") {
		return nil, auth.Forbidden("forbidden: a " + string(principal.Kind) + " token may not watch")
	}
	// A stream only reads. The platform experiment it is narrowed to is admitted by the access
	// check on its own, so none is named for a listing here.
	return auth.WithRead(auth.WithPrincipal(ctx, principal), ""), nil
}

// stillAdmitted asks again what the upgrade asked: a stream outlives the request that opened
// it, and the token may have died or lost the project or the platform experiment since.
func (h *Handler) stillAdmitted(ctx context.Context, filter domain.EventFilter) error {
	principal, err := auth.Recheck(ctx)
	if err != nil {
		return err
	}
	ctx, err = h.admit(ctx, principal)
	if err != nil {
		return err
	}
	if filter.ProjectID != "" {
		return h.access.Project(ctx, filter.ProjectID)
	}
	return h.access.PlatformExperiment(ctx, filter.PlatformExperimentID)
}

// subscription is a request's filter and where it reads on from, admitted for its caller. A
// project's carries the platform experiments its listing holds for the caller, as scope and
// withShared.
type subscription struct {
	filter     domain.EventFilter
	after      int64
	scope      domain.TenantScope
	withShared bool
}

// open admits the caller of r and resolves what it subscribes to, answering the refusal itself.
// ok is false when it did.
func (h *Handler) open(w http.ResponseWriter, r *http.Request) (context.Context, subscription, bool) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: reached without passing auth.Gate", errors.New("no principal")))
		return nil, subscription{}, false
	}
	ctx, err := h.admit(r.Context(), principal)
	if err != nil {
		auth.WriteError(w, h.logger, err)
		return nil, subscription{}, false
	}
	filter, after, err := parseWatchQuery(r)
	if err != nil {
		writeWatchError(w, http.StatusBadRequest, err.Error())
		return nil, subscription{}, false
	}
	if principal.Kind == auth.KindAgent {
		filter.AgentID = principal.AgentID
	}
	if filter.ProjectID != "" {
		scope, withShared, err := h.project(ctx, principal, filter.ProjectID)
		if err != nil {
			auth.WriteError(w, h.logger, err)
			return nil, subscription{}, false
		}
		return h.fromHead(ctx, w, subscription{filter: filter, after: after, scope: scope, withShared: withShared})
	}
	if filter.ExperimentID != "" {
		if err := h.access.Experiment(ctx, filter.ExperimentID); err != nil {
			auth.WriteError(w, h.logger, err)
			return nil, subscription{}, false
		}
	}
	if filter.PlatformExperimentID == "" {
		exp, err := h.experiments.GetExperiment(ctx, filter.ExperimentID)
		if err != nil {
			auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: resolve experiment", err, zap.String("id", filter.ExperimentID)))
			return nil, subscription{}, false
		}
		if exp == nil {
			writeWatchError(w, http.StatusNotFound, "no such experiment "+filter.ExperimentID)
			return nil, subscription{}, false
		}
		filter.PlatformExperimentID = exp.PlatformExperimentID
	}
	if err := h.access.PlatformExperiment(ctx, filter.PlatformExperimentID); err != nil {
		auth.WriteError(w, h.logger, err)
		return nil, subscription{}, false
	}
	return h.fromHead(ctx, w, subscription{filter: filter, after: after})
}

// project admits an account to the stream of a project, the platform experiments its listing
// holds for the caller (Access.PlatformExperimentListScope), and answers which those are.
func (h *Handler) project(ctx context.Context, principal auth.Principal, projectID string) (domain.TenantScope, bool, error) {
	if !principal.HoldsAccount() {
		return domain.TenantScope{}, false, auth.Forbidden("forbidden: a " + string(principal.Kind) + " token watches only its own platform experiment")
	}
	if err := h.access.Project(ctx, projectID); err != nil {
		return domain.TenantScope{}, false, err
	}
	return h.access.PlatformExperimentListScope(ctx, projectID)
}

// fromHead resolves a subscription's after=head to the newest change id, answering a failure
// itself.
func (h *Handler) fromHead(ctx context.Context, w http.ResponseWriter, sub subscription) (context.Context, subscription, bool) {
	if sub.after == head {
		var err error
		if sub.after, err = h.changes.Head(ctx); err != nil {
			auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: head", err))
			return nil, subscription{}, false
		}
	}
	return ctx, sub, true
}

// read reads one page of the changes after after that sub admits. A read and a stream go
// through it alike, for a backlog and for what is appended live.
func (h *Handler) read(ctx context.Context, sub subscription, after int64, limit int) (Page, error) {
	filter := sub.filter
	changes, next, caughtUp, err := h.scoped(ctx, sub, after, limit)
	if err != nil {
		return Page{}, err
	}
	kept := make([]domain.Event, 0, len(changes))
	for _, c := range changes {
		if filter.Matches(c) {
			kept = append(kept, c)
		}
	}
	kept, err = h.concerning(ctx, filter, kept)
	if err != nil {
		return Page{}, err
	}
	return Page{Changes: kept, Next: next, CaughtUp: caughtUp}, nil
}

// scoped reads the changes of sub's project or platform experiment after after, for Matches to
// filter.
func (h *Handler) scoped(ctx context.Context, sub subscription, after int64, limit int) ([]domain.Event, int64, bool, error) {
	if sub.filter.ProjectID != "" {
		return h.changes.ProjectAfter(ctx, sub.scope, sub.withShared, after, limit)
	}
	return h.changes.After(ctx, sub.filter.PlatformExperimentID, after, limit)
}

// concerning completes Matches for an agent filter: a hypothesis change stays only when it
// concerns a hypothesis that agent registered. The graph is read once for all of them.
func (h *Handler) concerning(ctx context.Context, filter domain.EventFilter, changes []domain.Event) ([]domain.Event, error) {
	if filter.AgentID == "" {
		return changes, nil
	}
	var asked []domain.Event
	for _, c := range changes {
		if domain.IsHypothesisActivity(c.Kind) {
			asked = append(asked, c)
		}
	}
	if len(asked) == 0 {
		return changes, nil
	}
	concerns, err := h.hypotheses.ConcernsAgent(ctx, filter.AgentID, asked)
	if err != nil {
		return nil, err
	}
	kept := changes[:0]
	next := 0
	for _, c := range changes {
		if domain.IsHypothesisActivity(c.Kind) {
			next++
			if !concerns[next-1] {
				continue
			}
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// writeReadError answers a read refused for its after with 410, and any other failure as the
// platform's.
func (h *Handler) writeReadError(w http.ResponseWriter, after int64, err error) {
	switch {
	case errors.Is(err, db.ErrChangeUnknown):
		writeWatchError(w, http.StatusGone, fmt.Sprintf("after %d is not a change id this platform issued", after))
	case errors.Is(err, db.ErrChangesPruned):
		writeWatchError(w, http.StatusGone, fmt.Sprintf("changes after %d are no longer held; re-read state and watch from head", after))
	default:
		auth.WriteError(w, h.logger, auth.Internal(h.logger, "watch: read changes", err))
	}
}

// ServeChanges answers GET /changes: one page of what GET /watch would stream from after.
func (h *Handler) ServeChanges(w http.ResponseWriter, r *http.Request) {
	ctx, sub, ok := h.open(w, r)
	if !ok {
		return
	}
	limit := pageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > pageSize {
			writeWatchError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a whole number from 1 to %d", pageSize))
			return
		}
		limit = parsed
	}
	page, err := h.read(ctx, sub, sub.after, limit)
	if err != nil {
		h.writeReadError(w, sub.after, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// ServeHTTP upgrades to a WebSocket and streams the changes the subscription admits after its
// after: first its subscription, then each change, each read ending with a progress frame that
// says where it stands. Backlog and live are one loop: read a page, write it, and when caught up
// wait to be woken by an append.
//
// Nothing may be sent up this socket. The control plane decides, and every decision it makes is
// written as desired state; anything a job or an agent could tell it down this socket would be a
// second path to an effect that already has one.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, sub, ok := h.open(w, r)
	if !ok {
		return
	}
	woke := h.hub.subscribe()
	defer h.hub.unsubscribe(woke)

	page, err := h.read(ctx, sub, sub.after, pageSize)
	if err != nil {
		h.writeReadError(w, sub.after, err)
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

	subscription := describe(sub.filter, sub.after)
	if err := writeJSON(conn, platformapi.Frame{Subscription: &subscription}); err != nil {
		return
	}
	ping := time.NewTicker(h.recheckEvery)
	defer ping.Stop()
	for {
		for _, c := range page.Changes {
			if err := writeJSON(conn, c); err != nil {
				return
			}
		}
		stand := platformapi.Progress{Next: page.Next, CaughtUp: page.CaughtUp}
		if err := writeJSON(conn, stand); err != nil {
			return
		}
		if page.CaughtUp && !h.wait(ctx, conn, sub.filter, stand, woke, ping.C, peerGone) {
			return
		}
		if page, err = h.read(ctx, sub, stand.Next, pageSize); err != nil {
			h.logger.Info("watch: closing a stream that could not read on", zap.Int64("after", stand.Next), zap.Error(err))
			return
		}
	}
}

// wait holds a caught-up stream until a change is appended, writing its progress frame stand
// again and asking its token again meanwhile, and says whether to read on.
func (h *Handler) wait(ctx context.Context, conn *wsConn, filter domain.EventFilter, stand platformapi.Progress, woke <-chan struct{}, ping <-chan time.Time, peerGone <-chan struct{}) bool {
	for {
		select {
		case <-woke:
			return true
		case <-ping:
			if err := h.stillAdmitted(ctx, filter); err != nil {
				var refusal *auth.Error
				if errors.As(err, &refusal) {
					h.logger.Info("watch: closing a stream whose token is no longer admitted",
						zap.String("project_id", filter.ProjectID), zap.String("platform_experiment_id", filter.PlatformExperimentID), zap.String("reason", refusal.Message))
					return false
				}
				// The platform's own failure says nothing about the caller: ask again next tick.
				h.logger.Warn("watch: could not re-check a stream's token", zap.Error(err))
			}
			if err := writeJSON(conn, stand); err != nil {
				return false
			}
		case <-peerGone:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func writeJSON(conn *wsConn, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteText(payload, watchWriteTimeout)
}

// knownKinds is the closed set a subscription may name, built from domain.EventKinds, which is
// also what GET /watch/kinds serves. An unrecognised kind is refused rather than ignored: a
// client filtering on a kind that does not exist would wait forever on a stream that looks
// healthy.
var knownKinds = func() map[string]bool {
	kinds := make(map[string]bool, len(domain.EventKinds))
	for _, k := range domain.EventKinds {
		kinds[k.Kind] = true
	}
	return kinds
}()

// head is the after of a subscription that reads on from the newest change.
const head = -1

func parseWatchQuery(r *http.Request) (domain.EventFilter, int64, error) {
	q := r.URL.Query()
	filter := domain.EventFilter{
		ProjectID:            q.Get("project_id"),
		PlatformExperimentID: q.Get("platform_experiment_id"),
		ExperimentID:         q.Get("experiment_id"),
		AgentID:              q.Get("agent_id"),
	}
	if q.Has("agent") {
		return filter, 0, errWatch("agent is no parameter of a watch: the agent is agent_id")
	}
	if q.Has("since") {
		return filter, 0, errWatch("since is no parameter of a watch: read on with after=<id of the last change you read>, or after=head")
	}
	if raw := q.Get("decisions"); raw != "" {
		decisions, err := strconv.ParseBool(raw)
		if err != nil {
			return filter, 0, errWatch("decisions must be true or false")
		}
		filter.Decisions = decisions
	}
	if filter.ProjectID != "" && (filter.PlatformExperimentID != "" || filter.ExperimentID != "") {
		return filter, 0, errWatch("project_id is not combined with platform_experiment_id or experiment_id")
	}
	if filter.ProjectID == "" && filter.PlatformExperimentID == "" && filter.ExperimentID == "" {
		return filter, 0, errWatch("one of project_id, platform_experiment_id or experiment_id is required")
	}
	kinds, err := parseKinds(q.Get("kinds"))
	if err != nil {
		return filter, 0, err
	}
	filter.Kinds = kinds
	var after int64
	switch raw := q.Get("after"); raw {
	case "":
	case "head":
		after = head
	default:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return filter, 0, errWatch("after must be the id of a change, or head")
		}
		after = parsed
	}
	return filter, after, nil
}

// parseKinds turns a comma-separated list into the set a filter admits. An empty list is the
// default subscription, and a list that names kinds means exactly those and nothing more.
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
