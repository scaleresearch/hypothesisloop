package domain

import (
	"slices"
	"testing"
)

// Each signup role runs as its agent role, from its built-in type when it names none, and only a
// role that submits jobs pushes, holds a share of the budget or is named an executor.
func TestEachSignupRoleRunsAsItsAgentRole(t *testing.T) {
	for _, c := range []struct {
		signup      SignupRole
		role        AgentRole
		agentType   string
		submitsJobs bool
		shares      bool
		name        string
	}{
		{SignupRoleExecutor, AgentRoleExecutor, DefaultAgentType, true, true, "Executor 1"},
		{SignupRoleReviewer, AgentRoleExecutor, DefaultAgentType, true, true, "Reviewer 1"},
		{SignupRoleResearcher, AgentRoleResearcher, ResearcherAgentType, false, false, "Researcher 1"},
		{SignupRoleBaseline, AgentRoleBaseline, "baseline", true, false, "Baseline 1"},
		{SignupRolePlanner, AgentRolePlanner, PlannerAgentType, false, false, "Planner 1"},
	} {
		role := RoleOfSignup(c.signup)
		if role != c.role || role.Info().AgentType != c.agentType {
			t.Errorf("%s: role %s of type %s, want %s of %s", c.signup, role, role.Info().AgentType, c.role, c.agentType)
		}
		if c.signup.SubmitsJobs() != c.submitsJobs || (AgentSignup{Role: c.signup}).SharesBudget() != c.shares {
			t.Errorf("%s: submits jobs %v and shares the budget %v, want %v and %v", c.signup, c.signup.SubmitsJobs(), (AgentSignup{Role: c.signup}).SharesBudget(), c.submitsJobs, c.shares)
		}
		if got, err := AgentName(c.signup, func(string) bool { return false }); err != nil || got != c.name {
			t.Errorf("%s: named %q, %v, want %q", c.signup, got, err, c.name)
		}
	}
}

func TestAResearcherJoinsARunningPlatformExperimentAsAnExecutorDoes(t *testing.T) {
	for _, role := range SignupRoles {
		want := role == SignupRoleExecutor || role == SignupRoleResearcher
		if got := TakesSignup(PlatformExpRunning, role); got != want {
			t.Errorf("%s joins a running platform experiment: %v, want %v", role, got, want)
		}
	}
}

// A researcher's prompt holds no agent type or hyperparameters, which only an executor is given.
func TestAResearcherPromptHoldsNoAgentTypeOrHyperparameters(t *testing.T) {
	if got := AgentPlaceholders(AgentRoleResearcher); slices.Contains(got, PlaceholderAgentType) || slices.Contains(got, PlaceholderHyperparameters) || !slices.Contains(got, PlaceholderAgentID) {
		t.Errorf("researcher placeholders = %v, want its agent id and no agent type or hyperparameters", got)
	}
	if got := AgentPlaceholders(AgentRoleExecutor); !slices.Contains(got, PlaceholderAgentType) || !slices.Contains(got, PlaceholderHyperparameters) {
		t.Errorf("executor placeholders = %v, want its agent type and hyperparameters", got)
	}
}

func TestAResearcherDefaultsToTheCoordinatorsModel(t *testing.T) {
	defaults := ModelDefaults{Agent: "anthropic/agent", Coordinator: "anthropic/coordinator"}
	for role, want := range map[AgentRole]string{
		AgentRoleExecutor: "anthropic/agent", AgentRoleBaseline: "anthropic/agent", AgentRolePlanner: "anthropic/agent",
		AgentRoleResearcher: "anthropic/coordinator", AgentRoleCoordinator: "anthropic/coordinator",
	} {
		if got := defaults.For(role); got != want {
			t.Errorf("%s: default %s, want %s", role, got, want)
		}
	}
}

// Every role is described once, in the order of AgentRoles, with what the rest of the domain says
// of it.
func TestEveryRoleIsDescribedAsTheDomainHasIt(t *testing.T) {
	if len(AgentRoleInfos) != len(AgentRoles) {
		t.Fatalf("%d roles described, want %d", len(AgentRoleInfos), len(AgentRoles))
	}
	for i, info := range AgentRoleInfos {
		if info.Role != AgentRoles[i] || info.Name == "" || info.Description == "" {
			t.Errorf("roles[%d] = %+v, want %s named and described", i, info, AgentRoles[i])
		}
		if info.AgentType != info.Role.Info().AgentType || info.OrgTypes != slices.Contains(OrgAgentRoles, info.Role) {
			t.Errorf("%s: type %s and org types %v, want %s and %v", info.Role, info.AgentType, info.OrgTypes, info.Role.Info().AgentType, slices.Contains(OrgAgentRoles, info.Role))
		}
		if info.SignupRole != "" && RoleOfSignup(info.SignupRole) != info.Role {
			t.Errorf("%s: added as %s, which runs as %s", info.Role, info.SignupRole, RoleOfSignup(info.SignupRole))
		}
	}
}
