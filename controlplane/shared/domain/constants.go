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

// ValidExperimentStatus reports whether s is one of the recognized statuses. Callers filtering on
// a status validate here first: the column is a Postgres enum, so an unrecognized value otherwise
// surfaces as a driver error and a 500 rather than the client mistake it is.
func ValidExperimentStatus(s ExperimentStatus) bool {
	switch s {
	case StatusSubmitted, StatusQueued, StatusAdmitted, StatusRunning,
		StatusCompleted, StatusFailed, StatusEvicted, StatusRejected:
		return true
	default:
		return false
	}
}

const (
	NotAdmittedCapacityUnavailable = "capacity_unavailable"
	NotAdmittedOutranked           = "outranked"
	NotAdmittedSummaryGate         = "summary_gate"
	NotAdmittedStageCut            = "stage_cut"
	NotAdmittedJobTooLong          = "job_too_long"
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
	EvictionSilent EvictionReason = "silent"
	// EvictionNeverReportedMetrics is silence by a job that never reported a single metric, as
	// opposed to one that reported and then went quiet. The distinction matters because the
	// causes are different: a hung training process versus a reporting path that never worked
	// at all (wrong URL, a stale metrics helper baked into the image, an exception the workload
	// swallows). Reported as plain silence, both look like "the job hung".
	EvictionNeverReportedMetrics EvictionReason = "never_reported_metrics"
	EvictionCrashLoop            EvictionReason = "crash_loop"
	EvictionQuotaExhaustion      EvictionReason = "quota_exhaustion"
	EvictionExperimentClosed     EvictionReason = "experiment_closed"
	EvictionAgentRemoved         EvictionReason = "agent_removed"
	EvictionCancelled            EvictionReason = "cancelled"
	// EvictionStageCut terminates an agent's jobs when it is cut at a stage boundary.
	// Terminal for the rest of the platform experiment — see the stage ladder.
	EvictionStageCut EvictionReason = "stage_cut"
	// EvictionJobTooLong terminates a job that has run longer than the current stage's
	// max_job_hours.
	EvictionJobTooLong EvictionReason = "job_too_long"
	// EvictionStuckPending marks a job that was admitted (SUBMITTED/ADMITTED) but never
	// reported RUNNING within StuckPendingTimeoutSeconds — e.g. unschedulable due to
	// fragmentation or a bad image. See job_watcher.go.
	EvictionStuckPending EvictionReason = "stuck_pending"
	// EvictionResourceDisbalance terminates a RUNNING job whose requested CPU/memory/storage is
	// grossly out of proportion to the accelerators it holds, while that request is provably the
	// only thing keeping a queued job off idle accelerators on the same physical node. Terminal
	// rather than requeued: the job's own spec is what makes it unplaceable alongside its
	// neighbours, so returning it to the queue unchanged would evict it again on the next tick.
	// See services/scheduler/loop_disbalance.go.
	EvictionResourceDisbalance EvictionReason = "resource_disbalance"
	// EvictionUnschedulable marks a job whose runtime reported a phase-detail reason in
	// NeverSelfHealsPhaseReasons (a bad image, a malformed container config) before the full
	// StuckPendingTimeout elapsed — evicted early with a full refund rather than left to occupy
	// a reservation until the generic timeout catches it.
	EvictionUnschedulable EvictionReason = "unschedulable"
)

// PhaseDetail is what a runtime observed about why a job's container hasn't started (or
// restarted), reported alongside phase in a cluster-agent's status push. Reason is one of a
// small vocabulary the control plane defines (see the PhaseReason* constants below); each
// runtime translates its own native taxonomy (Kubernetes container-status reasons, a container
// engine's own error text) into that vocabulary, so the control plane never learns a runtime's
// vocabulary (important.md #6). Message is free text for a human/agent to read; Reason is what
// code acts on.
type PhaseDetail struct {
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	RestartCount int32  `json:"restart_count,omitempty"`
}

const (
	// PhaseReasonImagePullFailed: the runtime could not pull the job's image (unknown
	// tag/repo, no registry credentials, image genuinely absent). Never self-heals without a
	// new image being pushed or the job resubmitted with a correct reference.
	PhaseReasonImagePullFailed = "image_pull_failed"
	// PhaseReasonConfigError: the runtime rejected the job's own container configuration
	// (a malformed command, an invalid resource request, a mount that cannot be satisfied).
	// Never self-heals without the job spec changing.
	PhaseReasonConfigError = "config_error"
	// PhaseReasonInvalidImage: the image reference itself is malformed, not merely unpullable.
	PhaseReasonInvalidImage = "invalid_image"
	// PhaseReasonContainerFailed: the workload's own process exited non-zero. Deliberately NOT
	// in NeverSelfHealsPhaseReasons — a job with retries left should use them; this reports what
	// happened, it does not decide the job's fate.
	PhaseReasonContainerFailed = "container_failed"
	// PhaseReasonOOMKilled: the container was killed for exceeding its memory limit. Distinct
	// from a plain non-zero exit because the fix is different — raise the request, not the code.
	PhaseReasonOOMKilled = "oom_killed"
)

// NeverSelfHealsPhaseReasons is the set of PhaseDetail.Reason values that will not resolve on
// their own, however long a job is left SUBMITTED/ADMITTED — a scheduler retry or the runtime's
// own reconcile loop cannot fix a bad image or a malformed config. A job reporting one of these
// is evicted EvictionUnschedulable well before StuckPendingTimeout, instead of occupying a
// reservation until the generic timeout catches it.
var NeverSelfHealsPhaseReasons = map[string]bool{
	PhaseReasonImagePullFailed: true,
	PhaseReasonConfigError:     true,
	PhaseReasonInvalidImage:    true,
}
