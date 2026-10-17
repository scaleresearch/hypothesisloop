package platformexperiments

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// InstructionsStore reads what an agent's and a coordinator's instructions are composed from.
// *db.PlatformExperimentsStore is the one in use.
type InstructionsStore interface {
	AgentInstructions(ctx context.Context, platformExpID, agentID string) (*db.InstructionsSource, error)
	CoordinatorInstructions(ctx context.Context, platformExpID string) (*db.InstructionsSource, error)
}

// Instructions is what an agent or a coordinator runs with: the prompt of the current version of
// its agent type with the platform's values filled in, and its settings.
type Instructions struct {
	AgentType    string                   `json:"agent_type" doc:"The agent type it runs, by id."`
	Version      int                      `json:"agent_type_version" doc:"The current version of the agent type, which it runs."`
	RulesVersion int                      `json:"rules_version" doc:"The version of the platform's rules the prompt holds."`
	Role         domain.AgentRole         `json:"role" enum:"researcher,baseline,coordinator,planner"`
	Prompt       string                   `json:"prompt" doc:"The system prompt, with the platform's rules and the agent's assignment filled in. It still holds each of placeholders, which only the machine knows: the harness fills them, in one pass, before the prompt is used."`
	Placeholders []string                 `json:"placeholders" doc:"The placeholders the prompt may still hold, each as {{name}}: hl_commands, the listing of the hl on the machine, and workdir, the directory the agent works in."`
	Settings     domain.AgentTypeSettings `json:"settings" doc:"What it runs with: the version's settings, with the model and hyperparameters the agent was given, where it was given its own. A model neither names is the platform's default."`
	Digest       string                   `json:"digest" doc:"sha256 of prompt, settings and rules_version. The harness compares it with the one it runs, on agent.instructions and at the start of each connection, and applies a change at the next turn boundary."`
}

// ErrNoInstructions answers for an agent or a coordinator the platform experiment does not run.
var ErrNoInstructions = errors.New("instructions not found")

// InstructionsService composes what an agent and a coordinator run with.
type InstructionsService struct {
	store    InstructionsStore
	defaults domain.ModelDefaults
	logger   *zap.Logger
}

func NewInstructionsService(store InstructionsStore, defaults domain.ModelDefaults, logger *zap.Logger) *InstructionsService {
	return &InstructionsService{store: store, defaults: defaults, logger: logger}
}

// Agent is what agentID runs with in platformExperimentID, or ErrNoInstructions.
func (s *InstructionsService) Agent(ctx context.Context, platformExperimentID, agentID string) (*Instructions, error) {
	src, err := s.store.AgentInstructions(ctx, platformExperimentID, agentID)
	return s.compose(platformExperimentID, agentID, src, err)
}

// Coordinator is what the coordinator of platformExperimentID runs with, or ErrNoInstructions.
func (s *InstructionsService) Coordinator(ctx context.Context, platformExperimentID string) (*Instructions, error) {
	src, err := s.store.CoordinatorInstructions(ctx, platformExperimentID)
	return s.compose(platformExperimentID, "", src, err)
}

func (s *InstructionsService) compose(platformExperimentID, agentID string, src *db.InstructionsSource, err error) (*Instructions, error) {
	if err != nil {
		return nil, auth.Internal(s.logger, "read instructions failed", err, zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID))
	}
	if src == nil {
		return nil, ErrNoInstructions
	}
	instructions, err := composeInstructions(platformExperimentID, agentID, src, s.defaults)
	if err != nil {
		return nil, auth.Internal(s.logger, "compose instructions failed", err, zap.String("platformExpID", platformExperimentID), zap.String("agentID", agentID))
	}
	return &instructions, nil
}

// composeInstructions fills the prompt of src with the platform's rules and the assignment of
// agentID, empty for a coordinator, in platform experiment platformExperimentID, and resolves its
// settings with defaults standing for a model neither the agent nor its type names.
func composeInstructions(platformExperimentID, agentID string, src *db.InstructionsSource, defaults domain.ModelDefaults) (Instructions, error) {
	settings := domain.EffectiveAgentSettings(src.Version.Settings, src.Model, src.Hyperparameters, defaults.For(src.Role))
	prompt, err := domain.ComposePrompt(src.Version.Prompt, domain.Assignment{
		Rules: src.Rules, AgentID: agentID, PlatformExperimentID: platformExperimentID,
		AgentType: src.Version.TypeID, RepositoryURL: src.RepositoryURL, Hyperparameters: settings.Hyperparameters,
	})
	if err != nil {
		return Instructions{}, err
	}
	digest, err := domain.InstructionsDigest(prompt, settings, src.RulesVersion)
	if err != nil {
		return Instructions{}, err
	}
	return Instructions{
		AgentType: src.Version.TypeID, Version: src.Version.Version, RulesVersion: src.RulesVersion, Role: src.Role, Prompt: prompt,
		Placeholders: domain.HarnessPlaceholders, Settings: settings, Digest: digest,
	}, nil
}
