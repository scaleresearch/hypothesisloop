package domain

// MinRemainingHours is the minimum rescheduled duration after preemption (15 minutes).
const MinRemainingHours = 0.25

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
	NotAdmittedStageCut            = "stage_cut"
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
	EvictionCrashLoop        EvictionReason = "crash_loop"
	EvictionQuotaExhaustion  EvictionReason = "quota_exhaustion"
	EvictionExperimentClosed EvictionReason = "experiment_closed"
	EvictionAgentRemoved     EvictionReason = "agent_removed"
	EvictionCancelled        EvictionReason = "cancelled"
	// EvictionStageCut terminates an agent's jobs when it is cut at a stage boundary.
	// Terminal for the rest of the platform experiment — see docs/stages.md.
	EvictionStageCut EvictionReason = "stage_cut"
	// EvictionStuckPending marks a job that was admitted (SUBMITTED/ADMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
)
