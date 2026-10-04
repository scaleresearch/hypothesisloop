package graphdb

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
