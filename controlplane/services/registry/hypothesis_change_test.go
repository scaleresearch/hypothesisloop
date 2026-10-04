package registry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
		refusal := ChangeRefusal(tc.h, tc.caller, tc.remove)
		switch {
		case tc.want == "" && refusal != nil:
			t.Errorf("%s: got = %v, want admitted", tc.name, refusal)
		case tc.want != "" && (refusal == nil || refusal.Reason != tc.want || refusal.GetStatus() != http.StatusForbidden):
			t.Errorf("%s: got = %#v, want 403 %s", tc.name, refusal, tc.want)
		}
	}
}

func registerAdas(t *testing.T, svc *Service, text string) *domain.Hypothesis {
	t.Helper()
	h, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", text, buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	return h
}

// requireInRecord holds that err is answered with 409 hypothesis_in_record,
// naming each of names.
func requireInRecord(t *testing.T, what string, err error, names ...string) {
	t.Helper()
	var refusal *api.Refusal
	if answered := (&Handler{}).hypothesisWriteError(err); !errors.As(answered, &refusal) || refusal.GetStatus() != http.StatusConflict || refusal.Reason != "hypothesis_in_record" {
		t.Fatalf("%s: got = %v, want 409 hypothesis_in_record", what, err)
	}
	for _, name := range append(names, "Add a comment") {
		if !strings.Contains(refusal.Message, name) {
			t.Errorf("%s: message %q does not name %q", what, refusal.Message, name)
		}
	}
}

func TestAnUntouchedIdeaIsEditedAndAnnouncesNothing(t *testing.T) {
	svc := poolService(newPoolStore())
	h := registerAdas(t, svc, "warmup helps")
	before := len(svc.events.(*recordedEvents).events)

	edited, err := svc.EditHypothesis(context.Background(), h.ID, "a longer warmup helps")
	if err != nil {
		t.Fatalf("EditHypothesis: got = %v, want nil", err)
	}
	if edited.Text != "a longer warmup helps" || edited.Actor.Name != "user-ada" {
		t.Errorf("edited: got = %+v, want the new text by user-ada, named", edited)
	}
	if got := len(svc.events.(*recordedEvents).events); got != before {
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
	child, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "a larger batch helps", buildOnRoot, nil)
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
	svc.events.(*recordedEvents).closed = map[string]bool{"pe-1": true}

	if _, err := svc.EditHypothesis(context.Background(), h.ID, "warmup hurts"); !errors.Is(err, db.ErrPlatformExperimentClosed) {
		t.Errorf("EditHypothesis on a closed experiment: got = %v, want %v", err, db.ErrPlatformExperimentClosed)
	}
	if err := svc.RemoveHypothesis(context.Background(), h.ID); !errors.Is(err, db.ErrPlatformExperimentClosed) {
		t.Errorf("RemoveHypothesis on a closed experiment: got = %v, want %v", err, db.ErrPlatformExperimentClosed)
	}
}
