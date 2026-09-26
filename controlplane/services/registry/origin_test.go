package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	graph                  *fakeGraph
	router                 chi.Router
	own, rivals, elsewhere *domain.Hypothesis
	// unowned is a hypothesis of pe-1 no agent owns, and seeded how many hypotheses there are.
	unowned *domain.Hypothesis
	seeded  int
}

func newRegistryAs(t *testing.T, caller auth.Principal) *registryAs {
	t.Helper()
	store := newPoolStore()
	svc := poolService(store)
	ctx := context.Background()
	seed := func(agent, pe, text string) *domain.Hypothesis {
		hyp, _, _, err := svc.RegisterHypothesis(ctx, agent, agentActor(agent), pe, text, []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: pe}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return hyp
	}
	fixture := &registryAs{store: store, graph: svc.graph.(*fakeGraph), router: chi.NewRouter(),
		own: seed("agent-1", "pe-1", "warmup helps"), rivals: seed("agent-2", "pe-1", "dropout hurts"), elsewhere: seed("agent-9", "pe-other", "decay hurts")}
	unowned, _, _, err := svc.RegisterHypothesis(ctx, "", adaActor, "pe-1", "label smoothing is neutral", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.unowned, fixture.seeded = unowned, len(fixture.graph.rows)
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

var (
	keyOne      = auth.Principal{Kind: auth.KindKey, KeyID: "key-1", OrgIDs: []string{"org-a"}}
	coordinator = auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}
	staffToken  = auth.Principal{Kind: auth.KindStaff}
)

// Who made a hypothesis, a link or a comment is the credential that sent it. agent_id says whose
// record it is, and no caller's word on who they are is taken: there is no field for it.
func TestAGraphWriteIsMadeByTheCredentialThatSentIt(t *testing.T) {
	adaNamed := domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}
	agent := domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}
	key := domain.Actor{Kind: domain.ActorKey, ID: "key-1", Name: "laptop"}
	for name, tc := range map[string]struct {
		caller auth.Principal
		fields map[string]any
		want   int
		owner  string
		actor  domain.Actor
	}{
		"an agent naming itself":             {agentOne, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", agent},
		"an agent naming nobody":             {agentOne, nil, 201, "agent-1", agent},
		"an agent naming another":            {agentOne, map[string]any{"agent_id": "agent-2"}, 403, "", domain.Actor{}},
		"an agent naming one of another org": {agentOne, map[string]any{"agent_id": "agent-9"}, 403, "", domain.Actor{}},
		"an agent claiming a person's name":  {agentOne, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"an agent as itself and a person":    {agentOne, map[string]any{"agent_id": "agent-1", "author": "Mallory"}, 422, "", domain.Actor{}},
		"a person":                           {ada, nil, 201, "", adaNamed},
		"a person claiming another's name":   {ada, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"a person for an agent":              {ada, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", adaNamed},
		"a person for another org's agent":   {ada, map[string]any{"agent_id": "agent-9"}, 404, "", domain.Actor{}},
		"a person with no name":              {nameless, nil, 201, "", domain.Actor{Kind: domain.ActorUser, ID: "user-x", Name: "user-x"}},
		"an api key":                         {keyOne, nil, 201, "", key},
		"an api key claiming a name":         {keyOne, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
		"an api key for an agent":            {keyOne, map[string]any{"agent_id": "agent-2"}, 201, "agent-2", key},
		"a coordinator":                      {coordinator, nil, 201, "", domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-1", Name: "coordinator"}},
		"a coordinator claiming a name":      {coordinator, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
		"a staff token":                      {staffToken, nil, 201, "", domain.Actor{Kind: domain.ActorStaff, Name: "staff"}},
		"a staff token claiming a name":      {staffToken, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
	} {
		f := newRegistryAs(t, tc.caller)
		f.store.actorNames = map[string]string{"user-ada": "Ada Lovelace", "key-1": "laptop"}
		type written struct {
			owner string
			actor domain.Actor
		}
		routes := map[string]struct {
			rec  *httptest.ResponseRecorder
			read func(body []byte) (written, error)
		}{
			"POST /hypotheses": {
				f.do(t, http.MethodPost, "/hypotheses", with(map[string]any{
					"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}}, tc.fields)),
				func(body []byte) (written, error) {
					var got hypothesisResponse
					if err := json.Unmarshal(body, &got); err != nil {
						return written{}, err
					}
					if got.Source != domain.SourceOf(tc.owner) {
						return written{}, errors.New("source is " + string(got.Source))
					}
					return written{got.AgentID, got.Actor}, nil
				}},
			// From a hypothesis no agent owns, which anyone may link.
			"POST /hypotheses/{id}/links": {
				f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.unowned.ID)+"/links", with(map[string]any{"type": "related", "to": f.rivals.ID}, tc.fields)),
				func(body []byte) (written, error) {
					var got []domain.Link
					if err := json.Unmarshal(body, &got); err != nil || len(got) != 1 {
						return written{}, errors.New("want one link")
					}
					return written{got[0].AgentID, got[0].Actor}, nil
				}},
			"POST /hypotheses/{id}/comments": {
				f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.own.ID)+"/comments", with(map[string]any{"text": "a note"}, tc.fields)),
				func(body []byte) (written, error) {
					var got domain.HypothesisComment
					if err := json.Unmarshal(body, &got); err != nil {
						return written{}, err
					}
					if got.Source != domain.SourceOf(tc.owner) {
						return written{}, errors.New("source is " + string(got.Source))
					}
					return written{got.AgentID, got.Actor}, nil
				}},
		}
		for route, r := range routes {
			if r.rec.Code != tc.want {
				t.Errorf("%s by %s: got = %d %s, want = %d", route, name, r.rec.Code, r.rec.Body, tc.want)
				continue
			}
			if tc.want != http.StatusCreated {
				continue
			}
			got, err := r.read(r.rec.Body.Bytes())
			if err != nil || got.owner != tc.owner || got.actor != tc.actor || strings.Contains(r.rec.Body.String(), `"author"`) {
				t.Errorf("%s by %s: got = %s (%v), want owner %q, actor %+v and no author", route, name, r.rec.Body, err, tc.owner, tc.actor)
			}
		}
		if tc.want != http.StatusCreated {
			if len(f.store.comments) != 0 || len(f.graph.rows) != f.seeded {
				t.Errorf("%s: a refused write left something behind: comments %v, %d hypotheses of %d seeded", name, f.store.comments, len(f.graph.rows), f.seeded)
			}
			continue
		}
		// What is stored is the kind and the id. The name is looked up when the record is read.
		if len(f.store.commentActors) != 1 || f.store.commentActors[0].Kind != tc.actor.Kind || f.store.commentActors[0].ID != tc.actor.ID {
			t.Errorf("%s: stored comment actors = %+v, want one of kind %s and id %q", name, f.store.commentActors, tc.actor.Kind, tc.actor.ID)
		}
	}
}

// Registering a claim that is already there answers the hypothesis as it stands: it stays whose
// it was and by whom, whoever asks again.
func TestRegisteringAnExistingClaimLeavesWhoRegisteredIt(t *testing.T) {
	f := newRegistryAs(t, ada)
	again := f.do(t, http.MethodPost, "/hypotheses", map[string]any{
		"platform_experiment_id": "pe-1", "text": "Warmup  helps", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
	var got hypothesisResponse
	if err := json.Unmarshal(again.Body.Bytes(), &got); err != nil || again.Code != http.StatusOK || !got.AlreadyExisted {
		t.Fatalf("registering the same text again: got = %d %s (%v), want 200 and already_existed", again.Code, again.Body, err)
	}
	if want := (domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}); got.AgentID != "agent-1" || got.Actor != want {
		t.Fatalf("the existing hypothesis: got agent %q actor %+v, want agent-1 and %+v", got.AgentID, got.Actor, want)
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
