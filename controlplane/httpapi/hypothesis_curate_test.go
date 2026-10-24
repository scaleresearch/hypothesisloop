package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

var registryResearcher = auth.Principal{Kind: auth.KindAgent, AgentID: "researcher-1", PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}}

func (g *fakeGraph) Merge(_ context.Context, dupID, intoID string, actor domain.Actor, reason string) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[dupID]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.Status != domain.HypothesisOpen {
		return nil, false, graphdb.ErrNotOpen
	}
	h.Status, h.MergedInto, h.StatusReason, h.StatusActor = domain.HypothesisMerged, intoID, reason, &actor
	return h, true, nil
}

func (g *fakeGraph) Retire(_ context.Context, id string, actor domain.Actor, reason string) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[id]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.Status != domain.HypothesisOpen {
		return nil, false, graphdb.ErrNotOpen
	}
	h.Status, h.StatusReason, h.StatusActor = domain.HypothesisRetired, reason, &actor
	return h, true, nil
}

func (g *fakeGraph) Unlink(_ context.Context, fromID string, spec domain.LinkSpec) (bool, []domain.Link, error) {
	key := fromID + string(spec.Type) + spec.To
	removed := g.links[key]
	delete(g.links, key)
	return removed, []domain.Link{}, nil
}

func refusedWith(t *testing.T, what string, rec interface {
	Result() *http.Response
}, code int, reason string) {
	t.Helper()
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || res.StatusCode != code || body["reason"] != reason {
		t.Errorf("%s: got = %d %v, want %d %s", what, res.StatusCode, body, code, reason)
	}
}

func path(parts ...string) string {
	out := ""
	for _, p := range parts {
		out += "/" + url.PathEscape(p)
	}
	return out
}

// Merge, retire and unlink are a researcher's; an executor and a person are refused with 403
// researcher_only and nothing changes.
func TestOnlyAResearcherCuratesTheGraph(t *testing.T) {
	for name, caller := range map[string]auth.Principal{"an executor": registryAgent, "a person": ada} {
		f := newRegistryAs(t, caller)
		refusedWith(t, "merge by "+name, f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "merge"), map[string]any{"into": f.rivals.ID}),
			http.StatusForbidden, platformapi.ReasonResearcherOnly)
		refusedWith(t, "retire by "+name, f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "retire"), map[string]any{"reason": "stale"}),
			http.StatusForbidden, platformapi.ReasonResearcherOnly)
		refusedWith(t, "unlink by "+name, f.do(t, http.MethodDelete, path("hypotheses", f.own.ID, "links", "builds_on", "pe-1"), nil),
			http.StatusForbidden, platformapi.ReasonResearcherOnly)
		if f.own.Status != domain.HypothesisOpen {
			t.Errorf("%s: status after the refused: got = %s, want open", name, f.own.Status)
		}
	}
}

func TestAResearcherMergesRetiresAndRelinks(t *testing.T) {
	f := newRegistryAs(t, registryResearcher)
	rec := f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "merge"), map[string]any{"into": f.rivals.ID, "reason": "same claim"})
	var merged domain.Hypothesis
	if err := json.Unmarshal(rec.Body.Bytes(), &merged); err != nil || rec.Code != http.StatusOK ||
		merged.Status != domain.HypothesisMerged || merged.MergedInto != f.rivals.ID || merged.StatusReason != "same claim" {
		t.Fatalf("merge: got = %d %s, want 200 merged into %s", rec.Code, rec.Body, f.rivals.ID)
	}
	want := domain.Change{Kind: platformapi.EventHypothesisStatus, Subject: f.own.ID, Value: "merged", Detail: f.rivals.ID, PlatformExperimentID: "pe-1", AgentID: "researcher-1"}
	if got := f.events.events[len(f.events.events)-1].Change; got != want {
		t.Errorf("merge event: got = %+v, want %+v", got, want)
	}

	refusedWith(t, "retire with no reason", f.do(t, http.MethodPost, path("hypotheses", f.unowned.ID, "retire"), map[string]any{}),
		http.StatusUnprocessableEntity, "invalid_request")
	if rec := f.do(t, http.MethodPost, path("hypotheses", f.unowned.ID, "retire"), map[string]any{"reason": "superseded"}); rec.Code != http.StatusOK {
		t.Errorf("retire: got = %d %s, want 200", rec.Code, rec.Body)
	}
	refusedWith(t, "a researcher's verdict", f.do(t, http.MethodPost, path("hypotheses", f.unowned.ID, "status"), map[string]any{"status": "inconclusive"}),
		http.StatusForbidden, platformapi.ReasonResearcherGivesNoVerdict)
	refusedWith(t, "a verdict of merged", f.do(t, http.MethodPost, path("hypotheses", f.rivals.ID, "status"), map[string]any{"status": "merged"}),
		http.StatusUnprocessableEntity, "invalid_request")

	if rec := f.do(t, http.MethodPost, path("hypotheses", f.rivals.ID, "links"), map[string]any{"type": "related", "to": f.unowned.ID}); rec.Code != http.StatusCreated {
		t.Errorf("a researcher linking an executor's hypothesis: got = %d %s, want 201", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodDelete, path("hypotheses", f.rivals.ID, "links", "related", f.unowned.ID), nil); rec.Code != http.StatusOK {
		t.Errorf("unlink: got = %d %s, want 200", rec.Code, rec.Body)
	}
	want = domain.Change{Kind: platformapi.EventHypothesisUnlink, Subject: f.rivals.ID, Value: "related", Detail: f.unowned.ID, PlatformExperimentID: "pe-1", AgentID: "researcher-1"}
	if got := f.events.events[len(f.events.events)-1].Change; got != want {
		t.Errorf("unlink event: got = %+v, want %+v", got, want)
	}
	refusedWith(t, "unlink an unknown type", f.do(t, http.MethodDelete, path("hypotheses", f.rivals.ID, "links", "extends", "pe-1"), nil),
		http.StatusUnprocessableEntity, "invalid_request")
}

func TestAMergeOfAHypothesisInFlightIsRefused(t *testing.T) {
	f := newRegistryAs(t, registryResearcher)
	f.store.jobs = append(f.store.jobs, jobOn("job-running", f.own.ID, domain.StatusRunning))
	refusedWith(t, "merge with a job running", f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "merge"), map[string]any{"into": f.rivals.ID}),
		http.StatusConflict, platformapi.ReasonHypothesisInFlight)
	refusedWith(t, "retire with a job running", f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "retire"), map[string]any{"reason": "stale"}),
		http.StatusConflict, platformapi.ReasonHypothesisInFlight)
}

func TestACurationNamingAHypothesisOutOfScopeIsNotFound(t *testing.T) {
	f := newRegistryAs(t, registryResearcher)
	for what, rec := range map[string]interface{ Result() *http.Response }{
		"merge of one elsewhere":   f.do(t, http.MethodPost, path("hypotheses", f.elsewhere.ID, "merge"), map[string]any{"into": f.rivals.ID}),
		"merge into one elsewhere": f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "merge"), map[string]any{"into": f.elsewhere.ID}),
		"retire of one elsewhere":  f.do(t, http.MethodPost, path("hypotheses", f.elsewhere.ID, "retire"), map[string]any{"reason": "stale"}),
		"unlink of one elsewhere":  f.do(t, http.MethodDelete, path("hypotheses", f.elsewhere.ID, "links", "builds_on", "pe-other"), nil),
	} {
		if code := rec.Result().StatusCode; code != http.StatusNotFound {
			t.Errorf("%s: got = %d, want 404", what, code)
		}
	}
}

func TestAClosedPlatformExperimentTakesNoCuration(t *testing.T) {
	f := newRegistryAs(t, registryResearcher)
	f.store.closed = map[string]bool{"pe-1": true}
	f.events.closed = f.store.closed
	refusedWith(t, "merge on a closed one", f.do(t, http.MethodPost, path("hypotheses", f.own.ID, "merge"), map[string]any{"into": f.rivals.ID}),
		http.StatusConflict, platformapi.ReasonExperimentClosed)
	if f.own.Status != domain.HypothesisOpen {
		t.Errorf("status after the refused: got = %s, want open", f.own.Status)
	}
}

func TestTheReadersActionsSayWhoCurates(t *testing.T) {
	for name, c := range map[string]struct {
		caller auth.Principal
		want   []domain.HypothesisAction
	}{
		"its owner":    {registryAgent, []domain.HypothesisAction{domain.HypothesisActionLink, domain.HypothesisActionComment}},
		"a researcher": {registryResearcher, []domain.HypothesisAction{domain.HypothesisActionLink, domain.HypothesisActionComment, domain.HypothesisActionMerge, domain.HypothesisActionRetire, domain.HypothesisActionUnlink}},
		"another agent": {auth.Principal{Kind: auth.KindAgent, AgentID: "agent-2", PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgIDs: []string{"org-a"}},
			[]domain.HypothesisAction{domain.HypothesisActionComment}},
	} {
		f := newRegistryAs(t, c.caller)
		a := hypothesesAPI{registry: f.svc, access: auth.NewAccess(watchTenancy), logger: zap.NewNop()}
		ctx := auth.WithPrincipal(context.Background(), c.caller)
		reader, err := a.reader(ctx, "pe-1")
		if err != nil {
			t.Fatalf("%s: reader: got = %v, want nil", name, err)
		}
		got, err := f.svc.HypothesisActions(ctx, f.own, reader)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: got = (%v, %v), want %v", name, got, err, c.want)
		}
	}
}
