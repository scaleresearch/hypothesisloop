package domain

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
)

// DefaultBranch is the branch hl repo init gives a repository that has none, as its first and
// empty commit. Once it exists no agent writes it.
const DefaultBranch = "main"

// BuildsGitPath is where under the git proxy an environment build reads the repository it is of.
const BuildsGitPath = "builds"

// BuildGitProxyURL is where the build of the environment key reads its repository through the
// git proxy at base. It has no .git suffix, so a URL rewrite of the repository's address with or
// without one lands on it.
func BuildGitProxyURL(base, key string) string { return base + "/" + BuildsGitPath + "/" + key }

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
// own: a baseline agent the baseline branch and those beside it (IsBaselineAgentBranch), any
// other agent hl/<platform experiment>/<AgentBranchName>/<anything>. Its own it may also delete.
// A baseline agent also creates DefaultBranch on a repository that has none, as hl repo init
// does, and nobody moves or deletes it.
func RefusedRefUpdate(platformExperimentID string, role SignupRole, agentName string, update RefUpdate) string {
	branch, ok := strings.CutPrefix(update.Ref, "refs/heads/")
	if !ok {
		return "only branches are pushed"
	}
	if branch == DefaultBranch {
		if role == SignupRoleBaseline && update.Created && !update.Deleted {
			return ""
		}
		return "the default branch " + DefaultBranch + " is no agent's"
	}
	switch role {
	case SignupRoleBaseline:
		if gitrefs.IsBaselineAgentBranch(platformExperimentID, branch) {
			return ""
		}
		return fmt.Sprintf("a baseline agent pushes %s and branches beside it, %s-<name>", gitrefs.BaselineBranch(platformExperimentID), gitrefs.BaselineBranch(platformExperimentID))
	case SignupRoleCompetitor, SignupRoleReviewer:
		own := AgentBranchPrefix(platformExperimentID, agentName)
		if own == "" {
			return fmt.Sprintf("agent name %q makes no branch name", agentName)
		}
		if len(branch) > len(own) && strings.HasPrefix(branch, own) {
			return ""
		}
		return "your branches are under " + own + ", which hl hypothesis checkout puts you on"
	}
	return "a " + string(role) + " pushes nothing"
}

// AgentBranchPrefix is what every branch a researcher or reviewer named agentName writes starts
// with, empty for a name that makes no branch name or one in the baseline's place.
func AgentBranchPrefix(platformExperimentID, agentName string) string {
	segment := gitrefs.AgentBranchName(agentName)
	if segment == "" || segment == "baseline" || strings.HasPrefix(segment, "baseline-") {
		return ""
	}
	return gitrefs.BranchPrefix(platformExperimentID) + segment + "/"
}
