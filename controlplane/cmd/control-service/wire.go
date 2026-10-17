package main

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost/github"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/imagedigest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/registrycred"
	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/gitproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/modelproxy"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/watch"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore/minio"
)

type apiGate struct {
	tokens auth.Tokens
	access *auth.Access
}

func newAPIServer(runCtx context.Context, gate apiGate, repos *accounts.Repositories, gitHost githost.Provider, pool *db.Pool, store *db.Store, graph *graphdb.Store, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.ControlService, metrics *metricsdb.Store, port string, logger *zap.Logger) *http.Server {
	// One client for the durable-data store, shared by everything that reads it: the listing
	// endpoint, the usage report, admission's ceiling, and the addressing handed to runtimes.
	storeCfg := pcfg.ObjectStore
	accessKeyID, issuer := objectStoreKey(storeCfg, logger)
	dataStore, err := objectstore.New(storeCfg.ControlPlaneEndpoint, storeCfg.Endpoint, storeCfg.Region, storeCfg.Bucket,
		accessKeyID, pcfg.ObjectStoreSecret, issuer)
	if err != nil {
		logger.Fatal("control-service: configure durable data store", zap.Error(err))
	}
	bucketCtx, bucketCancel := context.WithTimeout(runCtx, 30*time.Second)
	err = dataStore.EnsureBucket(bucketCtx)
	bucketCancel()
	if err != nil {
		logger.Fatal("control-service: durable data store bucket", zap.Error(err))
	}
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second

	// One observation cadence for the whole process: the quota service, the settler, the
	// controller and the scheduler loop must all measure "how long did this run" the same way,
	// or the same job's cost depends on which of them is asked.
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	imageRegistry := newImageRegistry(runCtx, pcfg, logger)

	peSvc := platformexperiments.NewService(store, quotaCfg, logger, metrics, observedGapCap).WithDataStore(dataStore, pcfg.ObjectStore.MaxBytesPerAgent).WithRepositories(gitHost).
		WithImages(imageRegistry, store, imagedigest.New(pcfg.PlainHTTPRegistries()...)).WithModelDefaults(modelDefaults(pcfg)).WithIdleNotice(store, time.Duration(pcfg.Agents.IdleNoticeMinutes)*time.Minute).WithPersonIdeas(graph)
	launch := platformexperiments.AgentLaunch{
		AgentSandbox: pcfg.Sandbox != nil, DefaultModel: pcfg.Agents.DefaultModel, CoordinatorModel: pcfg.Agents.CoordinatorModel,
		SigningSecret: pcfg.SigningSecret, MaxAgentSandboxes: pcfg.Agents.MaxAgentSandboxesPerPlatformExperiment,
	}
	peSvc = peSvc.WithPlanning(platformexperiments.Planning{
		Limit:       time.Duration(pcfg.Planning.LimitMinutes) * time.Minute,
		ProposedFor: time.Duration(pcfg.Planning.ProposedForDays) * 24 * time.Hour,
		PlannerKept: time.Duration(pcfg.Planning.PlannerKeptMinutes) * time.Minute,
		AutoAccept:  time.Duration(pcfg.Planning.AutoAcceptSeconds) * time.Second,
	}).
		WithGitProxy(pcfg.GitProxyURL()).
		WithLiveCapacity(connectedWithin, store, domain.NewPodResources(pcfg.PodResourcesByName)).
		WithAgentLaunch(launch)

	jobs, clusterExchange := newSchedulerParts(runCtx, gitHost, imageRegistry, pool, store, graph, peSvc, quotaCfg, pcfg, metrics, dataStore, logger)
	registrySvc := registry.New(store, graph, store, logger, metrics, dataStore)
	// The change stream: one LISTEN for this process wakes whatever /watch streams are open, and
	// each reads the changes table from its own cursor.
	watchHandler := watch.NewHandler(store, graph, store, gate.access, logger)
	watchHandler.Start(runCtx)

	modelProxy, err := modelproxy.New(store, metrics, peSvc,
		modelproxy.Policy{Models: pcfg.Models.Catalog, MaxInFlightPerOrg: pcfg.Models.MaxCallsInFlightPerOrg},
		modelproxy.AnthropicUpstream, pcfg.ModelToken, logger)
	if err != nil {
		logger.Fatal("control-service: configure the model proxy", zap.Error(err))
	}

	r := httpapi.NewRouter(httpapi.Handlers{
		Tokens:              gate.tokens,
		Access:              gate.access,
		UIOrigin:            pcfg.Auth.UIOrigin,
		Logger:              logger,
		Agents:              accounts.NewAgents(store, logger),
		PlatformExperiments: peSvc,
		Hypotheses:          graph,
		Launch:              launch,
		Sharing:             platformexperiments.NewSharing(store, logger),
		Sessions:            platformexperiments.NewSessions(dataStore),
		Instructions:        platformexperiments.NewComposer(store, modelDefaults(pcfg), logger),
		Drafts:              platformexperiments.NewDrafts(store, logger),
		Accounts: accounts.NewService(db.OrgDefaults{Store: store, NewOrgLimit: pcfg.Quota.NewOrgComputeLimit()}, store, metrics, connectedWithin, pcfg.SigningSecret, accounts.RuntimeAddresses{APIURL: pcfg.Services.PublicAPIURL, MetricsURL: pcfg.Runtimes.MetricsURL, Registry: pcfg.Runtimes.Registry, ImageTag: pcfg.Runtimes.ImageTag, RegistryPlainHTTP: pcfg.Runtimes.RegistryPlainHTTP},
			platformClusters(pcfg), accounts.Limits{MaxOrgsPerUser: pcfg.Accounts.MaxOrgsPerUser, MaxProjectsPerOrg: pcfg.Accounts.MaxProjectsPerOrg}, gate.access, logger),
		AgentTypes:      accounts.NewAgentTypes(store, gate.access, logger),
		Repositories:    repos,
		Scheduler:       jobs,
		Registry:        registrySvc,
		ClusterExchange: clusterExchange,
		Watch:           watchHandler,
		GitProxy:        gitproxy.New(store, gitHost, logger),
		ModelProxy:      modelProxy,
	})

	return &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// newImageRegistry is the registry experiments push their images to and jobs pull them from,
// nil on a deployment whose settings have no container_registry section.
func newImageRegistry(ctx context.Context, pcfg *hypothesisloopcfg.ControlService, logger *zap.Logger) platformexperiments.ImageRegistry {
	r := pcfg.ContainerRegistry
	if r == nil {
		return nil
	}
	// A run ends at most max_platform_experiment_days after it starts, which is at most
	// baseline_limit_minutes after the platform experiment's start.
	keepFor := time.Duration(pcfg.Quota.MaxPlatformExperimentDays)*24*time.Hour + time.Duration(pcfg.Planning.BaselineLimitMinutes)*time.Minute
	registry, err := registrycred.New(ctx, *r, keepFor)
	if err != nil {
		logger.Fatal("control-service: the image registry", zap.Error(err))
	}
	return registry
}

// openGraph connects to the hypothesis graph and makes sure its schema exists.
func openGraph(ctx context.Context, url string) (*graphdb.Store, error) {
	graph, err := graphdb.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := graph.EnsureSchema(ctx); err != nil {
		graph.Close(ctx) //nolint:errcheck
		return nil, err
	}
	return graph, nil
}

// modelDefaults is the platform's model for an agent and for a coordinator that names none.
func platformClusters(pcfg *hypothesisloopcfg.ControlService) []string {
	names := make([]string, 0, len(pcfg.CloudCompute))
	for _, cc := range pcfg.CloudCompute {
		names = append(names, cc.ClusterName)
	}
	return names
}

func modelDefaults(pcfg *hypothesisloopcfg.ControlService) domain.ModelDefaults {
	return domain.ModelDefaults{Agent: pcfg.Agents.DefaultModel, Coordinator: pcfg.Agents.CoordinatorModel}
}

// objectStoreKey is the key id the object store the settings name is reached with, from its
// provider's section, and how that store creates a job's key.
func objectStoreKey(store hypothesisloopcfg.ObjectStoreConfig, logger *zap.Logger) (string, func(*objectstore.Client) objectstore.KeyIssuer) {
	switch store.Provider {
	case hypothesisloopcfg.ObjectStoreProviderMinIO:
		return store.MinIO.AccessKeyID, func(c *objectstore.Client) objectstore.KeyIssuer { return minio.New(c, c.SecretAccessKey) }
	}
	logger.Fatal("control-service: the settings name no object store provider this build knows", zap.String("provider", store.Provider))
	return "", nil
}

// newGitHost is the git host the settings name, one of two. On a plain one there is no GitHub App
// and no client for it: connecting GitHub answers that there is none.
func newGitHost(pcfg *hypothesisloopcfg.ControlService, store *db.Store, logger *zap.Logger) (githubApp accounts.GitHubApp, gitHost githost.Provider) {
	switch pcfg.GitHost.Provider {
	case hypothesisloopcfg.GitHostProviderGitHub:
		client, err := github.New(*pcfg.GitHost.GitHub, pcfg.GitHubKey)
		if err != nil {
			logger.Fatal("control-service: configure the GitHub App", zap.Error(err))
		}
		githubApp, gitHost = github.NewConnections(client, pcfg.GitHubClientSecret, logger), github.NewHost(store, client, pcfg.GitHost.ExperimentTemplateRepository, logger)
	case hypothesisloopcfg.GitHostProviderPlain:
		plain := pcfg.GitHost.Plain
		var err error
		gitHost, err = githost.NewPlain(plain.BaseURL, plain.Username, pcfg.GitPassword, pcfg.GitHost.ExperimentTemplateRepository, store)
		if err != nil {
			logger.Fatal("control-service: configure the git host", zap.Error(err))
		}
	default:
		logger.Fatal("control-service: the settings name no git host", zap.String("provider", pcfg.GitHost.Provider))
	}
	return githubApp, gitHost
}
