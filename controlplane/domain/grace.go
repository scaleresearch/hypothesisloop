package domain

import "fmt"

// GraceLimits are the operator's shutdown and checkpoint grace bounds. Admission resolves them
// into the job spec it hands to a runtime, which applies the result verbatim.
type GraceLimits struct {
	DefaultShutdownSeconds int64
	MaxShutdownSeconds     int64
	MaxCheckpointSeconds   int64
}

// Resolve returns job with TerminationGracePeriodSeconds always set (the requested value or the
// default, capped) and CheckpointGraceSeconds capped. A nil CheckpointGraceSeconds stays nil:
// the job declared no window.
func (g GraceLimits) Resolve(job JobSpec) (JobSpec, error) {
	if g.DefaultShutdownSeconds <= 0 || g.MaxShutdownSeconds <= 0 || g.MaxCheckpointSeconds <= 0 {
		return JobSpec{}, fmt.Errorf("grace limits must be positive: %+v", g)
	}
	shutdown := g.DefaultShutdownSeconds
	if job.TerminationGracePeriodSeconds != nil {
		shutdown = *job.TerminationGracePeriodSeconds
	}
	shutdown = min(shutdown, g.MaxShutdownSeconds)
	job.TerminationGracePeriodSeconds = &shutdown
	if job.CheckpointGraceSeconds != nil {
		checkpoint := min(*job.CheckpointGraceSeconds, g.MaxCheckpointSeconds)
		job.CheckpointGraceSeconds = &checkpoint
	}
	return job, nil
}
