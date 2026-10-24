package domain

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
)

// GitProxyURL is the address the repository of a platform experiment is cloned from through the
// control plane's git proxy served at base.
func GitProxyURL(base, platformExperimentID string) string {
	return strings.TrimRight(base, "/") + "/" + platformExperimentID + ".git"
}

// RefUpdate is one ref a push asks to change: created when it had no value, deleted when it is
// to have none.
type RefUpdate struct {
	Ref     string
	Created bool
	Deleted bool
}

// RefusedRefUpdate says why the agent of role named agentName may not make update in platform
// experiment platformExperimentID, "" when it may. An agent writes branches alone, and only its
// own: a baseline agent the baseline branch and those beside it (IsBaselineAgentBranch), an
// executor or a reviewer hl/<platform experiment>/<AgentBranchName>/<anything>. Its own it may
// also delete. A researcher or a planner pushes nothing.
func RefusedRefUpdate(platformExperimentID string, role SignupRole, agentName string, update RefUpdate) string {
	branch, ok := strings.CutPrefix(update.Ref, "refs/heads/")
	if !ok {
		return "only branches are pushed"
	}
	switch role {
	case SignupRoleBaseline:
		if gitrefs.IsBaselineAgentBranch(platformExperimentID, branch) {
			return ""
		}
		return fmt.Sprintf("a baseline agent pushes %s and branches beside it, %s-<name>", gitrefs.BaselineBranch(platformExperimentID), gitrefs.BaselineBranch(platformExperimentID))
	case SignupRoleExecutor, SignupRoleReviewer:
		own := gitrefs.AgentBranchPrefix(platformExperimentID, agentName)
		if own == "" {
			return fmt.Sprintf("agent name %q makes no branch name", agentName)
		}
		if gitrefs.IsUnder(own, branch) {
			return ""
		}
		return "your branches are under " + own + ", which hl hypothesis checkout puts you on"
	}
	return "a " + string(role) + " pushes nothing"
}
