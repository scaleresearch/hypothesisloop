package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/providers"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/agentsandbox/launcher"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/compute"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// newRented builds what rents machines elsewhere, each a loop the leader runs: the launcher of
// sandboxed agents on the sandbox provider, and one runtime per cloud compute account. A provider
// that cannot be built from its settings stops the process here.
func newRented(pcfg *hypothesisloopcfg.ControlService, store *db.Store, clusters *clusterexchange.Service, metrics *metricsdb.Store, logger *zap.Logger) ([]func(context.Context), error) {
	var loops []func(context.Context)
	if s := pcfg.Sandbox; s != nil {
		sandboxes, err := newLauncher(pcfg, s, store, metrics, logger)
		if err != nil {
			return nil, err
		}
		loops = append(loops, func(ctx context.Context) { _ = sandboxes.Run(ctx) })
	}
	byID := map[string]string{}
	for _, cc := range pcfg.CloudCompute {
		cluster, id, err := newCloudCluster(pcfg, cc, store, clusters, logger)
		if err != nil {
			return nil, err
		}
		if other, taken := byID[id]; taken {
			return nil, fmt.Errorf("cloud_compute %s: rents in namespace %s of the same account as %s, which would destroy each other's instances", cc.ClusterName, cc.Namespace, other)
		}
		byID[id] = cc.ClusterName
		loops = append(loops, cluster.Run)
	}
	return loops, nil
}

func newLauncher(pcfg *hypothesisloopcfg.ControlService, s *hypothesisloopcfg.SandboxConfig, store *db.Store, metrics *metricsdb.Store, logger *zap.Logger) (*launcher.Launcher, error) {
	log := logger.Named("agentsandbox")
	// An agent instance has no accelerator, so the provider is asked for no accelerator type.
	rented, err := providers.New(s.Provider, s.Sections[s.Provider], pcfg.SandboxSecret, nil, log.Named(s.Provider).Sugar().Infof)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	warmPool := *s.WarmPoolSize
	decider := agentsandbox.NewDecider(store, metrics, agentsandbox.Settings{
		Image: pcfg.Agents.Image, APIURL: pcfg.Services.PublicAPIURL, SigningSecret: pcfg.SigningSecret,
		BootLimit:       time.Duration(pcfg.Agents.BootLimitSeconds) * time.Second,
		ProgressLimit:   time.Duration(pcfg.Agents.ProgressLimitSeconds) * time.Second,
		MaxNewPerAnswer: pcfg.Agents.MaxNewInstancesPerAnswer,
	}, log)
	sandboxes, err := launcher.New(launcher.Config{
		Name:     s.Name,
		Command:  s.Instance.Command,
		Size:     launcher.Size{CPUMillis: s.Instance.CPUMillis, MemoryBytes: s.Instance.MemoryBytes, DiskBytes: s.Instance.DiskBytes},
		Provider: rented, Platform: decider,
		Interval: s.Interval(), CreatePause: s.CreatePause(), WarmPool: warmPool,
		Log: log.Sugar().Infof,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	log.Info("sandboxed agents run on the sandbox provider", zap.String("provider", s.Provider), zap.String("name", s.Name),
		zap.Strings("capabilities", provider.Capabilities(rented)), zap.Int("warm_pool", warmPool))
	return sandboxes, nil
}

// newCloudCluster is the runtime of one cloud_compute entry and the id it reports: the provider,
// the account and the namespace, which together name the instances it owns and stay the same when
// the cluster is renamed.
func newCloudCluster(pcfg *hypothesisloopcfg.ControlService, cc hypothesisloopcfg.CloudComputeConfig, store *db.Store, clusters *clusterexchange.Service, logger *zap.Logger) (*compute.Cluster, string, error) {
	shapes := make([]compute.Shape, 0, len(cc.Shapes))
	var acceleratorTypes []string
	for _, s := range cc.Shapes {
		shapes = append(shapes, compute.Shape{
			Name: s.Name, AcceleratorType: s.AcceleratorType, Accelerators: s.Accelerators,
			CPUMillis: s.CPUMillis, MemoryBytes: s.MemoryBytes, StorageBytes: s.StorageBytes, MaxInstances: s.MaxInstances,
		})
		if s.Accelerators > 0 {
			acceleratorTypes = append(acceleratorTypes, s.AcceleratorType)
		}
	}
	log := logger.Named("cloudcompute").With(zap.String("cluster", cc.ClusterName))
	rented, err := providers.New(cc.Provider, cc.Sections[cc.Provider], pcfg.CloudComputeSecrets[cc.ClusterName], acceleratorTypes, log.Named(cc.Provider).Sugar().Infof)
	if err != nil {
		return nil, "", fmt.Errorf("cloud_compute %s: %w", cc.ClusterName, err)
	}
	account, accounted := rented.(provider.Accounted)
	if !accounted {
		return nil, "", fmt.Errorf("cloud_compute %s: provider %s names no account, which the cluster's id is made of", cc.ClusterName, cc.Provider)
	}
	id := cc.Provider + "-" + account.Account() + "-" + cc.Namespace
	cluster, err := compute.NewCluster(compute.ClusterConfig{
		Name: cc.ClusterName,
		Executor: compute.Config{
			ClusterID: id, Namespace: cc.Namespace,
			APIURL: pcfg.Services.PublicAPIURL,
			Shapes: shapes, Provider: rented,
			OfferWait: time.Duration(cc.OfferWaitSeconds) * time.Second, Startup: time.Duration(cc.StartupSeconds) * time.Second,
			Demand: queuedFor{store: store, cluster: cc.ClusterName},
			Log:    log.Sugar().Infof,
		},
		Exchange: clusters, Registry: store, Version: pcfg.Runtimes.ImageTag,
		ReconcileInterval: time.Duration(cc.ReconcileIntervalSeconds) * time.Second,
		StatusInterval:    time.Duration(cc.StatusIntervalSeconds) * time.Second,
	})
	if err != nil {
		return nil, "", err
	}
	log.Info("jobs of the cluster run on the cloud compute provider", zap.String("provider", cc.Provider),
		zap.Strings("capabilities", provider.Capabilities(rented)), zap.Int("shapes", len(shapes)))
	return cluster, id, nil
}

// queuedFor is the jobs queued that cluster may run: those of the orgs it hosts.
type queuedFor struct {
	store   *db.Store
	cluster string
}

func (q queuedFor) Waiting(ctx context.Context) ([]compute.Waiting, error) {
	cluster, err := q.store.GetCluster(ctx, q.cluster)
	if err != nil || cluster == nil {
		return nil, err
	}
	queued, err := q.store.ListQueuedExperiments(ctx)
	if err != nil {
		return nil, err
	}
	var waiting []compute.Waiting
	for _, exp := range queued {
		if cluster.Hosts(exp.OrgID) {
			waiting = append(waiting, compute.Waiting{AcceleratorType: exp.AcceleratorType, Job: exp.Job})
		}
	}
	return waiting, nil
}
