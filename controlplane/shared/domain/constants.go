package domain

// MinRemainingHours is the minimum rescheduled duration after preemption (15 minutes).
const MinRemainingHours = 0.25

// Phase1ExploreFraction is the fraction of total budget consumed before phase-2 eviction
// triggers. Agents' initial quotas are capped to this fraction so no single agent can
// exhaust the explore window alone.
const Phase1ExploreFraction = 0.40

// ExperimentStatus represents the lifecycle state of an agent job.
type ExperimentStatus string

const (
	StatusSubmitted ExperimentStatus = "SUBMITTED"
	StatusQueued    ExperimentStatus = "QUEUED"
	StatusAdmitted  ExperimentStatus = "ADMITTED"
	StatusRunning   ExperimentStatus = "RUNNING"
	StatusCompleted ExperimentStatus = "COMPLETED"
	StatusFailed    ExperimentStatus = "FAILED"
	StatusEvicted   ExperimentStatus = "EVICTED"
	StatusRejected  ExperimentStatus = "REJECTED"
)

const (
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	NotAdmittedOutranked           = "outranked"
	NotAdmittedSummaryGate         = "summary_gate"
	NotAdmittedPhase2Hold          = "phase2_hold"
	NotAdmittedWorkloadCreation    = "workload_creation_failed"
)

// IsTerminal reports whether the status is a final lifecycle state that no further execution
// or progress reporting can follow.
func (s ExperimentStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusEvicted, StatusRejected:
		return true
	default:
		return false
	}
}

// EvictionReason classifies why a job was terminated early.
type EvictionReason string

const (
	EvictionSilent           EvictionReason = "silent"
	EvictionOverrun          EvictionReason = "overrun"
	EvictionCrashLoop        EvictionReason = "crash_loop"
	EvictionQuotaExhaustion  EvictionReason = "quota_exhaustion"
	EvictionExperimentClosed EvictionReason = "experiment_closed"
	EvictionAgentRemoved     EvictionReason = "agent_removed"
	EvictionCancelled        EvictionReason = "cancelled"
	EvictionPhase2Hold       EvictionReason = "phase2_hold"
	EvictionMetricDecline    EvictionReason = "metric_decline"
	// EvictionStuckPending marks a job that was admitted (SUBMITTED/ADMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
)
