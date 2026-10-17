package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// SendMessageRequest is the body of POST /platform-experiments/{id}/messages.
type SendMessageRequest struct {
	AgentID     string `json:"agent_id,omitempty" doc:"The agent to send it to. Exactly one of agent_id, all and coordinator."`
	All         bool   `json:"all,omitempty" doc:"Send it to every researcher (role competitor) at work: none that finished or was cut."`
	Coordinator bool   `json:"coordinator,omitempty" doc:"Send it to the hosted coordinator, from a person or a key."`
	Text        string `json:"text" minLength:"1" doc:"What the agent is told, at most 1500 bytes."`
}

// RefusedMessage is a recipient an all-message was not sent to.
type RefusedMessage struct {
	AgentID    string    `json:"agent_id"`
	Reason     string    `json:"reason" doc:"message_rate_limited."`
	RetryAfter time.Time `json:"retry_after" doc:"When the next message can be sent to it."`
}

// SendMessage sends text from by to the agent named, with all to every researcher at work that by
// may message now and names the rest as refused, or with coordinator to the hosted coordinator.
func (s *Service) SendMessage(ctx context.Context, platformExpID string, req SendMessageRequest, by domain.Actor) ([]domain.AgentMessage, []RefusedMessage, error) {
	written := strings.TrimSpace(req.Text)
	text := strings.TrimSpace(domain.EventDetail(written))
	named := 0
	for _, set := range []bool{req.AgentID != "", req.All, req.Coordinator} {
		if set {
			named++
		}
	}
	switch {
	case named != 1:
		return nil, nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_recipient", "invalid_recipient: name exactly one of agent_id, all and coordinator")
	case req.Coordinator && by.Kind == domain.ActorCoordinator:
		return nil, nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_recipient", "invalid_recipient: the coordinator sends no message to itself")
	case text == "":
		return nil, nil, refusal.Refuse(http.StatusUnprocessableEntity, "empty_message", "empty_message: text is empty")
	case len(written) > domain.MaxAgentMessageBytes:
		return nil, nil, refusal.Refuse(http.StatusUnprocessableEntity, "message_too_long", fmt.Sprintf("message_too_long: text is %d bytes, at most %d: say the one direction that matters, with the job and hypothesis ids it rests on", len(written), domain.MaxAgentMessageBytes))
	}
	var messages []domain.AgentMessage
	var refused []RefusedMessage
	var err error
	if req.Coordinator {
		var message *domain.AgentMessage
		if message, err = s.store.SendCoordinatorMessage(ctx, platformExpID, text, by); message != nil {
			messages = []domain.AgentMessage{*message}
		}
	} else {
		var to []string
		if req.AgentID != "" {
			to = []string{req.AgentID}
		}
		var limited []db.MessagesRateLimited
		messages, limited, err = s.store.SendAgentMessages(ctx, platformExpID, to, text, by)
		for _, l := range limited {
			refused = append(refused, RefusedMessage{AgentID: l.AgentID, Reason: "message_rate_limited", RetryAfter: l.Next})
		}
	}
	var limited *db.MessagesRateLimited
	switch {
	case errors.As(err, &limited):
		return nil, nil, refusal.Refuse(http.StatusTooManyRequests, "message_rate_limited", fmt.Sprintf(
			"message_rate_limited: %s was sent %d messages by the %s in the last hour, the most an hour allows; the next can be sent from %s. Say in one message what you would say in several",
			limited.AgentID, domain.MaxAgentMessagesPerHour, limited.From, limited.Next.Format(time.RFC3339)))
	case errors.Is(err, db.ErrNoCoordinator):
		return nil, nil, refusal.Refuse(http.StatusConflict, "no_coordinator", "no_coordinator: the platform hosts no coordinator for platform experiment "+platformExpID)
	case errors.Is(err, domain.ErrPlatformExperimentClosed):
		return nil, nil, refusal.Refuse(http.StatusConflict, "experiment_closed", "experiment_closed: platform experiment "+platformExpID+" is closed: no agent of it is at work")
	case errors.Is(err, domain.ErrNotSignedUp):
		return nil, nil, refuseSignup(http.StatusNotFound, "not_signed_up", "%s of platform experiment %s: hl platform-experiments agents list names its agents", err.Error(), platformExpID)
	case errors.Is(err, db.ErrAgentEnded):
		return nil, nil, refuseSignup(http.StatusConflict, "agent_ended", "%s, it finished or was cut, and nothing reads a message to it", err.Error())
	case errors.Is(err, db.ErrNoResearcherAtWork):
		return nil, nil, refuseSignup(http.StatusConflict, "no_researcher_at_work", "every researcher of platform experiment %s finished or was cut, or none joined", platformExpID)
	case err != nil:
		return nil, nil, fmt.Errorf("platform_experiments.SendMessage: %w", err)
	}
	return messages, refused, nil
}

// SendNamedMessage is SendMessage with the actor of each message sent named.
func (s *Service) SendNamedMessage(ctx context.Context, platformExpID string, req SendMessageRequest, by domain.Actor) ([]domain.AgentMessage, []RefusedMessage, error) {
	messages, refused, err := s.SendMessage(ctx, platformExpID, req, by)
	if err != nil {
		return nil, nil, err
	}
	if err := s.nameMessageActors(ctx, messages); err != nil {
		return nil, nil, err
	}
	return messages, refused, nil
}

// ListMessages is every message sent to an agent of the platform experiment or its coordinator,
// oldest first, with the actor of each named; agentID narrows it to that agent's.
func (s *Service) ListMessages(ctx context.Context, platformExpID, agentID string) ([]domain.AgentMessage, error) {
	messages, err := s.store.ListAgentMessages(ctx, platformExpID, agentID)
	if err != nil {
		return nil, err
	}
	if err := s.nameMessageActors(ctx, messages); err != nil {
		return nil, err
	}
	if messages == nil {
		messages = []domain.AgentMessage{}
	}
	return messages, nil
}

func (s *Service) nameMessageActors(ctx context.Context, messages []domain.AgentMessage) error {
	if len(messages) == 0 {
		return nil
	}
	actors := make([]*domain.Actor, len(messages))
	for i := range messages {
		actors[i] = &messages[i].Actor
	}
	return s.store.NameActors(ctx, actors)
}
