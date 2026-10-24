package agenttypes_test

import (
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes/agenttypestest"
)

func TestEveryRoleHasABuiltinDefault(t *testing.T) {
	var executors []string
	for _, t := range agenttypestest.Catalog() {
		if t.Role == domain.AgentRoleExecutor {
			executors = append(executors, t.ID)
		}
	}
	if !slices.Contains(executors, domain.DefaultAgentType) {
		t.Fatalf("the default type %q is no built-in executor", domain.DefaultAgentType)
	}
	for _, role := range domain.AgentRoles {
		if _, err := domain.LookupAgentType(agenttypestest.Catalog(), domain.AgentTypeOfRole(role), role); err != nil {
			t.Errorf("role %s has no built-in default: %v", role, err)
		}
	}
}
