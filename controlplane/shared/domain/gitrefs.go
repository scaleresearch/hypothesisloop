package domain

import (
	"fmt"
	"strings"
)

// DefaultBranch is the branch hl repo init gives a repository that has none, as its first and
// empty commit. Once it exists no agent writes it.
const DefaultBranch = "main"

// GitProxyURL is the address the repository of a platform experiment is cloned from through the
// control plane's git proxy served at base.
func GitProxyURL(base, platformExperimentID string) string {
	return strings.TrimRight(base, "/") + "/" + platformExperimentID + ".git"
}

// AgentBranchName is an agent's name as the segment of the branches it writes under
// BranchPrefix: lowercase letters and digits, every run of anything else one dash. hl hypothesis
// checkout names a branch with the same rule.
func AgentBranchName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
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
		if IsBaselineAgentBranch(platformExperimentID, branch) {
			return ""
		}
		return fmt.Sprintf("a baseline agent pushes %s and branches beside it, %s-<name>", BaselineBranch(platformExperimentID), BaselineBranch(platformExperimentID))
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
	segment := AgentBranchName(agentName)
	if segment == "" || segment == "baseline" || strings.HasPrefix(segment, "baseline-") {
		return ""
	}
	return BranchPrefix(platformExperimentID) + segment + "/"
}
