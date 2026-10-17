package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// Who may change an idea, by who wrote it and who asks. Only a person changes one, only a
// person's own for no agent, and an owner of its org may also remove it.
func TestOnlyTheAuthorEditsAPersonsIdeaAndTheAuthorOrAnOrgOwnerRemovesIt(t *testing.T) {
	adas := &domain.Hypothesis{ID: "h-ada", Actor: adaActor}
	forAgent := &domain.Hypothesis{ID: "h-for-agent", AgentID: "agent-1", Actor: adaActor}
	agents := &domain.Hypothesis{ID: "h-agent", AgentID: "agent-1", Actor: agentActor("agent-1")}
	byKey := &domain.Hypothesis{ID: "h-key", Actor: domain.Actor{Kind: domain.ActorKey, ID: "key-1"}}
	ada := Changer{UserID: "user-ada"}
	member := Changer{UserID: "user-bob"}
	owner := Changer{UserID: "user-own", OrgOwner: true}
	noPerson := Changer{}

	for _, tc := range []struct {
		name   string
		h      *domain.Hypothesis
		caller Changer
		remove bool
		want   string
	}{
		{"the author edits", adas, ada, false, ""},
		{"the author removes", adas, ada, true, ""},
		{"another member edits", adas, member, false, "not_the_author"},
		{"another member removes", adas, member, true, "not_the_author"},
		{"an org owner edits", adas, owner, false, "not_the_author"},
		{"an org owner removes", adas, owner, true, ""},
		{"an agent's or a key's token edits", adas, noPerson, false, "person_only"},
		{"an agent's or a key's token removes", adas, noPerson, true, "person_only"},
		{"the author edits one written for an agent", forAgent, ada, false, "agent_hypothesis"},
		{"the author removes one written for an agent", forAgent, ada, true, "agent_hypothesis"},
		{"an org owner removes an agent's", agents, owner, true, "agent_hypothesis"},
		{"an org owner removes one a key wrote", byKey, owner, true, "not_a_persons_idea"},
	} {
		refused := ChangeRefusal(tc.h, tc.caller, tc.remove)
		switch {
		case tc.want == "" && refused != nil:
			t.Errorf("%s: got = %v, want admitted", tc.name, refused)
		case tc.want != "" && (refused == nil || refused.Reason != tc.want || refused.GetStatus() != http.StatusForbidden):
			t.Errorf("%s: got = %#v, want 403 %s", tc.name, refused, tc.want)
		}
	}
}

func registerAdas(t *testing.T, svc *Service, text string) *domain.Hypothesis {
	t.Helper()
	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", text, buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	return h
}

// requireInRecord holds that err is answered with 409 hypothesis_in_record,
// naming each of names.
func requireInRecord(t *testing.T, what string, err error, names ...string) {
	t.Helper()
	var refused *refusal.Refusal
	if answered := (&Service{}).WriteRefusal(err); !errors.As(answered, &refused) || refused.GetStatus() != http.StatusConflict || refused.Reason != "hypothesis_in_record" {
		t.Fatalf("%s: got = %v, want 409 hypothesis_in_record", what, err)
	}
	for _, name := range append(names, "Add a comment") {
		if !strings.Contains(refused.Message, name) {
			t.Errorf("%s: message %q does not name %q", what, refused.Message, name)
		}
	}
}

func TestAnUntouchedIdeaIsEditedAndAnnouncesNothing(t *testing.T) {
	svc := poolService(newPoolStore())
	h := registerAdas(t, svc, "warmup helps")
	before := len(svc.changes.(*recordedEvents).events)

	edited, err := svc.EditHypothesis(context.Background(), h.ID, "a longer warmup helps")
	if err != nil {
		t.Fatalf("EditHypothesis: got = %v, want nil", err)
	}
	if edited.Text != "a longer warmup helps" || edited.Actor.Name != "user-ada" {
		t.Errorf("edited: got = %+v, want the new text by user-ada, named", edited)
	}
	if got := len(svc.changes.(*recordedEvents).events); got != before {
		t.Errorf("events after an edit: got = %d, want %d", got, before)
	}
}

func TestAnIdeaAJobTestedOrAnotherLinksToIsNeitherEditedNorRemoved(t *testing.T) {
	store := newPoolStore()
	svc := poolService(store)
	tested := registerAdas(t, svc, "warmup helps")
	store.jobs = append(store.jobs, &domain.Experiment{ID: "job-7", HypothesisID: tested.ID})

	_, err := svc.EditHypothesis(context.Background(), tested.ID, "warmup hurts")
	requireInRecord(t, "editing a tested idea", err, "job job-7", "can no longer be edited")
	err = svc.RemoveHypothesis(context.Background(), tested.ID)
	requireInRecord(t, "removing a tested idea", err, "job job-7", "can no longer be removed")
	if got, _ := svc.graph.GetHypothesis(context.Background(), tested.ID); got == nil || got.Text != "warmup helps" {
		t.Errorf("the tested idea after both refusals: got = %+v, want it as it was", got)
	}

	parent := registerAdas(t, svc, "batch size matters")
	child, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "a larger batch helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis (child): got = %v, want nil", err)
	}
	if _, err := svc.AddHypothesisLink(context.Background(), child.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: parent.ID}); err != nil {
		t.Fatalf("AddHypothesisLink: got = %v, want nil", err)
	}
	_, err = svc.EditHypothesis(context.Background(), parent.ID, "batch size does not matter")
	requireInRecord(t, "editing an idea another builds on", err, child.ID+" (builds_on)")
	err = svc.RemoveHypothesis(context.Background(), parent.ID)
	requireInRecord(t, "removing an idea another builds on", err, child.ID+" (builds_on)")
}

func TestRemovingAnUntouchedIdeaTakesItsCommentsWithIt(t *testing.T) {
	store := newPoolStore()
	svc := poolService(store)
	h := registerAdas(t, svc, "warmup helps")
	kept := registerAdas(t, svc, "dropout helps")
	for _, id := range []string{h.ID, kept.ID} {
		if _, err := svc.AddHypothesisComment(context.Background(), id, "", adaActor, "a note"); err != nil {
			t.Fatalf("AddHypothesisComment: got = %v, want nil", err)
		}
	}

	if err := svc.RemoveHypothesis(context.Background(), h.ID); err != nil {
		t.Fatalf("RemoveHypothesis: got = %v, want nil", err)
	}
	if got, _ := svc.graph.GetHypothesis(context.Background(), h.ID); got != nil {
		t.Errorf("the removed idea: got = %+v, want none", got)
	}
	if len(store.commentRows) != 1 || store.commentRows[0].HypothesisID != kept.ID {
		t.Errorf("comments after the removal: got = %+v, want only the one on %s", store.commentRows, kept.ID)
	}
	if err := svc.RemoveHypothesis(context.Background(), h.ID); !errors.Is(err, ErrHypothesisNotFound) {
		t.Errorf("removing it again: got = %v, want %v", err, ErrHypothesisNotFound)
	}
}

func TestAClosedExperimentKeepsItsIdeas(t *testing.T) {
	svc := poolService(newPoolStore())
	h := registerAdas(t, svc, "warmup helps")
	svc.changes.(*recordedEvents).closed = map[string]bool{"pe-1": true}

	if _, err := svc.EditHypothesis(context.Background(), h.ID, "warmup hurts"); !errors.Is(err, domain.ErrPlatformExperimentClosed) {
		t.Errorf("EditHypothesis on a closed experiment: got = %v, want %v", err, domain.ErrPlatformExperimentClosed)
	}
	if err := svc.RemoveHypothesis(context.Background(), h.ID); !errors.Is(err, domain.ErrPlatformExperimentClosed) {
		t.Errorf("RemoveHypothesis on a closed experiment: got = %v, want %v", err, domain.ErrPlatformExperimentClosed)
	}
}

// similarGraph is fakeGraph finding every new hypothesis close to one an agent wrote, and holding
// it back as the graph does for anyone but a signed-in person.
type similarGraph struct{ *fakeGraph }

func (g similarGraph) Register(ctx context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, []domain.SimilarHypothesis, error) {
	similar := []domain.SimilarHypothesis{{ID: "h-close", AgentID: "agent-1", Actor: agentActor("agent-1"), Status: domain.HypothesisOpen, Text: "warmup helps", Reason: "close_text"}}
	if n.Actor.Kind != domain.ActorUser {
		return nil, nil, false, similar, &graphdb.SimilarError{Similar: similar}
	}
	h, links, existed, _, err := g.fakeGraph.Register(ctx, n)
	return h, links, existed, similar, err
}

func TestAPersonsCloseIdeaIsRegisteredWithItsSimilarOnesAsAHint(t *testing.T) {
	svc := poolService(newPoolStore())
	svc.graph = similarGraph{newFakeGraph()}

	h, _, _, similar, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", "a longer warmup helps", buildOnRoot, nil)
	if err != nil || h == nil {
		t.Fatalf("RegisterHypothesis by a person: got = (%+v, %v), want it registered", h, err)
	}
	if len(similar) != 1 || similar[0].ID != "h-close" || similar[0].Actor.Name != "agent-1" {
		t.Errorf("similar: got = %+v, want h-close with its actor named", similar)
	}
	body, err := json.Marshal(HypothesisResponse{Hypothesis: h, Links: []domain.Link{}, Similar: similar})
	if err != nil || !strings.Contains(string(body), `"similar":[{"id":"h-close"`) {
		t.Errorf("the response: got = %s (%v), want similar beside the hypothesis", body, err)
	}

	_, _, _, _, err = svc.RegisterHypothesis(context.Background(), "agent-2", agentActor("agent-2"), "pe-1", "a longer warmup helps a lot", buildOnRoot, nil)
	held, ok := (&Service{}).WriteRefusal(err).(*similarHypothesesError)
	if !ok || held.GetStatus() != http.StatusConflict || held.Reason != "similar_hypotheses" || len(held.Similar) != 1 {
		t.Errorf("an agent's close claim: got = %v, want 409 similar_hypotheses with the close one", err)
	}
}

// An agent held back for a refuted parent reads how to keep building on it.
func TestTheHoldSaysHowToKeepBuildingOnARefutedParent(t *testing.T) {
	for _, says := range []string{"distinct_from (hl: --distinct-from ID)", "keep the builds_on link", "changes what refuted it"} {
		if !strings.Contains(similarDetail, says) {
			t.Errorf("the 409 similar_hypotheses message does not say %q", says)
		}
	}
}
