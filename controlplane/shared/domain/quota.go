package domain

import (
	"math"
	"time"
)

// AgentQuota is the compute allocation for an agent within a platform experiment.
// Accelerator-hours (AccH-normalized) is the only hours-tracked dimension — RAM/storage are hard
// physical-fit-checked at admission (Experiment.Footprint()/Fits), never hours-budgeted.
type AgentQuota struct {
	ID                         string  `json:"id"`
	AgentID                    string  `json:"agent_id"`
	PlatformExperimentID       string  `json:"platform_experiment_id"`
	GuaranteedAcceleratorHours float64 `json:"guaranteed_accelerator_hours"`
	BurstAcceleratorHours      float64 `json:"burst_accelerator_hours"`
	UsedGuaranteedAccH         float64 `json:"used_guaranteed_acch"`
	UsedBurstAccH              float64 `json:"used_burst_acch"`

	CreatedAt time.Time `json:"created_at"`
}

// AvailableGuaranteed returns AccH available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteed() float64 {
	return math.Max(0, q.GuaranteedAcceleratorHours-q.UsedGuaranteedAccH)
}

// AvailableBurst returns AccH available for new burst jobs.
func (q *AgentQuota) AvailableBurst() float64 {
	return math.Max(0, q.BurstAcceleratorHours-q.UsedBurstAccH)
}

// DominantUtilization is used/guaranteed AccH — the fairness signal scheduler.sortBurst orders
// on. Always reads the *guaranteed* columns, even for burst-tier ordering: an agent's standing
// under its guaranteed allocation is the fairness signal both passes use — burst's own usage is
// deliberately not part of this ratio. Returns 0 if the agent's guaranteed AccH is 0.
func (q *AgentQuota) DominantUtilization(exp *Experiment) float64 {
	if q.GuaranteedAcceleratorHours <= 0 || exp.EstimatedCostAccH <= 0 {
		return 0
	}
	return q.UsedGuaranteedAccH / q.GuaranteedAcceleratorHours
}

// DominantCostFraction is one job's own estimated AccH divided by the agent's guaranteed AccH —
// "how big a bite out of my guaranteed budget is this job." Used by computePriority's
// cost-efficiency term and the scheduler's smallest-job-first tiebreak. Returns 0 if the agent's
// guaranteed AccH is 0.
func (q *AgentQuota) DominantCostFraction(exp *Experiment) float64 {
	if q.GuaranteedAcceleratorHours <= 0 || exp.EstimatedCostAccH <= 0 {
		return 0
	}
	return exp.EstimatedCostAccH / q.GuaranteedAcceleratorHours
}
