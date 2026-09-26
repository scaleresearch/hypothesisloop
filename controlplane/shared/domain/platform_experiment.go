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
	// runs works out what to measure and proposes the rest, nobody else joins, and nothing but
	// the planning allowance is spent until a person accepts the proposal, which opens it.
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
	// MetricRoleConstraint must be reported and must satisfy Bound; a job that violates it is
	// ineligible for standings but never itself ranked or cut on.
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

// GitHubHost is where every repository lives.
const GitHubHost = "github.com"

// RepositoryURL is the HTTPS address a repository named owner/name is cloned from, empty for none.
func RepositoryURL(repository string) string {
	if repository == "" {
		return ""
	}
	return "https://" + GitHubHost + "/" + repository + ".git"
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
	MaxAgents              int     `json:"max_agents"`
	// MaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING, summed across
	// this platform experiment's agents' jobs) at any moment. Nil defers to
	// quota.default_max_concurrent_accelerators. Speculative submission makes capacity appear on
	// demand and removes the live-capacity ceiling that used to bound concurrency implicitly —
	// this is the explicit replacement, enforced in ReserveAdmittedFlavorTx on every submit.
	MaxConcurrentAccelerators *int                     `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   []MetricDefinition       `json:"metrics"`                 // metrics jobs must emit; used for ranking
	ReportIntervalSeconds     int                      `json:"report_interval_seconds"` // expected reporting cadence (for silent-eviction guard)
	StartsAt                  time.Time                `json:"starts_at"`
	EndsAt                    time.Time                `json:"ends_at"`
	Status                    PlatformExperimentStatus `json:"status"`
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

	// Repository is the code the platform experiment works on, owner/name on GitHub, and
	// RepositoryURL the address it is cloned from. AgentImage is the image its hosted agents and
	// coordinator run, empty for the platform's own. All fixed at creation.
	Repository    string `json:"repository,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty" doc:"The HTTPS address agents and jobs clone, derived from repository."`
	AgentImage    string `json:"agent_image,omitempty"`

	// Goal is the sentence a draft was created from, fixed from then on. Empty for a platform
	// experiment that was created whole.
	Goal string `json:"goal,omitempty"`
	// Actor is whose credential created it, unverified with no id on one created before this
	// was kept. Carried by a read of one platform experiment and left out of a list.
	Actor *Actor `json:"actor,omitempty"`
	// Proposal is what a draft's planner proposed, as it was accepted by the check on writing it,
	// and ProposedAt when. ProposedAt is absent while a draft is being planned or revised.
	// Carried by a read of one platform experiment and left out of a list.
	Proposal   json.RawMessage `json:"proposal,omitempty" doc:"The proposal as PUT /platform-experiments/{id}/proposal took it."`
	ProposedAt *time.Time      `json:"proposed_at,omitempty"`
	// DeclinedReason is why a draft's planner proposed nothing: the goal could not be made into
	// an experiment. With it ProposedAt is when the planner said so and there is no Proposal.
	// It stays while a revision is planned and goes with the next proposal.
	DeclinedReason string `json:"declined_reason,omitempty"`
	// Revisions are what a person asked the planner to change, oldest first. One made after
	// ProposedAt, or with ProposedAt absent, is not answered by a proposal yet.
	Revisions []PlanningRequest `json:"revisions,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

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
	// point of a baseline is that its numbers are visible and comparable, not that it wins.
	SignupRoleBaseline SignupRole = "baseline"
	// SignupRoleReviewer re-checks other agents' claims and records agreement or dispute. Never
	// ranked, never cut.
	SignupRoleReviewer SignupRole = "reviewer"
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
