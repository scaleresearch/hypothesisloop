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

	"github.com/scaleresearch/hypothesisloop/controlplane/services/controller"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metrics"
)

// metrics-service hosts registry-service and metric-controller together: one
// writes samples to GreptimeDB (registry, via remote write) and the other
// reads them back to drive eviction decisions (metric-controller), so they're
// the two halves of the same metrics pipeline. The node-agent metric push
// endpoint and the reconcile/eviction loop stay on their own internal mux,
// separate from the registry's experiment API, exactly as they were as
// standalone services.
func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg := hypothesisloopcfg.MustLoad(requiredEnv("HYPOTHESISLOOP_CONFIG", logger))
	registryPort := strconv.Itoa(pcfg.Services.RegistryPort)
	controllerPort := strconv.Itoa(pcfg.Services.MetricControllerPort)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Fatal("DATABASE_URL environment variable is required")
	}

	metricsDBURL := pcfg.Services.MetricsDBURL

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn})
	cancel()
	if err != nil {
		logger.Fatal("metrics-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	store := db.NewStore(pool, metricsDBURL)

	domain.SetAcceleratorRates(pcfg.RateByName)
	domain.SetCPUCoreHourRate(pcfg.CPUCoreHourRate)
	domain.SetRAMGBHourRate(pcfg.RAMGBHourRate)
	domain.SetStorageGBHourRate(pcfg.StorageGBHourRate)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:     pcfg.Quota.Top3BonusFraction,
		BurstFraction:         pcfg.Quota.BurstFraction,
		MetricDeclineFraction: pcfg.Quota.MetricDeclineFraction,
	}

	peFullStore := db.NewPlatformExperimentsFullStore(store)

	registryServer := newRegistryServer(store, metricsDBURL, registryPort, logger)
	controllerServer, runCancel := newControllerServer(store, peFullStore, quotaCfg, pcfg, metricsDBURL, controllerPort, logger)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Info("registry listening", zap.String("addr", registryServer.Addr))
		if err := registryServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("metrics-service: registry listen", zap.Error(err))
		}
	}()
	go func() {
		logger.Info("metric-controller health endpoint", zap.String("addr", controllerServer.Addr))
		if err := controllerServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics-service: metric-controller listen", zap.Error(err))
		}
	}()

	<-sigCh
	logger.Info("metrics-service: shutting down")
	runCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := registryServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics-service: registry shutdown", zap.Error(err))
	}
	if err := controllerServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics-service: metric-controller shutdown", zap.Error(err))
	}
	logger.Info("metrics-service: stopped")
}

func newRegistryServer(store *db.Store, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	svc := registry.New(store, logger, metricsDBURL)
	handler := registry.NewHandler(svc, logger)

	outer := chi.NewRouter()
	outer.Use(api.RecoveryMiddleware(logger))
	outer.Use(api.LoggingMiddleware(logger))
	outer.Use(api.CORSMiddleware)
	outer.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	outer.Handle("/metrics", promhttp.Handler())
	// Registry API via Huma, registered at full /registry/* paths so /openapi.json and
	// /explore live at the port root.
	doc := apidocs.New(outer, "hypothesisloop registry-service", "1.0.0",
		"Hypotheses, experiment metrics and lineage. See the quota-service /explore for the cross-cutting platform rules.\n")
	registry.RegisterHuma(doc, handler)
	doc.MountExplore(outer)

	return &http.Server{
		Addr:         ":" + port,
		Handler:      outer,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func newControllerServer(store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) (*http.Server, context.CancelFunc) {
	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)

	// metric-controller never connects to a cluster directly, and needs no cluster/workload
	// wiring at all: eviction is purely a status update (services/controller) — the
	// cluster-agent's own reconcile loop removes the Job once status leaves the desired set.
	ctrl := controller.New(store, peSvc, logger).
		WithMetricDeclineFraction(quotaCfg.MetricDeclineFraction).
		WithSilenceMultiplier(pcfg.Scheduler.SilenceMultiplier).
		WithOverrunMultiplier(pcfg.Scheduler.OverrunMultiplier).
		WithDefaultReportInterval(time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds)*time.Second).
		WithMinSilenceWindow(time.Duration(pcfg.Scheduler.MinSilenceWindowSeconds)*time.Second).
		WithReconcileInterval(time.Duration(pcfg.Scheduler.ReconcileIntervalSeconds)*time.Second).
		WithGCSweep(
			time.Duration(pcfg.Scheduler.StaleDesiredStateSweepIntervalSeconds)*time.Second,
			time.Duration(pcfg.Scheduler.StaleDesiredStateThresholdSeconds)*time.Second,
		)
	// Wire Phase 2 store and GreptimeDB (Prometheus-compatible) URL for Domain 10 two-phase execution.
	ctrl = ctrl.WithPhase2Store(peFullStore, metricsDBURL).
		WithPhase2Boundary(pcfg.Phase2.BoundaryFraction).
		WithPhase2AdmissionPercentile(pcfg.Phase2.AdmissionPercentile)

	// settler durably writes a terminal experiment's final observed usage (see
	// services/settlement) — used both inline by the controller for the fast path and by the
	// reconciler below to retry any experiment a crash or metrics-DB outage left unsettled.
	// gapCap/step mirror Controller.observedGapCap/observedStep exactly, so every observed-usage
	// query in this deployment agrees on what "how long did this run" means.
	observedGapCap := time.Duration(pcfg.Scheduler.SilenceMultiplier * float64(pcfg.Scheduler.DefaultReportIntervalSeconds) * float64(time.Second))
	observedStep := time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second
	settler := settlement.New(peSvc, metricsDBURL, observedGapCap, observedStep, scheduler.ObservedMaxLookback)
	ctrl = ctrl.WithSettler(settler)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)
	go settlementReconciler.Start(context.Background())

	runCtx, runCancel := context.WithCancel(context.Background())
	if err := ctrl.Start(runCtx); err != nil {
		logger.Fatal("metrics-service: start reconcile loop", zap.Error(err))
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/internal/node-metrics", metrics.NewPushHandler(metricsDBURL))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      api.CORSMiddleware(mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	return srv, runCancel
}

func requiredEnv(key string, logger *zap.Logger) string {
	value := os.Getenv(key)
	if value == "" {
		logger.Fatal(key + " environment variable is required")
	}
	return value
}
