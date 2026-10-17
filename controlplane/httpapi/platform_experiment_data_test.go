package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// signupKeys stands in for the signup rows: one key per agent, kept once created.
type signupKeys struct {
	platformexperiments.PlatformExperimentsStore
	err  error
	keys map[string]domain.DataKey
}

func (s *signupKeys) AgentDataKey(ctx context.Context, _, agentID string, create func(context.Context) (domain.DataKey, error)) (domain.DataKey, error) {
	if s.err != nil {
		return domain.DataKey{}, s.err
	}
	if key, ok := s.keys[agentID]; ok {
		return key, nil
	}
	key, err := create(ctx)
	if err != nil {
		return domain.DataKey{}, err
	}
	s.keys[agentID] = key
	return key, nil
}

// GetPlatformExperiment and IsAgentCut hold every agent active in a running platform experiment.
func (s *signupKeys) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id, Status: domain.PlatformExpRunning}, nil
}

func (s *signupKeys) IsAgentCut(context.Context, string, string) (bool, error) { return false, nil }

func experimentsWithData(store platformexperiments.PlatformExperimentsStore, client *objectstore.Client) *platformexperiments.PlatformExperimentsService {
	return testExperiments(store).WithDataStore(client, 0)
}

func dataKeyRouter(t *testing.T, keys *signupKeys, storeStatus int) (http.Handler, *int) {
	t.Helper()
	created := 0
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		created++
		w.WriteHeader(storeStatus)
	}))
	t.Cleanup(store.Close)
	client, err := objectstore.New("minio", store.URL, store.URL, "us-east-1", "hl-data", "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	r := authtest.Router(authtest.Staff)
	RegisterExperimentData(apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop()), experimentsWithData(keys, client), zap.NewNop())
	return r, &created
}

func postDataKey(r http.Handler, body string) *httptest.ResponseRecorder {
	req := authtest.Authorize(httptest.NewRequest(http.MethodPost, "/platform-experiments/pe-1/data-key", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestDataKeyIsRefusedWithoutASignupOrOnceClosed(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{domain.ErrNotSignedUp, http.StatusNotFound},
		{domain.ErrPlatformExperimentClosed, http.StatusConflict},
	} {
		r, created := dataKeyRouter(t, &signupKeys{err: tc.err}, http.StatusOK)
		if rec := postDataKey(r, `{"agent_id":"agent-a"}`); rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d: %s", tc.err, rec.Code, tc.want, rec.Body)
		}
		if *created != 0 {
			t.Errorf("%v: a key was created in the store", tc.err)
		}
	}
	r, _ := dataKeyRouter(t, &signupKeys{}, http.StatusOK)
	if rec := postDataKey(r, `{"agent_id":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty agent_id: status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestDataKeyIsCreatedOnceAndTheSameOnEveryCall(t *testing.T) {
	r, created := dataKeyRouter(t, &signupKeys{keys: map[string]domain.DataKey{}}, http.StatusOK)
	var first, second domain.DataAccess
	for _, out := range []*domain.DataAccess{&first, &second} {
		rec := postDataKey(r, `{"agent_id":"agent-a"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	if first.AccessKeyID == "" || first.SecretAccessKey == "" || first != second {
		t.Fatalf("first call %+v, second %+v: want one non-empty key both times", first, second)
	}
	if *created != 1 {
		t.Fatalf("the store was asked for a key %d times, want 1", *created)
	}
	if first.URI != "s3://hl-data/pe-1/agent-a/" || first.Shared != "s3://hl-data/pe-1/" || first.Region != "us-east-1" || first.Endpoint == "" {
		t.Fatalf("access = %+v", first)
	}
}

// A key the store refused must not be stored, or the agent would hold credentials that work nowhere.
func TestDataKeyTheStoreRefusedIsNotKept(t *testing.T) {
	keys := &signupKeys{keys: map[string]domain.DataKey{}}
	r, _ := dataKeyRouter(t, keys, http.StatusBadRequest)
	if rec := postDataKey(r, `{"agent_id":"agent-a"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if len(keys.keys) != 0 {
		t.Fatalf("a refused key was stored: %+v", keys.keys)
	}
}

// windDownStore holds one platform experiment in a given status with one agent cut, and records
// what a request went on to write.
type windDownStore struct {
	platformexperiments.PlatformExperimentsStore
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
		client, err := objectstore.New("minio", "http://127.0.0.1:1", "http://127.0.0.1:1", "us-east-1", "hl-data", "root", "root-secret")
		if err != nil {
			t.Fatal(err)
		}
		caller := authtest.As(auth.Principal{Kind: auth.KindAgent, AgentID: tc.agent, PlatformExperimentID: "pe-1", ProjectID: "prj", OrgIDs: []string{"org"}})
		r := authtest.Router(caller)
		doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
		RegisterExperimentData(doc, experimentsWithData(store, client), zap.NewNop())

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
