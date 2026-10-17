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
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

// testExperiments is the platform experiments service over store, with nothing else wired.
func testExperiments(store platformexperiments.PlatformExperimentsStore) *platformexperiments.PlatformExperimentsService {
	return platformexperiments.NewPlatformExperimentsService(store, domain.QuotaConfig{}, zap.NewNop(), "", 3*time.Minute)
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
	platformexperiments.PlatformExperimentsStore
}

func (catalogStore) AgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypes.Catalog(), nil
}

func (catalogStore) ProjectAgentTypeCatalog(context.Context, string) (domain.AgentTypeCatalog, error) {
	return agenttypes.Catalog(), nil
}
