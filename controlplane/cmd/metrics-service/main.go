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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/controller"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metrics"
)

// metrics-service hosts the metric-controller: the node-agent metric push endpoint and the
// reconcile/eviction loop that reads samples back out of GreptimeDB to drive stage boundaries
// and evictions. It serves nothing agent- or UI-facing — the whole public API lives on
// control-service's single listener.
func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg := hypothesisloopcfg.MustLoad(requiredEnv("HYPOTHESISLOOP_CONFIG", logger))
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

	store := db.NewStore(pool, metricsDBURL, pcfg.Scheduler.MaxInfrastructureRequeues)

	domain.SetAcceleratorRates(pcfg.RateByName)
	domain.SetCPUCoreHourRate(pcfg.CPUCoreHourRate)
	domain.SetRAMGBHourRate(pcfg.RAMGBHourRate)
	domain.SetStorageGBHourRate(pcfg.StorageGBHourRate)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction: pcfg.Quota.Top3BonusFraction,
		BurstFraction:     pcfg.Quota.BurstFraction,
	}

	peFullStore := db.NewPlatformExperimentsFullStore(store)

	controllerServer, runCancel := newControllerServer(store, peFullStore, quotaCfg, pcfg, metricsDBURL, controllerPort, logger)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

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
	if err := controllerServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics-service: metric-controller shutdown", zap.Error(err))
	}
	logger.Info("metrics-service: stopped")
}

func newControllerServer(store *db.Store, peFullStore *db.PlatformExperimentsFullStore, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) (*http.Server, context.CancelFunc) {
	observedGapCap := pcfg.Scheduler.ObservationCadence()
	peSvc := quota.NewPlatformExperimentsService(peFullStore, quotaCfg, logger, metricsDBURL, observedGapCap)

	// metric-controller never connects to a cluster directly, and needs no cluster/workload
	// wiring at all: eviction is purely a status update (services/controller) — the
	// cluster-agent's own reconcile loop removes the Job once status leaves the desired set.
	ctrl := controller.New(store, peSvc, logger).
		WithSilenceMultiplier(pcfg.Scheduler.SilenceMultiplier).
		WithDefaultReportInterval(time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second).
		WithMinSilenceWindow(time.Duration(pcfg.Scheduler.MinSilenceWindowSeconds) * time.Second).
		WithClusterSilenceCeiling(time.Duration(pcfg.Scheduler.ClusterStatusSilenceCeilingSeconds) * time.Second).
		WithReconcileInterval(time.Duration(pcfg.Scheduler.ReconcileIntervalSeconds) * time.Second)
	// Wire the stage-ladder store and GreptimeDB (Prometheus-compatible) URL. The ladder itself
	// is per-platform-experiment config, not a controller-wide setting.
	ctrl = ctrl.WithStagesStore(peFullStore, metricsDBURL)

	// settler durably writes a terminal experiment's final observed usage (see
	// services/settlement) — used both inline by the controller for the fast path and by the
	// reconciler below to retry any experiment a crash or metrics-DB outage left unsettled.
	// gapCap mirrors Controller.observedGapCap exactly, so every observed-usage query in this
	// deployment agrees on what "how long did this run" means.
	settler := settlement.New(peSvc, metricsDBURL, observedGapCap)
	ctrl = ctrl.WithSettler(settler)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)

	// metrics-service runs single-replica. The reconcile loop below is not leader-elected, and
	// two of them would compute stage cuts from two different mid-flight snapshots — computeCut is
	// read-decide-then-write, not a CAS, so whichever commits first wins and the other's verdict
	// is lost. (The settlement reconciler above is safe either way: Settle is an idempotent
	// absolute set.) Scale this out only behind the same leaderelection.Run gate control-service
	// uses for its scheduler loop.
	runCtx, runCancel := context.WithCancel(context.Background())
	go settlementReconciler.Start(runCtx)
	if err := ctrl.Start(runCtx); err != nil {
		logger.Fatal("metrics-service: start reconcile loop", zap.Error(err))
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/internal/node-metrics", metrics.NewPushHandler(metricsDBURL))
	// Both spellings, same as the API server — a caller should never have to remember which
	// of /health and /healthz a given process happens to answer.
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	}
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/healthz", health)

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
