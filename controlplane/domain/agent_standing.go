package domain

import "time"

// AgentStanding is what an agent's token stands on, read in one statement: where its platform
// experiment is and when it closed, whether the agent is in it, and when its run ended for each
// of the reasons that end one.
type AgentStanding struct {
	// Tenancy is nil when there is no such platform experiment, or it was removed.
	Tenancy  *Tenancy
	ClosedAt *time.Time
	SignedUp bool
	CutAt    *time.Time
	// PlannerAnsweredAt is the moment the run of a planner ended: it answered its draft, with a
	// proposal or with why there is none, and the time its machine is kept after an answer has
	// passed (planner_ends_at). nil for an agent that is no planner, for a planner that is
	// planning, which a change of the goal makes true again, and for one kept after its answer.
	PlannerAnsweredAt *time.Time
	FinishedAt        *time.Time
}
