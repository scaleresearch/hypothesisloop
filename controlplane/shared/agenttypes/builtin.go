// Package agenttypes holds the built-in agent types as the repository has them. control-service
// seeds them into the database at start (db.SeedBuiltinAgentTypes), which serves them from then on.
package agenttypes

import (
	"embed"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

//go:embed builtin/*.md
var prompts embed.FS

// Builtin is one built-in agent type: its id, which is also the file its prompt is in, name, role
// and settings.
type Builtin struct {
	ID       string
	Name     string
	Role     domain.AgentRole
	Prompt   string
	Settings domain.AgentTypeSettings
}

var builtins = []struct {
	id, name string
	role     domain.AgentRole
	effort   string
}{
	{domain.DefaultAgentFlavor, "Generalist", domain.AgentRoleResearcher, "high"},
	{"hyperparameter-search", "Hyperparameter search", domain.AgentRoleResearcher, "high"},
	{"architecture-search", "Architecture search", domain.AgentRoleResearcher, "high"},
	{"data-search", "Data search", domain.AgentRoleResearcher, "high"},
	{"baseline", "Baseline", domain.AgentRoleBaseline, "medium"},
	{domain.PlannerFlavor, "Planner", domain.AgentRolePlanner, "medium"},
	{domain.CoordinatorFlavor, "Coordinator", domain.AgentRoleCoordinator, "high"},
}

// Builtins is every built-in agent type, each checked as an org's would be on save.
func Builtins() ([]Builtin, error) {
	out := make([]Builtin, 0, len(builtins))
	for _, b := range builtins {
		prompt, err := prompts.ReadFile("builtin/" + b.id + ".md")
		if err != nil {
			return nil, fmt.Errorf("agenttypes: %w", err)
		}
		builtin := Builtin{
			ID: b.id, Name: b.name, Role: b.role, Prompt: string(prompt),
			Settings: domain.AgentTypeSettings{MaxSubagents: 4, PacingSubagents: 2, Effort: b.effort, Hyperparameters: map[string]any{}},
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

// Catalog is the built-in types as a catalog at version 1, as a database seeded once holds them.
func Catalog() domain.AgentTypeCatalog {
	all, err := Builtins()
	if err != nil {
		panic(err)
	}
	catalog := make(domain.AgentTypeCatalog, len(all))
	for i, b := range all {
		catalog[i] = domain.CurrentAgentType{
			AgentType: domain.AgentType{ID: b.ID, Name: b.Name, Role: b.Role, Builtin: true, CurrentVersion: 1},
			Current:   domain.AgentTypeVersion{TypeID: b.ID, Version: 1, Prompt: b.Prompt, Settings: b.Settings},
		}
	}
	return catalog
}
