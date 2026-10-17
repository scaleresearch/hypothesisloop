package domain

import (
	"errors"
	"fmt"
)

// ComputeLimit bounds the compute the platform pays for that an org's placed jobs reserve, in
// accelerator hours per hour on shared clusters: Org for all of its projects together and Project
// for each one. Admission decides on what placed jobs reserve, so lowering a limit evicts nothing.
type ComputeLimit struct {
	Org     float64 `json:"org" doc:"Accelerator hours per hour of the platform's shared clusters the org's placed jobs may reserve together."`
	Project float64 `json:"project" doc:"Accelerator hours per hour of the platform's shared clusters the placed jobs of any one of the org's projects may reserve."`
}

func (l ComputeLimit) Validate() error {
	if !(l.Project > 0) || l.Project > l.Org {
		return fmt.Errorf("platform compute limits want 0 < project <= org, got org %g and project %g", l.Org, l.Project)
	}
	return nil
}

// ClaimRefusal is why a claim placed nothing: the room was taken (NotAdmittedCapacityUnavailable)
// or the job on a shared cluster would take Holder, an org or a project, past its limit
// (NotAdmittedOrgComputeLimit, NotAdmittedProjectComputeLimit): it holds Held of Limit and the job
// needs Needs, each in accelerator hours per hour.
type ClaimRefusal struct {
	Code               string
	Holder             string
	Held, Limit, Needs float64
}

// AccHPerHour is what a job of count units of t reserves while it is placed.
func AccHPerHour(t AcceleratorType, count int) (float64, error) {
	rate, ok := LookupAcceleratorCost(t)
	if !ok {
		return 0, errors.New("accelerator type " + string(t) + " has no configured acch_rate")
	}
	return rate * float64(count), nil
}

// ExceedsLimit reports whether reserved is above limit by more than sums of float rates drift.
func ExceedsLimit(reserved, limit float64) bool {
	return reserved > limit*(1+1e-9)
}
