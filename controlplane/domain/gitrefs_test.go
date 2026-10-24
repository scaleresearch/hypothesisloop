package domain

import "testing"

func TestAnAgentWritesItsOwnBranchesAndNothingElse(t *testing.T) {
	const pe = "pe-1"
	for _, c := range []struct {
		name    string
		role    SignupRole
		agent   string
		update  RefUpdate
		allowed bool
	}{
		{"an executor's own branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-2/faster-attention-abc123"}, true},
		{"an executor creating its own branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-2/x", Created: true}, true},
		{"a reviewer's own branch", SignupRoleReviewer, "Reviewer 1", RefUpdate{Ref: "refs/heads/hl/pe-1/reviewer-1/x"}, true},
		{"another agent's branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-3/x"}, false},
		{"a branch whose agent's name starts alike", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-20/x"}, false},
		{"the agent's prefix alone", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-2/"}, false},
		{"another platform experiment's branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-2/executor-2/x"}, false},
		{"the baseline branch by an executor", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/baseline"}, false},
		{"the default branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/main"}, false},
		{"creating the default branch as an executor", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/main", Created: true}, false},
		{"deleting its own branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-2/x", Deleted: true}, true},
		{"deleting another agent's branch", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-3/x", Deleted: true}, false},
		{"deleting the baseline branch as an executor", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/heads/hl/pe-1/baseline", Deleted: true}, false},
		{"deleting the default branch", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/main", Deleted: true}, false},
		{"deleting a tag", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/tags/v1", Deleted: true}, false},
		{"a tag", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/tags/v1", Created: true}, false},
		{"a ref outside heads", SignupRoleExecutor, "Executor 2", RefUpdate{Ref: "refs/notes/commits"}, false},
		{"the baseline branch", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/hl/pe-1/baseline"}, true},
		{"a branch beside the baseline", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/hl/pe-1/baseline-check"}, true},
		{"an executor's branch by the baseline", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/hl/pe-1/executor-2/x"}, false},
		{"the default branch created by the baseline", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/main", Created: true}, false},
		{"the default branch moved by the baseline", SignupRoleBaseline, "Baseline 1", RefUpdate{Ref: "refs/heads/main"}, false},
		{"a planner's push", SignupRolePlanner, "Planner 1", RefUpdate{Ref: "refs/heads/hl/pe-1/planner-1/x"}, false},
		{"a researcher's push to a branch of its own name", SignupRoleResearcher, "Researcher 1", RefUpdate{Ref: "refs/heads/hl/pe-1/researcher-1/x", Created: true}, false},
		{"an executor named into the baseline's place", SignupRoleExecutor, "Baseline Check", RefUpdate{Ref: "refs/heads/hl/pe-1/baseline-check/x"}, false},
		{"a name that makes no branch name", SignupRoleExecutor, "!!!", RefUpdate{Ref: "refs/heads/hl/pe-1//x"}, false},
	} {
		refusal := RefusedRefUpdate(pe, c.role, c.agent, c.update)
		if (refusal == "") != c.allowed {
			t.Errorf("%s: refusal = %q, want allowed %v", c.name, refusal, c.allowed)
		}
	}
}
