// Package agenttypes holds the built-in agent types as the repository has them. control-service
// seeds them into the database at start (db.SeedBuiltins), which serves them from then on.
package agenttypes

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

//go:embed builtin/*.md builtin/descriptions.json
var files embed.FS

// Release numbers what this binary seeds: the built-in agent types and the platform's rules. A
// seeded version keeps the release that wrote it, and a start writes what it has when that differs
// from the current version and Release is that version's or newer. So an older replica of a
// rolling deploy never writes its own back over a newer one's, and a revert of a prompt, shipped
// under the next Release, becomes current like any other change. Raise it by one with every change
// of a built-in's name, description, prompt or settings, or of the rules: TestReleaseNamesWhatItSeeds
// keeps the digest of each.
const Release = 11

// executorFocus marks where builtin/executor.md, the body every executor shares, takes the
// executor's own builtin/<id>.md.
const executorFocus = "{{focus}}"

var builtins = []struct {
	id, name string
	role     domain.AgentRole
	effort   string
}{
	{domain.DefaultAgentType, "Generalist", domain.AgentRoleExecutor, "high"},
	{"hyperparameter-search", "Hyperparameter search", domain.AgentRoleExecutor, "high"},
	{"architecture-search", "Architecture search", domain.AgentRoleExecutor, "high"},
	{"data-search", "Data search", domain.AgentRoleExecutor, "high"},
	{domain.ResearcherAgentType, "Researcher", domain.AgentRoleResearcher, "high"},
	{"baseline", "Baseline", domain.AgentRoleBaseline, "medium"},
	{domain.PlannerAgentType, "Planner", domain.AgentRolePlanner, "medium"},
	{domain.CoordinatorAgentType, "Coordinator", domain.AgentRoleCoordinator, "high"},
}

// Builtins is every built-in agent type, each checked as an org's would be on save.
func Builtins() ([]domain.BuiltinAgentType, error) {
	raw, err := files.ReadFile("builtin/descriptions.json")
	if err != nil {
		return nil, fmt.Errorf("agenttypes: %w", err)
	}
	var descriptions map[string]string
	if err := json.Unmarshal(raw, &descriptions); err != nil {
		return nil, fmt.Errorf("agenttypes: descriptions.json: %w", err)
	}
	if len(descriptions) != len(builtins) {
		return nil, fmt.Errorf("agenttypes: descriptions.json holds %d descriptions for %d built-in types", len(descriptions), len(builtins))
	}
	out := make([]domain.BuiltinAgentType, 0, len(builtins))
	for _, b := range builtins {
		if err := domain.ValidateBuiltinAgentTypeID(b.id); err != nil {
			return nil, fmt.Errorf("agenttypes: %w", err)
		}
		prompt, err := builtinPrompt(b.id, b.role)
		if err != nil {
			return nil, err
		}
		builtin := domain.BuiltinAgentType{
			ID: b.id, Name: b.name, Description: descriptions[b.id], Role: b.role, Prompt: prompt,
			Settings: domain.AgentTypeSettings{MaxSubagents: 4, Effort: b.effort, Hyperparameters: map[string]any{}},
		}
		if err := domain.ValidateAgentTypeDescription(builtin.Description); err != nil {
			return nil, fmt.Errorf("agenttypes: built-in %s: %w", b.id, err)
		}
		if err := domain.ValidatePrompt(builtin.Role, builtin.Prompt); err != nil {
			return nil, fmt.Errorf("agenttypes: built-in %s: %w", b.id, err)
		}
		if err := builtin.Settings.Validate(); err != nil {
			return nil, fmt.Errorf("agenttypes: built-in %s: %w", b.id, err)
		}
		out = append(out, builtin)
	}
	return out, nil
}

// builtinPrompt is builtin/<id>.md, which for an executor is its focus set into builtin/executor.md.
func builtinPrompt(id string, role domain.AgentRole) (string, error) {
	own, err := files.ReadFile("builtin/" + id + ".md")
	if err != nil {
		return "", fmt.Errorf("agenttypes: %w", err)
	}
	if role != domain.AgentRoleExecutor {
		return string(own), nil
	}
	body, err := files.ReadFile("builtin/executor.md")
	if err != nil {
		return "", fmt.Errorf("agenttypes: %w", err)
	}
	if n := strings.Count(string(body), executorFocus); n != 1 {
		return "", fmt.Errorf("agenttypes: builtin/executor.md holds %s %d times, want once", executorFocus, n)
	}
	return strings.Replace(string(body), executorFocus, strings.TrimSpace(string(own)), 1), nil
}
