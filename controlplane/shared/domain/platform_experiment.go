package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	// PlatformExpDraft is a platform experiment being planned from a goal: a planner the platform
	// runs works out what to measure and proposes the rest, nobody else joins, and no job runs
	// until a person accepts the proposal, which opens it.
	PlatformExpDraft   PlatformExperimentStatus = "draft"
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// ValidPlatformExperimentStatus reports whether s is one of the recognized statuses. As with
// ValidExperimentStatus, filtering on an unrecognized value would otherwise reach Postgres as a
// bad enum literal and surface as a 500.
func ValidPlatformExperimentStatus(s PlatformExperimentStatus) bool {
	switch s {
	case PlatformExpDraft, PlatformExpOpen, PlatformExpRunning, PlatformExpClosed:
		return true
	default:
		return false
	}
}

// MetricRole classifies what a declared MetricDefinition is for. Declaring a metric no longer
// implies it's ranked — see MetricDefinition.Role.
type MetricRole string

const (
	// MetricRoleRanking counts for stage cuts and standings — today's behaviour, and the
	// default when Role is left empty, so an experiment created before this field existed
	// keeps ranking on every declared metric exactly as it always did.
	MetricRoleRanking MetricRole = "ranking"
	// MetricRoleConstraint must be reported and must satisfy Bound. It is judged per job: a
	// completed job that violates it or never reported it ranks nowhere (metricsdb.RankableJobs),
	// and the metric itself is never ranked or cut on.
	MetricRoleConstraint MetricRole = "constraint"
	// MetricRoleAttribute is reported and shown, never ranked or cut on, and never gates
	// standings eligibility — "which kind of result is this" facts (precision class, result
	// category, ...) that the objective may care about but that have no single order.
	MetricRoleAttribute MetricRole = "attribute"
)

// ValidMetricRole reports whether role is one of the three declared roles, or empty (which
// PlatformExperiment.Metrics' consumers treat as MetricRoleRanking for backward compatibility).
func ValidMetricRole(role MetricRole) bool {
	switch role {
	case "", MetricRoleRanking, MetricRoleConstraint, MetricRoleAttribute:
		return true
	default:
		return false
	}
}

// EffectiveRole returns d.Role, defaulting empty to MetricRoleRanking.
func (d MetricDefinition) EffectiveRole() MetricRole {
	if d.Role == "" {
		return MetricRoleRanking
	}
	return d.Role
}

// MetricDefinition declares a single metric that jobs in a PlatformExperiment must emit.
// The key is the Prometheus metric name; direction is "maximize" or "minimize". Role decides
// what the declaration is *for* — only MetricRoleRanking metrics ever rank or cut (see
// ValidateMetricDefinitions); Bound is required and only meaningful for MetricRoleConstraint,
// expressed in the same direction as Direction (maximize: value must be >= Bound; minimize:
// value must be <= Bound).
type MetricDefinition struct {
	Key       string     `json:"key"`
	Direction string     `json:"direction"` // "maximize" | "minimize"
	Role      MetricRole `json:"role,omitempty"`
	Bound     *float64   `json:"bound,omitempty"`
}

// Holds reports whether value satisfies the bound of a constraint metric.
func (d MetricDefinition) Holds(value float64) bool {
	if d.Direction == "minimize" {
		return value <= *d.Bound
	}
	return value >= *d.Bound
}

// The ways the values one job reported on a metric become the one value it is read by.
const (
	JobValueMax  = "max"
	JobValueMin  = "min"
	JobValueLast = "last"
)

// JobValueRule says how the values one job reported on the metric become the one it is read by,
// everywhere a job's value is shown, ranked or judged. A ranking metric is read by the job's
// best in the metric's direction, which standings and stage cuts rank. A constraint is read by
// the last value, which its bound is held against, and so is an attribute, which nothing judges.
func (d MetricDefinition) JobValueRule() string {
	switch {
	case d.EffectiveRole() != MetricRoleRanking:
		return JobValueLast
	case d.Direction == "minimize":
		return JobValueMin
	}
	return JobValueMax
}

// ValidateMetricDefinitions enforces the metric contract at creation, so nothing downstream
// (stage cuts, standings) needs to defend against a malformed one. Fail-fast per important.md #1.
func ValidateMetricDefinitions(defs []MetricDefinition) error {
	seen := make(map[string]bool, len(defs))
	for i, d := range defs {
		if d.Key == "" {
			return fmt.Errorf("metric %d: key is required", i)
		}
		if seen[d.Key] {
			return fmt.Errorf("metric %d: duplicate key %q", i, d.Key)
		}
		seen[d.Key] = true
		if d.Direction != "maximize" && d.Direction != "minimize" {
			return fmt.Errorf("metric %q: direction must be \"maximize\" or \"minimize\", got %q", d.Key, d.Direction)
		}
		if !ValidMetricRole(d.Role) {
			return fmt.Errorf("metric %q: role must be one of ranking, constraint, attribute (or empty), got %q", d.Key, d.Role)
		}
		if d.EffectiveRole() == MetricRoleConstraint && d.Bound == nil {
			return fmt.Errorf("metric %q: constraint role requires bound", d.Key)
		}
		if d.EffectiveRole() != MetricRoleConstraint && d.Bound != nil {
			return fmt.Errorf("metric %q: bound is only meaningful for constraint role", d.Key)
		}
	}
	return nil
}

// PrimaryRankingMetric is the first declared ranking metric, the primary one by convention.
func PrimaryRankingMetric(defs []MetricDefinition) (MetricDefinition, bool) {
	for _, d := range defs {
		if d.EffectiveRole() == MetricRoleRanking {
			return d, true
		}
	}
	return MetricDefinition{}, false
}

// RequireRankingMetric rejects a metric contract that can never rank anyone. Constraint and
// attribute metrics gate and describe; only a ranking metric orders a field. Without one the
// ladder runs to the end cutting nobody and the results endpoint returns no standings — a run
// that consumed a real budget and answered nothing, silently. Enforced where the contract is
// written (create/update), not on read: a row predating this rule must still reconcile.
func RequireRankingMetric(defs []MetricDefinition) error {
	if _, ok := PrimaryRankingMetric(defs); ok {
		return nil
	}
	return fmt.Errorf("at least one metric must have role \"ranking\": nothing else can order the field or cut a stage")
}

// MetricDefinitionsEqual reports whether two metric declarations are the same set in the same
// order — order matters (the first ranking metric is primary), so this is not a set comparison.
func MetricDefinitionsEqual(a, b []MetricDefinition) bool {
	return reflect.DeepEqual(a, b)
}

// MaxAgentImageLength bounds an image reference.
const MaxAgentImageLength = 512

// ValidateAgentImage accepts an image reference a launcher can be handed: printable, without
// spaces. Whether the image exists is the launcher's provider's to say.
func ValidateAgentImage(image string) error {
	if len(image) > MaxAgentImageLength {
		return fmt.Errorf("agent_image is at most %d characters", MaxAgentImageLength)
	}
	for _, r := range image {
		if r <= ' ' || r > '~' {
			return fmt.Errorf("agent_image must be an image reference: printable characters with no spaces")
		}
	}
	return nil
}

// ExperimentFile is where a platform experiment's repository keeps the base of its definition,
// read at the head of its default branch when one is created with from_repo.
const ExperimentFile = ".hypothesisloop/experiment.yaml"

// PlatformExperiment is the operator-defined compute envelope agents compete within.
type PlatformExperiment struct {
	ID                     string  `json:"id"`
	ProjectID              string  `json:"project_id"`
	Name                   string  `json:"name"`
	Description            string  `json:"description"`
	BudgetAcceleratorHours float64 `json:"budget_accelerator_hours"` // total compute in accelerator-hours (AccH), H100-equivalent
	// ConsumedAcceleratorHours is what the platform experiment has spent of its budget: what its
	// jobs that ended were billed, measuring the baseline included. Read from the metrics store
	// when one platform experiment is read, stored nowhere, and left out of a list.
	ConsumedAcceleratorHours *float64 `json:"consumed_accelerator_hours,omitempty" doc:"What it has spent of budget_accelerator_hours: what its jobs that ended were billed, the baseline's among them. A running job's cost joins when the job ends. On a read of one platform experiment that is no draft, and the whole spend once it is closed."`
	MaxAgents                int      `json:"max_agents" doc:"How many researchers may join over its whole life: the agents of role competitor, whatever their flavor. One that finished or was cut still counts. Agents of role baseline, reviewers and the coordinator never count. On a draft: the most researchers its planner may propose, which accepting replaces with the number accepted."`
	// MaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING, summed across
	// this platform experiment's agents' jobs) at any moment. Nil defers to
	// quota.default_max_concurrent_accelerators. Speculative submission makes capacity appear on
	// demand and removes the live-capacity ceiling that used to bound concurrency implicitly —
	// this is the explicit replacement, enforced in ReserveAdmittedFlavorTx on every submit.
	MaxConcurrentAccelerators *int               `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   []MetricDefinition `json:"metrics"`                 // metrics jobs must emit; used for ranking
	ReportIntervalSeconds     int                `json:"report_interval_seconds"` // expected reporting cadence (for silent-eviction guard)
	// StartsAt is the earliest moment the platform starts the run, zero for as soon as the
	// baseline is declared. StartedAt is when the run did start, and the run lasts DurationHours
	// from then: EndsAt is the two added, written at the start. Preparing the baseline takes
	// nothing from the run.
	StartsAt      time.Time `json:"starts_at" doc:"The earliest moment the platform starts the run. The zero time when none was stated: it starts once its baseline is declared."`
	StartedAt     time.Time `json:"started_at,omitzero" doc:"When the run started: the moment it became running. Absent before."`
	DurationHours float64   `json:"duration_hours,omitzero" doc:"How long the run lasts, counted from started_at. Absent on a draft."`
	// A platform experiment has exactly one moment at which the platform closes it if nobody
	// does, under the name of what ends then: PlanningEndsAt or ProposalExpiresAt on a draft,
	// BaselineDueAt while it is open, EndsAt once it runs. One that closed keeps the one it had.
	// See ClosesAt.
	EndsAt            time.Time                `json:"ends_at,omitzero" doc:"When the run ends and the platform closes it: started_at plus duration_hours. Absent until the run starts."`
	BaselineDueAt     time.Time                `json:"baseline_due_at,omitzero" doc:"On an open platform experiment: when the platform closes it, with close_reason baseline_limit_reached, if it is not running by then. planning.baseline_limit_minutes after its starts_at, or after it was created or accepted when that is later."`
	PlanningEndsAt    time.Time                `json:"planning_ends_at,omitzero" doc:"On a draft being planned or revised: when the platform closes it if its planner has answered nothing by then."`
	ProposalExpiresAt time.Time                `json:"proposal_expires_at,omitzero" doc:"On a draft whose planner proposed or declined: when the platform closes it if no person has accepted or revised it by then."`
	Status            PlatformExperimentStatus `json:"status"`
	// StatusReason is where a draft stands with its planner, and why an open platform experiment
	// is not running yet. Worked out when one is read and stored nowhere.
	StatusReason *StatusReason `json:"status_reason,omitempty" doc:"On a draft or an open platform experiment, read one by one: where it stands. A draft's reason is planning, revising, proposed (it waits for a person to accept) or declined (its planner proposed nothing, for declined_reason, and it waits for a person to revise or close it). An open one's is why it is not running yet: baseline_required, starts_at_ahead, no_researchers, budget_spent, or starting once nothing is missing and the platform starts it within a minute."`
	// Stages is the elimination ladder, fixed at creation.
	Stages []Stage `json:"stages"`
	// CurrentStage is the 1-based index into Stages of the stage currently running.
	CurrentStage int `json:"current_stage"`
	// Summary is the operator's narrative verdict on the finished run. Prose only: the standings
	// are derived from the metrics store on read, never copied here.
	Summary        string   `json:"summary,omitempty"`
	SignedUpAgents []string `json:"signed_up_agents,omitempty"`
	SignupCount    int      `json:"signup_count"`
	// DefaultQuotaTier is used when a signup does not choose its own tier. Empty means this row
	// predates the policy and ResolveQuotaTier must use the legacy kind-based default.
	DefaultQuotaTier QuotaTier `json:"default_quota_tier,omitempty"`
	// MaxHosted is how many machines the platform may run for this platform experiment, its
	// coordinator included. Fixed at creation.
	MaxHosted int `json:"max_hosted"`
	// Shared is the platform's decision that every account may read this platform experiment and
	// everything in it. Staff set it; nobody outside its org writes to it.
	Shared bool `json:"shared"`

	// CoordinatorHosted means the platform runs this platform experiment's coordinator. Fixed at
	// creation. Coordinator says on which launcher and model and what became of its machine; it
	// is carried by a read of one platform experiment and left out of a list.
	CoordinatorHosted bool               `json:"coordinator_hosted"`
	Coordinator       *HostedCoordinator `json:"coordinator,omitempty"`

	// Repository is the code the platform experiment works on, as the deployment's git host
	// names it, and RepositoryURL the address it is cloned from. AgentImage is the image its hosted agents and
	// coordinator run, empty for the platform's own. All fixed at creation.
	Repository    string `json:"repository,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty" doc:"The HTTPS address agents and jobs clone."`
	AgentImage    string `json:"agent_image,omitempty"`

	// BaselineJobID is the completed job that measured the baseline, as the platform checked it
	// when it was declared, and JobEstimatedDurationHours the estimate of a job that states
	// none: the hours that job was observed running, with headroom. A platform experiment
	// starts only with a baseline.
	BaselineJobID             string  `json:"baseline_job_id,omitempty" doc:"The completed job that measured the baseline. Absent until one is declared with PATCH /platform-experiments/{id}."`
	JobEstimatedDurationHours float64 `json:"job_estimated_duration_hours,omitzero" doc:"The estimated_duration_hours of a job that states none: one and a half times the hours the baseline job was observed running. Absent without a baseline, and for one too short to be observed alive."`

	// Goal is the sentence a draft was created from, fixed from then on. Empty for a platform
	// experiment that was created whole.
	Goal string `json:"goal,omitempty"`
	// Actor is whose credential created it, unverified with no id on one created before this
	// was kept. Carried by a read of one platform experiment and left out of a list.
	Actor *Actor `json:"actor,omitempty"`
	// A draft's planner answers its goal in one of two ways, and the draft carries the one that
	// stands. Proposal is what it proposed, as the check on writing it took it, and ProposedAt
	// when. DeclinedReason is why it proposed nothing, the goal being none an experiment can be
	// made of, and DeclinedAt when: a decline clears the proposal made before. Both moments are
	// absent while the draft is being planned or revised, when the last proposal or reason stays
	// to be read until the planner answers again. See Answered.
	// Carried by a read of one platform experiment and left out of a list.
	Proposal       json.RawMessage `json:"proposal,omitempty" doc:"The proposal as PUT /platform-experiments/{id}/proposal took it."`
	ProposedAt     *time.Time      `json:"proposed_at,omitempty" doc:"On a draft: when its planner proposed what proposal holds and the draft began to wait for a person. Absent while it is planned or revised, and on one its planner declined."`
	DeclinedAt     *time.Time      `json:"declined_at,omitempty" doc:"On a draft: when its planner declined to propose, for declined_reason, and the draft began to wait for a person. Absent while it is planned or revised, and on one that has a proposal."`
	DeclinedReason string          `json:"declined_reason,omitempty" doc:"Why the draft's planner proposed nothing. It stays while a revision is planned and goes with the next proposal."`
	// Revisions are what a person asked the planner to change, oldest first. One made after the
	// planner's last answer, or while there is none, is not answered yet.
	Revisions []PlanningRequest `json:"revisions,omitempty"`
	// ClosedAt is when it closed and CloseReason why; ClosedBy is whose credential closed it,
	// absent when the platform did at ClosesAt. A list carries the first two and no ClosedBy.
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	CloseReason CloseReason `json:"close_reason,omitempty" enum:"end_time_reached,closed_by_request,planning_limit_reached,proposal_expired,baseline_limit_reached" doc:"Absent while it is not closed, and on one closed before reasons were kept."`
	ClosedBy    *Actor      `json:"closed_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ClosesAt is when the platform closes it if nobody does: EndsAt, BaselineDueAt, PlanningEndsAt
// or ProposalExpiresAt, whichever it has. It is the one moment stored.
func (p *PlatformExperiment) ClosesAt() time.Time {
	switch {
	case !p.PlanningEndsAt.IsZero():
		return p.PlanningEndsAt
	case !p.ProposalExpiresAt.IsZero():
		return p.ProposalExpiresAt
	case !p.BaselineDueAt.IsZero():
		return p.BaselineDueAt
	}
	return p.EndsAt
}

// Answered says a draft's planner has answered its goal, with a proposal or a decline, and the
// draft waits for a person.
func (p *PlatformExperiment) Answered() bool {
	return p.ProposedAt != nil || p.DeclinedAt != nil
}

// SetAnswered records when the planner's answer was stored, nil for none that stands, under the
// name of what the answer was: a decline leaves no proposal.
func (p *PlatformExperiment) SetAnswered(at *time.Time, proposed bool) {
	p.ProposedAt, p.DeclinedAt = nil, nil
	if proposed {
		p.ProposedAt = at
	} else {
		p.DeclinedAt = at
	}
}

// SetClosesAt records at under the name of what ends then, which Status, StartedAt, Goal,
// StartsAt and Answered decide. One that ran ends its run then. A draft, or one closed as a
// draft, which was planned from a goal and never got a start, is being planned until its planner
// answers and waits for a person from then. Any other is open, or closed before it ran, and
// has until then to be running.
func (p *PlatformExperiment) SetClosesAt(at time.Time) {
	p.EndsAt, p.BaselineDueAt, p.PlanningEndsAt, p.ProposalExpiresAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	switch {
	case !p.StartedAt.IsZero():
		p.EndsAt = at
	case p.Status != PlatformExpDraft && (p.Goal == "" || !p.StartsAt.IsZero()):
		p.BaselineDueAt = at
	case p.Answered():
		p.ProposalExpiresAt = at
	default:
		p.PlanningEndsAt = at
	}
}

// OpenUntil is the BaselineDueAt of a platform experiment that becomes open, or whose start is
// set, at now: limit after its start, or after now when it states no start or one already past.
func (p *PlatformExperiment) OpenUntil(now time.Time, limit time.Duration) time.Time {
	if p.StartsAt.After(now) {
		return p.StartsAt.Add(limit)
	}
	return now.Add(limit)
}

// StatusReason says why a platform experiment is in its status: a stable code and a sentence for
// whoever reads it.
type StatusReason struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// CloseReason is why a platform experiment closed. Nothing else closes one: a spent budget
// leaves the experiment running to its end time.
type CloseReason string

const (
	// CloseEndTimeReached is a run at its EndsAt.
	CloseEndTimeReached CloseReason = "end_time_reached"
	CloseByRequest      CloseReason = "closed_by_request"
	// ClosePlanningLimitReached is a draft whose planner answered nothing inside the planning
	// limit, and CloseProposalExpired one whose proposal or decline no person took up in time.
	ClosePlanningLimitReached CloseReason = "planning_limit_reached"
	CloseProposalExpired      CloseReason = "proposal_expired"
	// CloseBaselineLimitReached is an open platform experiment that was not running by its
	// BaselineDueAt: nobody declared a baseline for it, or it could not start with one.
	CloseBaselineLimitReached CloseReason = "baseline_limit_reached"
)

// PlanningRequest is one request to a draft's planner to change its proposal.
type PlanningRequest struct {
	Message string    `json:"message"`
	Actor   Actor     `json:"actor"`
	At      time.Time `json:"at"`
}

// MaxGoalBytes bounds the sentence a draft is created from.
const MaxGoalBytes = 2000

// SignupRole is what an agent was signed up to do in one platform experiment. It lives on the
// signup rather than on the agent — the same agent may compete in one platform experiment and
// hold the baseline in another — and is fixed at signup: changing it mid-run would retroactively
// rewrite who a completed cut applied to.
//
// Only ranking reads it. A non-competitor's jobs are admitted, billed, evicted and settled by
// identical code, and its metrics are recorded and readable in full.
type SignupRole string

const (
	// SignupRoleCompetitor is ranked, cut-eligible, takes a quota share and earns the top-3 bonus.
	SignupRoleCompetitor SignupRole = "competitor"
	// SignupRoleBaseline runs the experiment's declared control. Never ranked, never cut — the
	// point of a baseline is that its numbers are visible and comparable, not that it wins. It
	// holds a quota from the moment it joins and is the one role whose job an open platform
	// experiment takes: the baseline is measured before anyone is ranked against it. It takes
	// no share at the start, and what it spent is taken from the budget before the shares.
	SignupRoleBaseline SignupRole = "baseline"
	// SignupRoleReviewer re-checks other agents' claims and records agreement or dispute. Never
	// ranked, never cut.
	SignupRoleReviewer SignupRole = "reviewer"
	// SignupRolePlanner is the agent the platform signs up to plan a draft. It proposes the
	// experiment, runs no job, holds no quota and takes no part once the draft is accepted. No
	// request can ask for it.
	SignupRolePlanner SignupRole = "planner"
)

// ParseSignupRole resolves a requested role. An empty string is the competitor default, so every
// caller written before roles existed keeps meaning what it meant. Anything else unrecognized is
// an error: defaulting a typo to competitor would silently rank an agent nobody meant to rank.
func ParseSignupRole(s string) (SignupRole, error) {
	switch SignupRole(s) {
	case "":
		return SignupRoleCompetitor, nil
	case SignupRoleCompetitor, SignupRoleBaseline, SignupRoleReviewer:
		return SignupRole(s), nil
	}
	return "", fmt.Errorf("unknown role %q: must be one of competitor, baseline, reviewer", s)
}
