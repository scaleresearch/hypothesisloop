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

	"github.com/scaleresearch/hypothesisloop/controlplane/services/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/clusteragentapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/dedup"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/launcherapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/leaderelection"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/queuebackend"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// control-service serves the entire agent- and UI-facing API from one listener: quota,
// scheduler and registry operations share one router, one /openapi.json and one /explore, so a
// caller needs a single base URL and never has to know which package implements a path.
func main() {
	// Every time the API answers with is RFC3339 in UTC (docs/api-conventions.md). A time read
	// from the database or taken from the clock is in the process's zone, which on a host that
	// is not in UTC put an offset on every answer and turned the zero time into another date.
	time.Local = time.UTC
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg := hypothesisloopcfg.MustLoad(requiredEnv("HYPOTHESISLOOP_CONFIG", logger))
	apiPort := strconv.Itoa(pcfg.Services.APIPort)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Fatal("DATABASE_URL environment variable is required")
	}

	metricsDBURL := pcfg.Services.MetricsDBURL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, MaxConns: db.ServiceMaxConns})
	cancel()
	if err != nil {
		logger.Fatal("control-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	// control-service is the sole owner of the schema (schema.sql is baked into this image only
	// — see controlplane/build/Dockerfile.control-service — not metrics-service's, which needs
	// Postgres to READ the same tables but never applies DDL to them). Every statement in it is
	// idempotent, so this is safe on every boot, not just the first; a Postgres advisory lock
	// inside ApplySchema serializes it against a second control-service replica doing the same.
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.ApplySchema(migrateCtx, pool)
	migrateCancel()
	if err != nil {
		logger.Fatal("control-service: apply schema", zap.Error(err))
	}

	metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = metricsdb.EnsureObservationTables(metricsCtx, metricsDBURL)
	metricsCancel()
	if err != nil {
		logger.Fatal("control-service: metrics store tables", zap.Error(err))
	}

	graphCtx, graphCancel := context.WithTimeout(context.Background(), 30*time.Second)
	graph, err := openGraph(graphCtx, pcfg.Services.GraphDBURL, pool)
	graphCancel()
	if err != nil {
		logger.Fatal("control-service: graph store", zap.Error(err))
	}
	defer graph.Close(context.Background()) //nolint:errcheck

	store := db.NewStore(pool, metricsDBURL, pcfg.Scheduler.MaxInfrastructureRequeues)

	domain.SetAcceleratorRates(pcfg.RateByName)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:                pcfg.Quota.Top3BonusFraction,
		BurstFraction:                    pcfg.Quota.BurstFraction,
		DefaultStages:                    pcfg.Stages.Default,
		DefaultReportIntervalSeconds:     pcfg.Scheduler.DefaultReportIntervalSeconds,
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

	directory := auth.NewStoreDirectory(pool)
	authenticator, err := auth.NewAuthenticator(pcfg.Auth, directory)
	if err != nil {
		logger.Fatal("control-service: configure auth", zap.Error(err))
	}
	gate := apiGate{authenticator: authenticator, access: auth.NewAccess(directory)}

	// A deployment with no GitHub App runs with no client for it: connecting GitHub and minting a
	// git credential answer that there is none.
	var githubApp accounts.GitHubApp
	if pcfg.GitHub.Configured() {
		client, err := github.New(pcfg.GitHub)
		if err != nil {
			logger.Fatal("control-service: configure the GitHub App", zap.Error(err))
		}
		githubApp = client
	}
	repos := accounts.NewRepositories(db.NewAccountsStore(pool), db.NewPlatformExperimentsStore(pool), githubApp, gate.access, logger)

	apiServer := newAPIServer(runCtx, gate, repos, pool, store, graph, peFullStore, quotaCfg, pcfg, metricsDBURL, apiPort, logger)

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
	authenticator *auth.Authenticator
	access        *auth.Access
}

func newAPIServer(runCtx context.Context, gate apiGate, repos *accounts.Repositories, pool *db.Pool, store *db.Store, graph *graphdb.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// One client for the durable-data store, shared by everything that reads it: the listing
	// endpoint, the usage report, admission's ceiling, and the addressing handed to runtimes.
	dataStore, err := objectstore.New(pcfg.DataStore.Endpoint, pcfg.DataStore.Region, pcfg.DataStore.Bucket,
		pcfg.DataStore.AccessKeyID, pcfg.DataStore.SecretAccessKey)
	if err != nil {
		logger.Fatal("control-service: configure durable data store", zap.Error(err))
	}
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second
	svc := quota.NewService(store, logger)
	handler := quota.NewHandler(svc, logger)

	// One observation cadence for the whole process: the quota service, the settler, the
	// controller and the scheduler loop must all measure "how long did this run" the same way,
	// or the same job's cost depends on which of them is asked.
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL, observedGapCap)
	acceleratorTypeInfos := make([]quota.AcceleratorTypeInfo, 0, len(pcfg.AcceleratorTypes))
	for _, g := range pcfg.AcceleratorTypes {
		acceleratorTypeInfos = append(acceleratorTypeInfos, quota.AcceleratorTypeInfo{Name: g.Name, Kind: domain.AcceleratorType(g.Name).Kind(), AccHRate: g.AccHRate})
	}
	resourceCatalog := quota.ResourceCatalog{
		AcceleratorTypes: acceleratorTypeInfos,
	}
	peHandler := quota.NewPlatformExperimentsHandler(peSvc, repos, logger).
		WithCatalog(resourceCatalog).
		WithLiveCapacity(metricsDBURL, connectedWithin, store).
		WithAgentLaunch(quota.AgentLaunch{
			Launcher: pcfg.Agents.Launcher, DefaultModel: pcfg.Agents.DefaultModel,
			SigningSecret: pcfg.Auth.SigningSecret, MaxHosted: pcfg.Agents.MaxHostedPerPlatformExperiment,
		}).
		WithPlanning(quota.Planning{
			AllowanceAcceleratorHours: pcfg.Planning.AllowanceAcceleratorHours,
			Limit:                     time.Duration(pcfg.Planning.LimitMinutes) * time.Minute,
			ProposedFor:               time.Duration(pcfg.Planning.ProposedForDays) * 24 * time.Hour,
			MaxConcurrentPerOrg:       pcfg.Planning.MaxConcurrentPerOrg,
			MaxPerDayPerOrg:           pcfg.Planning.MaxPerDayPerOrg,
		})

	schedulerHandler, clusterAgentRouter := newSchedulerParts(runCtx, gate, pool, store, graph, peSvc, quotaCfg, pcfg, metricsDBURL, dataStore, logger)
	// The change stream: one LISTEN connection for this process, fanned out to whatever /watch
	// connections are open. Nothing is persisted for it and nothing is held between connections
	// — see services/registry/watch.go.
	events := db.NewEventsStore(pool, graph)
	registrySvc := registry.New(store, graph, logger, metricsDBURL, dataStore).WithEvents(events)
	registryHandler := registry.NewHandler(registrySvc, logger)
	watchHandler := registry.NewWatchHandler(events, store, gate.authenticator, gate.access, logger)
	watchHandler.Start(runCtx)
	dataUsageHandler := quota.NewDataUsageHandler(dataStore, store, peSvc, pcfg.DataStore.MaxBytesPerAgent)

	r := chi.NewRouter()
	r.Use(api.RecoveryMiddleware(logger))
	r.Use(api.LoggingMiddleware(logger))
	r.Use(api.CORSMiddleware(pcfg.Auth.UIOrigin))
	r.Handle("/metrics", gate.authenticator.RequireStaff(logger, promhttp.Handler()))
	for _, path := range []string{"/health", "/healthz"} {
		r.Get(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"status":"ok"}`)
		})
	}
	// /watch is a WebSocket upgrade, so it is mounted on the router rather than registered on
	// the Huma doc below: Huma owns the response for every operation registered with it, and an
	// upgrade needs the raw connection. Agents learn it from `hl watch`, documented in the
	// experimentator's system prompt.
	r.Get("/watch", watchHandler.ServeHTTP)

	// Cluster-agent traffic (Go cluster-agent binaries, not research agents) keeps its own
	// registration and its own docs under a distinct prefix — a different audience entirely.
	r.Mount("/internal/clusters", clusterAgentRouter)

	// Agent-launchers ask here which agent instances should exist. A third audience with its own
	// registration and docs, like the cluster-agents above.
	launcherRouter := chi.NewRouter()
	launcherDoc := apidocs.New(launcherRouter, "hypothesisloop agent-launcher API", "1.0.0", "", gate.authenticator, gate.access, logger)
	launcherapi.RegisterHuma(launcherDoc, launcherapi.NewHandler(store, launcherapi.Settings{
		Image: pcfg.Agents.Image, APIURL: pcfg.Agents.APIURL, SigningSecret: pcfg.Auth.SigningSecret,
		BootLimit:       time.Duration(pcfg.Agents.BootLimitSeconds) * time.Second,
		ProgressLimit:   time.Duration(pcfg.Agents.ProgressLimitSeconds) * time.Second,
		MaxNewPerAnswer: pcfg.Agents.MaxNewInstancesPerAnswer,
	}, logger))
	launcherDoc.MountExplore(launcherRouter)
	r.Mount("/internal/launchers", launcherRouter)

	// One doc for the whole public API: every operation an agent or the UI can call is
	// registered here, so /openapi.json and /explore describe all of it in one place.
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", apidocs.PlatformRules, gate.authenticator, gate.access, logger)
	quota.RegisterHuma(doc, handler, peHandler)
	quota.RegisterAgentsHuma(doc, peHandler)
	quota.RegisterPlanningHuma(doc, peHandler)
	quota.RegisterShareHuma(doc, db.NewPlatformExperimentsStore(pool), logger)
	quota.RegisterDataUsageHuma(doc, dataUsageHandler)
	quota.RegisterSessionsHuma(doc, dataStore, logger)
	quota.RegisterCodeHuma(doc, dataStore, peSvc, logger)
	connectedClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterHeartbeats(ctx, metricsDBURL, connectedWithin)
	}
	autoscaledClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterAutoscalerCapability(ctx, metricsDBURL, connectedWithin)
	}
	accounts.RegisterHuma(doc, accounts.NewHandler(db.NewAccountsStore(pool), store, connectedClusters, pcfg.Auth.SigningSecret, accounts.RuntimeAddresses{APIURL: pcfg.Runtimes.APIURL, MetricsURL: pcfg.Runtimes.MetricsURL, Registry: pcfg.Runtimes.Registry, ImageTag: pcfg.Runtimes.ImageTag},
		accounts.Limits{MaxOrgsPerUser: pcfg.Accounts.MaxOrgsPerUser, MaxProjectsPerOrg: pcfg.Accounts.MaxProjectsPerOrg}, logger).WithAutoscalers(autoscaledClusters))
	accounts.RegisterRepositoriesHuma(doc, repos)
	scheduler.RegisterHuma(doc, schedulerHandler)
	registry.RegisterHuma(doc, registryHandler)
	// /watch itself cannot be registered here (it is an upgrade), but its vocabulary can be, so
	// an agent reading /explore learns what the stream carries rather than having to be told.
	registry.RegisterWatchHuma(doc, watchHandler)
	// /explore is the agent-only digest: nothing an agent isn't permitted to call.
	// /explore/coordinator is the same registrations' operator/dashboard-admin-only view, and
	// /explore/rules the rules alone, which a research agent's system prompt carries.
	doc.MountExploreAudience(r, "/explore", apidocs.AudienceAgent)
	doc.MountExploreAudience(r, "/explore/coordinator", apidocs.AudienceCoordinator)
	doc.MountRules(r, "/explore/rules")

	return &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// newSchedulerParts builds the scheduler's handler and starts its background loops, returning
// the pieces newAPIServer mounts: the handler goes on the shared API doc, the cluster-agent
// router under its own prefix.
func newSchedulerParts(runCtx context.Context, gate apiGate, pool *db.Pool, store *db.Store, graph *graphdb.Store, expQuotaSvc *quota.PlatformExperimentsService, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL string, dataStore *objectstore.Client, logger *zap.Logger) (*scheduler.Handler, chi.Router) {
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	// jwc is workload.Backend; swap this line to plug in a different scheduling mechanism
	// (see workload/backend.go). queuebackend.Backend only reads/writes Postgres.
	queueBackend, err := queuebackend.New(store, metricsDBURL,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second)
	if err != nil {
		logger.Fatal("control-service: configure scheduler backend", zap.Error(err))
	}
	var jwc workload.Backend = queueBackend

	// settler is the sole path that durably writes a terminal experiment's final observed usage
	// (see services/settlement) — used inline by JobWatcher and by the reconciler below to
	// retry anything a crash or metrics-DB outage left unsettled.
	settler := settlement.New(expQuotaSvc, metricsDBURL, observedGapCap)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)

	watcher := scheduler.NewJobWatcher(store, jwc, settler, logger).
		WithPollInterval(time.Duration(pcfg.Scheduler.JobPollIntervalSeconds)*time.Second).
		WithStuckPendingTimeout(time.Duration(pcfg.Scheduler.StuckPendingTimeoutSeconds)*time.Second).
		WithStartTimeout(time.Duration(pcfg.Scheduler.StartTimeoutSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap).
		WithScaleUpTimeout(time.Duration(pcfg.Scheduler.ScaleUpTimeoutSeconds) * time.Second)

	grace := domain.GraceLimits{
		DefaultShutdownSeconds: int64(pcfg.Scheduler.DefaultTerminationGracePeriodSeconds),
		MaxShutdownSeconds:     int64(pcfg.Scheduler.MaxTerminationGracePeriodSeconds),
		MaxCheckpointSeconds:   int64(pcfg.Scheduler.MaxCheckpointGraceSeconds),
	}
	noveltyDetector := dedup.New()
	schedulerSvc := scheduler.NewService(store, expQuotaSvc, jwc, noveltyDetector, graph, settler, grace, metricsDBURL, logger).
		WithQuotaConfig(quotaCfg).
		WithDataStore(dataStore, pcfg.DataStore.MaxBytesPerAgent)

	schedulerLoop := scheduler.NewLoop(store, expQuotaSvc, jwc, grace, logger).
		WithReprioritizer(schedulerSvc).
		WithHeartbeat(time.Duration(pcfg.Scheduler.LoopHeartbeatSeconds)*time.Second).
		WithGuaranteedFairnessWindow(time.Duration(pcfg.Scheduler.GuaranteedFairnessWindowSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap).
		WithDisbalanceEvictor(schedulerSvc, pcfg.Scheduler.ResourceDisbalanceTolerance).
		WithQuotaSettler(settler).
		WithSpeculation(time.Duration(pcfg.Scheduler.TriedClusterTTLSeconds) * time.Second)
	schedulerSvc = schedulerSvc.WithLoop(schedulerLoop)

	// Admission (read-decide-write, not a CAS) and JobWatcher's per-experiment pollers must run
	// on exactly one replica. leaderelection.Run holds a Postgres advisory lock to pick that
	// replica; others stand by and take over if the leader dies. Only these loops are
	// leader-gated — the HTTP API stays multi-replica.
	// The settlement reconciler and the platform-experiment expiry sweep join the leader-gated
	// loops rather than running per replica. Both are idempotent — Settle is an absolute set,
	// Close() logs invalid_transition when it loses a race — so concurrent copies are not
	// incorrect, just N redundant sweeps racing over the same row. One owner.
	go leaderelection.Run(runCtx, pool, leaderelection.SchedulerLockKey,
		5*time.Second, logger, func(leaderCtx context.Context) {
			go settlementReconciler.Start(leaderCtx)
			// Auto-close platform experiments past their ends_at deadline.
			go expQuotaSvc.StartExpirySweep(leaderCtx, 60*time.Second)
			schedulerLoop.Start(leaderCtx)
			watcher.Start(leaderCtx)
		})

	schedulerHandler := scheduler.NewHandler(schedulerSvc)

	// Cluster-agent-facing surface (Go cluster-agent binaries, not the research agent) gets its
	// own Huma registration with its own openapi/explore docs, mounted under its own prefix.
	clusterAgentHandler := clusteragentapi.NewHandler(store,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, dataStore,
		time.Duration(pcfg.Scheduler.MaxCheckpointGraceSeconds)*time.Second,
		pcfg.AcceleratorTypeNames(), pcfg.Scheduler.MaxLogTailLineChars, pcfg.Auth.SigningSecret, logger)
	clusterAgentRouter := chi.NewRouter()
	caDoc := apidocs.New(clusterAgentRouter, "hypothesisloop cluster-agent API", "1.0.0", "", gate.authenticator, gate.access, logger)
	clusteragentapi.RegisterHuma(caDoc, clusterAgentHandler)
	caDoc.MountExplore(clusterAgentRouter)

	return schedulerHandler, clusterAgentRouter
}

// openGraph connects to the hypothesis graph, makes sure its schema exists, moves any hypotheses
// still in the retired Postgres table into it (see db.MoveLegacyHypotheses), and gives the
// records of signed-in people written before actors were kept their account (see
// db.MoveHumanWrites).
func openGraph(ctx context.Context, url string, pool *db.Pool) (*graphdb.Store, error) {
	graph, err := graphdb.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := graph.EnsureSchema(ctx); err != nil {
		graph.Close(ctx) //nolint:errcheck
		return nil, err
	}
	err = db.MoveLegacyHypotheses(ctx, pool, func(rows []*domain.Hypothesis) error {
		return graph.ImportLegacy(ctx, rows)
	})
	if err != nil {
		graph.Close(ctx) //nolint:errcheck
		return nil, fmt.Errorf("move hypotheses from postgres to the graph: %w", err)
	}
	err = db.MoveHumanWrites(ctx, pool, func(writes []db.HumanWrite) error {
		return graph.CreditUsers(ctx, writes)
	})
	if err != nil {
		graph.Close(ctx) //nolint:errcheck
		return nil, fmt.Errorf("give earlier records the accounts behind them: %w", err)
	}
	return graph, nil
}

func requiredEnv(key string, logger *zap.Logger) string {
	value := os.Getenv(key)
	if value == "" {
		logger.Fatal(key + " environment variable is required")
	}
	return value
}
