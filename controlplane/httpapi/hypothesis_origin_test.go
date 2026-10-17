package httpapi

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

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

var (
	registryAgent = auth.Principal{Kind: auth.KindAgent, AgentID: "agent-1", PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}
	ada           = auth.Principal{Kind: auth.KindUser, UserID: "user-ada", Name: "Ada Lovelace", OrgIDs: []string{"org-a"}}
	nameless      = auth.Principal{Kind: auth.KindUser, UserID: "user-x", OrgIDs: []string{"org-a"}}
)

// registryAs serves the registry's operations to one caller over a store and graph the test
// keeps, holding one hypothesis of agent-1 and one of agent-2 in pe-1 and one in pe-other.
type registryAs struct {
	store                  *poolStore
	graph                  *fakeGraph
	events                 *recordedEvents
	router                 chi.Router
	own, rivals, elsewhere *domain.Hypothesis
	// unowned is a hypothesis of pe-1 no agent owns, and seeded how many hypotheses there are.
	unowned *domain.Hypothesis
	seeded  int
}

func newRegistryAs(t *testing.T, caller auth.Principal) *registryAs {
	t.Helper()
	store, graph, events := newPoolStore(), newFakeGraph(), &recordedEvents{}
	svc := registry.New(store, graph, events, zap.NewNop(), "", nil)
	ctx := context.Background()
	seed := func(agent, pe, text string) *domain.Hypothesis {
		hyp, _, _, _, err := svc.RegisterHypothesis(ctx, agent, agentActor(agent), pe, text, []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: pe}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return hyp
	}
	fixture := &registryAs{store: store, graph: graph, events: events, router: authtest.Router(authtest.As(caller)),
		own: seed("agent-1", "pe-1", "warmup helps"), rivals: seed("agent-2", "pe-1", "dropout hurts"), elsewhere: seed("agent-9", "pe-other", "decay hurts")}
	unowned, _, _, _, err := svc.RegisterHypothesis(ctx, "", adaActor, "pe-1", "label smoothing is neutral", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.unowned, fixture.seeded = unowned, len(fixture.graph.rows)
	for _, h := range []*domain.Hypothesis{fixture.own, fixture.rivals, unowned} {
		store.jobs = append(store.jobs, jobOn("job-"+h.ID, h.ID, domain.StatusFailed))
	}
	RegisterRegistry(apidocs.New(fixture.router, "test", "1.0.0", "", auth.NewAccess(watchTenancy), zap.NewNop()), svc, zap.NewNop())
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
	keyOne              = auth.Principal{Kind: auth.KindKey, KeyID: "key-1", OrgIDs: []string{"org-a"}}
	registryCoordinator = auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}
	staffToken          = auth.Principal{Kind: auth.KindStaff}
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
		"an agent naming itself":             {registryAgent, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", agent},
		"an agent naming nobody":             {registryAgent, nil, 201, "agent-1", agent},
		"an agent naming another":            {registryAgent, map[string]any{"agent_id": "agent-2"}, 403, "", domain.Actor{}},
		"an agent naming one of another org": {registryAgent, map[string]any{"agent_id": "agent-9"}, 403, "", domain.Actor{}},
		"an agent claiming a person's name":  {registryAgent, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"an agent as itself and a person":    {registryAgent, map[string]any{"agent_id": "agent-1", "author": "Mallory"}, 422, "", domain.Actor{}},
		"a person":                           {ada, nil, 201, "", adaNamed},
		"a person claiming another's name":   {ada, map[string]any{"author": "Mallory"}, 422, "", domain.Actor{}},
		"a person for an agent":              {ada, map[string]any{"agent_id": "agent-1"}, 201, "agent-1", adaNamed},
		"a person for another org's agent":   {ada, map[string]any{"agent_id": "agent-9"}, 404, "", domain.Actor{}},
		"a person with no name":              {nameless, nil, 201, "", domain.Actor{Kind: domain.ActorUser, ID: "user-x", Name: "user-x"}},
		"an api key":                         {keyOne, nil, 201, "", key},
		"an api key claiming a name":         {keyOne, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
		"an api key for an agent":            {keyOne, map[string]any{"agent_id": "agent-2"}, 201, "agent-2", key},
		"a coordinator":                      {registryCoordinator, nil, 201, "", domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-1", Name: "coordinator"}},
		"a coordinator claiming a name":      {registryCoordinator, map[string]any{"author": "Ada Lovelace"}, 422, "", domain.Actor{}},
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
					var got registry.HypothesisResponse
					if err := json.Unmarshal(body, &got); err != nil {
						return written{}, err
					}
					if bytes.Contains(body, []byte(`"source"`)) {
						return written{}, errors.New("the answer still states a source")
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
					if bytes.Contains(body, []byte(`"source"`)) {
						return written{}, errors.New("the answer still states a source")
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

// Once a platform experiment closed, only the coordinator token was refused hypothesis writes, by
// the gate: a person or an agent still added ideas, links, verdicts and comments to its graph.
// Every caller is refused each of them with the one 409, and every caller makes them while it is
// open.
func TestAClosedPlatformExperimentTakesNoHypothesisWriteFromAnyone(t *testing.T) {
	closedCoordinator := registryCoordinator
	closedCoordinator.PlatformExperimentClosed = true
	writes := map[string]func(f *registryAs) *httptest.ResponseRecorder{
		"POST /hypotheses": func(f *registryAs) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses", map[string]any{
				"platform_experiment_id": "pe-1", "text": "cosine decay", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
		},
		"POST /hypotheses/{id}/links": func(f *registryAs) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.unowned.ID)+"/links", map[string]any{"type": "related", "to": f.rivals.ID})
		},
		"POST /hypotheses/{id}/status": func(f *registryAs) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.unowned.ID)+"/status", map[string]any{"status": "refuted", "despite": enoughSaid})
		},
		"POST /hypotheses/{id}/comments": func(f *registryAs) *httptest.ResponseRecorder {
			return f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(f.unowned.ID)+"/comments", map[string]any{"text": "a note"})
		},
	}
	for name, callers := range map[string]struct{ open, closed auth.Principal }{
		"a person":        {ada, ada},
		"an agent":        {registryAgent, registryAgent},
		"an api key":      {keyOne, keyOne},
		"the coordinator": {registryCoordinator, closedCoordinator},
	} {
		for route, write := range writes {
			f := newRegistryAs(t, callers.closed)
			f.store.closed = map[string]bool{"pe-1": true}
			f.events.closed = f.store.closed
			rec := write(f)
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusConflict || len(body) != 2 ||
				body["reason"] != "experiment_closed" || body["message"] == "" {
				t.Errorf("%s by %s on a closed platform experiment: got = %d %s, want 409 experiment_closed in {reason, message}", route, name, rec.Code, rec.Body)
			}
			if len(f.store.comments) != 0 || len(f.graph.rows) != f.seeded || len(f.graph.links) != 0 || f.graph.rows[f.unowned.ID].Status != domain.HypothesisOpen {
				t.Errorf("%s by %s on a closed platform experiment left something behind", route, name)
			}

			f = newRegistryAs(t, callers.open)
			if rec := write(f); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Errorf("%s by %s while open: got = %d %s, want it made", route, name, rec.Code, rec.Body)
			}
		}
	}
}

// Registering a claim that is already there answers the hypothesis as it stands: it stays whose
// it was and by whom, whoever asks again.
func TestRegisteringAnExistingClaimLeavesWhoRegisteredIt(t *testing.T) {
	f := newRegistryAs(t, ada)
	again := f.do(t, http.MethodPost, "/hypotheses", map[string]any{
		"platform_experiment_id": "pe-1", "text": "Warmup  helps", "links": []map[string]string{{"type": "builds_on", "to": "pe-1"}}})
	var got registry.HypothesisResponse
	if err := json.Unmarshal(again.Body.Bytes(), &got); err != nil || again.Code != http.StatusOK || !got.AlreadyExisted {
		t.Fatalf("registering the same text again: got = %d %s (%v), want 200 and already_existed", again.Code, again.Body, err)
	}
	if want := (domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}); got.AgentID != "agent-1" || got.Actor != want {
		t.Fatalf("the existing hypothesis: got agent %q actor %+v, want agent-1 and %+v", got.AgentID, got.Actor, want)
	}
}

// A verdict is the owning agent's, or anyone's on a hypothesis no agent owns, and who gave it is
// the credential that sent it.
func TestAVerdictIsGivenByTheCredentialThatSentIt(t *testing.T) {
	adaNamed := domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "Ada Lovelace"}
	agent := domain.Actor{Kind: domain.ActorAgent, ID: "agent-1", Name: "agent-1"}
	for name, tc := range map[string]struct {
		caller auth.Principal
		on     func(f *registryAs) string
		fields map[string]any
		want   int
		actor  domain.Actor
	}{
		"an agent on its own, named":              {registryAgent, func(f *registryAs) string { return f.own.ID }, map[string]any{"agent_id": "agent-1"}, 200, agent},
		"an agent on its own, naming nobody":      {registryAgent, func(f *registryAs) string { return f.own.ID }, nil, 200, agent},
		"an agent on a rival's, as the rival":     {registryAgent, func(f *registryAs) string { return f.rivals.ID }, map[string]any{"agent_id": "agent-2"}, 403, domain.Actor{}},
		"an agent on a rival's, as itself":        {registryAgent, func(f *registryAs) string { return f.rivals.ID }, nil, 403, domain.Actor{}},
		"an agent on one nobody owns":             {registryAgent, func(f *registryAs) string { return f.unowned.ID }, nil, 200, agent},
		"a person for the owning agent":           {ada, func(f *registryAs) string { return f.own.ID }, map[string]any{"agent_id": "agent-1"}, 200, adaNamed},
		"a person for an agent that does not own": {ada, func(f *registryAs) string { return f.own.ID }, map[string]any{"agent_id": "agent-2"}, 403, domain.Actor{}},
		"a person naming nobody on an agent's":    {ada, func(f *registryAs) string { return f.own.ID }, nil, 403, domain.Actor{}},
		"a person on one nobody owns":             {ada, func(f *registryAs) string { return f.unowned.ID }, nil, 200, adaNamed},
		"a person stating who they are":           {ada, func(f *registryAs) string { return f.unowned.ID }, map[string]any{"author": "Mallory"}, 422, domain.Actor{}},
		"a key for the owning agent":              {keyOne, func(f *registryAs) string { return f.own.ID }, map[string]any{"agent_id": "agent-1"}, 200, domain.Actor{Kind: domain.ActorKey, ID: "key-1", Name: "laptop"}},
		"a staff token on one nobody owns":        {staffToken, func(f *registryAs) string { return f.unowned.ID }, nil, 200, domain.Actor{Kind: domain.ActorStaff, Name: "staff"}},
	} {
		f := newRegistryAs(t, tc.caller)
		f.store.actorNames = map[string]string{"user-ada": "Ada Lovelace", "key-1": "laptop"}
		id := tc.on(f)
		before := *f.graph.rows[id]
		rec := f.do(t, http.MethodPost, "/hypotheses/"+url.PathEscape(id)+"/status", with(map[string]any{"status": "refuted", "despite": enoughSaid}, tc.fields))
		if rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, rec.Code, rec.Body, tc.want)
			continue
		}
		if tc.want != http.StatusOK {
			if after := f.graph.rows[id]; after.Status != before.Status || after.StatusActor != nil || len(f.store.comments) != 0 {
				t.Errorf("%s: a refused verdict left the hypothesis %s by %+v with notes %v", name, after.Status, after.StatusActor, f.store.comments)
			}
			continue
		}
		var got domain.Hypothesis
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v in %s", name, err, rec.Body)
		}
		if got.Status != domain.HypothesisRefuted || got.StatusActor == nil || *got.StatusActor != tc.actor || got.AgentID != before.AgentID {
			t.Errorf("%s: got = %s, want refuted by %+v and still %q's", name, rec.Body, tc.actor, before.AgentID)
		}
		if len(f.store.commentActors) != 1 || f.store.commentActors[0].Kind != tc.actor.Kind || f.store.commentActors[0].ID != tc.actor.ID {
			t.Errorf("%s: the basis was written by %+v, want the one who gave the verdict", name, f.store.commentActors)
		}
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

// A limit over the maximum was clamped to it, so a client asking for 500 read 200 as the whole
// set. It is refused, as every list refuses it.
func TestAHypothesisPageOverTheMaximumIsRefused(t *testing.T) {
	f := newRegistryAs(t, ada)
	for _, path := range []string{
		"/hypotheses?limit=201",
		"/hypotheses?limit=-1",
		"/hypotheses?offset=-1",
		"/hypotheses/" + url.PathEscape(f.own.ID) + "?limit=201",
		"/hypotheses/" + url.PathEscape(f.own.ID) + "?offset=-1",
	} {
		rec := f.do(t, http.MethodGet, path, nil)
		var refusal struct{ Reason string }
		if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil || rec.Code != http.StatusUnprocessableEntity || refusal.Reason != "invalid_page" {
			t.Errorf("GET %s: got = %d %s, want 422 invalid_page", path, rec.Code, rec.Body.String())
		}
	}
	for limit, want := range map[int]int{0: defaultPageLimit, 1: 1, maxPageLimit: maxPageLimit} {
		if got, _, err := page(limit, 0); err != nil || got != want {
			t.Errorf("page(%d, 0): got = (%d, %v), want %d", limit, got, err, want)
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
		"agent on a rival's job":        {registryAgent, "exp-of-2", http.StatusForbidden},
		"agent on another org's job":    {registryAgent, "exp-other", http.StatusNotFound},
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
