package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

type agentMessagesBody struct {
	Messages []domain.AgentMessage                `json:"messages" doc:"The messages sent, one for each agent sent to."`
	Refused  []platformexperiments.RefusedMessage `json:"refused,omitempty" doc:"With all, each researcher not sent to because the sender sent it the most an hour allows. Absent when none was refused."`
}

type messagesAPI struct {
	experiments *platformexperiments.PlatformExperimentsService
	access      *auth.Access
	logger      *zap.Logger
}

// registerMessages registers the direct messages to the agents of a platform experiment.
func registerMessages(doc *Doc, experiments *platformexperiments.PlatformExperimentsService, logger *zap.Logger) {
	a := messagesAPI{experiments: experiments, access: doc.Access, logger: logger}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "send-platform-experiment-message", Method: "POST", Path: "/platform-experiments/{id}/messages",
		Summary: "Send a message to an agent", Tags: []string{"platform-experiments"},
		DefaultStatus: http.StatusCreated,
		Description: "The coordinator, or a person or an API key of the org, tells one agent (agent_id), or every researcher at work (all), something directly: a steer, a direction to take, what to stop. " +
			"Each agent is told it as agent.message, which wakes it at once, with the text as its detail and from coordinator or owner as its value, and is told it again after a restart until its cursor passed it. A comment stays the place for a fact about a hypothesis. " +
			"A person or an API key tells the hosted coordinator (coordinator) the same way: agent.message with coordinator as its subject and owner as its value, on the stream of the whole platform experiment and no agent's, which wakes the coordinator. 409 no_coordinator where the platform hosts none, 422 invalid_recipient from the coordinator itself. " +
			"The answer is messages, one for each agent sent to. An agent's token sends none (403): what a researcher has to tell goes in a comment, or the reason of its finish. " +
			"404 not_signed_up for an agent not in the platform experiment, 409 agent_ended for one that finished or was cut, 409 no_researcher_at_work for all with none, 409 experiment_closed once it closed. " +
			"A recipient is sent at most 6 messages in any hour by the coordinator and 6 by the owner, each counted apart, so the owner's never spend the coordinator's: 429 message_rate_limited past that, saying when the next can be sent. With all, a researcher the sender may not message yet is left out and named in refused {agent_id, reason, retry_after}, the rest are sent and the answer is 201: 429 only when none could be sent. 422 message_too_long above 1500 bytes.",
	}, a.send)

	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "list-platform-experiment-messages", Method: "GET", Path: "/platform-experiments/{id}/messages",
		Summary: "List the messages sent to agents", Tags: []string{"platform-experiments"},
		Description: "Every message sent to an agent of the platform experiment or its coordinator (POST /platform-experiments/{id}/messages), oldest first, with whom it was sent to, from coordinator or owner, the actor whose credential sent it, and when. " +
			"agent_id narrows it to that agent's, coordinator to the coordinator's. An agent's token reads its own alone, whatever agent_id says.",
	}, a.list)

}

func (a messagesAPI) send(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body platformexperiments.SendMessageRequest
}) (*struct{ Body agentMessagesBody }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	messages, refusedTo, err := a.experiments.SendNamedMessage(ctx, in.ID, in.Body, caller.Actor())
	if err != nil {
		return nil, refused(a.logger, "send message", err)
	}
	return &struct{ Body agentMessagesBody }{Body: agentMessagesBody{Messages: messages, Refused: refusedTo}}, nil
}

func (a messagesAPI) list(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `query:"agent_id" doc:"Only the messages to this agent."`
}) (*struct{ Body agentMessagesBody }, error) {
	if err := a.access.PlatformExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, err := caller(ctx, a.logger)
	if err != nil {
		return nil, err
	}
	agentID := in.AgentID
	if caller.Kind == auth.KindAgent {
		agentID = caller.AgentID
	}
	messages, err := a.experiments.ListMessages(ctx, in.ID, agentID)
	if err != nil {
		return nil, refused(a.logger, "list messages", err)
	}
	return &struct{ Body agentMessagesBody }{Body: agentMessagesBody{Messages: messages}}, nil
}
