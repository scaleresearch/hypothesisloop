package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	cloudagentsandbox "github.com/scaleresearch/hypothesisloop/runtime/cloud/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/compute"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider/providers"
)

// defaultWarmPool is the warm pool on a sandbox provider that can warm instances when the
// settings name none.
const defaultWarmPool = 2

// newRented builds what rents machines elsewhere, each a loop the leader runs: the launcher of
// sandboxed agents on the sandbox provider, and one runtime per cloud compute account. A provider
// that cannot be built from its settings stops the process here.
func newRented(pcfg *hypothesisloopcfg.ControlService, store *db.Store, clusters *clusterexchange.Service, metrics *metricsdb.Store, logger *zap.Logger) ([]func(context.Context), error) {
	var loops []func(context.Context)
	if s := pcfg.Sandbox; s != nil {
		launcher, err := newLauncher(pcfg, s, store, metrics, logger)
		if err != nil {
			return nil, err
		}
		loops = append(loops, func(ctx context.Context) { _ = launcher.Run(ctx) })
	}
	environments := ""
	if pcfg.Environments != nil {
		environments = pcfg.Environments.Repository
	}
	for _, cc := range pcfg.CloudCompute {
		cluster, err := newCloudCluster(pcfg, cc, environments, store, clusters, logger)
		if err != nil {
			return nil, err
		}
		loops = append(loops, cluster.Run)
	}
	return loops, nil
}

func newLauncher(pcfg *hypothesisloopcfg.ControlService, s *hypothesisloopcfg.SandboxConfig, store *db.Store, metrics *metricsdb.Store, logger *zap.Logger) (*cloudagentsandbox.Launcher, error) {
	log := logger.Named("agentsandbox")
	// An agent instance has no accelerator, so the provider is asked for no accelerator type.
	rented, err := providers.New(s.Provider.Name, s.Provider.Settings, pcfg.SandboxSecret, nil, log.Named(s.Provider.Name).Sugar().Infof)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	warmPool := 0
	if _, warms := rented.(provider.Warmer); warms {
		warmPool = defaultWarmPool
	}
	if s.WarmPoolSize != nil {
		warmPool = *s.WarmPoolSize
	}
	decider := agentsandbox.NewDecider(store, metrics, agentsandbox.Settings{
		Image: pcfg.Agents.Image, APIURL: pcfg.Services.PublicAPIURL, SigningSecret: pcfg.SigningSecret,
		BootLimit:       time.Duration(pcfg.Agents.BootLimitSeconds) * time.Second,
		ProgressLimit:   time.Duration(pcfg.Agents.ProgressLimitSeconds) * time.Second,
		MaxNewPerAnswer: pcfg.Agents.MaxNewInstancesPerAnswer,
	}, log)
	launcher, err := cloudagentsandbox.New(cloudagentsandbox.Config{
		Name:     s.Name,
		Command:  s.Instance.Command,
		Size:     cloudagentsandbox.Size{CPUMillis: s.Instance.CPUMillis, MemoryBytes: s.Instance.MemoryBytes, DiskBytes: s.Instance.DiskBytes},
		Provider: rented, Platform: decider,
		Interval: s.Interval(), CreatePause: s.CreatePause(), WarmPool: warmPool,
		Log: log.Sugar().Infof,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	log.Info("sandboxed agents run on the sandbox provider", zap.String("provider", s.Provider.Name), zap.String("name", s.Name),
		zap.Strings("capabilities", provider.Capabilities(rented)), zap.Int("warm_pool", warmPool))
	return launcher, nil
}

func newCloudCluster(pcfg *hypothesisloopcfg.ControlService, cc hypothesisloopcfg.CloudComputeConfig, environments string, store *db.Store, clusters *clusterexchange.Service, logger *zap.Logger) (*compute.Cluster, error) {
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
	rented, err := providers.New(cc.Provider.Name, cc.Provider.Settings, pcfg.CloudComputeSecrets[cc.ClusterName], acceleratorTypes, log.Named(cc.Provider.Name).Sugar().Infof)
	if err != nil {
		return nil, fmt.Errorf("cloud_compute %s: %w", cc.ClusterName, err)
	}
	cluster, err := compute.NewCluster(compute.ClusterConfig{
		Name: cc.ClusterName,
		Executor: compute.Config{
			ClusterID: cc.Provider.Name + "-" + cc.Provider.AccountID, Namespace: cc.Namespace,
			APIURL: pcfg.Services.PublicAPIURL, JobImage: cc.JobImage, EnvironmentsRepository: environments,
			Shapes: shapes, Provider: rented,
			OfferWait: time.Duration(cc.OfferWaitSeconds) * time.Second, Startup: time.Duration(cc.StartupSeconds) * time.Second,
			Log: log.Sugar().Infof,
		},
		Exchange: clusters, Registry: store, Version: pcfg.Runtimes.ImageTag,
		ReconcileInterval: time.Duration(cc.ReconcileIntervalSeconds) * time.Second,
		StatusInterval:    time.Duration(cc.StatusIntervalSeconds) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	log.Info("jobs of the cluster run on the cloud compute provider", zap.String("provider", cc.Provider.Name),
		zap.Strings("capabilities", provider.Capabilities(rented)), zap.Int("shapes", len(shapes)))
	return cluster, nil
}
