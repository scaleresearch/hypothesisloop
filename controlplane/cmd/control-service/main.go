package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/services/clusteragentapi"
	"github.com/scaleresearch/openresearch/controlplane/services/dedup"
	"github.com/scaleresearch/openresearch/controlplane/services/quota"
	"github.com/scaleresearch/openresearch/controlplane/services/scheduler"
	"github.com/scaleresearch/openresearch/controlplane/services/settlement"
	"github.com/scaleresearch/openresearch/controlplane/shared/api"
	openresearchcfg "github.com/scaleresearch/openresearch/controlplane/shared/config"
	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/leaderelection"
	"github.com/scaleresearch/openresearch/controlplane/shared/queuebackend"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// control-service hosts quota-service and scheduler-service together: they
// already share the same Postgres store and quota domain logic (scheduler
// calls straight into the quota package for refunds/adjustments), so running
// them as one process removes a duplicated build/deploy unit without
// changing either one's HTTP surface. Each keeps its own listener on its
// historical port so existing callers (UI, e2e tests, cluster-agents) don't
// need to change.
func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	quotaPort := envOrDefault("QUOTA_PORT", "8081")
	schedulerPort := envOrDefault("SCHEDULER_PORT", "8082")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Fatal("DATABASE_URL environment variable is required")
	}

	metricsDBURL := os.Getenv("GREPTIMEDB_URL")
	if metricsDBURL == "" {
		metricsDBURL = "http://greptimedb:4000"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn})
	cancel()
	if err != nil {
		logger.Fatal("control-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	store := db.NewStore(pool, metricsDBURL)

	pcfg := openresearchcfg.MustLoad(envOrDefault("OPENRESEARCH_CONFIG", "settings/openresearch.yaml"))
	domain.SetAcceleratorRates(pcfg.RateByName)
	domain.SetCPUCoreHourRate(pcfg.CPUCoreHourRate)
	domain.SetRAMGBHourRate(pcfg.RAMGBHourRate)
	domain.SetStorageGBHourRate(pcfg.StorageGBHourRate)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:     pcfg.Quota.Top3BonusFraction,
		BurstFraction:         pcfg.Quota.BurstFraction,
		Phase1ExploreFraction: pcfg.Phase2.BoundaryFraction,
		MaxSubmissionsPerHour: pcfg.Quota.MaxSubmissionsPerHour,
		MetricDeclineFraction: pcfg.Quota.MetricDeclineFraction,
		MaxAcceleratorCountPerJob:     pcfg.Quota.MaxAcceleratorCountPerJob,
		MaxCPUCoresPerJob:     pcfg.Quota.MaxCPUCoresPerJob,
		MaxRAMGBPerJob:        pcfg.Quota.MaxRAMGBPerJob,
		MaxStorageGBPerJob:    pcfg.Quota.MaxStorageGBPerJob,
	}

	peFullStore := db.NewPlatformExperimentsFullStore(store)

	quotaServer := newQuotaServer(store, peFullStore, quotaCfg, pcfg, metricsDBURL, quotaPort, logger)
	schedulerServer := newSchedulerServer(pool, store, peFullStore, quotaCfg, pcfg, metricsDBURL, schedulerPort, logger)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Info("quota listening", zap.String("addr", quotaServer.Addr))
		if err := quotaServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("control-service: quota listen", zap.Error(err))
		}
	}()
	go func() {
		logger.Info("scheduler listening", zap.String("addr", schedulerServer.Addr))
		if err := schedulerServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("control-service: scheduler listen", zap.Error(err))
		}
	}()

	<-sigCh
	logger.Info("control-service: shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := quotaServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("control-service: quota shutdown", zap.Error(err))
	}
	if err := schedulerServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("control-service: scheduler shutdown", zap.Error(err))
	}
	logger.Info("control-service: stopped")
}

func newQuotaServer(store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *openresearchcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// quota-service never connects to a cluster directly (ProvisionAgent is a no-op on the
	// native backend regardless — no per-agent k8s object to create — but wiring it through
	// queuebackend keeps every service consistent about never dialing a cluster).
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second
	provisioner := quota.AgentProvisioner(queuebackend.New(store, nil, nil, metricsDBURL, connectedWithin))
	svc := quota.NewService(store, provisioner, logger)
	handler := quota.NewHandler(svc, logger)

	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)
	acceleratorTypeInfos := make([]quota.AcceleratorTypeInfo, 0, len(pcfg.AcceleratorTypes))
	for _, g := range pcfg.AcceleratorTypes {
		acceleratorTypeInfos = append(acceleratorTypeInfos, quota.AcceleratorTypeInfo{Name: g.Name, AccHRate: g.AccHRate})
	}
	resourceCatalog := quota.ResourceCatalog{
		AcceleratorTypes:          acceleratorTypeInfos,
		CPUCoreHourRate:   domain.CPUCoreHourRate(),
		RAMGBHourRate:     domain.RAMGBHourRate(),
		StorageGBHourRate: domain.StorageGBHourRate(),
	}
	peHandler := quota.NewPlatformExperimentsHandler(peSvc, logger).
		WithCatalog(resourceCatalog).
		WithLiveCapacity(metricsDBURL, pcfg.AcceleratorNameForFlavor, connectedWithin)

	// Auto-close platform experiments once their ends_at deadline passes (e.g. a "24h" experiment
	// otherwise stays open forever — EndsAt was previously stored but nothing ever read it). Runs
	// under context.Background() rather than the process shutdown context: it's a plain periodic
	// scan with no per-request state to drain, so it's fine for it to just stop when the process
	// exits. Close() is safe to race across replicas — a second caller just gets
	// invalid_transition and logs it, no corruption.
	go peSvc.StartExpirySweep(context.Background(), 60*time.Second)

	r := chi.NewRouter()
	r.Use(api.CORSMiddleware)
	handler.RegisterRoutes(r)
	peHandler.RegisterRoutes(r)
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	return &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func newSchedulerServer(pool *db.Pool, store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *openresearchcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	expQuotaSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)

	// Configured target clusters (clusters.yaml / CLUSTERS_CONFIG_PATH), falling back to a
	// single "default" cluster name if absent. The control plane never connects to any of
	// these directly — it only needs their names, to route commands to the right outbox
	// partition and to know which clusters exist for admission purposes. All actual
	// Kubernetes access happens inside each cluster's own cluster-agent, which polls this
	// service for work; see controlplane/services/clusteragentapi and
	// controlplane/shared/queuebackend.
	clusterEntries, err := openresearchcfg.LoadClusters(envOrDefault("CLUSTERS_CONFIG_PATH", "settings/clusters.yaml"))
	if err != nil {
		logger.Fatal("control-service: load clusters config", zap.Error(err))
	}
	openresearchWorkloadCfg := &workload.OpenResearchConfig{
		NameByFlavor: pcfg.NameByFlavor,
		AcceleratorsByFlavor: pcfg.AcceleratorsByFlavor,
		FlavorOrder:  pcfg.FlavorOrder(),
	}
	var clusterNames []string
	if len(clusterEntries) == 0 {
		clusterNames = []string{workload.DefaultClusterName}
	} else {
		for _, ce := range clusterEntries {
			clusterNames = append(clusterNames, ce.Name)
		}
	}
	// jwc's static type is workload.Backend: this is the one line that would change to plug
	// in a different scheduling mechanism — see controlplane/shared/workload/backend.go.
	// queuebackend.Backend never dials into a cluster; it only reads/writes Postgres.
	var jwc workload.Backend = queuebackend.New(store, clusterNames, openresearchWorkloadCfg, metricsDBURL,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second)

	// Cancelled implicitly when the process exits; the watcher and loop below run for the
	// life of control-service, same as they did in standalone scheduler-service.
	observedGapCap := time.Duration(pcfg.Scheduler.SilenceMultiplier * float64(pcfg.Scheduler.DefaultReportIntervalSeconds) * float64(time.Second))
	observedStep := time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second

	// settler is the sole path that durably writes a terminal experiment's final observed usage
	// (see services/settlement) — used both inline by JobWatcher for the fast path and by the
	// reconciler below to retry any experiment a crash or metrics-DB outage left unsettled.
	settler := settlement.New(expQuotaSvc, metricsDBURL, observedGapCap, observedStep, scheduler.ObservedMaxLookback)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)
	go settlementReconciler.Start(context.Background())

	watcher := scheduler.NewJobWatcher(store, jwc, logger).
		WithQuotaSettler(settler).
		WithQuotaAdjuster(expQuotaSvc).
		WithPollInterval(time.Duration(pcfg.Scheduler.JobPollIntervalSeconds) * time.Second).
		WithScanInterval(time.Duration(pcfg.Scheduler.AdmittedScanIntervalSeconds) * time.Second).
		WithStuckPendingTimeout(time.Duration(pcfg.Scheduler.StuckPendingTimeoutSeconds) * time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap, observedStep)

	noveltyDetector := dedup.New()
	schedulerSvc := scheduler.NewService(store, expQuotaSvc, jwc, noveltyDetector, store).
		WithQuotaConfig(quotaCfg).
		WithObservedTimeConfig(metricsDBURL, observedGapCap, observedStep)

	schedulerLoop := scheduler.NewLoop(store, expQuotaSvc, jwc, logger).
		WithReprioritizer(schedulerSvc).
		WithHeartbeat(time.Duration(pcfg.Scheduler.LoopHeartbeatSeconds) * time.Second).
		WithPreemptTimeout(time.Duration(pcfg.Scheduler.PreemptTimeoutSeconds) * time.Second).
		WithGuaranteedFairnessWindow(time.Duration(pcfg.Scheduler.GuaranteedFairnessWindowSeconds) * time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap, observedStep)
	schedulerSvc = schedulerSvc.WithLoop(schedulerLoop)

	// The admission tick reads capacity then decides then writes (not a CAS), and JobWatcher's
	// scan starts one poller per SUBMITTED/ADMITTED experiment — both must run on exactly one
	// control-service replica at a time. leaderelection.Run holds a Postgres advisory lock to
	// pick that replica; every other replica stays in standby and takes over if the leader dies.
	// The HTTP API stays multi-replica — only these two background loops are leader-gated.
	go leaderelection.Run(context.Background(), pool.Raw(), leaderelection.SchedulerLockKey,
		5*time.Second, logger, func(leaderCtx context.Context) {
			schedulerLoop.Start(leaderCtx)
			watcher.Start(leaderCtx)
		})

	schedulerHandler := scheduler.NewHandler(schedulerSvc)
	r := chi.NewRouter()
	schedulerHandler.Routes(r)

	gw := api.NewGateway(
		http.NotFoundHandler(),
		r,
		http.NotFoundHandler(),
	)
	gwHandler := gw.Handler(logger)

	clusterAgentHandler := clusteragentapi.NewHandler(store,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, logger)
	clusterAgentRouter := chi.NewRouter()
	clusterAgentHandler.Routes(clusterAgentRouter)

	outer := chi.NewRouter()
	// Same CORS policy as the quota-service router above (api.CORSMiddleware) — without it,
	// the UI's cross-origin calls into this port (fetchClusters, cancelExperiment, job
	// submission) succeed at the network level but the browser silently blocks the response,
	// which surfaces as a misleading "Cannot reach scheduler service" rather than a CORS error.
	outer.Use(api.CORSMiddleware)
	outer.Handle("/metrics", promhttp.Handler())
	outer.Mount("/internal/clusters", clusterAgentRouter)
	outer.Mount("/", gwHandler)

	return &http.Server{
		Addr:         ":" + port,
		Handler:      outer,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
