package domain

import (
	"errors"
	"fmt"
)

// ComputeLimit bounds the compute the platform pays for that an org's placed jobs reserve, in
// accelerator hours per hour on the platform's clusters: Org for all of its projects together and Project
// for each one. Admission decides on what placed jobs reserve, so lowering a limit evicts nothing.
type ComputeLimit struct {
	Org     float64 `json:"org" doc:"Accelerator hours per hour of the platform's clusters the org's placed jobs may reserve together."`
	Project float64 `json:"project" doc:"Accelerator hours per hour of the platform's clusters the placed jobs of any one of the org's projects may reserve."`
}

func (l ComputeLimit) Validate() error {
	if !(l.Project > 0) || l.Project > l.Org {
		return fmt.Errorf("platform compute limits want 0 < project <= org, got org %g and project %g", l.Org, l.Project)
	}
	return nil
}

// ClaimRefusal is why a claim placed nothing: the room was taken (NotAdmittedCapacityUnavailable)
// or the job on a platform cluster would take Holder, an org or a project, past its limit
// (NotAdmittedOrgComputeLimit, NotAdmittedProjectComputeLimit): it holds Held of Limit and the job
// needs Needs, each in accelerator hours per hour.
type ClaimRefusal struct {
	Code               string
	Holder             string
	Held, Limit, Needs float64
}

// cpuCoreSharedRate is what one core of CPUType reserves of a ComputeLimit, set at startup from
// quota.shared_acch_per_cpu_core. A core's acch_rate of 1 makes a budget read as core-hours,
// which as a share of the platform's GPU-scale limit would hold a project to a few dozen cores.
var cpuCoreSharedRate = 0.02

// SetCPUCoreSharedRate registers quota.shared_acch_per_cpu_core. Call once at startup.
func SetCPUCoreSharedRate(rate float64) {
	if !(rate > 0) {
		panic("shared_acch_per_cpu_core must be positive")
	}
	cpuCoreSharedRate = rate
}

// CPUCoreSharedRate is what one core reserves of a ComputeLimit.
func CPUCoreSharedRate() float64 { return cpuCoreSharedRate }

// AccHPerHour is what a job of count units of t reserves while it is placed: count times t's
// acch_rate, and for cores count times cpuCoreSharedRate.
func AccHPerHour(t AcceleratorType, count int) (float64, error) {
	if t.Is(CPUType) {
		return cpuCoreSharedRate * float64(count), nil
	}
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
