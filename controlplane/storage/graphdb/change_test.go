package graphdb

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func registerAs(t *testing.T, s *Store, pe, text string, links []domain.LinkSpec) *domain.Hypothesis {
	t.Helper()
	h, _, _, _, err := s.Register(context.Background(), NewHypothesis{Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: text, Links: links})
	if err != nil {
		t.Fatalf("Register %q: got = %v, want nil", text, err)
	}
	return h
}

func TestEditTextReplacesTheTextOfAnIdeaNothingLinksTo(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	h := registerAs(t, s, pe, "warmup helps", onRoot(pe))
	other := registerAs(t, s, pe, "dropout helps", onRoot(pe))
	ctx := context.Background()

	edited, err := s.EditText(ctx, h.ID, "a longer warmup helps")
	if err != nil {
		t.Fatalf("EditText: got = %v, want nil", err)
	}
	if edited.Text != "a longer warmup helps" || edited.Actor != userActor("user-ada") || !edited.CreatedAt.Equal(h.CreatedAt) {
		t.Errorf("edited: got = %+v, want the new text and everything else as it was", edited)
	}
	again, _, existed, _, err := s.Register(ctx, NewHypothesis{Actor: userActor("user-bob"), PlatformExperimentID: pe, Text: "A longer  warmup helps", Links: onRoot(pe)})
	if err != nil || !existed || again.ID != h.ID {
		t.Errorf("registering the new text: got = (%+v, existed=%v, %v), want the edited hypothesis", again, existed, err)
	}
	if _, err := s.EditText(ctx, h.ID, "Dropout helps"); !errors.Is(err, ErrDuplicateText) {
		t.Errorf("editing to the text of %s: got = %v, want %v", other.ID, err, ErrDuplicateText)
	}
	if _, err := s.EditText(ctx, "no-such-hypothesis", "t"); !errors.Is(err, ErrNotFound) {
		t.Errorf("editing a hypothesis that does not exist: got = %v, want %v", err, ErrNotFound)
	}
}

func TestRemoveDeletesAnIdeaWithItsOwnLinksAndRefusesOneOthersLinkTo(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	parent := registerAs(t, s, pe, "batch size matters", onRoot(pe))
	related := registerAs(t, s, pe, "learning rate matters", onRoot(pe))
	h := registerAs(t, s, pe, "a larger batch helps", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: parent.ID}, {Type: domain.LinkRelated, To: related.ID}})
	ctx := context.Background()

	var linked *LinkedError
	if err := s.Remove(ctx, parent.ID); !errors.As(err, &linked) || len(linked.In) != 1 || linked.In[0].HypothesisID != h.ID || linked.In[0].Type != domain.LinkBuildsOn {
		t.Fatalf("removing an idea another builds on: got = %v, want a LinkedError naming %s builds_on", err, h.ID)
	}
	if _, err := s.EditText(ctx, related.ID, "the learning rate matters most"); !errors.As(err, &linked) || linked.In[0].Type != domain.LinkRelated {
		t.Errorf("editing an idea another is related to: got = %v, want a LinkedError naming the related link", err)
	}

	if err := s.Remove(ctx, h.ID); err != nil {
		t.Fatalf("Remove: got = %v, want nil", err)
	}
	if got, err := s.GetHypothesis(ctx, h.ID); err != nil || got != nil {
		t.Errorf("the removed idea: got = (%+v, %v), want none", got, err)
	}
	for _, id := range []string{parent.ID, related.ID} {
		links, err := s.Links(ctx, id)
		if err != nil || len(links.In) != 0 {
			t.Errorf("links into %s after the removal: got = (%+v, %v), want none", id, links.In, err)
		}
	}
	if err := s.Remove(ctx, parent.ID); err != nil {
		t.Errorf("removing the parent once nothing builds on it: got = %v, want nil", err)
	}
	if err := s.Remove(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing it again: got = %v, want %v", err, ErrNotFound)
	}
}

// A person's idea is never held back: what an agent would be refused for comes back as a hint
// beside the idea, which is registered.
func TestRegisterTakesAPersonsCloseIdeaAndListsWhatItReadsCloseTo(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	bs8 := register(t, s, pe, "agent-1", "batch size 8 beats batch size 32 at the same epoch budget", onRoot(pe))
	register(t, s, pe, "agent-1", "label noise caps validation accuracy", onRoot(pe))
	text := "batch size 8 does not beat batch size 32 at the same epoch budget"

	_, _, _, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-2", Actor: agentActor("agent-2"), PlatformExperimentID: pe, Text: text, Links: onRoot(pe)})
	if got := similarIDs(t, err); len(got) != 1 || got[bs8.ID] != "close_text" {
		t.Fatalf("an agent's close claim: got = %v, want refused naming %s", got, bs8.ID)
	}

	h, _, existed, similar, err := s.Register(ctx, NewHypothesis{Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: text, Links: onRoot(pe)})
	if err != nil || existed || h == nil {
		t.Fatalf("a person's close idea: got = (%+v, existed=%v, %v), want it registered", h, existed, err)
	}
	if len(similar) != 1 || similar[0].ID != bs8.ID || similar[0].Reason != "close_text" {
		t.Errorf("similar beside a person's idea: got = %+v, want %s as close_text", similar, bs8.ID)
	}

	_, _, _, similar, err = s.Register(ctx, NewHypothesis{Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: "dropout helps", Links: onRoot(pe)})
	if err != nil || len(similar) != 0 {
		t.Errorf("a person's idea close to nothing: got = (%+v, %v), want no similar", similar, err)
	}
}

// Building on a refuted idea is research when the new claim changes what refuted it: naming the
// parent in distinct_from is all it takes, and the parent stays the parent.
func TestARefutedParentNamedInDistinctFromStaysTheParent(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	parent := register(t, s, pe, "agent-1", "learning rate 0.2 overshoots", onRoot(pe))
	other := register(t, s, pe, "agent-2", "gradient clipping steadies training", onRoot(pe))
	if _, _, err := s.UpdateStatus(ctx, parent.ID, "agent-1", agentActor("agent-1"), domain.HypothesisRefuted); err != nil {
		t.Fatal(err)
	}
	combo := NewHypothesis{AgentID: "agent-1", Actor: agentActor("agent-1"), PlatformExperimentID: pe,
		Text:  "with clipping at norm 1, a step of 0.2 converges within forty epochs",
		Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: parent.ID}, {Type: domain.LinkBuildsOn, To: other.ID}}}
	if _, _, _, _, err := s.Register(ctx, combo); similarIDs(t, err)[parent.ID] != "refuted_parent" {
		t.Fatalf("building on a refuted parent unnamed: got = %v, want it held for %s", err, parent.ID)
	}
	combo.DistinctFrom = []string{parent.ID}
	h, links, existed, _, err := s.Register(ctx, combo)
	if err != nil || existed {
		t.Fatalf("with the refuted parent in distinct_from: got = (%v, existed=%v), want it registered", err, existed)
	}
	parents := map[string]bool{}
	for _, l := range links {
		if l.Type == domain.LinkBuildsOn {
			parents[l.HypothesisID] = true
		}
	}
	if !parents[parent.ID] || !parents[other.ID] || len(parents) != 2 {
		t.Errorf("%s builds on: got = %v, want %s and %s", h.ID, parents, parent.ID, other.ID)
	}
}
