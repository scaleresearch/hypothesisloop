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

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
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

	pcfg, err := hypothesisloopcfg.LoadControlService()
	if err != nil {
		logger.Fatal("control-service: settings", zap.Error(err))
	}
	apiPort := strconv.Itoa(pcfg.Services.APIPort)

	metrics := metricsdb.New(pcfg.MetricsDBURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(ctx, db.ConfigOf(pcfg.Database, pcfg.DatabaseURL, db.ServiceMaxConns))
	cancel()
	if err != nil {
		logger.Fatal("control-service: connect db", zap.Error(err))
	}
	defer pool.Close()

	ensureSchema(pool, logger)
	store := db.NewStore(pool, pcfg.Scheduler.MaxInfrastructureRequeues)
	seedBuiltins(store, pcfg.Quota.NewOrgComputeLimit(), logger)

	metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = metrics.EnsureSchema(metricsCtx)
	metricsCancel()
	if err != nil {
		logger.Fatal("control-service: metrics store schema", zap.Error(err))
	}

	graphCtx, graphCancel := context.WithTimeout(context.Background(), 30*time.Second)
	graph, err := openGraph(graphCtx, pcfg.GraphDBURL)
	graphCancel()
	if err != nil {
		logger.Fatal("control-service: graph store", zap.Error(err))
	}
	defer graph.Close(context.Background()) //nolint:errcheck

	domain.SetAcceleratorRates(pcfg.RateByName)
	domain.SetAcceleratorLabels(pcfg.LabelByName)
	domain.SetCPUCoreSharedRate(pcfg.Quota.SharedAccHPerCPUCore)

	quotaCfg := domain.QuotaConfig{
		Top3BonusFraction:                pcfg.Quota.Top3BonusFraction,
		BurstFraction:                    pcfg.Quota.BurstFraction,
		BudgetReference:                  pcfg.BudgetReference,
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
		DefaultModelBudgetUSD:            pcfg.Quota.DefaultModelBudgetUSD,
		MaxModelBudgetUSD:                pcfg.Quota.MaxModelBudgetUSD,
	}

	// One root context for every background loop, cancelled before the HTTP server drains.
	// They used to run on context.Background(), so SIGTERM tore the process down mid-admission
	// pass while the connection pool was closing underneath them.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	authenticator, err := auth.NewAuthenticator(pcfg.Auth, pcfg.SigningSecret, db.OrgDefaults{Store: store, NewOrgLimit: pcfg.Quota.NewOrgComputeLimit()})
	if err != nil {
		logger.Fatal("control-service: configure auth", zap.Error(err))
	}
	gate := apiGate{tokens: authenticator, access: auth.NewAccess(store)}

	githubApp, gitHost := newGitHost(pcfg, store, logger)
	repos := accounts.NewRepositories(store, githubApp, gate.access, logger)

	apiServer := newAPIServer(runCtx, gate, repos, gitHost, pool, store, graph, quotaCfg, pcfg, metrics, apiPort, logger)

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
