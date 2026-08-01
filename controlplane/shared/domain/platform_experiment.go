package domain

import "time"

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// MetricDefinition declares a single metric that jobs in a PlatformExperiment must emit.
// The key is the Prometheus metric name; direction is "maximize" or "minimize".
type MetricDefinition struct {
	Key       string `json:"key"`
	Direction string `json:"direction"` // "maximize" | "minimize"
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
	CurrentStage   int       `json:"current_stage"`
	SignedUpAgents []string  `json:"signed_up_agents,omitempty"`
	SignupCount    int       `json:"signup_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}
