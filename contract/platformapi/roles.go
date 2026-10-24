package platformapi

// OrgRole is what a member may do in an org.
type OrgRole string

const (
	OrgRoleOwner  OrgRole = "owner"
	OrgRoleMember OrgRole = "member"
)

// OrgRoles is every role in an org.
var OrgRoles = []OrgRole{OrgRoleOwner, OrgRoleMember}

// SignupRole is what an agent was signed up to do in one platform experiment. It lives on the
// signup rather than on the agent (the same agent may compete in one platform experiment and
// hold the baseline in another) and is fixed at signup: changing it mid-run would retroactively
// rewrite who a completed cut applied to.
//
// Ranking reads it, and whether the agent submits jobs (SubmitsJobs). A non-competitor's jobs are
// admitted, billed, evicted and settled by identical code, and its metrics are recorded and
// readable in full.
type SignupRole string

const (
	// SignupRoleCompetitor is an executor: it implements experiments and runs their jobs. It is
	// ranked, cut-eligible, takes a quota share and earns the top-3 bonus.
	SignupRoleCompetitor SignupRole = "competitor"
	// SignupRoleBaseline runs the experiment's declared control. Never ranked, never cut: the
	// point of a baseline is that its numbers are visible and comparable. It holds a quota from
	// the moment it joins and is the one role whose job an open platform experiment takes: the
	// baseline is measured before anyone is ranked against it. It takes no share at the start,
	// and what it spent is taken from the budget before the shares.
	SignupRoleBaseline SignupRole = "baseline"
	// SignupRoleReviewer re-checks other agents' claims and records agreement or dispute. Never
	// ranked, never cut.
	SignupRoleReviewer SignupRole = "reviewer"
	// SignupRoleResearcher keeps the hypothesis graph correct and points the work: it proposes,
	// reviews, merges, retires and relinks hypotheses. It runs no job, pushes nothing, holds no
	// quota, is never ranked or cut and takes none of the places max_agents counts.
	SignupRoleResearcher SignupRole = "researcher"
	// SignupRolePlanner is the agent the platform signs up to plan a draft. It proposes the
	// experiment, runs no job, holds no quota and takes no part once the draft is accepted. No
	// request can ask for it.
	SignupRolePlanner SignupRole = "planner"
)

// SignupRoles is every role of a signup.
var SignupRoles = []SignupRole{SignupRoleCompetitor, SignupRoleBaseline, SignupRoleReviewer, SignupRoleResearcher, SignupRolePlanner}

// SubmitsJobs says an agent of role r submits jobs and pushes its branches: every role but planner
// and researcher.
func (r SignupRole) SubmitsJobs() bool {
	return r != SignupRolePlanner && r != SignupRoleResearcher
}
