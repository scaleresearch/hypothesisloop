package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var watchTenancy = authtest.Placement{
	PlatformExperiments: map[string]domain.Tenancy{
		"pe-1":     {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"},
		"pe-other": {PlatformExperimentID: "pe-other", ProjectID: "prj-b", OrgID: "org-b"},
	},
	Experiments: map[string]domain.Tenancy{
		"exp-1":     {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a", AgentID: "agent-1"},
		"exp-of-2":  {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a", AgentID: "agent-2"},
		"exp-other": {PlatformExperimentID: "pe-other", ProjectID: "prj-b", OrgID: "org-b", AgentID: "agent-9"},
	},
	Agents: map[string]string{"agent-1": "org-a", "agent-2": "org-a", "agent-9": "org-b"},
}

func watchServerAs(t *testing.T, principal auth.Principal) (*httptest.Server, *WatchHandler) {
	t.Helper()
	source := newFakeEvents()
	handler := NewWatchHandler(source, &lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}},
		authtest.As(principal), auth.NewAccess(watchTenancy), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx)
	<-source.listening
	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	t.Cleanup(func() {
		server.Close()
		cancel()
	})
	return server, handler
}

func watchStatus(t *testing.T, server *httptest.Server, query string, protocols ...string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/watch?"+query, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, p := range protocols {
		request.Header.Add("Sec-WebSocket-Protocol", p)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response.Body.Close()
	return response.StatusCode
}

// The upgrade is refused with 400 once a request is let through, since these are plain GETs. A
// 401, 403 or 404 before that is the access decision under test.
func TestWatchTakesItsTokenFromTheSubprotocolOnly(t *testing.T) {
	server, _ := watchServerAs(t, auth.Principal{Kind: auth.KindStaff})
	query := "platform_experiment_id=pe-1"
	for name, tc := range map[string]struct {
		protocols []string
		want      int
	}{
		"no subprotocol":            {nil, http.StatusUnauthorized},
		"subprotocol without token": {[]string{WatchSubprotocol}, http.StatusUnauthorized},
		"token without subprotocol": {[]string{"test"}, http.StatusUnauthorized},
		"token first":               {[]string{"test, " + WatchSubprotocol}, http.StatusUnauthorized},
		"a third value":             {[]string{WatchSubprotocol + ", test, extra"}, http.StatusUnauthorized},
		"one header":                {[]string{WatchSubprotocol + ", test"}, http.StatusBadRequest},
		"two headers":               {[]string{WatchSubprotocol, "test"}, http.StatusBadRequest},
	} {
		if got := watchStatus(t, server, query, tc.protocols...); got != tc.want {
			t.Errorf("%s: got = %d, want = %d", name, got, tc.want)
		}
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/watch?"+query, nil)
	request.Header.Set("Authorization", "Bearer test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("token in Authorization: got = %d, want = 401, the subprotocol is the one way in", response.StatusCode)
	}
}

func TestWatchKeepsToTheCallersScope(t *testing.T) {
	offer := WatchSubprotocol + ", test"
	orgA := []string{"org-a"}
	for name, tc := range map[string]struct {
		principal auth.Principal
		query     string
		want      int
	}{
		"user in the org":                   {auth.Principal{Kind: auth.KindUser, OrgIDs: orgA}, "platform_experiment_id=pe-1", http.StatusBadRequest},
		"user of another org":               {auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-b"}}, "platform_experiment_id=pe-1", http.StatusNotFound},
		"user of another org, by job":       {auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-b"}}, "experiment_id=exp-1", http.StatusNotFound},
		"key bound to another project":      {auth.Principal{Kind: auth.KindKey, OrgIDs: orgA, ProjectID: "prj-z"}, "platform_experiment_id=pe-1", http.StatusNotFound},
		"agent of this experiment":          {auth.Principal{Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-1", OrgIDs: orgA}, "platform_experiment_id=pe-1", http.StatusBadRequest},
		"agent of another experiment":       {auth.Principal{Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-other", OrgIDs: []string{"org-b"}}, "platform_experiment_id=pe-1", http.StatusNotFound},
		"coordinator of another experiment": {auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-other", OrgIDs: []string{"org-b"}}, "experiment_id=exp-1", http.StatusNotFound},
		"job token":                         {auth.Principal{Kind: auth.KindJob, ExperimentID: "exp-1", PlatformExperimentID: "pe-1", OrgIDs: orgA}, "experiment_id=exp-1", http.StatusForbidden},
		"runtime token":                     {auth.Principal{Kind: auth.KindRuntime, ClusterName: "c", OrgIDs: orgA}, "platform_experiment_id=pe-1", http.StatusForbidden},
	} {
		server, _ := watchServerAs(t, tc.principal)
		if got := watchStatus(t, server, tc.query, offer); got != tc.want {
			t.Errorf("%s: got = %d, want = %d", name, got, tc.want)
		}
	}
}

// An agent's subscription is its own whatever it asks for: the agent filter comes from the token.
func TestAnAgentTokenWatchesOnlyItsOwnAgent(t *testing.T) {
	server, handler := watchServerAs(t, auth.Principal{Kind: auth.KindAgent, AgentID: "agent-a", PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}})
	for _, asked := range []string{"agent-b", ""} {
		query := url.Values{"platform_experiment_id": {"pe-1"}}
		if asked != "" {
			query.Set("agent", asked)
		}
		client := dialWatch(t, server, query)
		client.sendText(t, "{}")
		if got := readSubscription(t, client).AgentID; got != "agent-a" {
			t.Errorf("asking for agent %q: subscribed as %q, want agent-a", asked, got)
		}
		client.conn.Close()
	}
	if got := handler.hub.list("pe-1", "agent-b"); len(got) != 0 {
		t.Errorf("subscriptions narrowed to agent-b: got = %d, want = 0", len(got))
	}
}

// switchable answers as one principal until refused or failed is set.
type switchable struct {
	mu        sync.Mutex
	principal auth.Principal
	err       error
}

func (s *switchable) Authenticate(context.Context, string) (auth.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principal, s.err
}

func (s *switchable) set(principal auth.Principal, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal, s.err = principal, err
}

// streamEnds reports whether the server closes the connection within the wait.
func streamEnds(client *watchClient, wait time.Duration) bool {
	_ = client.conn.SetReadDeadline(time.Now().Add(wait))
	_, err := io.Copy(io.Discard, client.r)
	var timeout net.Error
	return !(errors.As(err, &timeout) && timeout.Timeout())
}

// A stream outlives the request that opened it, so the token is asked again on every ping.
func TestAnOpenWatchStreamClosesOnceItsTokenIsRefused(t *testing.T) {
	inOrgA := auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a"}}
	for name, tc := range map[string]struct {
		principal auth.Principal
		err       error
		closes    bool
	}{
		"the token died":                         {auth.Principal{}, auth.Forbidden("revoked"), true},
		"the user left the org":                  {auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-b"}}, nil, true},
		"the token became one that cannot watch": {auth.Principal{Kind: auth.KindRuntime, ClusterName: "c", OrgIDs: []string{"org-a"}}, nil, true},
		"nothing changed":                        {inOrgA, nil, false},
		"the directory is down":                  {auth.Principal{}, errors.New("database is down"), false},
	} {
		source := newFakeEvents()
		caller := &switchable{principal: inOrgA}
		handler := NewWatchHandler(source, &lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}},
			caller, auth.NewAccess(watchTenancy), zap.NewNop())
		handler.recheckEvery = 20 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		handler.Start(ctx)
		<-source.listening
		server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))

		client := dialWatch(t, server, url.Values{"platform_experiment_id": {"pe-1"}})
		client.sendText(t, "{}")
		readSubscription(t, client)
		caller.set(tc.principal, tc.err)
		if got := streamEnds(client, 400*time.Millisecond); got != tc.closes {
			t.Errorf("%s: stream closed = %v, want = %v", name, got, tc.closes)
		}
		client.conn.Close()
		server.Close()
		cancel()
	}
}

// Naming a hypothesis of a platform experiment the caller cannot reach answers 404 before the
// service is asked, whose own refusal would name that hypothesis's platform experiment.
func TestAWriteNamingAHypothesisOutOfScopeIsNotFound(t *testing.T) {
	svc := poolService(newPoolStore())
	ctx := context.Background()
	own, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, _, err := svc.RegisterHypothesis(ctx, "agent-9", agentActor("agent-9"), "pe-other", "decay hurts",
		[]domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-other"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	caller := authtest.As(auth.Principal{Kind: auth.KindUser, UserID: "user-1", OrgIDs: []string{"org-a"}})
	RegisterHuma(apidocs.New(r, "test", "1.0.0", "", caller, auth.NewAccess(watchTenancy), zap.NewNop()), NewHandler(svc, zap.NewNop()))
	post := func(path string, body map[string]any) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, authtest.Authorize(request))
		return rec
	}
	link := func(from, to string) *httptest.ResponseRecorder {
		return post("/hypotheses/"+url.PathEscape(from)+"/links", map[string]any{"agent_id": "agent-1", "type": "related", "to": to})
	}
	register := func(buildsOn string, distinctFrom ...string) *httptest.ResponseRecorder {
		return post("/hypotheses", map[string]any{
			"agent_id": "agent-1", "platform_experiment_id": "pe-1", "text": "cosine decay",
			"links": []map[string]string{{"type": "builds_on", "to": buildsOn}}, "distinct_from": distinctFrom,
		})
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"link to it":         link(own.ID, foreign.ID),
		"link from it":       link(foreign.ID, own.ID),
		"build on it":        register(foreign.ID),
		"told apart from it": register("pe-1", foreign.ID),
	} {
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "pe-other") {
			t.Errorf("%s: got = %d %s, want 404 naming nothing of the other platform experiment", name, rec.Code, rec.Body.String())
		}
	}
	if rec := register("pe-1"); rec.Code >= 300 {
		t.Errorf("a hypothesis inside the caller's platform experiment: got = %d %s", rec.Code, rec.Body.String())
	}
}
