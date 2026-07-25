package domain

import (
	"math"
	"time"
)

// AgentQuota is the compute allocation for an agent within a platform experiment. Accelerator-hours
// (AccH-normalized) is the original, always-populated dimension; CPU/RAM/storage fields are
// zero ("not tracked") unless the platform experiment sets a non-zero budget for them.
type AgentQuota struct {
	ID                         string  `json:"id"`
	AgentID                    string  `json:"agent_id"`
	PlatformExperimentID       string  `json:"platform_experiment_id"`
	GuaranteedAcceleratorHours float64 `json:"guaranteed_accelerator_hours"`
	BurstAcceleratorHours      float64 `json:"burst_accelerator_hours"`
	UsedGuaranteedAccH         float64 `json:"used_guaranteed_acch"`
	UsedBurstAccH              float64 `json:"used_burst_acch"`

	GuaranteedCPUCoreHours float64 `json:"guaranteed_cpu_core_hours,omitempty"`
	BurstCPUCoreHours      float64 `json:"burst_cpu_core_hours,omitempty"`
	UsedGuaranteedCPUCoreH float64 `json:"used_guaranteed_cpu_core_h,omitempty"`
	UsedBurstCPUCoreH      float64 `json:"used_burst_cpu_core_h,omitempty"`

	GuaranteedRAMGBHours float64 `json:"guaranteed_ram_gb_hours,omitempty"`
	BurstRAMGBHours      float64 `json:"burst_ram_gb_hours,omitempty"`
	UsedGuaranteedRAMGBH float64 `json:"used_guaranteed_ram_gb_h,omitempty"`
	UsedBurstRAMGBH      float64 `json:"used_burst_ram_gb_h,omitempty"`

	GuaranteedStorageGBHours float64 `json:"guaranteed_storage_gb_hours,omitempty"`
	BurstStorageGBHours      float64 `json:"burst_storage_gb_hours,omitempty"`
	UsedGuaranteedStorageGBH float64 `json:"used_guaranteed_storage_gb_h,omitempty"`
	UsedBurstStorageGBH      float64 `json:"used_burst_storage_gb_h,omitempty"`

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

// AvailableGuaranteedCPU returns CPU-core-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedCPU() float64 {
	return math.Max(0, q.GuaranteedCPUCoreHours-q.UsedGuaranteedCPUCoreH)
}

// AvailableBurstCPU returns CPU-core-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstCPU() float64 {
	return math.Max(0, q.BurstCPUCoreHours-q.UsedBurstCPUCoreH)
}

// AvailableGuaranteedRAM returns RAM-GB-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedRAM() float64 {
	return math.Max(0, q.GuaranteedRAMGBHours-q.UsedGuaranteedRAMGBH)
}

// AvailableBurstRAM returns RAM-GB-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstRAM() float64 {
	return math.Max(0, q.BurstRAMGBHours-q.UsedBurstRAMGBH)
}

// AvailableGuaranteedStorage returns storage-GB-hours available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteedStorage() float64 {
	return math.Max(0, q.GuaranteedStorageGBHours-q.UsedGuaranteedStorageGBH)
}

// AvailableBurstStorage returns storage-GB-hours available for new burst jobs.
func (q *AgentQuota) AvailableBurstStorage() float64 {
	return math.Max(0, q.BurstStorageGBHours-q.UsedBurstStorageGBH)
}

// DominantUtilization implements dominant resource fairness generalized across every
// hours-tracked dimension (Accelerator/CPU today — RAM/storage are Class B and drop out, see
// ResourceRAMGBHours): max(used/guaranteed) over dimensions exp actually requests (estimated
// amount > 0) AND q tracks (guaranteed > 0). A dimension neither requested nor tracked is
// EXCLUDED from the max, not treated as 0 — otherwise an agent exhausted on accelerator quota
// but untouched on CPU would look artificially idle from an irrelevant 0/0 ratio. Returns 0 if
// no dimension is both requested and tracked.
//
// Always reads the *guaranteed* columns, even for burst-tier ordering: an agent's standing under
// its guaranteed allocation is the fairness signal both passes use (see scheduler.sortBurst) —
// burst's own usage is deliberately not part of this ratio.
func (q *AgentQuota) DominantUtilization(exp *Experiment) float64 {
	dominant := 0.0
	consider := func(used, guaranteed, requested float64) {
		if guaranteed <= 0 || requested <= 0 {
			return
		}
		if u := used / guaranteed; u > dominant {
			dominant = u
		}
	}
	consider(q.UsedGuaranteedAccH, q.GuaranteedAcceleratorHours, exp.EstimatedCostAccH)
	consider(q.UsedGuaranteedCPUCoreH, q.GuaranteedCPUCoreHours, exp.EstimatedCPUCoreHours)
	consider(q.UsedGuaranteedRAMGBH, q.GuaranteedRAMGBHours, exp.EstimatedRAMGBHours)
	consider(q.UsedGuaranteedStorageGBH, q.GuaranteedStorageGBHours, exp.EstimatedStorageGBHours)
	return dominant
}

// DominantCostFraction returns max(requested/guaranteed) across the same requested-AND-tracked
// dimensions as DominantUtilization, but for one job's own estimated amount rather than the
// agent's cumulative usage — a dimensionless "how big a bite out of my guaranteed budget is this
// job" fraction, comparable across resource types unlike summing raw, unit-incompatible hours.
// Used by computePriority's cost-efficiency term and the scheduler's smallest-job-first tiebreak.
// Returns 0 if no dimension is both requested and tracked.
func (q *AgentQuota) DominantCostFraction(exp *Experiment) float64 {
	dominant := 0.0
	consider := func(requested, guaranteed float64) {
		if guaranteed <= 0 || requested <= 0 {
			return
		}
		if f := requested / guaranteed; f > dominant {
			dominant = f
		}
	}
	consider(exp.EstimatedCostAccH, q.GuaranteedAcceleratorHours)
	consider(exp.EstimatedCPUCoreHours, q.GuaranteedCPUCoreHours)
	consider(exp.EstimatedRAMGBHours, q.GuaranteedRAMGBHours)
	consider(exp.EstimatedStorageGBHours, q.GuaranteedStorageGBHours)
	return dominant
}
