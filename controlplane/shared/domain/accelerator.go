package domain

import "strings"

// AcceleratorType identifies the hardware tier for a job. It is deliberately an open, string-based
// identifier, not a closed enum: the const block below only seeds a fallback rate table for
// tests/local runs without config — the real catalog is entirely operator-defined via
// openresearch.yaml's accelerator_types (see config.AcceleratorTypeConfig), and any vendor's model name works
// (NVIDIA H100, AMD MI300X, ...). Nothing in this codebase switches on these specific consts.
type AcceleratorType string

const (
	AcceleratorT4   AcceleratorType = "T4"
	AcceleratorL40  AcceleratorType = "L40"
	AcceleratorA100 AcceleratorType = "A100"
	AcceleratorH100 AcceleratorType = "H100"
	AcceleratorH200 AcceleratorType = "H200"
)

// acceleratorRateRegistry is populated by SetAcceleratorRates() at startup from openresearch.yaml.
// Falls back to hardcoded defaults when not set (tests, local runs without config).
// Rates are in accelerator-hours (AccH), H100-equivalent: 1 AccH = 1 H100-hour, so H100 is
// exactly 1.0 and every other tier is its H100-relative fraction.
var acceleratorRateRegistry = map[AcceleratorType]float64{
	AcceleratorT4:   0.125,
	AcceleratorL40:  0.25,
	AcceleratorA100: 0.375,
	AcceleratorH100: 1.0,
	AcceleratorH200: 1.25,
}

// SetAcceleratorRates registers AccH rates loaded from config. Call once at startup.
func SetAcceleratorRates(rates map[string]float64) {
	for name, rate := range rates {
		acceleratorRateRegistry[AcceleratorType(name)] = rate
	}
}

// FlavorName returns the flavor name for this accelerator type ("flavor-t4", etc.).
func (g AcceleratorType) FlavorName() string {
	return "flavor-" + strings.ToLower(string(g))
}

// Cost returns the accelerator-hour (AccH, H100-equivalent) cost per accelerator-hour for this
// accelerator type, falling back to 0.125 (the T4 tier's rate, the cheapest baseline) for an
// unregistered type. Safe for pre-flight estimates, where a wrong guess
// just gets corrected once the job is actually admitted onto a real, catalog-known type — but NOT
// safe for final billing, where a typo'd or deprecated type must not silently settle at the wrong
// rate. Billing code should use LookupCost instead and treat "not found" as an error.
func (g AcceleratorType) Cost() float64 {
	if r, ok := acceleratorRateRegistry[g]; ok {
		return r
	}
	return 0.125
}

// LookupCost returns this accelerator type's registered accelerator-hour (AccH) rate and whether it was found in the
// catalog (populated by SetAcceleratorRates from openresearch.yaml's accelerator_types at startup). Unlike Cost,
// this never silently substitutes a fallback rate — used by final-settlement paths (see
// metricsdb.ObservedAcceleratorCost) where an unrecognized type must fail loudly rather than bill at the
// wrong tier's price.
func (g AcceleratorType) LookupCost() (float64, bool) {
	r, ok := acceleratorRateRegistry[g]
	return r, ok
}

// cpuCoreHourRate is a flat per-unit rate (1.0 by default — unlike accelerator there is no
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

// ResourceType identifies one quota-tracked resource dimension. Accelerator-hours is the original,
// always-populated dimension (billed at AcceleratorType's own tiered rate); CPU-hours is a flat
// per-unit pool (see cpuCoreHourRate) checked/debited/refunded against its own guaranteed/burst
// pool on AgentQuota, same as accelerator-hours.
type ResourceType string

const (
	ResourceAcceleratorHours     ResourceType = "accelerator_hours"
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
