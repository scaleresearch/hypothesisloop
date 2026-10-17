// Package platformapi is the vocabulary of the platform's API that its clients read and write
// beside the OpenAPI document: the values a client branches on and the bodies it sends.
package platformapi

import "slices"

// ExperimentStatus is the lifecycle state of a job.
type ExperimentStatus string

const (
	StatusSubmitted ExperimentStatus = "SUBMITTED"
	StatusQueued    ExperimentStatus = "QUEUED"
	StatusRunning   ExperimentStatus = "RUNNING"
	StatusCompleted ExperimentStatus = "COMPLETED"
	StatusFailed    ExperimentStatus = "FAILED"
	StatusEvicted   ExperimentStatus = "EVICTED"
	StatusRejected  ExperimentStatus = "REJECTED"
	// StatusCancelled is what a job somebody cancelled is answered as, and no row holds it: such
	// a job is stored REJECTED when it never ran and EVICTED when it did.
	StatusCancelled ExperimentStatus = "CANCELLED"
)

// ExperimentStatuses is every status a job is answered with.
var ExperimentStatuses = []ExperimentStatus{StatusSubmitted, StatusQueued, StatusRunning, StatusCompleted, StatusFailed, StatusEvicted, StatusRejected, StatusCancelled}

// EndedStatuses are the statuses a job ends in: no execution or report follows one.
var EndedStatuses = []ExperimentStatus{StatusCompleted, StatusFailed, StatusEvicted, StatusRejected, StatusCancelled}

// Ended reports whether a job of this status has ended.
func (s ExperimentStatus) Ended() bool { return slices.Contains(EndedStatuses, s) }

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	// PlatformExpDraft is a platform experiment being planned from a goal: a planner the platform
	// runs works out what to measure and proposes the rest, nobody else joins, and no job runs
	// until a person accepts the proposal, which opens it.
	PlatformExpDraft   PlatformExperimentStatus = "draft"
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// PlatformExperimentStatuses is every status of a platform experiment, in the order it goes through them.
var PlatformExperimentStatuses = []PlatformExperimentStatus{PlatformExpDraft, PlatformExpOpen, PlatformExpRunning, PlatformExpClosed}
