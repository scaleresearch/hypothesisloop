package graphdb

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func on(ids ...string) []domain.LinkSpec {
	links := make([]domain.LinkSpec, len(ids))
	for i, id := range ids {
		links[i] = domain.LinkSpec{Type: domain.LinkBuildsOn, To: id}
	}
	return links
}

func linkSet(links []domain.Link) map[string]domain.Link {
	out := map[string]domain.Link{}
	for _, l := range links {
		out[string(l.Type)+">"+l.HypothesisID] = l
	}
	return out
}

func links(t *testing.T, s *Store, id string) domain.HypothesisLinks {
	t.Helper()
	l, err := s.Links(context.Background(), id)
	if err != nil {
		t.Fatalf("Links %s: got = %v, want nil", id, err)
	}
	return l
}

func TestMergeMovesTheLinksToTheSurvivorWithTheirProvenance(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	into := register(t, s, pe, "agent-1", "gradient clipping at norm one stabilises training", onRoot(pe))
	dup := register(t, s, pe, "agent-2", "label smoothing of ten percent lifts validation accuracy", onRoot(pe))
	child := register(t, s, pe, "agent-3", "dropout on attention weights reduces overfitting", on(dup.ID))
	if _, err := s.AddLink(ctx, child.ID, "agent-3", agentActor("agent-3"), domain.LinkSpec{Type: domain.LinkRelated, To: dup.ID}); err != nil {
		t.Fatalf("AddLink related: got = %v, want nil", err)
	}
	both := register(t, s, pe, "agent-4", "mixed precision halves the step time", on(dup.ID, into.ID))
	if _, err := s.AddLink(ctx, into.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkRelated, To: dup.ID}); err != nil {
		t.Fatalf("AddLink from into: got = %v, want nil", err)
	}
	before := linkSet(links(t, s, child.ID).Out)

	merged, changed, err := s.Merge(ctx, dup.ID, into.ID, agentActor("researcher-1"), "same claim")
	if err != nil || !changed {
		t.Fatalf("Merge: got = (%v, changed=%v), want (nil, true)", err, changed)
	}
	if merged.Status != domain.HypothesisMerged || merged.MergedInto != into.ID || merged.StatusReason != "same claim" ||
		merged.StatusActor == nil || merged.StatusActor.ID != "researcher-1" || merged.StatusChangedAt == nil {
		t.Errorf("merged: got = %+v, want merged into %s by researcher-1 with its reason", merged, into.ID)
	}
	if in := links(t, s, dup.ID).In; len(in) != 0 {
		t.Errorf("links into the duplicate: got = %v, want none", in)
	}
	if out := linkSet(links(t, s, dup.ID).Out); len(out) != 1 || out["builds_on>"+pe].HypothesisID != pe {
		t.Errorf("the duplicate's own links: got = %v, want its builds_on the objective kept", out)
	}
	after := linkSet(links(t, s, child.ID).Out)
	for _, typ := range []string{"builds_on>", "related>"} {
		moved, ok := after[typ+into.ID]
		was := before[typ+dup.ID]
		if !ok || moved.AgentID != "agent-3" || moved.Actor.ID != "agent-3" || !moved.CreatedAt.Equal(was.CreatedAt) {
			t.Errorf("%s moved: got = %+v, want agent-3's link created at %v", typ, moved, was.CreatedAt)
		}
	}
	if len(after) != 2 {
		t.Errorf("child links: got = %v, want builds_on and related to %s only", after, into.ID)
	}
	if out := links(t, s, both.ID).Out; len(out) != 1 || out[0].HypothesisID != into.ID {
		t.Errorf("a hypothesis on both: got = %v, want one builds_on %s", out, into.ID)
	}
	if out := linkSet(links(t, s, into.ID).Out); len(out) != 1 {
		t.Errorf("the survivor's links: got = %v, want its builds_on the objective only", out)
	}

	again, changed, err := s.Merge(ctx, dup.ID, into.ID, agentActor("researcher-1"), "said again")
	if err != nil || changed || again.StatusReason != "same claim" {
		t.Errorf("Merge again: got = (%+v, changed=%v, %v), want an unchanged no-op", again, changed, err)
	}
	h, _, existed, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-5", Actor: agentActor("agent-5"), PlatformExperimentID: pe,
		Text: "Label smoothing of ten percent lifts validation accuracy", Links: onRoot(pe)})
	if err != nil || !existed || h.ID != dup.ID || h.MergedInto != into.ID {
		t.Errorf("re-registering the duplicate: got = (%+v, existed=%v, %v), want the merged %s", h, existed, err, dup.ID)
	}
}

func TestMergeRefusesWhatTheGraphCannotTake(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	other := testExperiment(t, s)
	ctx := context.Background()
	researcher := agentActor("researcher-1")
	dup := register(t, s, pe, "agent-1", "warmup over five hundred steps", onRoot(pe))
	into := register(t, s, pe, "agent-1", "cosine decay to zero", onRoot(pe))
	// child builds on dup, and into builds on child: moving child's builds_on to into would close a cycle.
	child := register(t, s, pe, "agent-2", "weight decay of a tenth", on(dup.ID))
	if _, err := s.AddLink(ctx, into.ID, "agent-1", researcher, domain.LinkSpec{Type: domain.LinkBuildsOn, To: child.ID}); err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}
	only := register(t, s, pe, "agent-3", "batch norm momentum of point nine", on(dup.ID))
	elsewhere := register(t, s, other, "agent-1", "rotary embeddings everywhere", onRoot(other))
	settled := register(t, s, pe, "agent-1", "layer norm before attention", onRoot(pe))
	if _, _, err := s.UpdateStatus(ctx, settled.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed); err != nil {
		t.Fatalf("UpdateStatus: got = %v, want nil", err)
	}
	retired := register(t, s, pe, "agent-1", "longer context windows", onRoot(pe))
	if _, _, err := s.Retire(ctx, retired.ID, researcher, "stale"); err != nil {
		t.Fatalf("Retire: got = %v, want nil", err)
	}

	for _, c := range []struct {
		name      string
		dup, into string
		want      error
	}{
		{"a cycle", dup.ID, into.ID, ErrInvalidLink},
		{"into itself", dup.ID, dup.ID, ErrInvalidLink},
		{"into one that builds on it alone", dup.ID, only.ID, ErrInvalidLink},
		{"into another platform experiment", dup.ID, elsewhere.ID, ErrInvalidLink},
		{"into an unknown hypothesis", dup.ID, "no-such-id", ErrInvalidLink},
		{"into a retired one", dup.ID, retired.ID, ErrInvalidLink},
		{"a settled one", settled.ID, into.ID, ErrNotOpen},
		{"a retired one", retired.ID, into.ID, ErrNotOpen},
		{"an unknown one", "no-such-id", into.ID, ErrNotFound},
	} {
		if _, _, err := s.Merge(ctx, c.dup, c.into, researcher, ""); !errors.Is(err, c.want) {
			t.Errorf("Merge %s: got = %v, want %v", c.name, err, c.want)
		}
	}
	if h, err := s.GetHypothesis(ctx, dup.ID); err != nil || h.Status != domain.HypothesisOpen {
		t.Errorf("the refused duplicate: got = (%+v, %v), want it open", h, err)
	}
	if in := links(t, s, dup.ID).In; len(in) != 2 {
		t.Errorf("links into the refused duplicate: got = %d, want 2 unmoved", len(in))
	}
}

func TestACuratedHypothesisTakesNoLinkAndNoVerdict(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	researcher := agentActor("researcher-1")
	into := register(t, s, pe, "agent-1", "warmup over five hundred steps", onRoot(pe))
	dup := register(t, s, pe, "agent-1", "cosine decay to zero", onRoot(pe))
	retired := register(t, s, pe, "agent-1", "weight decay of a tenth", onRoot(pe))
	if _, _, err := s.Merge(ctx, dup.ID, into.ID, researcher, ""); err != nil {
		t.Fatalf("Merge: got = %v, want nil", err)
	}
	if _, _, err := s.Retire(ctx, retired.ID, researcher, "superseded by the schedule sweep"); err != nil {
		t.Fatalf("Retire: got = %v, want nil", err)
	}
	for _, id := range []string{dup.ID, retired.ID} {
		if _, err := s.AddLink(ctx, id, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkRelated, To: into.ID}); !errors.Is(err, ErrCurated) {
			t.Errorf("link from %s: got = %v, want %v", id, err, ErrCurated)
		}
		if _, err := s.AddLink(ctx, into.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkRelated, To: id}); !errors.Is(err, ErrInvalidLink) {
			t.Errorf("link to %s: got = %v, want %v", id, err, ErrInvalidLink)
		}
		if _, _, err := s.UpdateStatus(ctx, id, "agent-1", agentActor("agent-1"), domain.HypothesisInconclusive); !errors.Is(err, ErrCurated) {
			t.Errorf("verdict on %s: got = %v, want %v", id, err, ErrCurated)
		}
		if _, _, err := s.Unlink(ctx, id, domain.LinkSpec{Type: domain.LinkBuildsOn, To: pe}); !errors.Is(err, ErrCurated) {
			t.Errorf("unlink from %s: got = %v, want %v", id, err, ErrCurated)
		}
	}
	_, _, _, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-2", Actor: agentActor("agent-2"), PlatformExperimentID: pe,
		Text: "gradient noise injection", Links: on(dup.ID)})
	if !errors.Is(err, ErrInvalidLink) {
		t.Errorf("register on a merged one: got = %v, want %v", err, ErrInvalidLink)
	}
}

func TestRetireTakesAnOpenIdeaWithAReason(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	researcher := agentActor("researcher-1")
	idea := register(t, s, pe, "agent-1", "warmup over five hundred steps", onRoot(pe))
	settled := register(t, s, pe, "agent-1", "cosine decay to zero", onRoot(pe))
	if _, _, err := s.UpdateStatus(ctx, settled.ID, "agent-1", agentActor("agent-1"), domain.HypothesisRefuted); err != nil {
		t.Fatalf("UpdateStatus: got = %v, want nil", err)
	}

	if _, _, err := s.Retire(ctx, settled.ID, researcher, "stale"); !errors.Is(err, ErrNotOpen) {
		t.Errorf("Retire a settled one: got = %v, want %v", err, ErrNotOpen)
	}
	h, changed, err := s.Retire(ctx, idea.ID, researcher, "the schedule sweep covers it")
	if err != nil || !changed || h.Status != domain.HypothesisRetired || h.StatusReason != "the schedule sweep covers it" || h.StatusActor.ID != "researcher-1" {
		t.Fatalf("Retire: got = (%+v, changed=%v, %v), want it retired with its reason", h, changed, err)
	}
	if _, changed, err := s.Retire(ctx, idea.ID, researcher, "again"); err != nil || changed {
		t.Errorf("Retire again: got = (changed=%v, %v), want an unchanged no-op", changed, err)
	}
}

func TestUnlinkRemovesOneLinkButNeverTheLastBuildsOn(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	a := register(t, s, pe, "agent-1", "warmup over five hundred steps", onRoot(pe))
	b := register(t, s, pe, "agent-2", "cosine decay to zero", onRoot(pe))
	toRoot := domain.LinkSpec{Type: domain.LinkBuildsOn, To: pe}

	if _, _, err := s.Unlink(ctx, b.ID, toRoot); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("unlink the last builds_on: got = %v, want %v", err, ErrInvalidLink)
	}
	removed, out, err := s.Unlink(ctx, b.ID, domain.LinkSpec{Type: domain.LinkRelated, To: a.ID})
	if err != nil || removed || len(out) != 1 {
		t.Errorf("unlink an absent link: got = (removed=%v, %v, %v), want an unchanged no-op", removed, out, err)
	}
	if _, err := s.AddLink(ctx, b.ID, "agent-2", agentActor("agent-2"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: a.ID}); err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}
	removed, out, err = s.Unlink(ctx, b.ID, toRoot)
	if err != nil || !removed || len(out) != 1 || out[0].HypothesisID != a.ID {
		t.Errorf("relink off the objective: got = (removed=%v, %v, %v), want builds_on %s alone", removed, out, err, a.ID)
	}
	if _, _, err := s.Unlink(ctx, b.ID, domain.LinkSpec{Type: "extends", To: a.ID}); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("unlink an unknown type: got = %v, want %v", err, ErrInvalidLink)
	}
	if _, _, err := s.Unlink(ctx, "no-such-id", toRoot); !errors.Is(err, ErrNotFound) {
		t.Errorf("unlink from an unknown hypothesis: got = %v, want %v", err, ErrNotFound)
	}
}
