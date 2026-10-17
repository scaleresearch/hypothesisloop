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
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/metricsingest"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/controller"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// metrics-service hosts the metric-controller: the node-agent metric push endpoint and the
// reconcile/eviction loop that reads samples back out of GreptimeDB to drive stage boundaries
// and evictions. It serves nothing agent- or UI-facing — the whole public API lives on
// control-service's single listener.
func main() {
	// Every time the API answers with is RFC3339 in UTC. A time read
	// from the database or taken from the clock is in the process's zone, which on a host that
	// is not in UTC put an offset on every answer and turned the zero time into another date.
	time.Local = time.UTC
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics-service: init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	pcfg, err := hypothesisloopcfg.LoadFromEnv()
	if err != nil {
		logger.Fatal("metrics-service: settings", zap.Error(err))
	}
	controllerPort := strconv.Itoa(pcfg.Services.MetricControllerPort)

	metrics := metricsdb.New(pcfg.Services.MetricsDBURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.ConfigOf(pcfg.Database, db.ServiceMaxConns))
	cancel()
	if err != nil {
		logger.Fatal("metrics-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	store := db.NewStore(pool, pcfg.Scheduler.MaxInfrastructureRequeues)

	domain.SetAcceleratorRates(pcfg.RateByName)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:                pcfg.Quota.Top3BonusFraction,
		BurstFraction:                    pcfg.Quota.BurstFraction,
		DefaultMaxConcurrentAccelerators: pcfg.Quota.DefaultMaxConcurrentAccelerators,
	}

	authenticator, err := auth.NewAuthenticator(pcfg.Auth, store)
	if err != nil {
		logger.Fatal("metrics-service: configure auth", zap.Error(err))
	}

	controllerServer, runCancel := newControllerServer(authenticator, pool, store, quotaCfg, pcfg, metrics, controllerPort, logger)

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

func newControllerServer(authenticator *auth.Authenticator, pool *db.Pool, store *db.Store, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metrics *metricsdb.Store, port string, logger *zap.Logger) (*http.Server, context.CancelFunc) {
	observedGapCap := pcfg.Scheduler.ObservationCadence()
	peSvc := platformexperiments.NewService(store, quotaCfg, logger, metrics, observedGapCap)

	// metric-controller never connects to a cluster directly, and needs no cluster/workload
	// wiring at all: eviction is purely a status update (service/controller) — the
	// cluster-agent's own reconcile loop removes the Job once status leaves the desired set.
	ctrl := controller.New(store, peSvc, logger).
		WithSilenceMultiplier(pcfg.Scheduler.SilenceMultiplier).
		WithDefaultReportInterval(time.Duration(pcfg.Scheduler.DefaultReportIntervalSeconds) * time.Second).
		WithMinSilenceWindow(time.Duration(pcfg.Scheduler.MinSilenceWindowSeconds) * time.Second).
		WithStartTimeout(time.Duration(pcfg.Scheduler.StartTimeoutSeconds) * time.Second).
		WithClusterSilenceCeiling(time.Duration(pcfg.Scheduler.ClusterStatusSilenceCeilingSeconds) * time.Second).
		WithReconcileInterval(time.Duration(pcfg.Scheduler.ReconcileIntervalSeconds) * time.Second)
	// Wire the stage-ladder store and GreptimeDB (Prometheus-compatible) URL. The ladder itself
	// is per-platform-experiment config, not a controller-wide setting.
	ctrl = ctrl.WithStagesStore(store, metrics)
	// Each pass keeps the researchers of every running platform experiment at work as its pool
	// says, launched as the control service launches the researchers it creates.
	ctrl = ctrl.WithResearcherKeeper(peSvc.WithAgentLaunch(platformexperiments.AgentLaunch{
		Hosting: pcfg.Sandbox != nil, DefaultModel: pcfg.Agents.DefaultModel,
	}))

	// settler durably writes a terminal experiment's final observed usage (see
	// service/settlement) — used both inline by the controller for the fast path and by the
	// reconciler below to retry any experiment a crash or metrics-DB outage left unsettled.
	// gapCap mirrors Controller.observedGapCap exactly, so every observed-usage query in this
	// deployment agrees on what "how long did this run" means.
	settler := settlement.New(peSvc, metrics, observedGapCap)
	ctrl = ctrl.WithSettler(settler)
	settlementReconciler := settlement.NewReconciler(store, settler, 30*time.Second, logger)

	// The controller's passes read, decide and write without a CAS, so one replica runs them, as
	// control-service runs its scheduler.
	if err := ctrl.Check(); err != nil {
		logger.Fatal("metrics-service: reconcile loop", zap.Error(err))
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	pass := pcfg.Scheduler.PassTimeout()
	go db.RunAsLeader(runCtx, pool, db.ControllerLockKey, 5*time.Second, logger,
		func(ctx context.Context) { settlementReconciler.Start(ctx, pass) },
		func(ctx context.Context) { ctrl.Run(ctx, pass) },
	)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      newRouter(authenticator, metricsingest.NewPushHandler(metrics, store), logger),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	return srv, runCancel
}

func newRouter(tokens auth.Tokens, push http.Handler, logger *zap.Logger) chi.Router {
	r := chi.NewRouter()
	r.Use(auth.Gate(tokens, logger, nil))
	r.Handle("/metrics", auth.RequireStaff(logger, promhttp.Handler()))
	r.Handle("/internal/node-metrics", auth.RequireRuntime(logger, push))
	// Both spellings, same as the API server — a caller should never have to remember which
	// of /health and /healthz a given process happens to answer.
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	}
	r.HandleFunc("/health", health)
	r.HandleFunc("/healthz", health)
	return r
}
