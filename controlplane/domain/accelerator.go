package domain

import (
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// The accelerator vocabulary is the cluster contract's own (clusterapi), so admission and a
// runtime read a type alike.
type (
	AcceleratorType = clusterapi.AcceleratorType
	TypeKind        = clusterapi.TypeKind
	CapacityTier    = clusterapi.CapacityTier
)

const (
	CPUType             = clusterapi.CPUType
	TypeKindAccelerator = clusterapi.TypeKindAccelerator
	TypeKindCPU         = clusterapi.TypeKindCPU
	CapacityGuaranteed  = clusterapi.CapacityGuaranteed
	CapacityBurst       = clusterapi.CapacityBurst
)

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

// TypeKinds is every kind of accelerator type.
var TypeKinds = []TypeKind{TypeKindAccelerator, TypeKindCPU}

// LookupAcceleratorCost returns this type's registered AccH rate and whether it was found.
func LookupAcceleratorCost(g AcceleratorType) (float64, bool) {
	r, ok := acceleratorRateRegistry[foldKey(string(g))]
	return r, ok
}

// CapacityTiers is every capacity tier.
var CapacityTiers = []CapacityTier{CapacityGuaranteed, CapacityBurst}

// ResourceType identifies one quota-tracked resource dimension. Accelerator-hours is the only
// one: RAM and ephemeral-storage are hard physical-fit-checked at admission
// (domain.Experiment.Footprint()/domain.Fits), never hours-budgeted or debited.
type ResourceType string

const (
	ResourceAcceleratorHours ResourceType = "accelerator_hours"
)
