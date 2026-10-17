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
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/environments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

type apiGate struct {
	tokens auth.Tokens
	access *auth.Access
}

func newAPIServer(runCtx context.Context, gate apiGate, repos *accounts.Repositories, gitHost githost.Provider, pool *db.Pool, store *db.Store, graph *graphdb.Store, quotaCfg domain.QuotaConfig, pcfg *hypothesisloopcfg.Config, metricsDBURL, port string, logger *zap.Logger) *http.Server {
	// One client for the durable-data store, shared by everything that reads it: the listing
	// endpoint, the usage report, admission's ceiling, and the addressing handed to runtimes.
	dataStore, err := objectstore.New(pcfg.DataStore.Provider, pcfg.DataStore.ControlPlaneEndpoint, pcfg.DataStore.Endpoint, pcfg.DataStore.Region, pcfg.DataStore.Bucket,
		pcfg.DataStore.AccessKeyID, pcfg.DataStore.SecretAccessKey)
	if err != nil {
		logger.Fatal("control-service: configure durable data store", zap.Error(err))
	}
	connectedWithin := time.Duration(pcfg.Scheduler.ClusterUnreachableAfterSeconds) * time.Second

	// One observation cadence for the whole process: the quota service, the settler, the
	// controller and the scheduler loop must all measure "how long did this run" the same way,
	// or the same job's cost depends on which of them is asked.
	observedGapCap := pcfg.Scheduler.ObservationCadence()

	// An environment is keyed on the digests of the images it is built from, asked of their
	// registry at most once a minute per tag.
	environmentsSvc := environments.NewService(store, gitHost).WithJobImages(pcfg.OfferedJobImages(), imagedigest.New(pcfg.PlainHTTPRegistries()...))

	peSvc := platformexperiments.NewPlatformExperimentsService(store, quotaCfg, logger, metricsDBURL, observedGapCap).WithDataStore(dataStore, pcfg.DataStore.MaxBytesPerAgent).WithRepositories(gitHost).
		WithEnvironments(environmentsSvc).WithModelDefaults(modelDefaults(pcfg)).WithIdleNotice(store, time.Duration(pcfg.Agents.IdleNoticeMinutes)*time.Minute).WithPersonIdeas(graph)
	launch := platformexperiments.AgentLaunch{
		Hosting: pcfg.Sandbox != nil, DefaultModel: pcfg.Agents.DefaultModel, CoordinatorModel: pcfg.Agents.CoordinatorModel,
		SigningSecret: pcfg.Auth.SigningSecret, MaxHosted: pcfg.Agents.MaxHostedPerPlatformExperiment,
	}
	peSvc = peSvc.WithPlanning(platformexperiments.Planning{
		Limit:       time.Duration(pcfg.Planning.LimitMinutes) * time.Minute,
		ProposedFor: time.Duration(pcfg.Planning.ProposedForDays) * 24 * time.Hour,
		PlannerKept: time.Duration(pcfg.Planning.PlannerKeptMinutes) * time.Minute,
		AutoAccept:  time.Duration(pcfg.Planning.AutoAcceptSeconds) * time.Second,
	}).
		WithJobImages(pcfg.OfferedJobImages()).
		WithGitProxy(pcfg.GitHost.ProxyURL).
		WithLiveCapacity(metricsDBURL, connectedWithin, store, domain.NewPodResources(pcfg.PodResourcesByName)).
		WithAgentLaunch(launch)

	jobs, clusterExchange := newSchedulerParts(runCtx, gitHost, environmentsSvc, pool, store, graph, peSvc, quotaCfg, pcfg, metricsDBURL, dataStore, logger)
	// The change stream: one LISTEN connection for this process, fanned out to whatever /watch
	// connections are open. Nothing is persisted for it and nothing is held between connections
	// — see httpapi/watch/watch.go.
	events := db.NewEventsStore(pool, graph)
	registrySvc := registry.New(store, graph, events, logger, metricsDBURL, dataStore)
	watchHandler := watch.NewHandler(events, store, gate.access, logger)
	watchHandler.Start(runCtx)
	connectedClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterHeartbeats(ctx, metricsDBURL, connectedWithin)
	}
	autoscaledClusters := func(ctx context.Context) (map[string]bool, error) {
		return metricsdb.LiveClusterAutoscalerCapability(ctx, metricsDBURL, connectedWithin)
	}
	clusterContacts := func(ctx context.Context) (map[string]accounts.Contact, error) {
		seen, err := metricsdb.ClusterLastContacts(ctx, metricsDBURL, clusterContactLookback)
		if err != nil {
			return nil, err
		}
		out := make(map[string]accounts.Contact, len(seen))
		for name, contact := range seen {
			out[name] = accounts.Contact{At: contact.At, AgentVersion: contact.AgentVersion}
		}
		return out, nil
	}

	modelProxy, err := modelproxy.New(store, modelproxy.MetricsDB{URL: metricsDBURL},
		modelproxy.AnthropicUpstream, pcfg.Models.Anthropic.OAuthToken, logger)
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
		Instructions:        platformexperiments.NewInstructionsService(store, modelDefaults(pcfg), logger),
		Drafts:              platformexperiments.NewDrafts(store, logger),
		Accounts: accounts.NewService(store, store, connectedClusters, autoscaledClusters, clusterContacts, pcfg.Auth.SigningSecret, accounts.RuntimeAddresses{APIURL: pcfg.Services.PublicAPIURL, MetricsURL: pcfg.Runtimes.MetricsURL, Registry: pcfg.Runtimes.Registry, ImageTag: pcfg.Runtimes.ImageTag, RegistryPlainHTTP: pcfg.Runtimes.RegistryPlainHTTP},
			accounts.Limits{MaxOrgsPerUser: pcfg.Accounts.MaxOrgsPerUser, MaxProjectsPerOrg: pcfg.Accounts.MaxProjectsPerOrg}, gate.access, logger),
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

// clusterContactLookback is how far back a cluster's last contact is looked for: one silent for
// longer is listed with none.
const clusterContactLookback = 30 * 24 * time.Hour

// newEnvironmentRegistry is where assigned environment builds push to and the logins to it, and
// the git host their files' repositories are read from, nil on a deployment whose settings have no
// environments section.
func newEnvironmentRegistry(ctx context.Context, pcfg *hypothesisloopcfg.Config, gitHost githost.Provider, logger *zap.Logger) *clusterexchange.EnvironmentRegistry {
	e := pcfg.Environments
	if e == nil {
		return nil
	}
	logins, err := registrycred.New(ctx, *e)
	if err != nil {
		logger.Fatal("control-service: the registry logins of environment builds", zap.Error(err))
	}
	return &clusterexchange.EnvironmentRegistry{Repository: e.Repository, Logins: logins, Git: gitHost}
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
func modelDefaults(pcfg *hypothesisloopcfg.Config) domain.ModelDefaults {
	return domain.ModelDefaults{Agent: pcfg.Agents.DefaultModel, Coordinator: pcfg.Agents.CoordinatorModel}
}

// newGitHost is the git host the settings name, one of two. On a plain one there is no GitHub App
// and no client for it: connecting GitHub answers that there is none.
func newGitHost(pcfg *hypothesisloopcfg.Config, store *db.Store, logger *zap.Logger) (githubApp accounts.GitHubApp, gitHost githost.Provider) {
	switch pcfg.GitHost.Provider {
	case hypothesisloopcfg.GitHostProviderGitHub:
		client, err := github.New(*pcfg.GitHost.GitHub)
		if err != nil {
			logger.Fatal("control-service: configure the GitHub App", zap.Error(err))
		}
		githubApp, gitHost = client, github.NewHost(store, client, logger)
	case hypothesisloopcfg.GitHostProviderPlain:
		plain := pcfg.GitHost.Plain
		var err error
		gitHost, err = githost.NewPlain(plain.BaseURL, plain.Username, plain.Password)
		if err != nil {
			logger.Fatal("control-service: configure the git host", zap.Error(err))
		}
	default:
		logger.Fatal("control-service: the settings name no git host", zap.String("provider", pcfg.GitHost.Provider))
	}
	return githubApp, gitHost
}
