package workload

type JobPhase int

const (
	JobPhasePending JobPhase = iota
	JobPhaseRunning
	JobPhaseSucceeded
	JobPhaseFailed
	JobPhaseGone
)

// String renders the phase as a stable lowercase name for wire transmission (e.g. cluster-agent
// status reports), where the int representation wouldn't be meaningful across processes.
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

// AttemptUnknown is the attempt of a workload whose generation cannot be established: a
// cluster-agent that predates the attempt field reports none, and its silence must survive
// decoding as silence. Read as 0 instead it would be indistinguishable from a genuine first
// attempt — first attempts would pass the generation fence while every retry read as a foreign
// generation and had its status discarded for the life of the job.
const AttemptUnknown = -1
