package quota

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// JobCost is what one job actually cost, as opposed to what it reserved. Every field is derived
// from the metrics store on read — nothing here is stored in Postgres, and nothing here is an
// estimate. Without this, a settled cost was only reachable by hand-writing PromQL, on a platform
// whose whole premise is metered research spend.
type JobCost struct {
	ExperimentID string `json:"experiment_id"`
	Status       string `json:"status"`
	// Settled is true once the terminal write has landed. Until then the figures below are the
	// live observed-so-far cost, which grows every tick.
	Settled bool `json:"settled"`
	// ObservedHours is confirmed-alive time, not wall-clock since submission: gaps longer than
	// the reporting cadence allows are not billed, and a job that never reported bills nothing.
	ObservedHours     float64 `json:"observed_hours"`
	AcceleratorHours  float64 `json:"accelerator_hours"`
	EstimatedCostAccH float64 `json:"estimated_cost_accelerator_hours"`
	AcceleratorType   string  `json:"accelerator_type,omitempty"`
	AcceleratorCount  int     `json:"accelerator_count,omitempty"`
}

// JobCost reports one job's actual cost. A terminal job reports what was settled; a running job
// reports its observed-so-far cost, computed the same way the controller's quota-exhaustion check
// computes it, so the two can never disagree about what a job has consumed.
func (s *PlatformExperimentsService) JobCost(ctx context.Context, experimentID string) (*JobCost, error) {
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("quota.JobCost: %w", err)
	}
	if exp == nil {
		return nil, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
	if err != nil {
		return nil, fmt.Errorf("quota.JobCost: %w", err)
	}
	if pe == nil {
		return nil, fmt.Errorf("quota.JobCost: platform experiment %s not found", exp.PlatformExperimentID)
	}

	out := &JobCost{
		ExperimentID:      exp.ID,
		Status:            string(exp.Status),
		EstimatedCostAccH: exp.EstimatedCostAccH,
		AcceleratorType:   string(exp.AcceleratorType),
		AcceleratorCount:  exp.AcceleratorCount,
	}

	settled, ok, err := metricsdb.SettledCostForJob(ctx, s.metricsDBURL, pe.CreatedAt, exp.ID)
	if err != nil {
		return nil, fmt.Errorf("quota.JobCost: %w", err)
	}
	if ok {
		out.Settled = true
		out.AcceleratorHours = settled[domain.ResourceAcceleratorHours]
	}

	// Observed hours are worth reporting either way: for a settled job they are the window the
	// bill was computed over, for a running one they are the bill so far.
	rc, err := s.newRunningCostCalc().costOf(ctx, exp)
	if err != nil {
		return nil, fmt.Errorf("quota.JobCost: %w", err)
	}
	out.ObservedHours = rc.hours
	if !ok {
		out.AcceleratorHours = rc.accH
	}
	return out, nil
}
