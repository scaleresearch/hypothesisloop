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
//
// Value compares with EqualFold: different discovery mechanisms case the same hardware
// differently (e.g. NVIDIA's GFD emits "NVIDIA-GeForce-RTX-4090", our own NVML probe emits
// "NVIDIA-GEFORCE-RTX-4090"). The key still compares exact — it's a vendor-qualified identifier.
func (g AcceleratorType) MatchesLabels(labels map[string]string) bool {
	key, value, ok := g.split()
	if !ok {
		return false
	}
	return strings.EqualFold(labels[key], value)
}

// MatchesAttributes reports whether a DRA device publishing these attributes carries this
// accelerator type. attributes are keyed by their bare attribute name as published in the
// ResourceSlice ("chipArch"); the type's key is vendor-qualified ("tenstorrent.com/chipArch"),
// and driver is the DRA driver name that qualifies them. See MatchesLabels re: casing.
func (g AcceleratorType) MatchesAttributes(driver string, attributes map[string]string) bool {
	key, value, ok := g.split()
	if !ok || g.Domain() != driver {
		return false
	}
	bare := key[strings.Index(key, "/")+1:]
	return strings.EqualFold(attributes[bare], value)
}

// foldKey normalizes an accelerator type string for case-insensitive map lookups.
func foldKey(s string) string { return strings.ToLower(s) }

// acceleratorRateRegistry is populated by SetAcceleratorRates() at startup from hypothesisloop.yaml.
// Rates are in accelerator-hours (AccH), H100-equivalent: 1 AccH = 1 H100-hour, so H100 is
// exactly 1.0 and every other tier is its H100-relative fraction. Keyed by foldKey (see
// MatchesLabels re: casing).
var acceleratorRateRegistry = map[string]float64{}

// SetAcceleratorRates registers AccH rates loaded from config. Call once at startup.
func SetAcceleratorRates(rates map[string]float64) {
	next := make(map[string]float64, len(rates))
	for name, rate := range rates {
		if name == "" || rate <= 0 {
			panic("accelerator catalog contains an empty name or non-positive rate")
		}
		next[foldKey(name)] = rate
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
	if r, ok := acceleratorRateRegistry[foldKey(string(g))]; ok {
		return r
	}
	panic("unknown accelerator type: " + string(g))
}

// LookupCost returns this type's registered AccH rate and whether it was found. Unlike Cost, it
// never silently substitutes another rate — used by settlement paths (see
// metricsdb.ObservedAcceleratorCost) where an unrecognized type must fail loudly.
func (g AcceleratorType) LookupCost() (float64, bool) {
	r, ok := acceleratorRateRegistry[foldKey(string(g))]
	return r, ok
}

// CapacityTier specifies whether a job uses guaranteed or burst capacity.
type CapacityTier string

const (
	CapacityGuaranteed CapacityTier = "guaranteed"
	CapacityBurst      CapacityTier = "burst"
)

// ResourceType identifies one quota-tracked resource dimension. Accelerator-hours is the only
// one: RAM and ephemeral-storage are hard physical-fit-checked at admission
// (domain.Experiment.Footprint()/domain.Fits), never hours-budgeted or debited.
type ResourceType string

const (
	ResourceAcceleratorHours ResourceType = "accelerator_hours"
)
