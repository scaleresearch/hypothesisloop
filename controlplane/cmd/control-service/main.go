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

	"github.com/scaleresearch/hypothesisloop/controlplane/services/clusteragentapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/dedup"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/leaderelection"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/queuebackend"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

type noopAgentProvisioner struct{}

func (noopAgentProvisioner) ProvisionAgent(context.Context, string) error { return nil }

// control-service serves the entire agent- and UI-facing API from one listener: quota,
// scheduler and registry operations share one router, one /openapi.json and one /explore, so a
// caller needs a single base URL and never has to know which package implements a path.
func main() {
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
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn})
	cancel()
	if err != nil {
		logger.Fatal("control-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	store := db.NewStore(pool, metricsDBURL)

	domain.SetAcceleratorRates(pcfg.RateByName)
	domain.SetCPUCoreHourRate(pcfg.CPUCoreHourRate)
	domain.SetRAMGBHourRate(pcfg.RAMGBHourRate)
	domain.SetStorageGBHourRate(pcfg.StorageGBHourRate)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:         pcfg.Quota.Top3BonusFraction,
		BurstFraction:             pcfg.Quota.BurstFraction,
		DefaultStages:             pcfg.Stages.Default,
		MaxSubmissionsPerHour:     pcfg.Quota.MaxSubmissionsPerHour,
		MaxAcceleratorCountPerJob: pcfg.Quota.MaxAcceleratorCountPerJob,
		MaxCPUCoresPerJob:         pcfg.Quota.MaxCPUCoresPerJob,
		MaxRAMGBPerJob:            pcfg.Quota.MaxRAMGBPerJob,
		MaxStorageGBPerJob:        pcfg.Quota.MaxStorageGBPerJob,
	}

	peFullStore := db.NewPlatformExperimentsFullStore(store)

	// One root context for every background loop, cancelled before the HTTP server drains.
	// They used to run on context.Background(), so SIGTERM tore the process down mid-admission
	// pass while the connection pool was closing underneath them.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	apiServer := newAPIServer(runCtx, pool, store, peFullStore, quotaCfg, pcfg, metricsDBURL, apiPort, logger)

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

func newAPIServer(runCtx context.Context, pool *db.Pool, store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// Native Kubernetes Jobs need no per-agent object; keeps agent registration
	// independent from the scheduler backend.
	provisioner := quota.AgentProvisioner(noopAgentProvisioner{})
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second
	svc := quota.NewService(store, provisioner, logger)
	handler := quota.NewHandler(svc, logger)

	// One observation cadence for the whole process: the quota service, the settler, the
	// controller and the scheduler loop must all measure "how long did this run" the same way,
	// or the same job's cost depends on which of them is asked.
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL, observedGapCap)
	acceleratorTypeInfos := make([]quota.AcceleratorTypeInfo, 0, len(pcfg.AcceleratorTypes))
	for _, g := range pcfg.AcceleratorTypes {
		acceleratorTypeInfos = append(acceleratorTypeInfos, quota.AcceleratorTypeInfo{Name: g.Name, AccHRate: g.AccHRate})
	}
	resourceCatalog := quota.ResourceCatalog{
		AcceleratorTypes:  acceleratorTypeInfos,
		CPUCoreHourRate:   domain.CPUCoreHourRate(),
		RAMGBHourRate:     domain.RAMGBHourRate(),
		StorageGBHourRate: domain.StorageGBHourRate(),
	}
	peHandler := quota.NewPlatformExperimentsHandler(peSvc, logger).
		WithCatalog(resourceCatalog).
		WithLiveCapacity(metricsDBURL, connectedWithin)

	schedulerHandler, clusterAgentRouter := newSchedulerParts(runCtx, pool, store, peSvc, quotaCfg, pcfg, metricsDBURL, logger)
	registryHandler := registry.NewHandler(registry.New(store, logger, metricsDBURL), logger)

	r := chi.NewRouter()
	r.Use(api.RecoveryMiddleware(logger))
	r.Use(api.LoggingMiddleware(logger))
	r.Use(api.CORSMiddleware)
	r.Handle("/metrics", promhttp.Handler())
	for _, path := range []string{"/health", "/healthz"} {
		r.Get(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"status":"ok"}`)
		})
	}
	// Cluster-agent traffic (Go cluster-agent binaries, not research agents) keeps its own
	// registration and its own docs under a distinct prefix — a different audience entirely.
	r.Mount("/internal/clusters", clusterAgentRouter)

	// One doc for the whole public API: every operation an agent or the UI can call is
	// registered here, so /openapi.json and /explore describe all of it in one place.
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", apidocs.PlatformRules)
	quota.RegisterHuma(doc, handler, peHandler)
	scheduler.RegisterHuma(doc, schedulerHandler)
	registry.RegisterHuma(doc, registryHandler)
	// /explore is what a research agent fetches into its own system prompt at startup — it must
	// contain nothing an agent isn't permitted to call, so it's the agent-only digest.
	// /explore/coordinator is the same registrations' operator/dashboard-admin-only view.
	doc.MountExploreAudience(r, "/explore", apidocs.AudienceAgent)
	doc.MountExploreAudience(r, "/explore/coordinator", apidocs.AudienceCoordinator)

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
func newSchedulerParts(runCtx context.Context, pool *db.Pool, store *db.Store, expQuotaSvc *quota.PlatformExperimentsService, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL string, logger *zap.Logger) (*scheduler.Handler, chi.Router) {
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
		WithObservedTimeConfig(metricsDBURL, observedGapCap)

	noveltyDetector := dedup.New()
	schedulerSvc := scheduler.NewService(store, expQuotaSvc, jwc, noveltyDetector, store, settler, metricsDBURL, logger).
		WithQuotaConfig(quotaCfg)

	schedulerLoop := scheduler.NewLoop(store, expQuotaSvc, jwc, logger).
		WithReprioritizer(schedulerSvc).
		WithHeartbeat(time.Duration(pcfg.Scheduler.LoopHeartbeatSeconds)*time.Second).
		WithGuaranteedFairnessWindow(time.Duration(pcfg.Scheduler.GuaranteedFairnessWindowSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap).
		WithDisbalanceEvictor(schedulerSvc, pcfg.Scheduler.ResourceDisbalanceTolerance).
		WithQuotaSettler(settler)
	schedulerSvc = schedulerSvc.WithLoop(schedulerLoop)

	// Admission (read-decide-write, not a CAS) and JobWatcher's per-experiment pollers must run
	// on exactly one replica. leaderelection.Run holds a Postgres advisory lock to pick that
	// replica; others stand by and take over if the leader dies. Only these loops are
	// leader-gated — the HTTP API stays multi-replica.
	// The settlement reconciler and the platform-experiment expiry sweep join the leader-gated
	// loops rather than running per replica. Both are idempotent — Settle is an absolute set,
	// Close() logs invalid_transition when it loses a race — so concurrent copies are not
	// incorrect, just N redundant sweeps racing over the same row. One owner.
	go leaderelection.Run(runCtx, pool.Raw(), leaderelection.SchedulerLockKey,
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
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, logger)
	clusterAgentRouter := chi.NewRouter()
	caDoc := apidocs.New(clusterAgentRouter, "hypothesisloop cluster-agent API", "1.0.0", "")
	clusteragentapi.RegisterHuma(caDoc, clusterAgentHandler)
	caDoc.MountExplore(clusterAgentRouter)

	return schedulerHandler, clusterAgentRouter
}
func requiredEnv(key string, logger *zap.Logger) string {
	value := os.Getenv(key)
	if value == "" {
		logger.Fatal(key + " environment variable is required")
	}
	return value
}
