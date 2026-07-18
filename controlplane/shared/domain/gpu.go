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

// cpuCoreHourRate/ramGBHourRate/storageGBHourRate are flat per-unit rates (1.0 by default —
// unlike GPU there is no hardware-tier variation to normalize away), operator-overridable via
// openresearch.yaml so tiering (e.g. "fast NVMe" vs "standard" storage) can be introduced later
// without another migration. There is deliberately no cross-resource exchange rate between
// these and gpuRateRegistry — CPU-hours, RAM-GB-hours, and storage-GB-hours are independent
// pools, not converted into T4h credits.
var (
	cpuCoreHourRate   = 1.0
	ramGBHourRate     = 1.0
	storageGBHourRate = 1.0
)

// SetCPUCoreHourRate/SetRAMGBHourRate/SetStorageGBHourRate register the flat per-unit rate for
// each non-GPU resource dimension, loaded from config at startup. Zero/negative is ignored
// (keeps the 1.0 default) so an absent config key doesn't zero out the rate.
func SetCPUCoreHourRate(rate float64) {
	if rate > 0 {
		cpuCoreHourRate = rate
	}
}

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

// CPUCoreHourRate/RAMGBHourRate/StorageGBHourRate return the current per-unit rate for each
// resource dimension (see the SetX functions above).
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
// always-populated dimension (billed at GPUType's own tiered rate); the other three are flat
// per-unit pools (see cpuCoreHourRate/ramGBHourRate/storageGBHourRate) with no exchange rate
// between dimensions — each is checked/debited/refunded independently against its own
// guaranteed/burst pool on AgentQuota.
type ResourceType string

const (
	ResourceGPUHours       ResourceType = "gpu_hours"
	ResourceCPUCoreHours   ResourceType = "cpu_core_hours"
	ResourceRAMGBHours     ResourceType = "ram_gb_hours"
	ResourceStorageGBHours ResourceType = "storage_gb_hours"
)
