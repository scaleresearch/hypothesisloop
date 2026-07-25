package domain

import (
	"fmt"
	"strings"
)

// AcceleratorType identifies a piece of hardware by a fact its own driver publishes, written
// as a fully-qualified "key=value" pair — for example:
//
//	nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3   (NVIDIA GPU Feature Discovery node label)
//	amd.com/gpu.product-name=Instinct-MI300X       (AMD node labeller node label)
//	tenstorrent.com/chipArch=blackhole             (Tenstorrent DRA device attribute)
//
// The string is the driver's own output, verbatim. There is no platform-side name for hardware
// and no table translating one to the other: the same string an agent writes in a job spec is
// the string capacity reports, the string quota bills against, and the string recorded on the
// job's pods. That is the whole point — a mapping is a place where two names can disagree, so
// there isn't one.
//
// Being fully qualified is what makes this work without any per-vendor knowledge in the code.
// The key carries its vendor domain, so matching is a plain lookup against live inventory rather
// than a rule about which field a given vendor calls its model name. Runtime code contains no
// vendor/model enum, and the operator's catalog (hypothesisloop.yaml) only attaches a price to
// these strings — it never defines or renames them.
type AcceleratorType string

// Key returns the label/attribute name (the part before "="), and Value the driver-published
// value after it. Both return "" for a malformed type; callers that need a hard failure use
// Validate.
func (g AcceleratorType) Key() string {
	key, _, ok := g.split()
	if !ok {
		return ""
	}
	return key
}

func (g AcceleratorType) Value() string {
	_, value, ok := g.split()
	if !ok {
		return ""
	}
	return value
}

// Domain returns the vendor domain the key belongs to — "nvidia.com" for
// "nvidia.com/gpu.product", "tenstorrent.com" for "tenstorrent.com/chipArch". This is how a type
// is tied back to the driver that publishes it (a DRA driver name, or the domain of the extended
// resource a device plugin advertises) without the code knowing anything vendor-specific.
func (g AcceleratorType) Domain() string {
	key := g.Key()
	slash := strings.Index(key, "/")
	if slash <= 0 {
		return ""
	}
	return key[:slash]
}

// Validate rejects anything that is not a fully-qualified driver-published key=value pair.
// Enforced at admission so a malformed type fails at submission with a clear message, rather
// than silently matching no hardware and queueing forever.
func (g AcceleratorType) Validate() error {
	key, value, ok := g.split()
	if !ok {
		return fmt.Errorf("accelerator type %q must be a driver-published \"key=value\" pair, e.g. nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", string(g))
	}
	if strings.Index(key, "/") <= 0 {
		return fmt.Errorf("accelerator type %q must have a vendor-qualified key (e.g. nvidia.com/gpu.product), so it can be traced to the driver that publishes it", string(g))
	}
	if value == "" {
		return fmt.Errorf("accelerator type %q has an empty value", string(g))
	}
	return nil
}

func (g AcceleratorType) split() (key, value string, ok bool) {
	s := string(g)
	eq := strings.Index(s, "=")
	if eq <= 0 || eq == len(s)-1 {
		return "", "", false
	}
	return s[:eq], s[eq+1:], true
}

// MatchesLabels reports whether a node publishing these labels carries this accelerator type.
// Used for device-plugin hardware, whose identity the vendor publishes as a node label.
func (g AcceleratorType) MatchesLabels(labels map[string]string) bool {
	key, value, ok := g.split()
	if !ok {
		return false
	}
	return labels[key] == value
}

// MatchesAttributes reports whether a DRA device publishing these attributes carries this
// accelerator type. attributes are keyed by their bare attribute name as published in the
// ResourceSlice ("chipArch"); the type's key is vendor-qualified ("tenstorrent.com/chipArch"),
// and driver is the DRA driver name that qualifies them.
func (g AcceleratorType) MatchesAttributes(driver string, attributes map[string]string) bool {
	key, value, ok := g.split()
	if !ok || g.Domain() != driver {
		return false
	}
	bare := key[strings.Index(key, "/")+1:]
	return attributes[bare] == value
}

// acceleratorRateRegistry is populated by SetAcceleratorRates() at startup from hypothesisloop.yaml.
// Rates are in accelerator-hours (AccH), H100-equivalent: 1 AccH = 1 H100-hour, so H100 is
// exactly 1.0 and every other tier is its H100-relative fraction.
var acceleratorRateRegistry = map[AcceleratorType]float64{}

// SetAcceleratorRates registers AccH rates loaded from config. Call once at startup.
func SetAcceleratorRates(rates map[string]float64) {
	next := make(map[AcceleratorType]float64, len(rates))
	for name, rate := range rates {
		if name == "" || rate <= 0 {
			panic("accelerator catalog contains an empty name or non-positive rate")
		}
		next[AcceleratorType(name)] = rate
	}
	if len(next) == 0 {
		panic("accelerator catalog is empty")
	}
	acceleratorRateRegistry = next
}

// Cost returns the registered accelerator-hour rate and panics for an unknown type. Admission
// validates catalog membership before cost calculation; internal callers must never substitute a
// different hardware rate.
func (g AcceleratorType) Cost() float64 {
	if r, ok := acceleratorRateRegistry[g]; ok {
		return r
	}
	panic("unknown accelerator type: " + string(g))
}

// LookupCost returns this type's registered AccH rate and whether it was found. Unlike Cost, it
// never silently substitutes another rate — used by settlement paths (see
// metricsdb.ObservedAcceleratorCost) where an unrecognized type must fail loudly.
func (g AcceleratorType) LookupCost() (float64, bool) {
	r, ok := acceleratorRateRegistry[g]
	return r, ok
}

// cpuCoreHourRate is a flat per-unit rate (no hardware-tier variation), operator-overridable via
// hypothesisloop.yaml.
//
// ramGBHourRate/storageGBHourRate: Deprecated, kept only so SetRAMGBHourRate/SetStorageGBHourRate
// remain valid no-ops for old callers — nothing reads these to compute a bill anymore (RAM/storage
// moved to Class B, hard-fit-checked only — see ResourceRAMGBHours' doc comment).
var (
	cpuCoreHourRate   = 1.0
	ramGBHourRate     = 1.0
	storageGBHourRate = 1.0
)

// SetCPUCoreHourRate registers the flat per-unit CPU-hour rate, loaded from config at startup.
func SetCPUCoreHourRate(rate float64) {
	if rate <= 0 {
		panic("CPU core-hour rate must be positive")
	}
	cpuCoreHourRate = rate
}

// SetRAMGBHourRate/SetStorageGBHourRate: Deprecated — see ramGBHourRate's doc comment. Still
// registers the value so old config keys don't error, but nothing reads it for billing anymore.
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
// always-populated dimension (billed at AcceleratorType's tiered rate); CPU-hours is a flat
// per-unit pool (see cpuCoreHourRate) checked/debited/refunded the same way on AgentQuota.
type ResourceType string

const (
	ResourceAcceleratorHours ResourceType = "accelerator_hours"
	ResourceCPUCoreHours     ResourceType = "cpu_core_hours"

	// ResourceRAMGBHours/ResourceStorageGBHours: Deprecated. RAM and ephemeral-storage are hard
	// physical-fit-checked at admission (domain.Experiment.Footprint()/domain.Fits) but never
	// hours-budgeted, debited, or preemption-rescaled. Kept as API identifiers for schema
	// compatibility only — not live billing dimensions.
	ResourceRAMGBHours     ResourceType = "ram_gb_hours"
	ResourceStorageGBHours ResourceType = "storage_gb_hours"
)
