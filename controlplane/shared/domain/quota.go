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
