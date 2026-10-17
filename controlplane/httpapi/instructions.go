package httpapi

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments"
)

const instructionsDescription = "Only the token of the agent or coordinator they are for reads them, and the people and the API keys of the org that owns the platform experiment; every other agent and a coordinator asking for an agent's are refused with 403, and another org's platform experiment is not found. " +
	"An edit or a restore of the agent type, and a new version of the platform's rules, tell every agent and coordinator whose run goes on with agent.instructions, and these answer the new version from then on."

type instructionsAPI struct {
	instructions *platformexperiments.InstructionsService
	access       *auth.Access
}

// registerInstructions registers where an agent and a coordinator read the instructions they
// run with.
func registerInstructions(doc *Doc, instructions *platformexperiments.InstructionsService) {
	a := instructionsAPI{instructions: instructions, access: doc.Access}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-agent-instructions", Method: "GET", Path: "/platform-experiments/{id}/agents/{agent_id}/instructions",
		Summary: "Read the instructions an agent runs with", Tags: []string{"platform-experiments"},
		Description: "The system prompt and settings of the current version of the agent's agent type. " + instructionsDescription,
	}, a.getAgent)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-coordinator-instructions", Method: "GET", Path: "/platform-experiments/{id}/coordinator/instructions",
		Summary: "Read the instructions the coordinator runs with", Tags: []string{"platform-experiments"},
		Description: "The system prompt and settings of the current version of the agent type of the platform experiment's coordinator. " + instructionsDescription,
	}, a.getCoordinator)
}

func (a instructionsAPI) getAgent(ctx context.Context, in *struct {
	ID      string `path:"id"`
	AgentID string `path:"agent_id"`
}) (*struct {
	Body platformexperiments.Instructions
}, error) {
	if err := a.access.ReadInstructions(ctx, in.ID, in.AgentID); err != nil {
		return nil, err
	}
	return answerInstructions(a.instructions.Agent(ctx, in.ID, in.AgentID))
}

func (a instructionsAPI) getCoordinator(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body platformexperiments.Instructions
}, error) {
	if err := a.access.ReadInstructions(ctx, in.ID, ""); err != nil {
		return nil, err
	}
	return answerInstructions(a.instructions.Coordinator(ctx, in.ID))
}

func answerInstructions(instructions *platformexperiments.Instructions, err error) (*struct {
	Body platformexperiments.Instructions
}, error) {
	if errors.Is(err, platformexperiments.ErrNoInstructions) {
		return nil, huma.Error404NotFound("instructions not found")
	}
	if err != nil {
		return nil, err
	}
	return &struct {
		Body platformexperiments.Instructions
	}{Body: *instructions}, nil
}
