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

	"github.com/scaleresearch/openresearch/controlplane/services/controller"
	"github.com/scaleresearch/openresearch/controlplane/services/quota"
	"github.com/scaleresearch/openresearch/controlplane/services/registry"
	"github.com/scaleresearch/openresearch/controlplane/shared/api"
	openresearchcfg "github.com/scaleresearch/openresearch/controlplane/shared/config"
	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metrics"
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

	registryPort := envOrDefault("REGISTRY_PORT", "8083")
	controllerPort := envOrDefault("METRIC_CONTROLLER_PORT", "8084")

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
		logger.Fatal("metrics-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	store := db.NewStore(pool, metricsDBURL)

	pcfg := openresearchcfg.MustLoad(envOrDefault("OPENRESEARCH_CONFIG", "settings/openresearch.yaml"))
	domain.SetGPURates(pcfg.RateByName)
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

	r := chi.NewRouter()
	handler.Mount(r)

	gw := api.NewGateway(
		http.NotFoundHandler(), // quota not served by this binary
		http.NotFoundHandler(), // scheduler not served by this binary
		r,
	)
	gwHandler := gw.Handler(logger)

	outer := chi.NewRouter()
	outer.Handle("/metrics", promhttp.Handler())
	outer.Mount("/", gwHandler)

	return &http.Server{
		Addr:         ":" + port,
		Handler:      outer,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func newControllerServer(store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *openresearchcfg.Config, metricsDBURL, port string, logger *zap.Logger) (*http.Server, context.CancelFunc) {
	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL)

	// metric-controller never connects to a cluster directly, and needs no cluster/workload
	// wiring at all: eviction is purely a status update (services/controller) — the
	// cluster-agent's own reconcile loop removes the Job once status leaves the desired set.
	ctrl := controller.New(store, peSvc, logger).
		WithMetricDeclineFraction(quotaCfg.MetricDeclineFraction).
		WithSilenceMultiplier(pcfg.Scheduler.SilenceMultiplier).
		WithOverrunMultiplier(pcfg.Scheduler.OverrunMultiplier).
		WithDefaultReportInterval(time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second).
		WithMinSilenceWindow(time.Duration(pcfg.Scheduler.MinSilenceWindowSeconds) * time.Second).
		WithReconcileInterval(time.Duration(pcfg.Scheduler.ReconcileIntervalSeconds) * time.Second).
		WithGCSweep(
			time.Duration(pcfg.Scheduler.StaleDesiredStateSweepIntervalSeconds)*time.Second,
			time.Duration(pcfg.Scheduler.StaleDesiredStateThresholdSeconds)*time.Second,
		)
	// Wire Phase 2 store and GreptimeDB (Prometheus-compatible) URL for Domain 10 two-phase execution.
	ctrl = ctrl.WithPhase2Store(peFullStore, metricsDBURL).
		WithPhase2Boundary(pcfg.Phase2.BoundaryFraction).
		WithPhase2AdmissionPercentile(pcfg.Phase2.AdmissionPercentile)

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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
