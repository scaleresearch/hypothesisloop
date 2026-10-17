package apitest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/watch"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// world is the rows every token and every access check of the authorization table is resolved
// against: two orgs, org-a holding two projects, one platform experiment in each project, and a
// second one in org-b that staff shared.
// It is the directory behind the real authenticator and the placement behind the real access
// checks, so the table exercises both as control-service joins them.
type world struct {
	keys map[string]*domain.APIKey
}

const (
	worldSecret   = "the-authorization-table-signing-secret"
	worldIssuer   = "https://issuer.test/auth/v1"
	worldAudience = "hypothesisloop-test"
	worldMember   = "user-m"
	// worldShared is the one platform experiment of org-b that staff shared with every account.
	worldShared = "pe-show"
)

var (
	worldOrgOf     = map[string]string{"prj-a1": "org-a", "prj-a2": "org-a", "prj-b": "org-b"}
	worldProjectOf = map[string]string{"pe-a1": "prj-a1", "pe-a2": "prj-a2", "pe-b": "prj-b", worldShared: "prj-b"}
	// Each job and hypothesis: its platform experiment and its agent.
	worldPlaced = map[string][2]string{
		"own": {"pe-a1", "agent-1"}, "peer": {"pe-a1", "agent-2"}, "sibling": {"pe-a2", "agent-2"}, "foreign": {"pe-b", "agent-9"},
		"shared": {worldShared, "agent-9"},
	}
	worldAgentOrg   = map[string]string{"agent-1": "org-a", "agent-2": "org-a", "agent-9": "org-b"}
	worldClusterOrg = map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"}
)

func (w *world) tenancy(platformExperimentID, agentID string) *domain.Tenancy {
	projectID, ok := worldProjectOf[platformExperimentID]
	if !ok {
		return nil
	}
	return &domain.Tenancy{PlatformExperimentID: platformExperimentID, ProjectID: projectID, OrgID: worldOrgOf[projectID], AgentID: agentID,
		Shared: platformExperimentID == worldShared}
}

// placed resolves an id of the form "<prefix>-<owner>", as every job and
// hypothesis of the world is named.
func (w *world) placed(prefix, id string) *domain.Tenancy {
	if len(id) <= len(prefix) || id[:len(prefix)] != prefix {
		return nil
	}
	at, ok := worldPlaced[id[len(prefix):]]
	if !ok {
		return nil
	}
	return w.tenancy(at[0], at[1])
}

func (w *world) EnsureUserHome(_ context.Context, userID, name, _ string) (*domain.User, []string, error) {
	return &domain.User{ID: userID, Name: name}, []string{"org-a"}, nil
}

func (w *world) APIKeyByHash(_ context.Context, hash string) (*domain.APIKey, error) {
	return w.keys[hash], nil
}

func (w *world) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, *time.Time, error) {
	return w.tenancy(id, ""), nil, nil
}

func (w *world) ExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	return w.placed("job-", id), false, nil
}

func (w *world) AgentCutAt(context.Context, string, string) (*time.Time, error) { return nil, nil }

func (w *world) AgentFinishedAt(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}

func (w *world) PlannerAnsweredAt(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}

func (w *world) IsSignedUp(_ context.Context, platformExperimentID, agentID string) (bool, error) {
	return platformExperimentID == "pe-a1" && agentID == "agent-1", nil
}

func (w *world) AgentOrg(_ context.Context, id string) (string, bool, error) {
	org, ok := worldAgentOrg[id]
	return org, ok, nil
}

func (w *world) ClusterOrg(_ context.Context, name string) (string, bool, error) {
	org, ok := worldClusterOrg[name]
	return org, ok, nil
}

func (w *world) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	org, ok := worldClusterOrg[name]
	if !ok {
		return nil, nil
	}
	return &domain.Cluster{Name: name, OrgID: org}, nil
}

// GetAPIKey knows one key of each org, key-of-<org>, made by a person, and the org key that calls.
func (w *world) GetAPIKey(_ context.Context, id string) (*domain.APIKey, error) {
	if id == "key-a" {
		return &domain.APIKey{ID: id, OrgID: "org-a", Actor: domain.Actor{Kind: domain.ActorUser, ID: "user-a"}}, nil
	}
	org, ok := strings.CutPrefix(id, "key-of-")
	if !ok || (org != "org-a" && org != "org-b") {
		return nil, nil
	}
	return &domain.APIKey{ID: id, OrgID: org}, nil
}

func (w *world) GetOrg(_ context.Context, id string) (*domain.Org, error) {
	if id != "org-a" && id != "org-b" {
		return nil, nil
	}
	return &domain.Org{ID: id}, nil
}

// OrgRole knows the two signed-in people of org-a: user-a owns it and user-m is a member.
func (w *world) OrgRole(_ context.Context, _ string, userID string) (domain.OrgRole, bool, error) {
	if userID == worldMember {
		return domain.OrgRoleMember, true, nil
	}
	return domain.OrgRoleOwner, true, nil
}

func (w *world) GetProject(_ context.Context, id string) (*domain.Project, error) {
	org, ok := worldOrgOf[id]
	if !ok {
		return nil, nil
	}
	return &domain.Project{ID: id, OrgID: org}, nil
}

func (w *world) PlatformExperimentIDs(_ context.Context, scope domain.TenantScope) ([]string, error) {
	ids := []string{}
	for id := range worldProjectOf {
		if scope.Admits(*w.tenancy(id, "")) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// worldGraph answers the one graph read a handler makes before it asks whether the caller
// reaches the hypothesis. Every other call is the service at work and trips the wire.
type worldGraph struct {
	registry.Graph
	world *world
}

func (g worldGraph) GetHypothesis(_ context.Context, id string) (*domain.Hypothesis, error) {
	t := g.world.placed("hyp-", id)
	if t == nil {
		return nil, nil
	}
	return &domain.Hypothesis{ID: id, PlatformExperimentID: t.PlatformExperimentID, AgentID: t.AgentID}, nil
}

var worldPlanning = platformexperiments.Planning{Limit: 30 * time.Minute, ProposedFor: 7 * 24 * time.Hour, PlannerKept: 10 * time.Minute}

// reached is what a request answers once every check let it through and the handler asked a
// service for something: the services here hold no store, so that first ask panics.
const reached = 599

func tripwire(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				w.WriteHeader(reached)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// tripAgentTypes and tripInstallations are stores with nothing behind them: the first call trips
// the wire, as the other services here do. No GitHub App is configured beside them.
type tripAgentTypes struct{ accounts.AgentTypeStore }

type tripInstallations struct{ accounts.InstallationStore }

// worldAPI is control-service's routing over the world: the same registrations on the same
// three documents, the raw /watch handler, and services with nothing behind them.
type worldAPI struct {
	router chi.Router
	// serve is router with every successful answer checked against the document.
	serve  http.Handler
	docs   map[string]*httpapi.Doc
	tokens map[string]string
}

func newWorldAPI(t *testing.T) *worldAPI {
	t.Helper()
	w := &world{keys: map[string]*domain.APIKey{}}
	signer, jwk := newWorldSigner(t)
	authenticator, err := auth.NewAuthenticator(config.AuthConfig{
		LocalDevelopment:  true,
		IdentityProviders: []config.IdentityProvider{{Issuer: worldIssuer, Audience: worldAudience, JWTKeys: []config.JWTKey{jwk}}},
	}, worldSecret, w)
	if err != nil {
		t.Fatal(err)
	}
	access := auth.NewAccess(w)
	logger := zap.NewNop()

	api := &worldAPI{router: chi.NewRouter(), docs: map[string]*httpapi.Doc{}, tokens: map[string]string{"none": ""}}
	api.router.Use(tripwire)
	api.router.Use(auth.Gate(authenticator, logger, map[string]string{"/watch": platformapi.WatchSubprotocol}))
	watcher := watch.NewHandler(nil, nil, nil, access, logger)
	api.router.Get("/watch", watcher.ServeHTTP)

	clusters := chi.NewRouter()
	api.docs["/internal/clusters"] = httpapi.NewDoc(clusters, "cluster-agent", "1.0.0", "", access, logger)
	httpapi.RegisterClusterAgent(api.docs["/internal/clusters"], clusterexchange.NewService(clusterexchange.Config{SigningSecret: worldSecret, ProxyURL: "https://api.test/git", Logger: logger}))
	api.router.Mount("/internal/clusters", clusters)

	doc := httpapi.NewDoc(api.router, "hypothesisloop API", "1.0.0", "", access, logger)
	api.docs[""] = doc
	launch := platformexperiments.AgentLaunch{AgentSandbox: true, DefaultModel: "model-1", CoordinatorModel: "model-2", SigningSecret: worldSecret, MaxAgentSandboxes: 20}
	peHandler := platformexperiments.NewService(nil, domain.QuotaConfig{}, logger, nil, time.Minute).WithRepositories(worldRepositories{}).WithPlanning(worldPlanning).WithAgentLaunch(launch)
	httpapi.Register(doc, httpapi.Handlers{
		Logger:              logger,
		PlatformExperiments: peHandler,
		Hypotheses:          worldGraph{world: w},
		Launch:              launch,
		Accounts:            accounts.NewService(nil, nil, nil, 0, worldSecret, accounts.RuntimeAddresses{}, nil, accounts.Limits{MaxOrgsPerUser: 3, MaxProjectsPerOrg: 10}, doc.Access, logger),
		Scheduler:           scheduler.NewService(nil, nil, nil, worldRepositories{}, contractCapacity{}, contractSettler{}, metricsdb.New("http://metrics.invalid"), logger).WithLoop(noopLoop{}),
		Registry:            registry.New(nil, worldGraph{world: w}, nil, logger, metricsdb.New("http://metrics.invalid"), nil),
		AgentTypes:          accounts.NewAgentTypes(tripAgentTypes{}, doc.Access, logger),
		Repositories:        accounts.NewRepositories(tripInstallations{}, nil, doc.Access, logger),
		Instructions:        platformexperiments.NewComposer(nil, domain.ModelDefaults{}, logger),
	})

	mint := func(scope string) string {
		token, err := auth.Mint(worldSecret, scope)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	key := func(apiKey *domain.APIKey) string {
		_, token, err := auth.NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		w.keys[auth.HashAPIKey(token)] = apiKey
		return token
	}
	api.tokens["user"] = signer(map[string]any{"iss": worldIssuer, "aud": worldAudience, "sub": "user-a", "name": "Ada", "exp": time.Now().Add(time.Hour).Unix()})
	api.tokens["member"] = signer(map[string]any{"iss": worldIssuer, "aud": worldAudience, "sub": worldMember, "name": "Mel", "exp": time.Now().Add(time.Hour).Unix()})
	api.tokens["org key"] = key(&domain.APIKey{ID: "key-a", OrgID: "org-a"})
	api.tokens["project key"] = key(&domain.APIKey{ID: "key-a1", OrgID: "org-a", ProjectID: "prj-a1"})
	api.tokens["agent"] = mint(scope.Agent("pe-a1", "agent-1"))
	api.tokens["coordinator"] = mint(scope.Coordinator("pe-a1"))
	api.tokens["job"] = mint(scope.Job("job-own"))
	api.tokens["runtime"] = mint(scope.Runtime("cluster-a", time.Time{}))
	api.tokens["staff"] = mint(scope.Staff(time.Now().Add(time.Hour)))
	api.serve = answering(t, doc.API, api.router)
	return api
}

// newWorldSigner returns a function signing ES256 JWTs and the public key to configure.
func newWorldSigner(t *testing.T) (func(claims map[string]any) string, config.JWTKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding
	segment := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b64.EncodeToString(raw)
	}
	sign := func(claims map[string]any) string {
		input := segment(map[string]any{"alg": "ES256", "kid": "world", "typ": "JWT"}) + "." + segment(claims)
		digest := sha256.Sum256([]byte(input))
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return input + "." + b64.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
	}
	return sign, config.JWTKey{Kid: "world", Kty: "EC", Alg: "ES256", Crv: "P-256",
		X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}
}

func (w *world) AgentStanding(ctx context.Context, pe, agent string) (domain.AgentStanding, error) {
	tenancy, closedAt, err := w.PlatformExperimentTenancy(ctx, pe)
	if err != nil || tenancy == nil {
		return domain.AgentStanding{}, err
	}
	signedUp, err := w.IsSignedUp(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	cutAt, err := w.AgentCutAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	answeredAt, err := w.PlannerAnsweredAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	finishedAt, err := w.AgentFinishedAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	return domain.AgentStanding{Tenancy: tenancy, ClosedAt: closedAt, SignedUp: signedUp, CutAt: cutAt, PlannerAnsweredAt: answeredAt, FinishedAt: finishedAt}, nil
}
