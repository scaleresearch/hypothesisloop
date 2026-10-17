package agenttypestest

import (
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
)

// Catalog is the built-in types as a catalog at version 1, as a database seeded once holds them.
func Catalog() domain.AgentTypeCatalog {
	all, err := agenttypes.Builtins()
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
