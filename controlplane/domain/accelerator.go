package domain

import (
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
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

// Key, Value, Domain, Validate, MatchesLabels, MatchesAttributes and Is are the cluster
// contract's (clusterapi.AcceleratorType), so admission and a runtime read a type alike.
func (g AcceleratorType) Key() string     { return clusterapi.AcceleratorType(g).Key() }
func (g AcceleratorType) Value() string   { return clusterapi.AcceleratorType(g).Value() }
func (g AcceleratorType) Domain() string  { return clusterapi.AcceleratorType(g).Domain() }
func (g AcceleratorType) Validate() error { return clusterapi.AcceleratorType(g).Validate() }

func (g AcceleratorType) MatchesLabels(labels map[string]string) bool {
	return clusterapi.AcceleratorType(g).MatchesLabels(labels)
}

func (g AcceleratorType) MatchesAttributes(driver string, attributes map[string]string) bool {
	return clusterapi.AcceleratorType(g).MatchesAttributes(driver, attributes)
}

func (g AcceleratorType) Is(other AcceleratorType) bool {
	return clusterapi.AcceleratorType(g).Is(clusterapi.AcceleratorType(other))
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

// PodResources is what every node of a job on a type needs beside the device, such as the
// hugepages a Tenstorrent chip's driver maps, by type as the settings' catalog names it.
type PodResources map[string]map[string]string

// NewPodResources keys the catalog's pod resources so that any spelling of a type finds its own.
func NewPodResources(byType map[string]map[string]string) PodResources {
	out := make(PodResources, len(byType))
	for name, resources := range byType {
		out[foldKey(name)] = resources
	}
	return out
}

// Of is what every node of a job on t needs beside the device.
func (p PodResources) Of(t AcceleratorType) map[string]string {
	return p[foldKey(string(t))]
}

// Over is a job's accelerator_pod_resources as it runs on t: what the job stated, and over it
// what the type always needs, so a job states none of that itself.
func (p PodResources) Over(t AcceleratorType, stated map[string]string) map[string]string {
	always := p.Of(t)
	if len(always) == 0 {
		return stated
	}
	out := make(map[string]string, len(stated)+len(always))
	for name, quantity := range stated {
		out[name] = quantity
	}
	for name, quantity := range always {
		out[name] = quantity
	}
	return out
}

// CPUType is the one type whose unit is a CPU core. Every runtime offers its nodes' whole cores
// as units of it, and a job that asks for cores and no accelerator holds that many units (see
// scheduler.ValidateExperiment). The catalog prices it like any other type.
const CPUType = AcceleratorType(clusterapi.CPUType)

// TypeKind says what one unit of a priced type is: a device, or one CPU core. It decides two
// things and nothing else: for the cpu type the count of a job is its cores, and sums and labels
// count cores apart from devices.
type TypeKind string

const (
	TypeKindAccelerator TypeKind = "accelerator"
	TypeKindCPU         TypeKind = "cpu"
)

// TypeKinds is every kind of accelerator type.
var TypeKinds = []TypeKind{TypeKindAccelerator, TypeKindCPU}

// Kind returns what one unit of the type is: a core for CPUType, a device for every other.
func (g AcceleratorType) Kind() TypeKind {
	if g.Is(CPUType) {
		return TypeKindCPU
	}
	return TypeKindAccelerator
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

// CapacityTiers is every capacity tier.
var CapacityTiers = []CapacityTier{CapacityGuaranteed, CapacityBurst}

// ResourceType identifies one quota-tracked resource dimension. Accelerator-hours is the only
// one: RAM and ephemeral-storage are hard physical-fit-checked at admission
// (domain.Experiment.Footprint()/domain.Fits), never hours-budgeted or debited.
type ResourceType string

const (
	ResourceAcceleratorHours ResourceType = "accelerator_hours"
)
