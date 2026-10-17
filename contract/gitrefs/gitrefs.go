// Package gitrefs names the git branches of a platform experiment, as agents write them and the
// platform checks them.
package gitrefs

import "strings"

// Root is what the name of every branch the platform's agents write starts with.
const Root = "hl/"

// BranchPrefix is what the name of every git branch of a platform experiment starts with. An
// agent's implementation of a hypothesis is one branch under it, and BaselineBranch another.
func BranchPrefix(platformExperimentID string) string { return Root + platformExperimentID + "/" }

// BaselineBranch is the branch the baseline of a platform experiment is measured on.
func BaselineBranch(platformExperimentID string) string {
	return BranchPrefix(platformExperimentID) + "baseline"
}

// IsBaselineAgentBranch is whether a baseline agent's job may run on branch: the baseline branch,
// or one beside it, baseline-<name>, where the agent tries its code as a researcher would. Only a
// job of the baseline branch itself is declared the baseline.
func IsBaselineAgentBranch(platformExperimentID, branch string) bool {
	baseline := BaselineBranch(platformExperimentID)
	if branch == baseline {
		return true
	}
	name, beside := strings.CutPrefix(branch, baseline+"-")
	return beside && name != "" && !strings.ContainsAny(name, "/ \t\n")
}

// Slug is text as part of a branch name: lowercase letters and digits, every run of anything else
// one dash, cut to limit when limit is positive.
func Slug(text string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() == limit {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// AgentBranchName is an agent's name as the segment of the branches it writes under BranchPrefix.
func AgentBranchName(name string) string { return Slug(name, 0) }
