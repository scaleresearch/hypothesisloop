package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// signupKeys stands in for the signup rows: one key per agent, kept once created.
type signupKeys struct {
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

func (s *signupKeys) ContinuedRuns(context.Context, string) ([]string, error) {
	return nil, nil
}

type anyoneActive struct{}

func (anyoneActive) RequireActive(context.Context, string, string) error { return nil }

func dataKeyRouter(t *testing.T, keys AgentDataKeyStore, storeStatus int) (http.Handler, *int) {
	t.Helper()
	created := 0
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		created++
		w.WriteHeader(storeStatus)
	}))
	t.Cleanup(store.Close)
	client, err := objectstore.New(store.URL, store.URL, "us-east-1", "hl-data", "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	RegisterDataUsageHuma(apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop()), NewDataUsageHandler(client, keys, anyoneActive{}, 0, zap.NewNop()))
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
		{db.ErrNotSignedUp, http.StatusNotFound},
		{db.ErrPlatformExperimentClosed, http.StatusConflict},
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
