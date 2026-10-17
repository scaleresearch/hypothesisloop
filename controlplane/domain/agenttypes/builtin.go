// Package agenttypes holds the built-in agent types as the repository has them. control-service
// seeds them into the database at start (db.SeedBuiltins), which serves them from then on.
package agenttypes

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

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
const Release = 6

var builtins = []struct {
	id, name string
	role     domain.AgentRole
	effort   string
}{
	{domain.DefaultAgentType, "Generalist", domain.AgentRoleResearcher, "high"},
	{"hyperparameter-search", "Hyperparameter search", domain.AgentRoleResearcher, "high"},
	{"architecture-search", "Architecture search", domain.AgentRoleResearcher, "high"},
	{"data-search", "Data search", domain.AgentRoleResearcher, "high"},
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
		prompt, err := files.ReadFile("builtin/" + b.id + ".md")
		if err != nil {
			return nil, fmt.Errorf("agenttypes: %w", err)
		}
		builtin := domain.BuiltinAgentType{
			ID: b.id, Name: b.name, Description: descriptions[b.id], Role: b.role, Prompt: string(prompt),
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

// Catalog is the built-in types as a catalog at version 1, as a database seeded once holds them.
func Catalog() domain.AgentTypeCatalog {
	all, err := Builtins()
	if err != nil {
		panic(err)
	}
	catalog := make(domain.AgentTypeCatalog, len(all))
	for i, b := range all {
		catalog[i] = domain.CurrentAgentType{
			AgentType: domain.AgentType{ID: b.ID, Name: b.Name, Description: b.Description, Role: b.Role, Builtin: true, CurrentVersion: 1},
			Current:   domain.AgentTypeVersion{TypeID: b.ID, Version: 1, Prompt: b.Prompt, Settings: b.Settings},
		}
	}
	return catalog
}

// Digest is sha256 of everything a release seeds: each built-in's id, name, description, role,
// prompt and settings, and rules.
func Digest(all []domain.BuiltinAgentType, rules string) (string, error) {
	sum := sha256.New()
	for _, b := range all {
		settings, err := json.Marshal(b.Settings)
		if err != nil {
			return "", err
		}
		for _, part := range []string{b.ID, b.Name, b.Description, string(b.Role), b.Prompt, string(settings)} {
			sum.Write([]byte(part))
			sum.Write([]byte{0})
		}
	}
	sum.Write([]byte(rules))
	return hex.EncodeToString(sum.Sum(nil)), nil
}
