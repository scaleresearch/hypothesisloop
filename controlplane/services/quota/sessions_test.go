package quota

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
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
	r := authtest.Router(authtest.As(caller))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
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

func TestASessionIsWrittenByItsOwnerAndReadByItsOwnerAndTheOrgThatOwnsTheExperiment(t *testing.T) {
	placement := authtest.Placement{PlatformExperiments: map[string]domain.Tenancy{
		"pe-1":    sessionPlacement.PlatformExperiments["pe-1"],
		"pe-2":    sessionPlacement.PlatformExperiments["pe-2"],
		"pe-show": {PlatformExperimentID: "pe-show", ProjectID: "prj-a", OrgID: "org-a", Shared: true},
	}}
	inOrg, refused := []string{"org-a"}, []int{http.StatusForbidden, http.StatusNotFound}
	callers := []struct {
		name               string
		caller             auth.Principal
		agents, coordinate int
	}{
		{"the agent", agentOne, http.StatusOK, http.StatusForbidden},
		{"another agent of the experiment", agentTwo, http.StatusForbidden, http.StatusForbidden},
		{"the coordinator", coordinator, http.StatusForbidden, http.StatusOK},
		{"an owner of the org", auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: inOrg}, http.StatusOK, http.StatusOK},
		{"an org key", auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg}, http.StatusOK, http.StatusOK},
		{"a key of the project", auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg, ProjectID: "prj-a"}, http.StatusOK, http.StatusOK},
		{"staff", auth.Principal{Kind: auth.KindStaff}, http.StatusOK, http.StatusOK},
		{"a key of another project", auth.Principal{Kind: auth.KindKey, OrgIDs: inOrg, ProjectID: "prj-b"}, http.StatusNotFound, http.StatusNotFound},
		{"a person of another org", auth.Principal{Kind: auth.KindUser, UserID: "u-2", OrgIDs: []string{"org-b"}}, http.StatusNotFound, http.StatusNotFound},
		{"a key of another org", auth.Principal{Kind: auth.KindKey, OrgIDs: []string{"org-b"}}, http.StatusNotFound, http.StatusNotFound},
		{"the same agent id elsewhere", auth.Principal{Kind: auth.KindAgent, OrgIDs: inOrg, ProjectID: "prj-a", PlatformExperimentID: "pe-2", AgentID: "ag-1"}, http.StatusNotFound, http.StatusForbidden},
		{"the coordinator of another experiment", auth.Principal{Kind: auth.KindCoordinator, OrgIDs: inOrg, ProjectID: "prj-a", PlatformExperimentID: "pe-2"}, http.StatusNotFound, http.StatusNotFound},
	}
	for _, c := range callers {
		store := &memorySessions{stored: map[string][]byte{"pe-1/ag-1": []byte("the agent's"), "pe-1/": []byte("the coordinator's")}}
		for path, want := range map[string]int{agentSession: c.agents, coordinatorSession: c.coordinate} {
			code, out := sessionCall(t, store, placement, c.caller, "GET", path, nil)
			if code != want {
				t.Errorf("%s, GET %s: %d %s, want %d", c.name, path, code, out, want)
			}
			owns := c.name == "the agent" && path == agentSession || c.name == "the coordinator" && path == coordinatorSession
			code, out = sessionCall(t, store, placement, c.caller, "PUT", path, []byte("overwritten"))
			if owns != (code == http.StatusOK) || !owns && code != refused[0] && code != refused[1] {
				t.Errorf("%s, PUT %s: %d %s, want only its owner to store it", c.name, path, code, out)
			}
		}
		for key, was := range map[string]string{"pe-1/ag-1": "the agent's", "pe-1/": "the coordinator's"} {
			owns := c.name == "the agent" && key == "pe-1/ag-1" || c.name == "the coordinator" && key == "pe-1/"
			if got := string(store.stored[key]); !owns && got != was {
				t.Errorf("%s: a refused write left %q stored", c.name, got)
			}
		}
	}

	// What staff share with every account is the platform experiment, and no session of it.
	store := &memorySessions{stored: map[string][]byte{"pe-show/ag-1": []byte("the agent's")}}
	outsider := auth.Principal{Kind: auth.KindUser, UserID: "u-2", OrgIDs: []string{"org-b"}}
	if code, out := sessionCall(t, store, placement, outsider, "GET", "/platform-experiments/pe-show/agents/ag-1/session", nil); code != http.StatusNotFound {
		t.Errorf("a person of another org on a shared platform experiment: %d %s, want 404", code, out)
	}
}

// Nothing about a session depends on the platform experiment's status: the org reads it for as
// long as it is kept, and stores none whenever it asks.
func TestTheOrgReadsASessionAndStoresNone(t *testing.T) {
	store := &memorySessions{stored: map[string][]byte{"pe-1/ag-1": []byte("the agent's")}}
	for name, caller := range map[string]auth.Principal{
		"an owner of the org": {Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}},
		"an org key":          {Kind: auth.KindKey, OrgIDs: []string{"org-a"}},
		"staff":               {Kind: auth.KindStaff},
	} {
		if code, out := sessionCall(t, store, sessionPlacement, caller, "GET", agentSession, nil); code != http.StatusOK || string(out) != "the agent's" {
			t.Errorf("%s: %d %s, want the stored session", name, code, out)
		}
		if code, out := sessionCall(t, store, sessionPlacement, caller, "PUT", agentSession, []byte("overwritten")); code != http.StatusForbidden {
			t.Errorf("%s storing: %d %s, want 403", name, code, out)
		}
	}
}

// A coordinator's last turn is the one that writes the summary and closes, so its session is
// stored with the token as it resolves once the experiment is closed, which leaves it reads and
// the writes marked for after the close. In four real runs that turn was never stored: the
// write was refused as one of the others. The token itself ends with the wind-down.
func TestACoordinatorStoresItsSessionAfterTheClose(t *testing.T) {
	store := &memorySessions{stored: map[string][]byte{}}
	coordinator := coordinator
	coordinator.PlatformExperimentClosed, coordinator.RunOver = true, true
	if code, body := sessionCall(t, store, sessionPlacement, coordinator, "PUT", coordinatorSession, []byte("last turn")); code != http.StatusOK {
		t.Fatalf("storing after the close: %d %s, want 200", code, body)
	}
	if code, body := sessionCall(t, store, sessionPlacement, coordinator, "GET", coordinatorSession, nil); code != http.StatusOK || string(body) != "last turn" {
		t.Fatalf("reading after the close: %d %s, want what was stored", code, body)
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
