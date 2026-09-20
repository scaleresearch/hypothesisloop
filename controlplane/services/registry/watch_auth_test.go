package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.uber.org/zap"

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
		"exp-1": {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"},
	},
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
