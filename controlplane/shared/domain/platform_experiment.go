package domain

import (
	"fmt"
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

// PlatformExperiment is the operator-defined compute envelope agents compete within.
type PlatformExperiment struct {
	ID                     string  `json:"id"`
	Name                   string  `json:"name"`
	Description            string  `json:"description"`
	BudgetAcceleratorHours float64 `json:"budget_accelerator_hours"` // total compute in accelerator-hours (AccH), H100-equivalent
	// BudgetCPUCoreHours is an optional additional resource budget tracked the same way as
	// BudgetAcceleratorHours. Current PostgreSQL estimates and settled metrics observations
	// contribute to its guaranteed/burst usage. Zero means the dimension is not tracked.
	BudgetCPUCoreHours float64 `json:"budget_cpu_core_hours,omitempty"`
	// BudgetRAMGBHours/BudgetStorageGBHours: Deprecated. RAM/storage are hard
	// physical-fit-checked at admission, never
	// hours-budgeted (see domain.ResourceRAMGBHours' doc comment for the full migration note).
	// Nothing reads these two fields to gate or debit anything anymore; the JSON keys are kept
	// accepted (not rejected) purely so an existing caller that still sends them doesn't start
	// failing submission, and a platform experiment created before this migration with a
	// non-zero value here simply keeps it inert in the DB — no debit ever consumes it again.
	BudgetRAMGBHours      float64                  `json:"budget_ram_gb_hours,omitempty"`
	BudgetStorageGBHours  float64                  `json:"budget_storage_gb_hours,omitempty"`
	MaxAgents             int                      `json:"max_agents"`
	Metrics               []MetricDefinition       `json:"metrics"`                 // metrics jobs must emit; used for ranking
	ReportIntervalSeconds int                      `json:"report_interval_seconds"` // expected reporting cadence (for silent-eviction guard)
	StartsAt              time.Time                `json:"starts_at"`
	EndsAt                time.Time                `json:"ends_at"`
	Status                PlatformExperimentStatus `json:"status"`
	// Stages is the elimination ladder, fixed at creation. See docs/stages.md.
	Stages []Stage `json:"stages"`
	// CurrentStage is the 1-based index into Stages of the stage currently running.
	CurrentStage int `json:"current_stage"`
	// Summary is the operator's narrative verdict on the finished run. Prose only: the standings
	// are derived from the metrics store on read, never copied here.
	Summary        string    `json:"summary,omitempty"`
	SignedUpAgents []string  `json:"signed_up_agents,omitempty"`
	SignupCount    int       `json:"signup_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
