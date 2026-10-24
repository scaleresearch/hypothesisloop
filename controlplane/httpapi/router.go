package httpapi

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/gitproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/middleware"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/modelproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/watch"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/models"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
)

// Handlers is everything the API serves, and who asks it.
type Handlers struct {
	Tokens   auth.Tokens
	Access   *auth.Access
	UIOrigin string
	Logger   *zap.Logger

	Agents              *accounts.Agents
	PlatformExperiments *platformexperiments.Service
	Hypotheses          platformexperiments.Hypotheses
	Launch              platformexperiments.AgentLaunch
	Sharing             *platformexperiments.Sharing
	Sessions            *platformexperiments.Sessions
	Instructions        *platformexperiments.Composer
	Drafts              *platformexperiments.Drafts
	Accounts            *accounts.Service
	AgentTypes          *accounts.AgentTypes
	Models              *models.Service
	Repositories        *accounts.Repositories
	Scheduler           *scheduler.Service
	Registry            *registry.Service
	ClusterExchange     *clusterexchange.Service
	Watch               *watch.Handler
	GitProxy            *gitproxy.Proxy
	ModelProxy          *modelproxy.Proxy
}

// webSockets are the paths a WebSocket client opens, with the subprotocol that carries its token.
var webSockets = map[string]string{"/watch": platformapi.WatchSubprotocol}

// NewRouter mounts every route of the API behind auth.Gate, which every request but a CORS
// preflight passes before any handler runs.
func NewRouter(h Handlers) chi.Router {
	r := chi.NewRouter()
	r.NotFound(refusal.NoRoute)
	r.MethodNotAllowed(refusal.NoMethod)
	r.Use(middleware.Recovery(h.Logger))
	r.Use(middleware.Logging(h.Logger))
	r.Use(middleware.CORS(h.UIOrigin))
	r.Use(auth.Gate(h.Tokens, h.Logger, webSockets))
	for _, path := range []string{"/health", "/healthz"} {
		r.Get(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"status":"ok"}`)
		})
	}
	// /watch is a WebSocket upgrade, so it is mounted on the router rather than registered on
	// the Huma doc below: Huma owns the response for every operation registered with it, and an
	// upgrade needs the raw connection. Its vocabulary is GET /watch/kinds, registered below, and
	// `hl watch` is its client, which the agent harness runs for an agent between turns.
	r.Get("/watch", h.Watch.ServeHTTP)
	r.Get("/changes", h.Watch.ServeChanges)
	// git's smart HTTP is no JSON API, so it is mounted beside the documented operations.
	r.Handle(auth.GitPrefix+"*", h.GitProxy)
	// The model calls of sandboxed agents are the provider's own API, passed on with the platform's
	// credential, and no operation of this one.
	r.Handle(modelproxy.Prefix+"*", h.ModelProxy)

	// Cluster-agent traffic (Go cluster-agent binaries, not research agents) keeps its own
	// registration and its own docs under a distinct prefix — a different audience entirely.
	clusterAgentRouter := chi.NewRouter()
	clusterAgentDoc := NewDoc(clusterAgentRouter, "hypothesisloop cluster-agent API", "1.0.0", "", h.Access, h.Logger)
	RegisterClusterAgent(clusterAgentDoc, h.ClusterExchange)
	clusterAgentDoc.MountExplore(clusterAgentRouter)
	r.Mount(clusterapi.ClustersPath, clusterAgentRouter)

	// One doc for the whole public API: every operation an agent or the UI can call is
	// registered here, so /openapi.json and /explore describe all of it in one place.
	doc := NewDoc(r, "hypothesisloop API", "1.0.0", agenttypes.PlatformRules, h.Access, h.Logger)
	Register(doc, h)
	// /explore is the agent-only digest: nothing an agent isn't permitted to call.
	// /explore/coordinator is the same registrations' operator/dashboard-admin-only view, and
	// /explore/rules the rules alone, which the instructions the platform serves an agent carry.
	doc.MountExploreAudience(r, "/explore", auth.AudienceAgent)
	doc.MountExploreAudience(r, "/explore/coordinator", auth.AudienceCoordinator)
	doc.MountRules(r, "/explore/rules")
	return r
}

// Register registers every operation of the public API on doc, in the order the document lists
// them.
func Register(doc *Doc, h Handlers) {
	registerAgents(doc, h.Agents)
	registerPlatformExperiments(doc, h.PlatformExperiments, h.Logger)
	registerSituation(doc, h.PlatformExperiments, h.Logger)
	registerPlatformExperimentBudget(doc, h.PlatformExperiments, h.Logger)
	registerMessages(doc, h.PlatformExperiments, h.Logger)
	registerExperimentAgents(doc, h.PlatformExperiments, h.Hypotheses, h.Launch, h.Logger)
	registerPlanning(doc, h.PlatformExperiments, h.Logger)
	registerSharing(doc, h.Sharing, h.PlatformExperiments, h.Logger)
	registerExperimentData(doc, h.PlatformExperiments, h.Logger)
	registerExperimentImage(doc, h.PlatformExperiments, h.Logger)
	registerSessions(doc, h.Sessions, h.Logger)
	registerInstructions(doc, h.Instructions)
	registerExperimentDrafts(doc, h.Drafts)
	registerAccounts(doc, h.Accounts)
	registerAgentTypes(doc, h.AgentTypes)
	registerModels(doc, h.Models, h.Logger)
	registerRepositories(doc, h.Repositories)
	registerJobs(doc, h.Scheduler, h.Logger)
	registerModelUsage(doc, h.PlatformExperiments, h.Logger)
	registerRegistry(doc, h.Registry, h.Logger)
	// /watch itself cannot be registered here (it is an upgrade), but its vocabulary can be, so
	// an agent reading /explore learns what the stream carries rather than having to be told.
	registerWatchKinds(doc)
}
