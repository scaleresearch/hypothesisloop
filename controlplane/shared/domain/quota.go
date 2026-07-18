package domain

import (
	"math"
	"time"
)

// AgentQuota is the compute allocation for an agent within a platform experiment. GPU-hours
// (T4h-normalized) is the original, always-populated dimension; CPU/RAM/storage fields are
// zero ("not tracked") unless the platform experiment sets a non-zero budget for them.
type AgentQuota struct {
	ID                   string  `json:"id"`
	AgentID              string  `json:"agent_id"`
	PlatformExperimentID string  `json:"platform_experiment_id"`
	GuaranteedT4Hours    float64 `json:"guaranteed_t4_hours"`
	BurstT4Hours         float64 `json:"burst_t4_hours"`
	UsedGuaranteedT4H    float64 `json:"used_guaranteed_t4h"`
	UsedBurstT4H         float64 `json:"used_burst_t4h"`

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

// AvailableGuaranteed returns T4h available for new guaranteed jobs.
func (q *AgentQuota) AvailableGuaranteed() float64 {
	return math.Max(0, q.GuaranteedT4Hours-q.UsedGuaranteedT4H)
}

// AvailableBurst returns T4h available for new burst jobs.
func (q *AgentQuota) AvailableBurst() float64 {
	return math.Max(0, q.BurstT4Hours-q.UsedBurstT4H)
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
// hours-tracked dimension (GPU/CPU today — RAM/storage are Class B and never estimated, see
// ResourceRAMGBHours' doc comment, so they naturally drop out below): max(used/guaranteed) over
// the dimensions exp actually requests (its own estimated amount > 0) AND that q tracks
// (guaranteed > 0). A dimension exp doesn't request, or q doesn't track at all, is EXCLUDED
// from the max, not treated as 0 utilization — the latter would make an agent that's exhausted
// its GPU quota but never touched an untracked CPU quota look artificially idle just because
// one irrelevant ratio happens to read 0/0. Returns 0 if no dimension is both requested and
// tracked (nothing to be unfair about).
//
// Always reads the *guaranteed* columns, even when called for burst-tier ordering: an agent's
// standing under its guaranteed allocation is the fairness signal both passes use (see
// scheduler.sortBurst's doc comment) — burst's own usage is deliberately not part of this ratio.
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
	consider(q.UsedGuaranteedT4H, q.GuaranteedT4Hours, exp.EstimatedCostT4H)
	consider(q.UsedGuaranteedCPUCoreH, q.GuaranteedCPUCoreHours, exp.EstimatedCPUCoreHours)
	consider(q.UsedGuaranteedRAMGBH, q.GuaranteedRAMGBHours, exp.EstimatedRAMGBHours)
	consider(q.UsedGuaranteedStorageGBH, q.GuaranteedStorageGBHours, exp.EstimatedStorageGBHours)
	return dominant
}

// DominantCostFraction returns max(requested/guaranteed) across the same requested-AND-tracked
// dimensions as DominantUtilization, but for one job's own estimated amount rather than the
// agent's cumulative usage — a dimensionless "how big a bite out of my own guaranteed budget is
// this one job" fraction, comparable across CPU/GPU/RAM/storage jobs unlike summing raw,
// unit-incompatible hours together. Used by computePriority's cost-efficiency term and by the
// scheduler's smallest-job-first sort tiebreak (replacing the old GPU-only GPUHours(), which
// was always zero for CPU-only jobs). Returns 0 if no dimension is both requested and tracked.
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
	consider(exp.EstimatedCostT4H, q.GuaranteedT4Hours)
	consider(exp.EstimatedCPUCoreHours, q.GuaranteedCPUCoreHours)
	consider(exp.EstimatedRAMGBHours, q.GuaranteedRAMGBHours)
	consider(exp.EstimatedStorageGBHours, q.GuaranteedStorageGBHours)
	return dominant
}
