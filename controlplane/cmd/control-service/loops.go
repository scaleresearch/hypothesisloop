package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/environments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/settlement"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// newSchedulerParts builds the scheduler's handler and starts its background loops, returning
// the handlers httpapi.NewRouter serves: the scheduler's on the shared API doc, the cluster-agents' under
// its own prefix.
func newSchedulerParts(runCtx context.Context, gitHost githost.Provider, environmentsSvc *environments.Service, pool *db.Pool, store *db.Store, graph *graphdb.Store, expQuotaSvc *platformexperiments.PlatformExperimentsService, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL string, dataStore *objectstore.Client, logger *zap.Logger) (*scheduler.Service, *clusterexchange.Service) {
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
	clusterExchange := clusterexchange.NewService(store,
		time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds)*time.Second, metricsDBURL, pcfg.Scheduler.ObservationCadence(), dataStore,
		time.Duration(pcfg.Scheduler.MaxCheckpointGraceSeconds)*time.Second,
		pcfg.AcceleratorTypeNames(), pcfg.Scheduler.MaxLogTailLineChars, pcfg.Auth.SigningSecret, pcfg.GitHost.ProxyURL, environmentRegistry, logger).
		WithBuildReported(schedulerLoop.Trigger)

	// The sandbox launcher and the cloud compute clusters rent machines, so one replica runs them.
	rented, err := newRented(pcfg, store, clusterExchange, metricsDBURL, logger)
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
		func(ctx context.Context) { expQuotaSvc.StartAutoAccept(ctx, 2*time.Second, pass) },
		func(ctx context.Context) { schedulerLoop.Start(ctx, pass) },
		func(ctx context.Context) { watcher.Start(ctx, pass) },
	}, rented...)...)

	return schedulerSvc, clusterExchange
}
