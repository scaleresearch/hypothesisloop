package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// windDownStore holds one platform experiment in a given status with one agent cut, and records
// what a request went on to write.
type windDownStore struct {
	PlatformExperimentsStore
	status    domain.PlatformExperimentStatus
	cut       string
	keysAsked int
}

func (s *windDownStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: s.status}, nil
}

func (s *windDownStore) IsAgentCut(_ context.Context, _, agentID string) (bool, error) {
	return agentID == s.cut, nil
}

func (s *windDownStore) AgentDataKey(context.Context, string, string, func(context.Context) (domain.DataKey, error)) (domain.DataKey, error) {
	s.keysAsked++
	return domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"}, nil
}

// An agent token outlives a close or its own cut by the wind-down so its last turn can file
// summaries. What spends or opens new work is refused through that window all the same.
func TestACutAgentOrAClosedPlatformExperimentGetsNoDataKey(t *testing.T) {
	placement := authtest.Placement{
		PlatformExperiments: map[string]domain.Tenancy{"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj", OrgID: "org"}},
		Agents:              map[string]string{"agent-1": "org", "agent-cut": "org"},
	}
	for name, tc := range map[string]struct {
		status domain.PlatformExperimentStatus
		agent  string
		want   int
		reason string
	}{
		"running, agent in":      {domain.PlatformExpRunning, "agent-1", 0, ""},
		"open, agent in":         {domain.PlatformExpOpen, "agent-1", 0, ""},
		"closed":                 {domain.PlatformExpClosed, "agent-1", http.StatusConflict, "experiment_closed"},
		"running, agent cut":     {domain.PlatformExpRunning, "agent-cut", http.StatusConflict, "agent_held"},
		"closed, agent also cut": {domain.PlatformExpClosed, "agent-cut", http.StatusConflict, "experiment_closed"},
	} {
		store := &windDownStore{status: tc.status, cut: "agent-cut"}
		svc := newSignupTestService(t, store)
		client, err := objectstore.New("http://127.0.0.1:1", "us-east-1", "hl-data", "root", "root-secret")
		if err != nil {
			t.Fatal(err)
		}
		caller := authtest.As(auth.Principal{Kind: auth.KindAgent, AgentID: tc.agent, PlatformExperimentID: "pe-1", ProjectID: "prj", OrgIDs: []string{"org"}})
		r := chi.NewRouter()
		doc := apidocs.New(r, "test", "1.0.0", "", caller, auth.NewAccess(placement), zap.NewNop())
		RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, &fakeRepositories{}, zap.NewNop()))
		RegisterDataUsageHuma(doc, NewDataUsageHandler(client, store, svc, 0, zap.NewNop()))

		post := func(path, body string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, authtest.Authorize(request))
			return rec
		}
		key := post("/platform-experiments/pe-1/data-key", `{"agent_id":"`+tc.agent+`"}`)
		if tc.want == 0 {
			if key.Code != http.StatusOK || store.keysAsked != 1 {
				t.Errorf("%s: key %d %s, want it through", name, key.Code, key.Body)
			}
			continue
		}
		if key.Code != tc.want || !strings.Contains(key.Body.String(), tc.reason) || store.keysAsked != 0 {
			t.Errorf("%s: data key got = %d %s after %d asks, want = %d %s and none", name, key.Code, key.Body, store.keysAsked, tc.want, tc.reason)
		}
	}
}
