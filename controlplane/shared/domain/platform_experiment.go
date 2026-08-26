package domain

import (
	"fmt"
	"reflect"
	"time"
)

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// ValidPlatformExperimentStatus reports whether s is one of the recognized statuses. As with
// ValidExperimentStatus, filtering on an unrecognized value would otherwise reach Postgres as a
// bad enum literal and surface as a 500.
func ValidPlatformExperimentStatus(s PlatformExperimentStatus) bool {
	switch s {
	case PlatformExpOpen, PlatformExpRunning, PlatformExpClosed:
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

// RequireRankingMetric rejects a metric contract that can never rank anyone. Constraint and
// attribute metrics gate and describe; only a ranking metric orders a field. Without one the
// ladder runs to the end cutting nobody and the results endpoint returns no standings — a run
// that consumed a real budget and answered nothing, silently. Enforced where the contract is
// written (create/update), not on read: a row predating this rule must still reconcile.
func RequireRankingMetric(defs []MetricDefinition) error {
	for _, d := range defs {
		if d.EffectiveRole() == MetricRoleRanking {
			return nil
		}
	}
	return fmt.Errorf("at least one metric must have role \"ranking\": nothing else can order the field or cut a stage")
}

// MetricDefinitionsEqual reports whether two metric declarations are the same set in the same
// order — order matters (the first ranking metric is primary), so this is not a set comparison.
func MetricDefinitionsEqual(a, b []MetricDefinition) bool {
	return reflect.DeepEqual(a, b)
}

// PlatformExperiment is the operator-defined compute envelope agents compete within.
type PlatformExperiment struct {
	ID                     string  `json:"id"`
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
	// HypothesisSubmitPolicy/JobSubmitPolicy independently control who may register a hypothesis
	// vs. submit a job within this platform experiment — see SubmitterPolicy. Empty resolves to
	// SubmitterPolicyMixed (today's behavior, unchanged) via ParseSubmitterPolicy.
	HypothesisSubmitPolicy SubmitterPolicy `json:"hypothesis_submit_policy,omitempty"`
	JobSubmitPolicy        SubmitterPolicy `json:"job_submit_policy,omitempty"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

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

// SubmitterPolicy governs who may submit into a platform experiment — a hypothesis or a job,
// each gated independently (see PlatformExperiment.HypothesisSubmitPolicy/JobSubmitPolicy) —
// distinguishing a human identity from an autonomous agent identity.
type SubmitterPolicy string

const (
	// SubmitterPolicyMixed allows both humans and agents — today's behavior, unchanged, and the
	// default when the field is left empty.
	SubmitterPolicyMixed SubmitterPolicy = "mixed"
	// SubmitterPolicyHumanOnly allows only a human submitter.
	SubmitterPolicyHumanOnly SubmitterPolicy = "human_only"
	// SubmitterPolicyAgentOnly allows only an autonomous-agent submitter.
	SubmitterPolicyAgentOnly SubmitterPolicy = "agent_only"
)

// ParseSubmitterPolicy resolves a requested policy. An empty string is the mixed default, so
// every caller written before this field existed keeps meaning what it meant. Anything else
// unrecognized is an error: defaulting a typo to mixed would silently open a restricted platform
// experiment to submitters the operator meant to exclude.
func ParseSubmitterPolicy(s string) (SubmitterPolicy, error) {
	switch SubmitterPolicy(s) {
	case "":
		return SubmitterPolicyMixed, nil
	case SubmitterPolicyMixed, SubmitterPolicyHumanOnly, SubmitterPolicyAgentOnly:
		return SubmitterPolicy(s), nil
	}
	return "", fmt.Errorf("unknown submitter policy %q: must be one of mixed, human_only, agent_only", s)
}

// AllowsHuman reports whether p permits a human submitter.
func (p SubmitterPolicy) AllowsHuman() bool {
	return p != SubmitterPolicyAgentOnly
}

// AllowsAgent reports whether p permits an autonomous-agent submitter.
func (p SubmitterPolicy) AllowsAgent() bool {
	return p != SubmitterPolicyHumanOnly
}
