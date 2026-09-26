package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	agentOne = auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1", PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}
	ada      = auth.Principal{Kind: auth.KindUser, UserID: "user-ada", Name: "Ada Lovelace", OrgIDs: []string{"org-a"}}
	nameless = auth.Principal{Kind: auth.KindUser, UserID: "user-x", OrgIDs: []string{"org-a"}}
)

// registryAs serves the registry's operations to one caller over a store and graph the test
// keeps, holding one hypothesis of agent-1 and one of agent-2 in pe-1 and one in pe-other.
type registryAs struct {
	store                  *poolStore
	router                 chi.Router
	own, rivals, elsewhere *domain.Hypothesis
}

func newRegistryAs(t *testing.T, caller auth.Principal) *registryAs {
	t.Helper()
	store := newPoolStore()
	svc := poolService(store)
	ctx := context.Background()
	seed := func(agent, pe, text string) *domain.Hypothesis {
		hyp, _, _, err := svc.RegisterHypothesis(ctx, agent, "", pe, text, []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: pe}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return hyp
	}
	fixture := &registryAs{store: store, router: chi.NewRouter(),
		own: seed("agent-1", "pe-1", "warmup helps"), rivals: seed("agent-2", "pe-1", "dropout hurts"), elsewhere: seed("agent-9", "pe-other", "decay hurts")}
	RegisterHuma(apidocs.New(fixture.router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(watchTenancy), zap.NewNop()), NewHandler(svc, zap.NewNop()))
	return fixture
}

func (f *registryAs) do(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, authtest.Authorize(request))
	return rec
}

func with(base map[string]any, origin map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range origin {
		out[k] = v
	}
	return out
}

// The two graph writes that take agent_id or author, as each origin an agent token might claim.
func TestAnAgentTokenWritesToTheGraphAsItsOwnAgentOnly(t *testing.T) {
	for origin, tc := range map[string]struct {
		fields map[string]any
		want   int
	}{
		"itself":                  {map[string]any{"agent_id": "agent-1"}, http.StatusCreated},
		"a person":                {map[string]any{"author": "Mallory"}, http.StatusForbidden},
		"itself and a person":     {map[string]any{"agent_id": "agent-1", "author": "Mallory"}, http.StatusForbidden},
		"another agent":           {map[string]any{"agent_id": "agent-2"}, http.StatusForbidden},
		"nobody":                  {map[string]any{}, http.StatusForbidden},
		"an agent of another org": {map[string]any{"agent_id": "agent-9"}, http.StatusForbidden},
	} {
		f := newRegistryAs(t, agentOne)
		for route, rec := range map[string]*httptest.ResponseRecorder{
			"POST /hypotheses": f.do(t, http.MethodPost, "/hypotheses", with(map[string]any{
				"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}}, tc.fields)),
			"POST /hypotheses/{id}/links": f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.own.ID)+"/links",
				with(map[string]any{"type": "related", "to": f.rivals.ID}, tc.fields)),
		} {
			if rec.Code != tc.want {
				t.Errorf("%s as %s: got = %d %s, want = %d", route, origin, rec.Code, rec.Body, tc.want)
			}
		}
		if len(f.store.humanWrites) != 0 {
			t.Errorf("as %s: an agent token left human writes %+v", origin, f.store.humanWrites)
		}
	}
}

var (
	keyOne      = auth.Principal{Kind: auth.KindKey, KeyID: "key-1", OrgIDs: []string{"org-a"}}
	coordinator = auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}
	staffToken  = auth.Principal{Kind: auth.KindStaff}
)

// Who wrote a comment is the credential that sent it. agent_id says whose note it is, and no
// caller's word on who they are is taken: there is no field for it.
func TestACommentIsWrittenByTheCredentialThatSentIt(t *testing.T) {
	for name, tc := range map[string]struct {
		caller auth.Principal
		fields map[string]any
		want   int
		owner  string
		actor  domain.Actor
	}{
		"an agent naming itself":             {agentOne, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}},
		"an agent naming nobody":             {agentOne, nil, 201, "agent-1", domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}},
		"an agent naming another":            {agentOne, map[string]any{"agent_id": "agent-2"}, 403, "", domain.Actor{}},
		"an agent naming one of another org": {agentOne, map[string]any{"agent_id": "agent-9"}, 403, "", domain.Actor{}},
		"an agent claiming a person's name":  {agentOne, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"a person":                           {ada, nil, 201, "", domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}},
		"a person claiming another's name":   {ada, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"a person for an agent":              {ada, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}},
		"a person for another org's agent":   {ada, map[string]any{"agent_id": "agent-9"}, 404, "", domain.Actor{}},
		"a person with no name":              {nameless, nil, 201, "", domain.Actor{Kind: domain.ActorUser, ID: "user-x", Name: "user-x"}},
		"an api key":                         {keyOne, nil, 201, "", domain.Actor{Kind: domain.ActorKey, ID: "key-1", Name: "laptop"}},
		"an api key claiming a name":         {keyOne, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
		"an api key for an agent":            {keyOne, map[string]any{"agent_id": "agent-2"}, 201, "agent-2", domain.Actor{Kind: domain.ActorKey, ID: "key-1", Name: "laptop"}},
		"a coordinator":                      {coordinator, nil, 201, "", domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-1", Name: "coordinator"}},
		"a coordinator claiming a name":      {coordinator, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
		"a staff token":                      {staffToken, nil, 201, "", domain.Actor{Kind: domain.ActorStaff, Name: "staff"}},
		"a staff token claiming a name":      {staffToken, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
	} {
		f := newRegistryAs(t, tc.caller)
		f.store.actorNames = map[string]string{"user-ada": "Ada Lovelace", "key-1": "laptop"}
		rec := f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.own.ID)+"/comments", with(map[string]any{"text": "a note"}, tc.fields))
		if rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, rec.Code, rec.Body, tc.want)
			continue
		}
		if tc.want != http.StatusCreated {
			if len(f.store.comments) != 0 {
				t.Errorf("%s: a refused comment was written: %v", name, f.store.comments)
			}
			continue
		}
		var got domain.HypothesisComment
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v in %s", name, err, rec.Body)
		}
		if got.AgentID != tc.owner || got.Actor != tc.actor || got.Source != domain.SourceOf(tc.owner) || strings.Contains(rec.Body.String(), `"author"`) {
			t.Errorf("%s: got = %s, want owner %q and actor %+v and no author", name, rec.Body, tc.owner, tc.actor)
		}
		// What is stored is the kind and the id. The name is looked up when the comment is read.
		stored := tc.actor
		if stored.Kind != domain.ActorUser {
			stored.Name = ""
		}
		if len(f.store.commentActors) != 1 || f.store.commentActors[0].Kind != stored.Kind || f.store.commentActors[0].ID != stored.ID {
			t.Errorf("%s: stored actors = %+v, want one of kind %s and id %q", name, f.store.commentActors, stored.Kind, stored.ID)
		}
		if len(f.store.humanWrites) != 0 {
			t.Errorf("%s: the account was kept beside the comment as well as on it: %+v", name, f.store.humanWrites)
		}
	}
}

func TestAnAgentTokenSettlesAsItselfOnly(t *testing.T) {
	f := newRegistryAs(t, agentOne)
	status := func(id, agent string) int {
		return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(id)+"/status", map[string]any{"agent_id": agent, "status": "confirmed", "despite": enoughSaid}).Code
	}
	if got := status(f.rivals.ID, "agent-2"); got != http.StatusForbidden {
		t.Errorf("settling a rival's hypothesis as the rival: got = %d, want = 403", got)
	}
	if got := status(f.own.ID, "agent-1"); got != http.StatusOK {
		t.Errorf("settling its own: got = %d, want = 200", got)
	}
}

// A signed-in user registers and links under the name the identity provider states, the account
// is kept beside it, and naming an agent makes the write that agent's.
func TestAUsersWriteCarriesTheProvidersNameAndTheirAccount(t *testing.T) {
	f := newRegistryAs(t, ada)
	register := f.do(t, http.MethodPost, "/hypotheses", map[string]any{
		"author": "Mallory", "platform_experiment_id": "pe-1", "text": "cosine decay",
		"links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
	var registered hypothesisResponse
	if err := json.Unmarshal(register.Body.Bytes(), &registered); err != nil || register.Code != http.StatusCreated {
		t.Fatalf("register: got = %d %s (%v), want = 201", register.Code, register.Body, err)
	}
	if registered.Author != "Ada Lovelace" || registered.AgentID != "" {
		t.Fatalf("registered as author %q agent %q, want Ada Lovelace and no agent", registered.Author, registered.AgentID)
	}
	link := f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(registered.ID)+"/links", map[string]any{"type": "related", "to": f.own.ID})
	if link.Code != http.StatusCreated {
		t.Fatalf("link: %d %s, want 201", link.Code, link.Body)
	}
	want := []domain.HumanWrite{
		{UserID: "user-ada", Author: "Ada Lovelace", Kind: domain.HumanWriteHypothesis, Subject: registered.ID, PlatformExperimentID: "pe-1"},
		{UserID: "user-ada", Author: "Ada Lovelace", Kind: domain.HumanWriteLink, Subject: registered.ID + " related " + f.own.ID, PlatformExperimentID: "pe-1"},
	}
	if !slices.Equal(f.store.humanWrites, want) {
		t.Fatalf("accounts kept:\n got = %+v\nwant = %+v", f.store.humanWrites, want)
	}

	again := f.do(t, http.MethodPost, "/hypotheses", map[string]any{
		"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
	if again.Code != http.StatusOK || len(f.store.humanWrites) != len(want) {
		t.Fatalf("registering the same text again: got = %d with %d accounts kept, want 200 and no new one", again.Code, len(f.store.humanWrites))
	}

}

func TestAUserWithNoNameIsToldSoAndWritesNothing(t *testing.T) {
	f := newRegistryAs(t, nameless)
	for route, rec := range map[string]*httptest.ResponseRecorder{
		"register": f.do(t, http.MethodPost, "/hypotheses", map[string]any{
			"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}}),
	} {
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no name") {
			t.Errorf("%s: got = %d %s, want 400 saying the sign-in carries no name", route, rec.Code, rec.Body)
		}
	}
}

func TestAWriteWhoseAccountCouldNotBeKeptIsAnErrorWithoutTheCause(t *testing.T) {
	f := newRegistryAs(t, ada)
	f.store.failRecord = errors.New("pq: connection refused at 10.0.0.7")
	rec := f.do(t, http.MethodPost, "/hypotheses", map[string]any{
		"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("got = %d %s, want 500 with no cause in the body", rec.Code, rec.Body)
	}
}

// Another org's hypothesis and one that does not exist are one answer, on the read and on
// every write that takes its id.
func TestAHypothesisElsewhereReadsAsAbsent(t *testing.T) {
	f := newRegistryAs(t, ada)
	for route, call := range map[string]func(id string) *httptest.ResponseRecorder{
		"GET /hypotheses/{id}": func(id string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodGet, "/hypotheses/"+url.PathEscape(id), nil)
		},
		"POST comments": func(id string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(id)+"/comments", map[string]any{"text": "x"})
		},
		"POST status": func(id string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(id)+"/status", map[string]any{"agent_id": "agent-1", "status": "confirmed"})
		},
		"POST links": func(id string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(id)+"/links", map[string]any{"agent_id": "agent-1", "type": "related", "to": f.own.ID})
		},
	} {
		elsewhere, absent := call(f.elsewhere.ID), call("hyp-that-never-was")
		if elsewhere.Code != http.StatusNotFound || absent.Code != http.StatusNotFound || elsewhere.Body.String() != absent.Body.String() {
			t.Errorf("%s: another org's answers %d %s, an absent one %d %s, want the same 404",
				route, elsewhere.Code, elsewhere.Body, absent.Code, absent.Body)
		}
	}
}

// A job token reports metrics for its own job, and an agent token for no job but its own agent's.
func TestMetricsAreAppendedByTheJobOrItsOwnAgentOnly(t *testing.T) {
	sample := map[string]any{"metric_name": "loss", "fraction_complete": 0.5, "metric_value": 1.0}
	for name, tc := range map[string]struct {
		caller auth.Principal
		job    string
		want   int
	}{
		"agent on a rival's job":        {agentOne, "exp-of-2", http.StatusForbidden},
		"agent on another org's job":    {agentOne, "exp-other", http.StatusNotFound},
		"job token on another job":      {auth.Principal{Kind: auth.KindJob, ExperimentID: "exp-1", PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}, "exp-of-2", http.StatusForbidden},
		"user of another org":           {auth.Principal{Kind: auth.KindUser, UserID: "u", OrgIDs: []string{"org-b"}}, "exp-1", http.StatusNotFound},
		"user on a job that is not one": {ada, "exp-1%2F..%2Fexp-other", http.StatusNotFound},
	} {
		f := newRegistryAs(t, tc.caller)
		if rec := f.do(t, http.MethodPost, "/experiments/"+tc.job+"/metrics", sample); rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, rec.Code, rec.Body, tc.want)
		}
	}
}
