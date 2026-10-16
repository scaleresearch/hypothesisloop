package workload

import "github.com/scaleresearch/hypothesisloop/contract/clusterapi"

// JobPhase and AttemptUnknown are the cluster contract's (clusterapi).
type JobPhase = clusterapi.JobPhase

const (
	JobPhasePending   = clusterapi.JobPhasePending
	JobPhaseRunning   = clusterapi.JobPhaseRunning
	JobPhaseSucceeded = clusterapi.JobPhaseSucceeded
	JobPhaseFailed    = clusterapi.JobPhaseFailed
	JobPhaseGone      = clusterapi.JobPhaseGone
)

const AttemptUnknown = clusterapi.AttemptUnknown
