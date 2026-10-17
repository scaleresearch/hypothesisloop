package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost/github"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/imagedigest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/gitproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/modelproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/environments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// control-service serves the entire agent- and UI-facing API from one listener: quota,
// scheduler and registry operations share one router, one /openapi.json and one /explore, so a
// caller needs a single base URL and never has to know which package implements a path.
func main() {
	// Every time the API answers with is RFC3339 in UTC. A time read
	// from the database or taken from the clock is in the process's zone, which on a host that
	// is not in UTC put an offset on every answer and turned the zero time into another date.
	time.Local = time.UTC
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg, err := hypothesisloopcfg.LoadFromEnv()
	if err != nil {
		logger.Fatal("control-service: settings", zap.Error(err))
	}
	apiPort := strconv.Itoa(pcfg.Services.APIPort)

	metricsDBURL := pcfg.Services.MetricsDBURL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.ConfigOf(pcfg.Database, db.ServiceMaxConns))
	cancel()
	if err != nil {
		logger.Fatal("control-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	// control-service is the sole owner of the schema: metrics-service reads the same tables and
	// never migrates them.
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), db.MigrationTimeouts.Statement)
	migrated, err := db.Migrate(migrateCtx, pool)
	migrateCancel()
	if err != nil {
		logger.Fatal("control-service: migrate database", zap.Error(err))
	}
	if migrated.Adopted {
		logger.Info("adopted a database from before migrations at the baseline migration, without running it")
	}
	for _, name := range migrated.Applied {
		logger.Info("applied database migration", zap.String("migration", name))
	}
	if len(migrated.Unknown) > 0 {
		logger.Warn("the database has migrations of a later release, which this one runs against until it is replaced",
			zap.Strings("migrations", migrated.Unknown))
	}
	seedBuiltins(pool, logger)

	metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = metricsdb.EnsureObservationTables(metricsCtx, metricsDBURL)
	metricsCancel()
	if err != nil {
		logger.Fatal("control-service: metrics store tables", zap.Error(err))
	}

	graphCtx, graphCancel := context.WithTimeout(context.Background(), 30*time.Second)
	graph, err := openGraph(graphCtx, pcfg.Services.GraphDBURL)
	graphCancel()
	if err != nil {
		logger.Fatal("control-service: graph store", zap.Error(err))
	}
	defer graph.Close(context.Background()) //nolint:errcheck

	store := db.NewStore(pool, pcfg.Scheduler.MaxInfrastructureRequeues)

	domain.SetAcceleratorRates(pcfg.RateByName)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:                pcfg.Quota.Top3BonusFraction,
		BurstFraction:                    pcfg.Quota.BurstFraction,
		DefaultStages:                    pcfg.Stages.Default,
		DefaultReportIntervalSeconds:     pcfg.Scheduler.DefaultReportIntervalSeconds,
		DefaultQuotaTier:                 domain.QuotaTier(pcfg.Quota.DefaultQuotaTier),
		MaxRunLength:                     time.Duration(pcfg.Quota.MaxPlatformExperimentDays) * 24 * time.Hour,
		BaselineLimit:                    time.Duration(pcfg.Planning.BaselineLimitMinutes) * time.Minute,
		MinBaseline:                      time.Duration(pcfg.Planning.MinBaselineMinutes) * time.Minute,
		AgentsDue:                        time.Duration((domain.MaxAgentInstanceRestarts+1)*pcfg.Agents.BootLimitSeconds) * time.Second,
		MaxSubmissionsPerHour:            pcfg.Quota.MaxSubmissionsPerHour,
		MaxAcceleratorCountPerJob:        pcfg.Quota.MaxAcceleratorCountPerJob,
		MaxCPUCoresPerJob:                pcfg.Quota.MaxCPUCoresPerJob,
		MaxRAMGBPerJob:                   pcfg.Quota.MaxRAMGBPerJob,
		MaxStorageGBPerJob:               pcfg.Quota.MaxStorageGBPerJob,
		DefaultMaxConcurrentAccelerators: pcfg.Quota.DefaultMaxConcurrentAccelerators,
	}

	peFullStore := db.NewPlatformExperimentsFullStore(store)

	// One root context for every background loop, cancelled before the HTTP server drains.
	// They used to run on context.Background(), so SIGTERM tore the process down mid-admission
	// pass while the connection pool was closing underneath them.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	authenticator, err := auth.NewAuthenticator(pcfg.Auth, store)
	if err != nil {
		logger.Fatal("control-service: configure auth", zap.Error(err))
	}
	gate := apiGate{tokens: authenticator, access: auth.NewAccess(store)}

	// The git host is one of two. On a plain one there is no GitHub App and no client for it:
	// connecting GitHub answers that there is none.
	accountsStore := db.NewAccountsStore(pool)
	var githubApp accounts.GitHubApp
	var gitHost githost.Provider
	switch pcfg.GitHost.Provider {
	case hypothesisloopcfg.GitHostProviderGitHub:
		client, err := github.New(*pcfg.GitHost.GitHub)
		if err != nil {
			logger.Fatal("control-service: configure the GitHub App", zap.Error(err))
		}
		githubApp, gitHost = client, github.NewHost(accountsStore, client, logger)
	case hypothesisloopcfg.GitHostProviderPlain:
		plain := pcfg.GitHost.Plain
		gitHost, err = githost.NewPlain(plain.BaseURL, plain.Username, plain.Password)
		if err != nil {
			logger.Fatal("control-service: configure the git host", zap.Error(err))
		}
	default:
		logger.Fatal("control-service: the settings name no git host", zap.String("provider", pcfg.GitHost.Provider))
	}
	repos := accounts.NewRepositories(accountsStore, githubApp, gate.access, logger)

	apiServer := newAPIServer(runCtx, gate, repos, gitHost, pool, store, graph, peFullStore, quotaCfg, pcfg, metricsDBURL, apiPort, logger)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Info("api listening", zap.String("addr", apiServer.Addr))
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("control-service: api listen", zap.Error(err))
		}
	}()

	<-sigCh
	logger.Info("control-service: shutting down")
	runCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("control-service: api shutdown", zap.Error(err))
	}
	logger.Info("control-service: stopped")
}

type apiGate struct {
	tokens auth.Tokens
	access *auth.Access
}

func newAPIServer(runCtx context.Context, gate apiGate, repos *accounts.Repositories, gitHost githost.Provider, pool *db.Pool, store *db.Store, graph *graphdb.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// One client for the durable-data store, shared by everything that reads it: the listing
	// endpoint, the usage report, admission's ceiling, and the addressing handed to runtimes.
	dataStore, err := objectstore.New(pcfg.DataStore.Provider, pcfg.DataStore.ControlPlaneEndpoint, pcfg.DataStore.Endpoint, pcfg.DataStore.Region, pcfg.DataStore.Bucket,
		pcfg.DataStore.AccessKeyID, pcfg.DataStore.SecretAccessKey)
	if err != nil {
		logger.Fatal("control-service: configure durable data store", zap.Error(err))
	}
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second
	svc := platformexperiments.NewService(store, logger)
	handler := platformexperiments.NewHandler(svc, logger)

	// One observation cadence for the whole process: the quota service, the settler, the
	// controller and the scheduler loop must all measure "how long did this run" the same way,
	// or the same job's cost depends on which of them is asked.
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	// An environment is keyed on the digests of the images it is built from, asked of their
	// registry at most once a minute per tag.
	environmentsSvc := environments.NewService(store.EnvironmentsStore, gitHost).WithJobImages(pcfg.OfferedJobImages(), imagedigest.New(pcfg.PlainHTTPRegistries()...))

	peSvc := platformexperiments.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL, observedGapCap).WithDataStore(dataStore, pcfg.DataStore.MaxBytesPerAgent).WithRepositories(gitHost).
		WithEnvironments(environmentsSvc).WithModelDefaults(modelDefaults(pcfg)).WithIdleNotice(peFullStore, time.Duration(pcfg.Agents.IdleNoticeMinutes)*time.Minute).WithPersonIdeas(graph)
	peHandler := platformexperiments.NewPlatformExperimentsHandler(peSvc, gitHost, platformexperiments.Planning{
		Limit:       time.Duration(pcfg.Planning.LimitMinutes) * time.Minute,
		ProposedFor: time.Duration(pcfg.Planning.ProposedForDays) * 24 * time.Hour,
		PlannerKept: time.Duration(pcfg.Planning.PlannerKeptMinutes) * time.Minute,
		AutoAccept:  time.Duration(pcfg.Planning.AutoAcceptSeconds) * time.Second,
	}, logger).
		WithJobImages(pcfg.OfferedJobImages()).
		WithGitProxy(pcfg.GitHost.ProxyURL).
		WithLiveCapacity(metricsDBURL, connectedWithin, store, domain.NewPodResources(pcfg.PodResourcesByName)).
		WithAgentLaunch(platformexperiments.AgentLaunch{
			Hosting: pcfg.Sandbox != nil, DefaultModel: pcfg.Agents.DefaultModel, CoordinatorModel: pcfg.Agents.CoordinatorModel,
			SigningSecret: pcfg.Auth.SigningSecret, MaxHosted: pcfg.Agents.MaxHostedPerPlatformExperiment,
		})

	schedulerHandler, clusterAgentHandler := newSchedulerParts(runCtx, gitHost, environmentsSvc, pool, store, graph, peSvc, peHandler, quotaCfg, pcfg, metricsDBURL, dataStore, logger)
	// The change stream: one LISTEN connection for this process, fanned out to whatever /watch
	// connections are open. Nothing is persisted for it and nothing is held between connections
	// — see service/registry/watch.go.
	events := db.NewEventsStore(pool, graph)
	registrySvc := registry.New(store, graph, events, logger, metricsDBURL, dataStore)
	watchHandler := registry.NewWatchHandler(events, store, gate.access, logger)
	watchHandler.Start(runCtx)
	connectedClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterHeartbeats(ctx, metricsDBURL, connectedWithin)
	}
	autoscaledClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterAutoscalerCapability(ctx, metricsDBURL, connectedWithin)
	}
	clusterContacts := func(ctx context.Context) (map[string]accounts.Contact, error) {
		seen, err := metricsdb.ClusterLastContacts(ctx, metricsDBURL, clusterContactLookback)
		if err != nil {
			return nil, err
		}
		out := make(map[string]accounts.Contact, len(seen))
		for name, contact := range seen {
			out[name] = accounts.Contact{At: contact.At, AgentVersion: contact.AgentVersion}
		}
		return out, nil
	}

	modelProxy, err := modelproxy.New(db.NewPlatformExperimentsStore(pool), modelproxy.MetricsDB{URL: metricsDBURL},
		modelproxy.AnthropicUpstream, pcfg.Models.Anthropic.OAuthToken, logger)
	if err != nil {
		logger.Fatal("control-service: configure the model proxy", zap.Error(err))
	}

	r := newRouter(gate, pcfg.Auth.UIOrigin, routes{
		quota:               handler,
		platformExperiments: peHandler,
		hypotheses:          graph,
		sharing:             db.NewPlatformExperimentsStore(pool),
		instructions:        platformexperiments.NewInstructionsService(db.NewPlatformExperimentsStore(pool), modelDefaults(pcfg), logger),
		agentTypes:          accounts.NewAgentTypesHandler(db.NewAgentTypesStore(pool), logger),
		dataUsage:           platformexperiments.NewDataUsageHandler(dataStore, store, peSvc, pcfg.DataStore.MaxBytesPerAgent, logger),
		sessions:            platformexperiments.NewSessions(dataStore),
		drafts:              db.NewExperimentDraftsStore(pool),
		accounts: accounts.NewHandler(db.NewAccountsStore(pool), store, connectedClusters, autoscaledClusters, clusterContacts, pcfg.Auth.SigningSecret, accounts.RuntimeAddresses{APIURL: pcfg.Services.PublicAPIURL, MetricsURL: pcfg.Runtimes.MetricsURL, Registry: pcfg.Runtimes.Registry, ImageTag: pcfg.Runtimes.ImageTag, RegistryPlainHTTP: pcfg.Runtimes.RegistryPlainHTTP},
			accounts.Limits{MaxOrgsPerUser: pcfg.Accounts.MaxOrgsPerUser, MaxProjectsPerOrg: pcfg.Accounts.MaxProjectsPerOrg}, logger),
		repositories: repos,
		scheduler:    schedulerHandler,
		registry:     registry.NewHandler(registrySvc, logger),
		watch:        watchHandler,
		clusterAgent: clusterAgentHandler,
		gitProxy: gitproxy.New(struct {
			*db.PlatformExperimentsStore
			*db.EnvironmentsStore
		}{db.NewPlatformExperimentsStore(pool), store.EnvironmentsStore}, gitHost, logger),
		modelProxy: modelProxy,
		modelUsage: peSvc,
	}, logger)

	return &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// routes is everything the API serves.
type routes struct {
	quota               *platformexperiments.Handler
	platformExperiments *platformexperiments.PlatformExperimentsHandler
	hypotheses          platformexperiments.Hypotheses
	sharing             platformexperiments.Sharing
	dataUsage           *platformexperiments.DataUsageHandler
	sessions            *platformexperiments.Sessions
	instructions        *platformexperiments.InstructionsService
	drafts              platformexperiments.ExperimentDraftStore
	accounts            *accounts.Handler
	agentTypes          *accounts.AgentTypesHandler
	repositories        *accounts.Repositories
	scheduler           *scheduler.Handler
	registry            *registry.Handler
	watch               *registry.WatchHandler
	clusterAgent        *clusterexchange.Handler
	gitProxy            *gitproxy.Proxy
	modelProxy          *modelproxy.Proxy
	modelUsage          *platformexperiments.PlatformExperimentsService
}

// webSockets are the paths a WebSocket client opens, with the subprotocol that carries its token.
// clusterContactLookback is how far back a cluster's last contact is looked for: one silent for
// longer is listed with none.
const clusterContactLookback = 30 * 24 * time.Hour

var webSockets = map[string]string{"/watch": registry.WatchSubprotocol}

// newRouter mounts every route of the API behind auth.Gate, which every request but a CORS
// preflight passes before any handler runs.
func newRouter(gate apiGate, uiOrigin string, h routes, logger *zap.Logger) chi.Router {
	r := chi.NewRouter()
	r.Use(api.RecoveryMiddleware(logger))
	r.Use(api.LoggingMiddleware(logger))
	r.Use(api.CORSMiddleware(uiOrigin))
	r.Use(auth.Gate(gate.tokens, logger, webSockets))
	r.Handle("/metrics", auth.RequireStaff(logger, promhttp.Handler()))
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
	// `hl watch` is its client, which the experimentator harness runs for an agent between turns.
	r.Get("/watch", h.watch.ServeHTTP)
	// git's smart HTTP is no JSON API, so it is mounted beside the documented operations.
	r.Handle(auth.GitPrefix+"*", h.gitProxy)
	// The model calls of hosted agents are the provider's own API, passed on with the platform's
	// credential, and no operation of this one.
	r.Handle(modelproxy.Prefix+"*", h.modelProxy)

	// Cluster-agent traffic (Go cluster-agent binaries, not research agents) keeps its own
	// registration and its own docs under a distinct prefix — a different audience entirely.
	clusterAgentRouter := chi.NewRouter()
	clusterAgentDoc := apidocs.New(clusterAgentRouter, "hypothesisloop cluster-agent API", "1.0.0", "", gate.access, logger)
	clusterexchange.RegisterHuma(clusterAgentDoc, h.clusterAgent)
	clusterAgentDoc.MountExplore(clusterAgentRouter)
	r.Mount("/internal/clusters", clusterAgentRouter)

	// One doc for the whole public API: every operation an agent or the UI can call is
	// registered here, so /openapi.json and /explore describe all of it in one place.
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", agenttypes.PlatformRules, gate.access, logger)
	platformexperiments.RegisterHuma(doc, h.quota, h.platformExperiments)
	platformexperiments.RegisterAgentsHuma(doc, h.platformExperiments, h.hypotheses)
	platformexperiments.RegisterPlanningHuma(doc, h.platformExperiments)
	platformexperiments.RegisterShareHuma(doc, h.sharing, h.platformExperiments.Answer, logger)
	platformexperiments.RegisterDataUsageHuma(doc, h.dataUsage)
	httpapi.RegisterSessions(doc, h.sessions, logger)
	httpapi.RegisterInstructions(doc, h.instructions)
	platformexperiments.RegisterExperimentDraftsHuma(doc, h.drafts, logger)
	accounts.RegisterHuma(doc, h.accounts)
	accounts.RegisterAgentTypesHuma(doc, h.agentTypes)
	accounts.RegisterRepositoriesHuma(doc, h.repositories)
	scheduler.RegisterHuma(doc, h.scheduler)
	httpapi.RegisterModelUsage(doc, h.modelUsage, logger)
	registry.RegisterHuma(doc, h.registry)
	// /watch itself cannot be registered here (it is an upgrade), but its vocabulary can be, so
	// an agent reading /explore learns what the stream carries rather than having to be told.
	registry.RegisterWatchHuma(doc)
	// /explore is the agent-only digest: nothing an agent isn't permitted to call.
	// /explore/coordinator is the same registrations' operator/dashboard-admin-only view, and
	// /explore/rules the rules alone, which the instructions the platform serves an agent carry.
	doc.MountExploreAudience(r, "/explore", apidocs.AudienceAgent)
	doc.MountExploreAudience(r, "/explore/coordinator", apidocs.AudienceCoordinator)
	doc.MountRules(r, "/explore/rules")
	return r
}

// newSchedulerParts builds the scheduler's handler and starts its background loops, returning
// the handlers newRouter serves: the scheduler's on the shared API doc, the cluster-agents' under
// its own prefix.
func newSchedulerParts(runCtx context.Context, gitHost githost.Provider, environmentsSvc *environments.Service, pool *db.Pool, store *db.Store, graph *graphdb.Store, expQuotaSvc *platformexperiments.PlatformExperimentsService, peHandler *platformexperiments.PlatformExperimentsHandler, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL string, dataStore *objectstore.Client, logger *zap.Logger) (*scheduler.Handler, *clusterexchange.Handler) {
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	clusterReports, err := scheduler.NewClusterReports(store, metricsDBURL,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second)
	if err != nil {
		logger.Fatal("control-service: configure cluster reports", zap.Error(err))
	}

	// settler is the sole path that durably writes a terminal experiment's final observed usage
	// (see service/settlement) — used inline by JobWatcher and by the reconciler below to
	// retry anything a crash or metrics-DB outage left unsettled.
	settler := settlement.New(expQuotaSvc, metricsDBURL, observedGapCap)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)

	watcher := scheduler.NewJobWatcher(store, clusterReports, settler, logger).
		WithPollInterval(time.Duration(pcfg.Scheduler.JobPollIntervalSeconds) * time.Second).
		WithStuckPendingTimeout(time.Duration(pcfg.Scheduler.StuckPendingTimeoutSeconds) * time.Second).
		WithStartTimeout(time.Duration(pcfg.Scheduler.StartTimeoutSeconds) * time.Second).
		WithScaleUpTimeout(time.Duration(pcfg.Scheduler.ScaleUpTimeoutSeconds) * time.Second)

	grace := domain.GraceLimits{
		DefaultShutdownSeconds: int64(pcfg.Scheduler.DefaultTerminationGracePeriodSeconds),
		MaxShutdownSeconds:     int64(pcfg.Scheduler.MaxTerminationGracePeriodSeconds),
		MaxCheckpointSeconds:   int64(pcfg.Scheduler.MaxCheckpointGraceSeconds),
	}
	schedulerSvc := scheduler.NewService(store, expQuotaSvc, graph, gitHost, clusterReports, settler, metricsDBURL, logger).
		WithQuotaConfig(quotaCfg).
		WithJobImages(pcfg.OfferedJobImages()).
		WithEnvironments(environmentsSvc).
		WithDataStore(dataStore, pcfg.DataStore.MaxBytesPerAgent)

	schedulerLoop := scheduler.NewLoop(store, expQuotaSvc, clusterReports, grace, logger).
		WithPodResources(domain.NewPodResources(pcfg.PodResourcesByName)).
		WithReprioritizer(schedulerSvc).
		WithHeartbeat(time.Duration(pcfg.Scheduler.LoopHeartbeatSeconds)*time.Second).
		WithGuaranteedFairnessWindow(time.Duration(pcfg.Scheduler.GuaranteedFairnessWindowSeconds)*time.Second).
		WithHeldJobNotice(time.Duration(pcfg.Scheduler.HeldJobNoticeSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap).
		WithDisbalanceEvictor(schedulerSvc, pcfg.Scheduler.ResourceDisbalanceTolerance).
		WithQuotaSettler(settler).
		WithSpeculation(time.Duration(pcfg.Scheduler.TriedClusterTTLSeconds) * time.Second)
	schedulerSvc = schedulerSvc.WithLoop(schedulerLoop).WithBuildAssigner(schedulerLoop)
	environmentRegistry := newEnvironmentRegistry(runCtx, pcfg, gitHost, logger)
	if environmentRegistry != nil {
		schedulerSvc = schedulerSvc.WithRegistryPulls(environmentRegistry.Logins)
	}
	watcher.WithCapacityFreed(schedulerLoop.Trigger)

	// Admission (read-decide-write, not a CAS) and JobWatcher's per-experiment pollers must run
	// on exactly one replica. db.RunAsLeader holds a Postgres advisory lock to pick that
	// replica; others stand by and take over if the leader dies. Only these loops are
	// leader-gated — the HTTP API stays multi-replica.
	// The settlement reconciler and the platform-experiment expiry sweep join the leader-gated
	// loops rather than running per replica. Both are idempotent — Settle is an absolute set,
	// Close() logs invalid_transition when it loses a race — so concurrent copies are not
	// incorrect, just N redundant sweeps racing over the same row. One owner.
	// Cluster-agent-facing surface (Go cluster-agent binaries, not the research agent) gets its
	// own Huma registration with its own openapi/explore docs, mounted under its own prefix.
	clusterAgentHandler := clusterexchange.NewHandler(store,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, pcfg.Scheduler.ObservationCadence(), dataStore,
		time.Duration(pcfg.Scheduler.MaxCheckpointGraceSeconds)*time.Second,
		pcfg.AcceleratorTypeNames(), pcfg.Scheduler.MaxLogTailLineChars, pcfg.Auth.SigningSecret, pcfg.GitHost.ProxyURL, environmentRegistry, logger).
		WithBuildReported(schedulerLoop.Trigger)

	// The sandbox launcher and the cloud compute clusters rent machines, so one replica runs them.
	rented, err := newRented(pcfg, pool, store, clusterAgentHandler, metricsDBURL, logger)
	if err != nil {
		logger.Fatal("control-service: rented machines", zap.Error(err))
	}

	pass := pcfg.Scheduler.PassTimeout()
	go db.RunAsLeader(runCtx, pool, db.SchedulerLockKey, 5*time.Second, logger, append([]func(context.Context){
		func(ctx context.Context) { settlementReconciler.Start(ctx, pass) },
		// Auto-close platform experiments past their ends_at deadline.
		func(ctx context.Context) { expQuotaSvc.StartExpirySweep(ctx, 60*time.Second, pass) },
		// A run closes within seconds of its ends_at.
		func(ctx context.Context) { expQuotaSvc.StartEndSweep(ctx, 5*time.Second, pass) },
		// A draft's create wakes this pass on the leader; the interval covers one created on
		// another replica and one left by a restart.
		func(ctx context.Context) { expQuotaSvc.StartRepositoryPass(ctx, 5*time.Second, pass) },
		// A proposal is accepted by itself planning.auto_accept_seconds after it is made.
		func(ctx context.Context) { peHandler.StartAutoAccept(ctx, 2*time.Second, pass) },
		func(ctx context.Context) { schedulerLoop.Start(ctx, pass) },
		func(ctx context.Context) { watcher.Start(ctx, pass) },
	}, rented...)...)

	return scheduler.NewHandler(schedulerSvc), clusterAgentHandler
}

// newEnvironmentRegistry is where assigned environment builds push to and the logins to it, and
// the git host their files' repositories are read from, nil on a deployment whose settings have no
// environments section.
func newEnvironmentRegistry(ctx context.Context, pcfg *hypothesisloopcfg.Config, gitHost githost.Provider, logger *zap.Logger) *clusterexchange.EnvironmentRegistry {
	e := pcfg.Environments
	if e == nil {
		return nil
	}
	logins, err := registrycred.New(ctx, *e)
	if err != nil {
		logger.Fatal("control-service: the registry logins of environment builds", zap.Error(err))
	}
	return &clusterexchange.EnvironmentRegistry{Repository: e.Repository, Logins: logins, Git: gitHost}
}

// openGraph connects to the hypothesis graph and makes sure its schema exists.
func openGraph(ctx context.Context, url string) (*graphdb.Store, error) {
	graph, err := graphdb.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := graph.EnsureSchema(ctx); err != nil {
		graph.Close(ctx) //nolint:errcheck
		return nil, err
	}
	return graph, nil
}

// modelDefaults is the platform's model for an agent and for a coordinator that names none.
func modelDefaults(pcfg *hypothesisloopcfg.Config) domain.ModelDefaults {
	return domain.ModelDefaults{Agent: pcfg.Agents.DefaultModel, Coordinator: pcfg.Agents.CoordinatorModel}
}

// seedBuiltins writes the built-in agent types and the platform's rules this build holds, under
// its release (agenttypes.Release), right after the schema and under its lock.
func seedBuiltins(pool *db.Pool, logger *zap.Logger) {
	builtins, err := agenttypes.Builtins()
	if err != nil {
		logger.Fatal("control-service: built-in agent types", zap.Error(err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seeded, err := db.SeedBuiltins(ctx, pool, agenttypes.Release, builtins, agenttypes.PlatformRules)
	if err != nil {
		logger.Fatal("control-service: seed built-in agent types and rules", zap.Error(err))
	}
	for _, s := range seeded.Types {
		logger.Info("built-in agent type has a new version", zap.String("agent_type", s.ID), zap.Int("version", s.Propagation.Version),
			zap.Int("agents_told", s.Propagation.Told))
	}
	if seeded.RulesVersion > 0 {
		logger.Info("the platform's rules have a new version", zap.Int("version", seeded.RulesVersion), zap.Int("agents_told", seeded.RulesTold))
	}
	if len(seeded.Skipped) > 0 {
		logger.Warn("a later release seeded these, which this one leaves as they are", zap.Int("release", agenttypes.Release), zap.Strings("seeded", seeded.Skipped))
	}
}
