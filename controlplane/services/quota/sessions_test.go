package quota

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

type memorySessions struct {
	stored map[string][]byte
	err    error
}

func (m *memorySessions) PutSession(_ context.Context, pe, agent string, snapshot []byte) error {
	if m.err != nil {
		return m.err
	}
	// The body is the request's own buffer, which the next request is read into.
	m.stored[pe+"/"+agent] = bytes.Clone(snapshot)
	return nil
}

func (m *memorySessions) GetSession(_ context.Context, pe, agent string, _ int64) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	snapshot, found := m.stored[pe+"/"+agent]
	if !found {
		return nil, objectstore.ErrNoSession
	}
	return snapshot, nil
}

func sessionCall(t *testing.T, store SessionStore, placement authtest.Placement, caller auth.Principal, method, path string, body []byte) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
	RegisterSessionsHuma(doc, store, zap.NewNop())
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	return rec.Code, rec.Body.Bytes()
}

var (
	sessionPlacement = authtest.Placement{PlatformExperiments: map[string]domain.Tenancy{
		"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"},
		"pe-2": {PlatformExperimentID: "pe-2", ProjectID: "prj-a", OrgID: "org-a"},
	}}
	agentOne    = auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1", AgentID: "ag-1"}
	agentTwo    = auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1", AgentID: "ag-2"}
	coordinator = auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "prj-a", PlatformExperimentID: "pe-1"}
)

const (
	agentSession       = "/platform-experiments/pe-1/agents/ag-1/session"
	coordinatorSession = "/platform-experiments/pe-1/coordinator/session"
)

func TestAnAgentAndACoordinatorEachStoreAndReadBackTheirOwnSessionByteForByte(t *testing.T) {
	store := &memorySessions{stored: map[string][]byte{}}
	snapshot := []byte{0x1f, 0x8b, 0x00, 0xff, '\n', 0x00, 'x'}
	for path, caller := range map[string]auth.Principal{agentSession: agentOne, coordinatorSession: coordinator} {
		if code, body := sessionCall(t, store, sessionPlacement, caller, "GET", path, nil); code != http.StatusNotFound {
			t.Fatalf("GET %s before anything was stored: %d %s, want 404", path, code, body)
		}
		if code, body := sessionCall(t, store, sessionPlacement, caller, "PUT", path, snapshot); code != http.StatusOK || !strings.Contains(string(body), `"size_bytes":7`) {
			t.Fatalf("PUT %s: %d %s", path, code, body)
		}
		code, body := sessionCall(t, store, sessionPlacement, caller, "GET", path, nil)
		if code != http.StatusOK || !bytes.Equal(body, snapshot) {
			t.Fatalf("GET %s: %d %v, want the bytes that were stored", path, code, body)
		}
	}
	if len(store.stored) != 2 || store.stored["pe-1/ag-1"] == nil || store.stored["pe-1/"] == nil {
		t.Fatalf("stored under %v, want the agent's and the coordinator's own place", store.stored)
	}
	if code, body := sessionCall(t, store, sessionPlacement, agentOne, "PUT", agentSession, []byte{}); code != http.StatusBadRequest {
		t.Fatalf("an empty session: %d %s, want 400", code, body)
	}
}

func TestNobodyButItsOwnerReadsOrWritesASession(t *testing.T) {
	store := &memorySessions{stored: map[string][]byte{"pe-1/ag-1": []byte("the agent's"), "pe-1/": []byte("the coordinator's")}}
	inOrg := []string{"org-a"}
	others := map[string]auth.Principal{
		"another agent of the experiment": agentTwo,
		"the coordinator":                 coordinator,
		"an owner of the org":             {Kind: auth.KindUser, UserID: "u-1", OrgIDs: inOrg},
		"an org key":                      {Kind: auth.KindKey, OrgIDs: inOrg},
		"staff":                           {Kind: auth.KindStaff},
		"the same agent id elsewhere":     {Kind: auth.KindAgent, OrgIDs: inOrg, ProjectID: "prj-a", PlatformExperimentID: "pe-2", AgentID: "ag-1"},
	}
	for name, caller := range others {
		for _, method := range []string{"GET", "PUT"} {
			var body []byte
			if method == "PUT" {
				body = []byte("overwritten")
			}
			if code, out := sessionCall(t, store, sessionPlacement, caller, method, agentSession, body); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Errorf("%s, %s of an agent's session: %d %s, want it refused", name, method, code, out)
			}
		}
	}
	delete(others, "the coordinator")
	others["an agent of the experiment"] = agentOne
	for name, caller := range others {
		for _, method := range []string{"GET", "PUT"} {
			var body []byte
			if method == "PUT" {
				body = []byte("overwritten")
			}
			if code, out := sessionCall(t, store, sessionPlacement, caller, method, coordinatorSession, body); code != http.StatusForbidden && code != http.StatusNotFound {
				t.Errorf("%s, %s of the coordinator's session: %d %s, want it refused", name, method, code, out)
			}
		}
	}
	if string(store.stored["pe-1/ag-1"]) != "the agent's" || string(store.stored["pe-1/"]) != "the coordinator's" {
		t.Fatalf("a refused write changed what is stored: %q", store.stored)
	}
}

// A coordinator's token still reads after the close. It stores nothing more once the wind-down
// is over, as nothing else is accepted from a run that ended.
func TestASessionIsStoredThroughTheWindDownAndNoLonger(t *testing.T) {
	closedFor := func(d time.Duration) authtest.Placement {
		closedAt := time.Now().Add(-d)
		return authtest.Placement{
			PlatformExperiments: sessionPlacement.PlatformExperiments,
			ClosedAt:            map[string]time.Time{"pe-1": closedAt},
		}
	}
	store := &memorySessions{stored: map[string][]byte{}}
	if code, body := sessionCall(t, store, closedFor(domain.AgentWindDown-10*time.Second), coordinator, "PUT", coordinatorSession, []byte("last turn")); code != http.StatusOK {
		t.Fatalf("inside the wind-down: %d %s, want 200", code, body)
	}
	over := closedFor(domain.AgentWindDown + time.Second)
	for path, caller := range map[string]auth.Principal{agentSession: agentOne, coordinatorSession: coordinator} {
		if code, body := sessionCall(t, store, over, caller, "PUT", path, []byte("too late")); code != http.StatusConflict {
			t.Fatalf("PUT %s after the wind-down: %d %s, want 409", path, code, body)
		}
	}
	if code, body := sessionCall(t, store, over, coordinator, "GET", coordinatorSession, nil); code != http.StatusOK || string(body) != "last turn" {
		t.Fatalf("reading after the wind-down: %d %s, want what was stored in it", code, body)
	}
}

func TestAStoreThatFailsAnswersWithoutItsWords(t *testing.T) {
	store := &memorySessions{err: errors.New("objectstore: put sessions/pe-1/agents/ag-1/session: status 403: SecretKey=abc")}
	for _, method := range []string{"GET", "PUT"} {
		code, body := sessionCall(t, store, sessionPlacement, agentOne, method, agentSession, []byte("x"))
		if code != http.StatusInternalServerError || strings.Contains(string(body), "SecretKey") {
			t.Fatalf("%s: %d %s, want 500 with no detail of the store", method, code, body)
		}
	}
}

func TestASessionAboveTheBoundIsRefusedBeforeItIsRead(t *testing.T) {
	store := &memorySessions{stored: map[string][]byte{}}
	code, body := sessionCall(t, store, sessionPlacement, agentOne, "PUT", agentSession, make([]byte, domain.MaxAgentSessionBytes+1))
	if code != http.StatusRequestEntityTooLarge || len(store.stored) != 0 {
		t.Fatalf("a session one byte above the bound: %d %s, want 413 and nothing stored", code, body)
	}
}
