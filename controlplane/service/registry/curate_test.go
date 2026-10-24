package registry

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// Merge, Retire and Unlink hold the graph's rules the registry relies on: only an open hypothesis
// is curated, and a repeat changes nothing. Link moves are the graph store's and are tested
// against Memgraph in storage/graphdb.
func (g *fakeGraph) Merge(_ context.Context, dupID, intoID string, actor domain.Actor, reason string) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[dupID]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.Status == domain.HypothesisMerged && h.MergedInto == intoID {
		return h, false, nil
	}
	if h.Status != domain.HypothesisOpen {
		return nil, false, graphdb.ErrNotOpen
	}
	at := g.tick()
	h.Status, h.MergedInto, h.StatusReason, h.StatusChangedAt, h.StatusActor = domain.HypothesisMerged, intoID, reason, &at, &actor
	return h, true, nil
}

func (g *fakeGraph) Retire(_ context.Context, id string, actor domain.Actor, reason string) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[id]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.Status == domain.HypothesisRetired {
		return h, false, nil
	}
	if h.Status != domain.HypothesisOpen {
		return nil, false, graphdb.ErrNotOpen
	}
	at := g.tick()
	h.Status, h.StatusReason, h.StatusChangedAt, h.StatusActor = domain.HypothesisRetired, reason, &at, &actor
	return h, true, nil
}

func (g *fakeGraph) Unlink(_ context.Context, fromID string, spec domain.LinkSpec) (bool, []domain.Link, error) {
	if _, ok := g.rows[fromID]; !ok {
		return false, nil, graphdb.ErrNotFound
	}
	key := fromID + string(spec.Type) + spec.To
	removed := g.links[key]
	delete(g.links, key)
	g.in[spec.To] = slices.DeleteFunc(g.in[spec.To], func(l domain.Link) bool { return l.HypothesisID == fromID && l.Type == spec.Type })
	return removed, []domain.Link{}, nil
}

func registered(t *testing.T, svc *Service, agentID, text string) *domain.Hypothesis {
	t.Helper()
	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), agentID, agentActor(agentID), "pe-1", text, buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis %q: got = %v, want nil", text, err)
	}
	return h
}

func requireRefusal(t *testing.T, what string, err error, status int, reason string) {
	t.Helper()
	var refused *refusal.Refusal
	if answered := (&Service{}).WriteRefusal(err); !errors.As(answered, &refused) || refused.GetStatus() != status || refused.Reason != reason {
		t.Errorf("%s: got = %v answered as %#v, want %d %s", what, err, answered, status, reason)
	}
}

// Curation rewrites what other agents' work hangs on, so it is the researcher's alone: an executor,
// a person and an agent of another platform experiment are refused, and nothing changes.
func TestOnlyAResearcherMergesRetiresAndUnlinks(t *testing.T) {
	store := newPoolStore()
	svc := poolService(store)
	ctx := context.Background()
	dup := registered(t, svc, "agent-1", "warmup helps")
	into := registered(t, svc, "agent-2", "a warmup helps")
	for _, agentID := range []string{"agent-1", "", "agent-elsewhere"} {
		_, err := svc.MergeHypothesis(ctx, dup.ID, into.ID, agentID, agentActor(agentID), "")
		requireRefusal(t, "merge by "+agentID, err, http.StatusForbidden, platformapi.ReasonResearcherOnly)
		_, err = svc.RetireHypothesis(ctx, dup.ID, agentID, agentActor(agentID), "stale")
		requireRefusal(t, "retire by "+agentID, err, http.StatusForbidden, platformapi.ReasonResearcherOnly)
		_, err = svc.UnlinkHypothesis(ctx, dup.ID, agentID, buildOnRoot[0])
		requireRefusal(t, "unlink by "+agentID, err, http.StatusForbidden, platformapi.ReasonResearcherOnly)
	}
	if dup.Status != domain.HypothesisOpen {
		t.Errorf("status after the refused: got = %s, want open", dup.Status)
	}

	merged, err := svc.MergeHypothesis(ctx, dup.ID, into.ID, "researcher-1", agentActor("researcher-1"), " same claim ")
	if err != nil || merged.Status != domain.HypothesisMerged || merged.MergedInto != into.ID || merged.StatusReason != "same claim" {
		t.Fatalf("merge by a researcher: got = (%+v, %v), want merged into %s", merged, err, into.ID)
	}
	_, err = svc.RetireHypothesis(ctx, into.ID, "researcher-1", agentActor("researcher-1"), " ")
	if !errors.Is(err, ErrReasonRequired) {
		t.Errorf("retire with no reason: got = %v, want %v", err, ErrReasonRequired)
	}
	_, err = svc.RetireHypothesis(ctx, dup.ID, "researcher-1", agentActor("researcher-1"), "stale")
	requireRefusal(t, "retire a merged one", err, http.StatusConflict, platformapi.ReasonHypothesisNotOpen)
}

// A running job is never stopped by a curation and its finding lands on the hypothesis it ran, so
// a hypothesis with one in flight is curated once it ends.
func TestACurationWaitsForTheJobsInFlight(t *testing.T) {
	store := newPoolStore()
	svc := poolService(store)
	ctx := context.Background()
	h := registered(t, svc, "agent-1", "warmup helps")
	into := registered(t, svc, "agent-2", "a warmup helps")
	for _, status := range []domain.ExperimentStatus{domain.StatusQueued, domain.StatusRunning} {
		store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, domain.StatusCompleted), jobOn("job-2", h.ID, status)}
		_, err := svc.MergeHypothesis(ctx, h.ID, into.ID, "researcher-1", agentActor("researcher-1"), "")
		requireRefusal(t, "merge with a job "+string(status), err, http.StatusConflict, platformapi.ReasonHypothesisInFlight)
		_, err = svc.RetireHypothesis(ctx, h.ID, "researcher-1", agentActor("researcher-1"), "stale")
		requireRefusal(t, "retire with a job "+string(status), err, http.StatusConflict, platformapi.ReasonHypothesisInFlight)
	}
	store.jobs = []*domain.Experiment{jobOn("job-1", h.ID, domain.StatusCompleted), jobOn("job-2", h.ID, domain.StatusFailed)}
	if _, err := svc.RetireHypothesis(ctx, h.ID, "researcher-1", agentActor("researcher-1"), "stale"); err != nil {
		t.Errorf("retire once its jobs ended: got = %v, want nil", err)
	}
}

// The owner of a merged or retired hypothesis hears of it on its stream, which drops an agent's
// own echo: the change is the researcher's, with the survivor or the reason as detail. A repeat
// and an unlink of a link that is not there announce nothing.
func TestACurationIsAnnouncedAsTheResearchersWithItsDetail(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.changes = events
	ctx := context.Background()
	dup := registered(t, svc, "agent-1", "warmup helps")
	into := registered(t, svc, "agent-2", "a warmup helps")
	stale := registered(t, svc, "agent-1", "cooldown helps")
	if _, err := svc.AddHypothesisLink(ctx, stale.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkRelated, To: into.ID}); err != nil {
		t.Fatalf("AddHypothesisLink: got = %v, want nil", err)
	}
	sent := len(events.events)
	researcher := agentActor("researcher-1")
	for range 2 {
		if _, err := svc.MergeHypothesis(ctx, dup.ID, into.ID, "researcher-1", researcher, ""); err != nil {
			t.Fatalf("MergeHypothesis: got = %v, want nil", err)
		}
		if _, err := svc.RetireHypothesis(ctx, stale.ID, "researcher-1", researcher, "superseded"); err != nil {
			t.Fatalf("RetireHypothesis: got = %v, want nil", err)
		}
	}
	for range 2 {
		if _, err := svc.UnlinkHypothesis(ctx, into.ID, "researcher-1", domain.LinkSpec{Type: domain.LinkRelated, To: dup.ID}); err != nil {
			t.Fatalf("UnlinkHypothesis an absent link: got = %v, want nil", err)
		}
	}
	got := events.events[sent:]
	want := []domain.Change{
		{Kind: platformapi.EventHypothesisStatus, Subject: dup.ID, Value: "merged", Detail: into.ID, PlatformExperimentID: "pe-1", AgentID: "researcher-1"},
		{Kind: platformapi.EventHypothesisStatus, Subject: stale.ID, Value: "retired", Detail: "superseded", PlatformExperimentID: "pe-1", AgentID: "researcher-1"},
	}
	if len(got) != len(want) {
		t.Fatalf("events: got = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Change != want[i] {
			t.Errorf("event %d: got = %+v, want %+v", i, got[i].Change, want[i])
		}
	}
}

func TestAnUnlinkIsAnnouncedWithItsTargetAsDetail(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.changes = events
	ctx := context.Background()
	a := registered(t, svc, "agent-1", "warmup helps")
	b := registered(t, svc, "agent-2", "cooldown helps")
	link := domain.LinkSpec{Type: domain.LinkContradicts, To: b.ID}
	if _, err := svc.AddHypothesisLink(ctx, a.ID, "agent-1", agentActor("agent-1"), link); err != nil {
		t.Fatalf("AddHypothesisLink: got = %v, want nil", err)
	}
	if _, err := svc.UnlinkHypothesis(ctx, a.ID, "researcher-1", link); err != nil {
		t.Fatalf("UnlinkHypothesis: got = %v, want nil", err)
	}
	got := events.events[len(events.events)-1].Change
	want := domain.Change{Kind: platformapi.EventHypothesisUnlink, Subject: a.ID, Value: "contradicts", Detail: b.ID, PlatformExperimentID: "pe-1", AgentID: "researcher-1"}
	if got != want {
		t.Errorf("unlink event: got = %+v, want %+v", got, want)
	}
}

// A researcher relinks any hypothesis of its platform experiment; an executor still links only
// its own and the ones no agent owns.
func TestAResearcherLinksAnyHypothesis(t *testing.T) {
	svc := poolService(newPoolStore())
	ctx := context.Background()
	a := registered(t, svc, "agent-1", "warmup helps")
	b := registered(t, svc, "agent-2", "cooldown helps")
	link := domain.LinkSpec{Type: domain.LinkBuildsOn, To: b.ID}
	if _, err := svc.AddHypothesisLink(ctx, a.ID, "agent-2", agentActor("agent-2"), link); !errors.Is(err, ErrHypothesisNotOwner) {
		t.Errorf("an executor linking another's: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
	if _, err := svc.AddHypothesisLink(ctx, a.ID, "researcher-1", agentActor("researcher-1"), link); err != nil {
		t.Errorf("a researcher linking another's: got = %v, want nil", err)
	}
}

// A researcher's ideas are for the executors to take: stored with no owning agent, so whoever
// tests one settles it, with the researcher kept as who registered it.
func TestAResearchersHypothesisIsUnowned(t *testing.T) {
	svc := poolService(newPoolStore())
	h := registered(t, svc, "researcher-1", "warmup helps")
	if h.AgentID != "" || h.Actor.Kind != domain.ActorAgent || h.Actor.ID != "researcher-1" {
		t.Errorf("a researcher's hypothesis: got = owner %q, actor %+v, want no owner and the researcher as actor", h.AgentID, h.Actor)
	}
}

// A researcher's watch drops its own echo by the agent a change names: its registration names it,
// though no agent owns the idea, so its own ideas do not wake it.
func TestAResearchersRegistrationIsAnnouncedAsItsOwn(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.changes = events
	h := registered(t, svc, "researcher-1", "warmup helps")
	want := domain.Change{Kind: platformapi.EventHypothesisNew, Subject: h.ID, Value: string(domain.ActorAgent), PlatformExperimentID: "pe-1", AgentID: "researcher-1"}
	if len(events.events) != 1 || events.events[0].Change != want {
		t.Errorf("events: got = %+v, want = [%+v]", events.events, want)
	}
}

// A merged or retired hypothesis is no claim anyone tests any more: no verdict, no checkout.
func TestACuratedHypothesisTakesNoVerdictAndNoCheckout(t *testing.T) {
	store := &checkoutStore{poolStore: newPoolStore()}
	svc := poolService(store)
	ctx := context.Background()
	dup := registered(t, svc, "agent-1", "warmup helps")
	into := registered(t, svc, "agent-2", "a warmup helps")
	stale := registered(t, svc, "agent-1", "cooldown helps")
	if _, err := svc.MergeHypothesis(ctx, dup.ID, into.ID, "researcher-1", agentActor("researcher-1"), ""); err != nil {
		t.Fatalf("MergeHypothesis: got = %v, want nil", err)
	}
	if _, err := svc.RetireHypothesis(ctx, stale.ID, "researcher-1", agentActor("researcher-1"), "superseded"); err != nil {
		t.Fatalf("RetireHypothesis: got = %v, want nil", err)
	}
	for h, reason := range map[*domain.Hypothesis]string{dup: platformapi.ReasonHypothesisMerged, stale: platformapi.ReasonHypothesisRetired} {
		_, err := svc.SetHypothesisStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisInconclusive})
		requireRefusal(t, "verdict on "+string(h.Status), err, http.StatusConflict, reason)
		_, err = svc.CheckOut(ctx, h, "agent-1", "hl/pe-1/a/idea")
		requireRefusal(t, "checkout of "+string(h.Status), err, http.StatusConflict, reason)
	}
	_, err := svc.SetHypothesisStatus(ctx, into.ID, "agent-2", agentActor("agent-2"), Verdict{Status: domain.HypothesisMerged})
	if err == nil {
		t.Errorf("a verdict of merged: got = nil, want an error")
	}
	_, err = svc.CheckOut(ctx, into, "researcher-1", "hl/pe-1/r/idea")
	requireRefusal(t, "a checkout by a researcher", err, http.StatusForbidden, platformapi.ReasonResearcherSubmitsNoJob)
	if len(store.recorded) != 0 {
		t.Errorf("checkouts recorded: got = %v, want none", store.recorded)
	}
}

func actionsOf(t *testing.T, svc *Service, h *domain.Hypothesis, agentID string, curates bool) []domain.HypothesisAction {
	t.Helper()
	actions, err := svc.HypothesisActions(context.Background(), h, Reader{Caller: auth.Principal{Kind: auth.KindAgent, AgentID: agentID}, Writes: true, Curates: curates})
	if err != nil {
		t.Fatalf("HypothesisActions: got = %v, want nil", err)
	}
	return actions
}

func TestTheActionsSayWhoCurates(t *testing.T) {
	svc := poolService(newPoolStore())
	ctx := context.Background()
	open := registered(t, svc, "agent-1", "warmup helps")
	settled := registered(t, svc, "agent-1", "cooldown helps")
	settled.Status = domain.HypothesisConfirmed
	retired := registered(t, svc, "agent-1", "dropout helps")
	if _, err := svc.RetireHypothesis(ctx, retired.ID, "researcher-1", agentActor("researcher-1"), "superseded"); err != nil {
		t.Fatalf("RetireHypothesis: got = %v, want nil", err)
	}
	const (
		link    = domain.HypothesisActionLink
		comment = domain.HypothesisActionComment
		merge   = domain.HypothesisActionMerge
		retire  = domain.HypothesisActionRetire
		unlink  = domain.HypothesisActionUnlink
	)
	for _, c := range []struct {
		name    string
		h       *domain.Hypothesis
		agentID string
		curates bool
		want    []domain.HypothesisAction
	}{
		{"its owner", open, "agent-1", false, []domain.HypothesisAction{link, comment}},
		{"another executor", open, "agent-2", false, []domain.HypothesisAction{comment}},
		{"a researcher on an open one", open, "researcher-1", true, []domain.HypothesisAction{link, comment, merge, retire, unlink}},
		{"a researcher on a settled one", settled, "researcher-1", true, []domain.HypothesisAction{link, comment, unlink}},
		{"a researcher on a retired one", retired, "researcher-1", true, []domain.HypothesisAction{comment}},
		{"its owner on a retired one", retired, "agent-1", false, []domain.HypothesisAction{comment}},
	} {
		if got := actionsOf(t, svc, c.h, c.agentID, c.curates); !slices.Equal(got, c.want) {
			t.Errorf("%s: got = %v, want %v", c.name, got, c.want)
		}
	}
}
