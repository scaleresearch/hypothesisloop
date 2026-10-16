package quota

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type messageStore struct {
	fakeStore
	to      []string
	text    string
	sender  domain.Actor
	refuse  error
	limited []db.MessagesRateLimited
	readFor *string
}

func (s *messageStore) SendAgentMessages(_ context.Context, _ string, to []string, text string, sender domain.Actor) ([]domain.AgentMessage, []db.MessagesRateLimited, error) {
	if s.refuse != nil {
		return nil, nil, s.refuse
	}
	s.to, s.text, s.sender = to, text, sender
	return []domain.AgentMessage{{AgentID: "veteran", From: domain.MessageFrom(sender), Text: text, Actor: sender, At: time.Now()}}, s.limited, nil
}

func (s *messageStore) SendCoordinatorMessage(_ context.Context, _, text string, sender domain.Actor) (*domain.AgentMessage, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	s.to, s.text, s.sender = []string{domain.CoordinatorRecipient}, text, sender
	return &domain.AgentMessage{AgentID: domain.CoordinatorRecipient, From: domain.MessageFrom(sender), Text: text, Actor: sender, At: time.Now()}, nil
}

func (s *messageStore) ListAgentMessages(_ context.Context, _, agentID string) ([]domain.AgentMessage, error) {
	s.readFor = &agentID
	return nil, nil
}

func (s *messageStore) NameActors(context.Context, []*domain.Actor) error { return nil }

func messagesRouter(t *testing.T, store *messageStore, caller auth.Principal) http.Handler {
	t.Helper()
	r := authtest.Router(authtest.As(caller))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerMessagesHuma(doc, NewPlatformExperimentsHandler(newSignupTestService(t, store), nil, testPlanning, zap.NewNop()))
	return r
}

// The coordinator and the owner message an agent, or every researcher; an agent messages nobody
// and reads only its own; a send past the hourly limit says when the next can go.
func TestTheCoordinatorAndTheOwnerMessageAgentsAndAnAgentReadsItsOwn(t *testing.T) {
	coordinator := auth.Principal{Kind: auth.KindCoordinator, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1"}
	store := &messageStore{}
	code, out := call(t, messagesRouter(t, store, coordinator), "POST", "/platform-experiments/pe-1/messages", `{"agent_id":"veteran","text":"  stop the lr line, try warmup  "}`)
	if code != http.StatusCreated || len(store.to) != 1 || store.to[0] != "veteran" || store.text != "stop the lr line, try warmup" || store.sender.Kind != domain.ActorCoordinator {
		t.Fatalf("coordinator's message: got = %d %v, stored to %v %q by %+v", code, out, store.to, store.text, store.sender)
	}
	if sent := out["messages"].([]any)[0].(map[string]any); sent["from"] != "coordinator" {
		t.Errorf("answered: got = %v, want from coordinator", sent)
	}

	owner := auth.Principal{Kind: auth.KindUser, UserID: "user-1", OrgIDs: []string{"org-a"}}
	store = &messageStore{}
	if code, out := call(t, messagesRouter(t, store, owner), "POST", "/platform-experiments/pe-1/messages", `{"all":true,"text":"queue more trials"}`); code != http.StatusCreated || store.to != nil || store.sender.Kind != domain.ActorUser {
		t.Fatalf("owner's message to all: got = %d %v, to %v by %+v", code, out, store.to, store.sender)
	}

	for body, reason := range map[string]string{
		`{"agent_id":"veteran","all":true,"text":"x"}`: "invalid_recipient",
		`{"all":true,"coordinator":true,"text":"x"}`:   "invalid_recipient",
		`{"coordinator":true,"text":"x"}`:              "invalid_recipient",
		`{"text":"x"}`:                                 "invalid_recipient",
		`{"agent_id":"veteran","text":"   "}`:          "empty_message",
		`{"agent_id":"veteran","text":"` + strings.Repeat("x", domain.MaxAgentMessageBytes+1) + `"}`: "message_too_long",
	} {
		if code, out := call(t, messagesRouter(t, &messageStore{}, coordinator), "POST", "/platform-experiments/pe-1/messages", body); code != http.StatusUnprocessableEntity || out["reason"] != reason {
			t.Errorf("%.60s: got = %d %v, want 422 %s", body, code, out, reason)
		}
	}
	limited := &messageStore{refuse: &db.MessagesRateLimited{AgentID: "veteran", From: "coordinator", Next: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}}
	if code, out := call(t, messagesRouter(t, limited, coordinator), "POST", "/platform-experiments/pe-1/messages", `{"agent_id":"veteran","text":"again"}`); code != http.StatusTooManyRequests || out["reason"] != "message_rate_limited" || !strings.Contains(out["message"].(string), "2026-10-09T12:00:00Z") || !strings.Contains(out["message"].(string), "by the coordinator") {
		t.Errorf("past the limit: got = %d %v, want 429 message_rate_limited with when", code, out)
	}

	// A message to every researcher reaches those not rate limited and names the others.
	partial := &messageStore{limited: []db.MessagesRateLimited{{AgentID: "dana", From: "owner", Next: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}}}
	code, out = call(t, messagesRouter(t, partial, owner), "POST", "/platform-experiments/pe-1/messages", `{"all":true,"text":"queue more trials"}`)
	refused, _ := out["refused"].([]any)
	if code != http.StatusCreated || len(out["messages"].([]any)) != 1 || len(refused) != 1 {
		t.Fatalf("all with one limited: got = %d %v, want the sent and the refused", code, out)
	}
	if r := refused[0].(map[string]any); r["agent_id"] != "dana" || r["reason"] != "message_rate_limited" || r["retry_after"] != "2026-10-09T12:00:00Z" {
		t.Errorf("refused = %v, want dana, message_rate_limited and when", r)
	}
	if _, listed := out["refused"]; !listed {
		t.Error("refused missing")
	}
	if code, out := call(t, messagesRouter(t, &messageStore{}, owner), "POST", "/platform-experiments/pe-1/messages", `{"all":true,"text":"queue more trials"}`); code != http.StatusCreated || out["refused"] != nil {
		t.Errorf("all with none limited: got = %d %v, want no refused", code, out)
	}
	allLimited := &messageStore{refuse: &db.MessagesRateLimited{AgentID: "dana", From: "owner", Next: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}}
	if code, out := call(t, messagesRouter(t, allLimited, owner), "POST", "/platform-experiments/pe-1/messages", `{"all":true,"text":"again"}`); code != http.StatusTooManyRequests || out["reason"] != "message_rate_limited" {
		t.Errorf("all with every one limited: got = %d %v, want 429", code, out)
	}

	// A person steers the coordinator the same way, and is told where none is hosted.
	store = &messageStore{}
	if code, out := call(t, messagesRouter(t, store, owner), "POST", "/platform-experiments/pe-1/messages", `{"coordinator":true,"text":"model the gap"}`); code != http.StatusCreated ||
		len(store.to) != 1 || store.to[0] != domain.CoordinatorRecipient || store.text != "model the gap" || store.sender.Kind != domain.ActorUser {
		t.Fatalf("owner's message to the coordinator: got = %d %v, to %v %q by %+v", code, out, store.to, store.text, store.sender)
	}
	if code, out := call(t, messagesRouter(t, &messageStore{refuse: db.ErrNoCoordinator}, owner), "POST", "/platform-experiments/pe-1/messages", `{"coordinator":true,"text":"x"}`); code != http.StatusConflict || out["reason"] != "no_coordinator" {
		t.Errorf("to no hosted coordinator: got = %d %v, want 409 no_coordinator", code, out)
	}

	agent := auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{"org-a"}, ProjectID: "project-a", PlatformExperimentID: "pe-1", AgentID: "veteran"}
	if code, _ := call(t, messagesRouter(t, &messageStore{}, agent), "POST", "/platform-experiments/pe-1/messages", `{"agent_id":"dana","text":"hi"}`); code != http.StatusForbidden {
		t.Errorf("an agent's message: got = %d, want 403", code)
	}
	store = &messageStore{}
	if code, out := call(t, messagesRouter(t, store, agent), "GET", "/platform-experiments/pe-1/messages?agent_id=dana", ""); code != http.StatusOK || store.readFor == nil || *store.readFor != "veteran" || out["messages"] == nil {
		t.Errorf("an agent's read: got = %d %v, read for %v, want its own, as a list", code, out, store.readFor)
	}
}
