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

// control-service hosts quota-service and scheduler-service in one process
// (they share the Postgres store and quota domain logic). Each keeps its own
// listener on its historical port so existing callers don't need to change.
func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "control-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg := hypothesisloopcfg.MustLoad(requiredEnv("HYPOTHESISLOOP_CONFIG", logger))
	quotaPort := strconv.Itoa(pcfg.Services.QuotaPort)
	schedulerPort := strconv.Itoa(pcfg.Services.SchedulerPort)

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
		Phase1ExploreFraction:     pcfg.Phase2.BoundaryFraction,
		MaxSubmissionsPerHour:     pcfg.Quota.MaxSubmissionsPerHour,
		MetricDeclineFraction:     pcfg.Quota.MetricDeclineFraction,
		MaxAcceleratorCountPerJob: pcfg.Quota.MaxAcceleratorCountPerJob,
		MaxCPUCoresPerJob:         pcfg.Quota.MaxCPUCoresPerJob,
		MaxRAMGBPerJob:            pcfg.Quota.MaxRAMGBPerJob,
		MaxStorageGBPerJob:        pcfg.Quota.MaxStorageGBPerJob,
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

func newQuotaServer(store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// Native Kubernetes Jobs need no per-agent object; keeps agent registration
	// independent from the scheduler backend.
	provisioner := quota.AgentProvisioner(noopAgentProvisioner{})
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second
	svc := quota.NewService(store, provisioner, logger)
	handler := quota.NewHandler(svc, logger)

	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)
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

	// Auto-close platform experiments past their ends_at deadline. Runs under
	// context.Background() since it's a periodic scan with no state to drain on shutdown.
	// Close() is safe to race across replicas — a second caller just logs invalid_transition.
	go peSvc.StartExpirySweep(context.Background(), 60*time.Second)

	r := chi.NewRouter()
	r.Use(api.CORSMiddleware)
	// Registers the quota API via Huma, exposing /openapi.json and a compact /explore digest
	// with the cross-cutting platform-rules preamble (agents talk to quota first).
	doc := apidocs.New(r, "hypothesisloop quota-service", "1.0.0", apidocs.PlatformRules)
	quota.RegisterHuma(doc, handler, peHandler)
	doc.MountExplore(r)
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

func newSchedulerServer(pool *db.Pool, store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	expQuotaSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)

	// jwc is workload.Backend; swap this line to plug in a different scheduling mechanism
	// (see workload/backend.go). queuebackend.Backend only reads/writes Postgres.
	queueBackend, err := queuebackend.New(store, metricsDBURL,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second)
	if err != nil {
		logger.Fatal("control-service: configure scheduler backend", zap.Error(err))
	}
	var jwc workload.Backend = queueBackend

	observedGapCap := time.Duration(pcfg.Scheduler.SilenceMultiplier * float64(pcfg.Scheduler.DefaultReportIntervalSeconds) * float64(time.Second))
	observedStep := time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second

	// settler is the sole path that durably writes a terminal experiment's final observed usage
	// (see services/settlement) — used inline by JobWatcher and by the reconciler below to
	// retry anything a crash or metrics-DB outage left unsettled.
	settler := settlement.New(expQuotaSvc, metricsDBURL, observedGapCap, observedStep, scheduler.ObservedMaxLookback)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)
	go settlementReconciler.Start(context.Background())

	watcher := scheduler.NewJobWatcher(store, jwc, logger).
		WithQuotaSettler(settler).
		WithPollInterval(time.Duration(pcfg.Scheduler.JobPollIntervalSeconds)*time.Second).
		WithStuckPendingTimeout(time.Duration(pcfg.Scheduler.StuckPendingTimeoutSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap, observedStep)

	noveltyDetector := dedup.New()
	schedulerSvc := scheduler.NewService(store, expQuotaSvc, jwc, noveltyDetector, store).
		WithQuotaConfig(quotaCfg).
		WithQuotaSettler(settler)

	schedulerLoop := scheduler.NewLoop(store, expQuotaSvc, jwc, logger).
		WithReprioritizer(schedulerSvc).
		WithHeartbeat(time.Duration(pcfg.Scheduler.LoopHeartbeatSeconds)*time.Second).
		WithGuaranteedFairnessWindow(time.Duration(pcfg.Scheduler.GuaranteedFairnessWindowSeconds)*time.Second).
		WithObservedTimeConfig(metricsDBURL, observedGapCap, observedStep)
	schedulerSvc = schedulerSvc.WithLoop(schedulerLoop)

	// Admission (read-decide-write, not a CAS) and JobWatcher's per-experiment pollers must run
	// on exactly one replica. leaderelection.Run holds a Postgres advisory lock to pick that
	// replica; others stand by and take over if the leader dies. Only these loops are
	// leader-gated — the HTTP API stays multi-replica.
	go leaderelection.Run(context.Background(), pool.Raw(), leaderelection.SchedulerLockKey,
		5*time.Second, logger, func(leaderCtx context.Context) {
			schedulerLoop.Start(leaderCtx)
			watcher.Start(leaderCtx)
		})

	schedulerHandler := scheduler.NewHandler(schedulerSvc)

	// Cluster-agent-facing surface (Go cluster-agent binaries, not the research agent) gets
	// its own Huma registration mounted at /internal/clusters with its own openapi/explore docs.
	clusterAgentHandler := clusteragentapi.NewHandler(store,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, logger)
	clusterAgentRouter := chi.NewRouter()
	caDoc := apidocs.New(clusterAgentRouter, "hypothesisloop cluster-agent API", "1.0.0", "")
	clusteragentapi.RegisterHuma(caDoc, clusterAgentHandler)
	caDoc.MountExplore(clusterAgentRouter)

	outer := chi.NewRouter()
	// Recovery + request logging plus CORS — without CORS the UI's cross-origin calls succeed at
	// the network level but the browser blocks the response, looking like "Cannot reach scheduler".
	outer.Use(api.RecoveryMiddleware(logger))
	outer.Use(api.LoggingMiddleware(logger))
	outer.Use(api.CORSMiddleware)
	outer.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	outer.Handle("/metrics", promhttp.Handler())
	outer.Mount("/internal/clusters", clusterAgentRouter)
	// Scheduler API via Huma at full /experiments/* paths on the port-root router, so
	// /openapi.json and /explore live at the port root.
	schedDoc := apidocs.New(outer, "hypothesisloop scheduler-service", "1.0.0",
		"Job submission and experiment lifecycle. See the quota-service /explore for the cross-cutting platform rules.\n")
	scheduler.RegisterHuma(schedDoc, schedulerHandler)
	schedDoc.MountExplore(outer)

	return &http.Server{
		Addr:         ":" + port,
		Handler:      outer,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func requiredEnv(key string, logger *zap.Logger) string {
	value := os.Getenv(key)
	if value == "" {
		logger.Fatal(key + " environment variable is required")
	}
	return value
}
