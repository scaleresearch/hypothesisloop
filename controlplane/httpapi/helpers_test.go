package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes/agenttypestest"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/models"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// testModels is the models the tests' platform serves: platformexperimentstest.Catalog's.
var testModels = models.New(map[string]models.Provider{domain.ModelProviderAnthropic: models.Catalog(platformexperimentstest.Catalog)}, platformexperimentstest.Defaults)

// testExperiments is the platform experiments service over store, with the models it serves
// and nothing else wired.
func testExperiments(store platformexperiments.Store) *platformexperiments.Service {
	return platformexperiments.NewService(store, domain.QuotaConfig{}, zap.NewNop(), nil, 3*time.Minute).WithServedModels(testModels)
}

var agentsTestPlacement = authtest.Placement{
	PlatformExperiments: map[string]domain.Tenancy{
		"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "project-a", OrgID: "org-a"},
	},
	Agents: map[string]string{"veteran": "org-a", "dana": "org-a", "outsider": "org-b"},
}

func call(t *testing.T, r http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code, out
}

// catalogStore is what the stores of these tests are made from: the built-in agent types are the
// catalog of every platform experiment and project, and every other method is left to whoever
// embeds it.
type catalogStore struct {
	platformexperiments.Store
}

func (catalogStore) AgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (catalogStore) ProjectAgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (catalogStore) OnCoresOnly(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// fakeStore is what the stores of these tests are made from: the built-in agent types are the
// catalog of every platform experiment and project, and every other method is left to whoever
// embeds it.
type fakeStore struct {
	platformexperiments.Store
}

func (fakeStore) AgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (fakeStore) ProjectAgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypestest.Catalog(), nil
}

func (fakeStore) OnCoresOnly(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// registerPlatformExperimentsAndBudget registers the platform experiment operations and their budget reads.
func registerPlatformExperimentsAndBudget(doc *Doc, peh *platformexperiments.Service) {
	registerPlatformExperiments(doc, peh, zap.NewNop())
	registerPlatformExperimentBudget(doc, peh, zap.NewNop())
}

// platformexperimentstest.Settings is the platform settings a service of these tests applies and holds a create to.
var testPlanning = platformexperiments.Planning{Limit: 30 * time.Minute, ProposedFor: 7 * 24 * time.Hour, PlannerKept: 10 * time.Minute, AutoAccept: 30 * time.Second}

// reaching has the org of every project reach the given accelerator types and no other.
func reaching(svc *platformexperiments.Service, types ...domain.AcceleratorType) *platformexperiments.Service {
	reachable := map[string][]domain.Cluster{}
	for _, t := range types {
		reachable[string(t)] = []domain.Cluster{{Name: "k3s"}}
	}
	svc.WithReachable(func(context.Context, string) (map[string][]domain.Cluster, error) { return reachable, nil })
	return svc
}

func newSignupTestService(t *testing.T, store platformexperiments.Store) *platformexperiments.Service {
	t.Helper()
	return reaching(platformexperiments.NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), 3*time.Minute).WithServedModels(testModels), domain.CPUType)
}
