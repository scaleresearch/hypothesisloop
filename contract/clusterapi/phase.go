package clusterapi

// JobPhase is where a job's workload is in its life, as a runtime observes it.
type JobPhase int

const (
	JobPhasePending JobPhase = iota
	JobPhaseRunning
	JobPhaseSucceeded
	JobPhaseFailed
	JobPhaseGone
)

// JobPhases is every phase, in its order.
var JobPhases = []JobPhase{JobPhasePending, JobPhaseRunning, JobPhaseSucceeded, JobPhaseFailed, JobPhaseGone}

// ParseJobPhase is the phase whose name on the wire is name, false for a name of none.
func ParseJobPhase(name string) (JobPhase, bool) {
	for _, p := range JobPhases {
		if p.String() == name {
			return p, true
		}
	}
	return JobPhasePending, false
}

// String is the phase's name on the wire (StatusReport.Phase).
func (p JobPhase) String() string {
	switch p {
	case JobPhaseRunning:
		return "running"
	case JobPhaseSucceeded:
		return "succeeded"
	case JobPhaseFailed:
		return "failed"
	case JobPhaseGone:
		return "gone"
	default:
		return "pending"
	}
}

// AttemptUnknown is the attempt of a workload whose generation cannot be established. On the
// wire it is the absence of StatusReport.Attempt: read as 0 it would pass for a first attempt.
const AttemptUnknown = -1

// The reasons a runtime explains a job's phase with (StatusReport.Reason). Each runtime
// translates its own native vocabulary into these, so the control plane never learns one.
const (
	// PhaseReasonImagePullFailed: the job's image could not be pulled.
	PhaseReasonImagePullFailed = "image_pull_failed"
	// PhaseReasonConfigError: the runtime rejected the job's own container configuration.
	PhaseReasonConfigError = "config_error"
	// PhaseReasonInvalidImage: the image reference itself is malformed.
	PhaseReasonInvalidImage = "invalid_image"
	// PhaseReasonContainerFailed: the job's process exited non-zero.
	PhaseReasonContainerFailed = "container_failed"
	// PhaseReasonOOMKilled: the container was killed for exceeding its memory limit.
	PhaseReasonOOMKilled = "oom_killed"
	// PhaseReasonCodeUnreachable: the job's process ended with ExitCodeCodeUnreachable.
	PhaseReasonCodeUnreachable = "code_unreachable"
	// PhaseReasonNoOffer: a runtime that rents machines found none to rent for the job within
	// the time its operator allows.
	PhaseReasonNoOffer = "no_offer"
	// PhaseReasonWaitingForOffer and PhaseReasonInstanceStarting say what a job on rented
	// compute waits for.
	PhaseReasonWaitingForOffer  = "waiting_for_offer"
	PhaseReasonInstanceStarting = "instance_starting"
	// PhaseReasonInstanceUnknown: the job's rented machine is listed and its provider does not
	// say whether the job runs on it.
	PhaseReasonInstanceUnknown = "instance_unknown"
	// PhaseReasonScaleUpRefused: the cluster's autoscaler gave up on adding a node for a pod of
	// the job that waits for one. The message is the autoscaler's.
	PhaseReasonScaleUpRefused = "scale_up_refused"
	// PhaseReasonCreateFailed: the runtime tried to create the job's workload and its platform
	// refused. The message is the refusal.
	PhaseReasonCreateFailed = "create_failed"
)

// ExitCodeCodeUnreachable is the one exit code with a meaning to the platform: hl-clone ends with
// it when neither the platform's git credential nor the repository answered, so the job's own
// command never started. It is EX_TEMPFAIL of sysexits.h.
const ExitCodeCodeUnreachable = 75

// PhaseReasonOfExit is the reason a process that ended with a non-zero code is reported under.
func PhaseReasonOfExit(code int) string {
	if code == ExitCodeCodeUnreachable {
		return PhaseReasonCodeUnreachable
	}
	return PhaseReasonContainerFailed
}
