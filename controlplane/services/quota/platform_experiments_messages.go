package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// SendMessageRequest is the body of POST /platform-experiments/{id}/messages.
type SendMessageRequest struct {
	AgentID     string `json:"agent_id,omitempty" doc:"The agent to send it to. Exactly one of agent_id, all and coordinator."`
	All         bool   `json:"all,omitempty" doc:"Send it to every researcher (role competitor) at work: none that finished or was cut."`
	Coordinator bool   `json:"coordinator,omitempty" doc:"Send it to the hosted coordinator, from a person or a key."`
	Text        string `json:"text" minLength:"1" doc:"What the agent is told, at most 1500 bytes."`
}

type agentMessagesBody struct {
	Messages []domain.AgentMessage `json:"messages" doc:"The messages sent, one for each agent sent to."`
	Refused  []refusedMessage      `json:"refused,omitempty" doc:"With all, each researcher not sent to because the sender sent it the most an hour allows. Absent when none was refused."`
}

// refusedMessage is a recipient an all-message was not sent to.
type refusedMessage struct {
	AgentID    string    `json:"agent_id"`
	Reason     string    `json:"reason" doc:"message_rate_limited."`
	RetryAfter time.Time `json:"retry_after" doc:"When the next message can be sent to it."`
}

// SendMessage sends text from by to the agent named, with all to every researcher at work that by
// may message now and names the rest as refused, or with coordinator to the hosted coordinator.
func (s *PlatformExperimentsService) SendMessage(ctx context.Context, platformExpID string, req SendMessageRequest, by domain.Actor) ([]domain.AgentMessage, []refusedMessage, error) {
	text := strings.TrimSpace(req.Text)
	named := 0
	for _, set := range []bool{req.AgentID != "", req.All, req.Coordinator} {
		if set {
			named++
		}
	}
	switch {
	case named != 1:
		return nil, nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_recipient", "invalid_recipient: name exactly one of agent_id, all and coordinator")
	case req.Coordinator && by.Kind == domain.ActorCoordinator:
		return nil, nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_recipient", "invalid_recipient: the coordinator sends no message to itself")
	case text == "":
		return nil, nil, api.Refuse(http.StatusUnprocessableEntity, "empty_message", "empty_message: text is empty")
	case len(text) > domain.MaxAgentMessageBytes:
		return nil, nil, api.Refuse(http.StatusUnprocessableEntity, "message_too_long", fmt.Sprintf("message_too_long: text is %d bytes, at most %d: say the one direction that matters, with the job and hypothesis ids it rests on", len(text), domain.MaxAgentMessageBytes))
	}
	var messages []domain.AgentMessage
	var refused []refusedMessage
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
			refused = append(refused, refusedMessage{AgentID: l.AgentID, Reason: "message_rate_limited", RetryAfter: l.Next})
		}
	}
	var limited *db.MessagesRateLimited
	switch {
	case errors.As(err, &limited):
		return nil, nil, api.Refuse(http.StatusTooManyRequests, "message_rate_limited", fmt.Sprintf(
			"message_rate_limited: %s was sent %d messages by the %s in the last hour, the most an hour allows; the next can be sent from %s. Say in one message what you would say in several",
			limited.AgentID, domain.MaxAgentMessagesPerHour, limited.From, limited.Next.Format(time.RFC3339)))
	case errors.Is(err, db.ErrNoCoordinator):
		return nil, nil, api.Refuse(http.StatusConflict, "no_coordinator", "no_coordinator: the platform hosts no coordinator for platform experiment "+platformExpID)
	case errors.Is(err, db.ErrPlatformExperimentClosed):
		return nil, nil, api.Refuse(http.StatusConflict, "experiment_closed", "experiment_closed: platform experiment "+platformExpID+" is closed: no agent of it is at work")
	case errors.Is(err, db.ErrNotSignedUp):
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

func (h *PlatformExperimentsHandler) nameMessageActors(ctx context.Context, messages []domain.AgentMessage) error {
	if len(messages) == 0 {
		return nil
	}
	actors := make([]*domain.Actor, len(messages))
	for i := range messages {
		actors[i] = &messages[i].Actor
	}
	return h.svc.store.NameActors(ctx, actors)
}

// registerMessagesHuma registers the direct messages to the agents of a platform experiment.
func registerMessagesHuma(doc *apidocs.Doc, h *PlatformExperimentsHandler) {
	access := doc.Access

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "send-platform-experiment-message", Method: "POST", Path: "/platform-experiments/{id}/messages",
		Summary: "Send a message to an agent", Tags: []string{"platform-experiments"},
		DefaultStatus: http.StatusCreated,
		Description: "The coordinator, or a person or an API key of the org, tells one agent (agent_id), or every researcher at work (all), something directly: a steer, a direction to take, what to stop. " +
			"Each agent is told it as agent.message, which wakes it at once, with the text as its detail and from coordinator or owner as its value, and is told it again after a restart until its cursor passed it. A comment stays the place for a fact about a hypothesis. " +
			"A person or an API key tells the hosted coordinator (coordinator) the same way: agent.message with coordinator as its subject and owner as its value, on the stream of the whole platform experiment and no agent's, which wakes the coordinator. 409 no_coordinator where the platform hosts none, 422 invalid_recipient from the coordinator itself. " +
			"The answer is messages, one for each agent sent to. An agent's token sends none (403): what a researcher has to tell goes in a comment, or the reason of its finish. " +
			"404 not_signed_up for an agent not in the platform experiment, 409 agent_ended for one that finished or was cut, 409 no_researcher_at_work for all with none, 409 experiment_closed once it closed. " +
			"A recipient is sent at most 6 messages in any hour by the coordinator and 6 by the owner, each counted apart, so the owner's never spend the coordinator's: 429 message_rate_limited past that, saying when the next can be sent. With all, a researcher the sender may not message yet is left out and named in refused {agent_id, reason, retry_after}, the rest are sent and the answer is 201: 429 only when none could be sent. 422 message_too_long above 1500 bytes.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body SendMessageRequest
	}) (*struct{ Body agentMessagesBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		messages, refused, err := h.svc.SendMessage(ctx, in.ID, in.Body, caller.Actor())
		if err != nil {
			return nil, h.refusal("send message", err)
		}
		if err := h.nameMessageActors(ctx, messages); err != nil {
			return nil, h.refusal("send message", err)
		}
		return &struct{ Body agentMessagesBody }{Body: agentMessagesBody{Messages: messages, Refused: refused}}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-messages", Method: "GET", Path: "/platform-experiments/{id}/messages",
		Summary: "List the messages sent to agents", Tags: []string{"platform-experiments"},
		Description: "Every message sent to an agent of the platform experiment or its coordinator (POST /platform-experiments/{id}/messages), oldest first, with whom it was sent to, from coordinator or owner, the actor whose credential sent it, and when. " +
			"agent_id narrows it to that agent's, coordinator to the coordinator's. An agent's token reads its own alone, whatever agent_id says.",
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `query:"agent_id" doc:"Only the messages to this agent."`
	}) (*struct{ Body agentMessagesBody }, error) {
		if err := access.PlatformExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		caller, err := h.caller(ctx)
		if err != nil {
			return nil, err
		}
		agentID := in.AgentID
		if caller.Kind == auth.KindAgent {
			agentID = caller.AgentID
		}
		messages, err := h.svc.store.ListAgentMessages(ctx, in.ID, agentID)
		if err != nil {
			return nil, h.refusal("list messages", err)
		}
		if err := h.nameMessageActors(ctx, messages); err != nil {
			return nil, h.refusal("list messages", err)
		}
		if messages == nil {
			messages = []domain.AgentMessage{}
		}
		return &struct{ Body agentMessagesBody }{Body: agentMessagesBody{Messages: messages}}, nil
	})
}
