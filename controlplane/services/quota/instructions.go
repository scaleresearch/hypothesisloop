package quota

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// InstructionsStore reads what an agent's and a coordinator's instructions are composed from.
// *db.PlatformExperimentsStore is the one in use.
type InstructionsStore interface {
	AgentInstructions(ctx context.Context, platformExpID, agentID string) (*db.InstructionsSource, error)
	CoordinatorInstructions(ctx context.Context, platformExpID string) (*db.InstructionsSource, error)
}

// Instructions is what an agent or a coordinator runs with: the prompt of the version of its agent
// type it is pinned to with the platform's values filled in, and its settings.
type Instructions struct {
	AgentType    string                   `json:"agent_type" doc:"The agent type it runs, by id."`
	Version      int                      `json:"agent_type_version" doc:"The version of the agent type it is pinned to."`
	Role         domain.AgentRole         `json:"role" enum:"researcher,baseline,coordinator,planner"`
	Prompt       string                   `json:"prompt" doc:"The system prompt, with the platform's rules and the agent's assignment filled in. It still holds each of placeholders, which only the machine knows: the harness fills them, in one pass, before the prompt is used."`
	Placeholders []string                 `json:"placeholders" doc:"The placeholders the prompt may still hold, each as {{name}}: hl_commands, the listing of the hl on the machine, and workdir, the directory the agent works in."`
	Settings     domain.AgentTypeSettings `json:"settings" doc:"What it runs with: the version's settings, with the model and hyperparameters of the agent itself, which follow the type's when an edit changes them."`
	Digest       string                   `json:"digest" doc:"sha256 of prompt and settings. The harness compares it with the one it runs, on agent.instructions and at the start of each connection, and applies a change at the next turn boundary."`
}

const instructionsDescription = "Only the token of the agent or coordinator they are for reads them, and the people and the API keys of the org that owns the platform experiment; every other agent and a coordinator asking for an agent's are refused with 403, and another org's platform experiment is not found. " +
	"An edit of the agent type moves every agent whose run goes on to the new version and tells each with agent.instructions, and these answer the new version from then on."

// RegisterInstructionsHuma registers where an agent and a coordinator read the instructions they
// run with.
func RegisterInstructionsHuma(doc *apidocs.Doc, store InstructionsStore, logger *zap.Logger) {
	access := doc.Access
	answer := func(platformExperimentID, agentID string, src *db.InstructionsSource, err error) (*struct{ Body Instructions }, error) {
		if err != nil {
			return nil, auth.Internal(logger, "read instructions failed", err, zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID))
		}
		if src == nil {
			return nil, huma.Error404NotFound("instructions not found")
		}
		instructions, err := composeInstructions(platformExperimentID, agentID, src)
		if err != nil {
			return nil, auth.Internal(logger, "compose instructions failed", err, zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID))
		}
		return &struct{ Body Instructions }{Body: instructions}, nil
	}
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-agent-instructions", Method: "GET", Path: "/platform-experiments/{id}/agents/{agent_id}/instructions",
		Summary: "Read the instructions an agent runs with", Tags: []string{"platform-experiments"},
		Description: "The system prompt and settings of the version of its agent type the agent is pinned to. " + instructionsDescription,
	}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		AgentID string `path:"agent_id"`
	}) (*struct{ Body Instructions }, error) {
		if err := access.ReadInstructions(ctx, in.ID, in.AgentID); err != nil {
			return nil, err
		}
		src, err := store.AgentInstructions(ctx, in.ID, in.AgentID)
		return answer(in.ID, in.AgentID, src, err)
	})
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-coordinator-instructions", Method: "GET", Path: "/platform-experiments/{id}/coordinator/instructions",
		Summary: "Read the instructions the coordinator runs with", Tags: []string{"platform-experiments"},
		Description: "The system prompt and settings of the version of its agent type the platform experiment's coordinator is pinned to. " + instructionsDescription,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body Instructions }, error) {
		if err := access.ReadInstructions(ctx, in.ID, ""); err != nil {
			return nil, err
		}
		src, err := store.CoordinatorInstructions(ctx, in.ID)
		return answer(in.ID, "", src, err)
	})
}

// composeInstructions fills the prompt src is pinned to with the platform's rules and the
// assignment of agentID, empty for a coordinator, in platform experiment platformExperimentID.
func composeInstructions(platformExperimentID, agentID string, src *db.InstructionsSource) (Instructions, error) {
	prompt, err := domain.ComposePrompt(src.Version.Prompt, domain.Assignment{
		Rules: apidocs.PlatformRules, AgentID: agentID, PlatformExperimentID: platformExperimentID,
		AgentType: src.Version.TypeID, RepositoryURL: src.RepositoryURL, Hyperparameters: src.Hyperparameters,
	})
	if err != nil {
		return Instructions{}, err
	}
	settings := domain.EffectiveAgentSettings(src.Version.Settings, src.Model, src.Hyperparameters)
	digest, err := domain.InstructionsDigest(prompt, settings)
	if err != nil {
		return Instructions{}, err
	}
	return Instructions{
		AgentType: src.Version.TypeID, Version: src.Version.Version, Role: src.Role, Prompt: prompt,
		Placeholders: domain.HarnessPlaceholders, Settings: settings, Digest: digest,
	}, nil
}
