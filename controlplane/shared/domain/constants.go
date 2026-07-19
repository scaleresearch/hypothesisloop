package domain

// MinRemainingHours is the floor for RemainingEstimatedHours (15 min).
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

// NotAdmittedReason classifies why a QUEUED job wasn't admitted on its most recent skipped
// tick — updated by Loop.tick each time it skips a job, cleared on admission.
const (
	// NotAdmittedCapacityUnavailable: no capacity for this flavor existed at all at the start
	// of the tick (even before any of this tick's own admissions), and — for guaranteed jobs —
	// preempting all same-flavor burst jobs still wasn't enough.
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	// NotAdmittedOutranked: capacity for this flavor existed at the start of the tick, but
	// other (higher-ranked) jobs in the same tier admitted ahead of this one already consumed
	// it this tick.
	NotAdmittedOutranked = "outranked"
	// NotAdmittedSummaryGate: the agent has a COMPLETED experiment without a summary — all of
	// its other jobs are held until one is written.
	NotAdmittedSummaryGate = "summary_gate"
)
