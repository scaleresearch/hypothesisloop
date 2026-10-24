// Package platformexperimentstest holds the fakes of the platform experiment stores that more
// than one package's tests use.
package platformexperimentstest

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes/agenttypestest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// PinnedInstructions runs agent ag-1 of pe-1 as the built-in generalist on a model of its own,
// ag-2 as it on the type's, and the coordinator as the built-in coordinator, under rules version
// RulesVersion.
type PinnedInstructions struct{ RulesVersion int }

const Rules = "## platform rules (read first)"

var Defaults = domain.ModelDefaults{Agent: "anthropic/model-default", Coordinator: "anthropic/model-c"}

func builtin(id string) domain.AgentTypeVersion {
	for _, t := range agenttypestest.Catalog() {
		if t.ID == id {
			return t.Current
		}
	}
	panic(id)
}

func (p PinnedInstructions) rules() int {
	return max(p.RulesVersion, 1)
}

func (p PinnedInstructions) AgentInstructions(_ context.Context, pe, agent string) (*db.InstructionsSource, error) {
	if pe != "pe-1" {
		return nil, nil
	}
	switch agent {
	case "ag-1":
		return &db.InstructionsSource{Role: domain.AgentRoleExecutor, Version: builtin("generalist"), Model: "anthropic/model-1",
			Hyperparameters: map[string]any{"lr": 0.001}, RepositoryURL: "https://github.com/acme/vision.git", Rules: Rules, RulesVersion: p.rules()}, nil
	case "ag-2":
		return &db.InstructionsSource{Role: domain.AgentRoleExecutor, Version: builtin("generalist"), RepositoryURL: "https://github.com/acme/vision.git", Rules: Rules, RulesVersion: p.rules()}, nil
	}
	return nil, nil
}

func (p PinnedInstructions) CoordinatorInstructions(_ context.Context, pe string) (*db.InstructionsSource, error) {
	if pe != "pe-1" {
		return nil, nil
	}
	return &db.InstructionsSource{Role: domain.AgentRoleCoordinator, Version: builtin("coordinator"), RepositoryURL: "https://github.com/acme/vision.git", Rules: Rules, RulesVersion: p.rules()}, nil
}
