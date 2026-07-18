package domain

import "strings"

// GPUType identifies the hardware tier for a job. It is deliberately an open, string-based
// identifier, not a closed enum: the const block below only seeds a fallback rate table for
// tests/local runs without config — the real catalog is entirely operator-defined via
// openresearch.yaml's gpu_types (see config.GPUTypeConfig), and any vendor's model name works
// (NVIDIA H100, AMD MI300X, ...). Nothing in this codebase switches on these specific consts.
type GPUType string

const (
	GPUT4   GPUType = "T4"
	GPUL40  GPUType = "L40"
	GPUA100 GPUType = "A100"
	GPUH100 GPUType = "H100"
	GPUH200 GPUType = "H200"
)

// gpuRateRegistry is populated by SetGPURates() at startup from openresearch.yaml.
// Falls back to hardcoded defaults when not set (tests, local runs without config).
var gpuRateRegistry = map[GPUType]float64{
	GPUT4:   1.0,
	GPUL40:  2.0,
	GPUA100: 3.0,
	GPUH100: 8.0,
	GPUH200: 10.0,
}

// SetGPURates registers T4h rates loaded from config. Call once at startup.
func SetGPURates(rates map[string]float64) {
	for name, rate := range rates {
		gpuRateRegistry[GPUType(name)] = rate
	}
}

// FlavorName returns the flavor name for this GPU type ("flavor-t4", etc.).
func (g GPUType) FlavorName() string {
	return "flavor-" + strings.ToLower(string(g))
}

// Cost returns the T4-GPU-hour cost per GPU-hour for this GPU type, falling back to 1.0 (the T4
// baseline rate) for an unregistered type. Safe for pre-flight estimates, where a wrong guess
// just gets corrected once the job is actually admitted onto a real, catalog-known type — but NOT
// safe for final billing, where a typo'd or deprecated type must not silently settle at the wrong
// rate. Billing code should use LookupCost instead and treat "not found" as an error.
func (g GPUType) Cost() float64 {
	if r, ok := gpuRateRegistry[g]; ok {
		return r
	}
	return 1.0
}

// LookupCost returns this GPU type's registered T4-GPU-hour rate and whether it was found in the
// catalog (populated by SetGPURates from openresearch.yaml's gpu_types at startup). Unlike Cost,
// this never silently substitutes a fallback rate — used by final-settlement paths (see
// metricsdb.ObservedGPUCost) where an unrecognized type must fail loudly rather than bill at the
// wrong tier's price.
func (g GPUType) LookupCost() (float64, bool) {
	r, ok := gpuRateRegistry[g]
	return r, ok
}

// cpuCoreHourRate is a flat per-unit rate (1.0 by default — unlike GPU there is no
// hardware-tier variation to normalize away), operator-overridable via openresearch.yaml.
//
// ramGBHourRate/storageGBHourRate: Deprecated, kept only so SetRAMGBHourRate/SetStorageGBHourRate
// and RAMGBHourRate/StorageGBHourRate remain valid no-op-ish calls for any config/caller that
// still references them — nothing in this codebase reads these two rates to compute a bill
// anymore (RAM/storage moved to Class B, hard-fit-checked only — see ResourceRAMGBHours' doc
// comment for the full migration note).
var (
	cpuCoreHourRate   = 1.0
	ramGBHourRate     = 1.0
	storageGBHourRate = 1.0
)

// SetCPUCoreHourRate registers the flat per-unit CPU-hour rate, loaded from config at startup.
// Zero/negative is ignored (keeps the 1.0 default) so an absent config key doesn't zero out the
// rate.
func SetCPUCoreHourRate(rate float64) {
	if rate > 0 {
		cpuCoreHourRate = rate
	}
}

// SetRAMGBHourRate/SetStorageGBHourRate: Deprecated — see ramGBHourRate's doc comment. Still
// registers the value (so an operator's existing openresearch.yaml with these keys doesn't
// error), but nothing reads it for billing anymore.
func SetRAMGBHourRate(rate float64) {
	if rate > 0 {
		ramGBHourRate = rate
	}
}

func SetStorageGBHourRate(rate float64) {
	if rate > 0 {
		storageGBHourRate = rate
	}
}

// CPUCoreHourRate returns the current per-unit CPU-hour rate.
func CPUCoreHourRate() float64   { return cpuCoreHourRate }
func RAMGBHourRate() float64     { return ramGBHourRate }
func StorageGBHourRate() float64 { return storageGBHourRate }

// CapacityTier specifies whether a job uses guaranteed or burst capacity.
type CapacityTier string

const (
	CapacityGuaranteed CapacityTier = "guaranteed"
	CapacityBurst      CapacityTier = "burst"
)

// ResourceType identifies one quota-tracked resource dimension. GPU-hours is the original,
// always-populated dimension (billed at GPUType's own tiered rate); CPU-hours is a flat
// per-unit pool (see cpuCoreHourRate) checked/debited/refunded against its own guaranteed/burst
// pool on AgentQuota, same as GPU-hours.
type ResourceType string

const (
	ResourceGPUHours     ResourceType = "gpu_hours"
	ResourceCPUCoreHours ResourceType = "cpu_core_hours"

	// ResourceRAMGBHours/ResourceStorageGBHours: Deprecated. RAM and ephemeral-storage moved to
	// Class B under SCHEDULING_GENERALIZATION_PLAN.md — hard physical-fit-checked at admission
	// (domain.Experiment.Footprint()/domain.Fits) but never hours-budgeted, debited, or
	// preemption-rescaled. Nothing in this codebase computes a non-zero amount for either
	// resource type anymore (see scheduler.Service.Submit's estimate step) — every debit/refund
	// call site skips them via their existing "amount <= 0" guard, so they are dead weight, not
	// live billing dimensions. Kept defined (not deleted) purely so historical rows already
	// written against them (AgentQuota's ram_gb_hours/storage_gb_hours columns, historical
	// Experiment.EstimatedRAMGBHours/ActualRAMGBHours values, metricsdb reservation series
	// keyed by these ResourceTypes) remain readable/interpretable rather than becoming orphaned
	// magic strings. A platform experiment created before this migration with a non-zero
	// BudgetRAMGBHours/BudgetStorageGBHours (see PlatformExperiment) keeps that number in the
	// DB, but nothing reads it anymore — its guaranteed/burst pools simply stop moving forward.
	ResourceRAMGBHours     ResourceType = "ram_gb_hours"
	ResourceStorageGBHours ResourceType = "storage_gb_hours"
)
