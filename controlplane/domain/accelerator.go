package domain

import "github.com/scaleresearch/hypothesisloop/contract/clusterapi"

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

// acceleratorRateRegistry is populated by SetAcceleratorRates() at startup from hypothesisloop.yaml.
// Rates are in accelerator-hours (AccH), H100-equivalent: 1 AccH = 1 H100-hour, so H100 is
// exactly 1.0 and every other tier is its H100-relative fraction.
var acceleratorRateRegistry = map[string]float64{}

// SetAcceleratorRates registers AccH rates loaded from config. Call once at startup.
func SetAcceleratorRates(rates map[string]float64) {
	next := make(map[string]float64, len(rates))
	for name, rate := range rates {
		if name == "" || rate <= 0 {
			panic("accelerator catalog contains an empty name or non-positive rate")
		}
		next[name] = rate
	}
	if len(next) == 0 {
		panic("accelerator catalog is empty")
	}
	acceleratorRateRegistry = next
}

// acceleratorLabelRegistry is each type's label as a person reads it, set by SetAcceleratorLabels
// at startup from the catalog's labels.
var acceleratorLabelRegistry = map[string]string{}

// SetAcceleratorLabels registers the catalog's labels. Call once at startup.
func SetAcceleratorLabels(labels map[string]string) {
	acceleratorLabelRegistry = labels
}

// AcceleratorLabel is t as a person reads it ("H100 SXM 80GB"), "" where the catalog gives none.
func AcceleratorLabel(t AcceleratorType) string {
	return acceleratorLabelRegistry[string(t)]
}

// PodResources is what every node of a job on a type needs beside the device, such as the
// hugepages a Tenstorrent chip's driver maps, by type as the settings' catalog names it.
type PodResources map[string]map[string]string

// NewPodResources is the catalog's pod resources by type.
func NewPodResources(byType map[string]map[string]string) PodResources {
	return PodResources(byType)
}

// Of is what every node of a job on t needs beside the device.
func (p PodResources) Of(t AcceleratorType) map[string]string {
	return p[string(t)]
}

// TypeKinds is every kind of accelerator type.
var TypeKinds = []TypeKind{TypeKindAccelerator, TypeKindCPU}

// LookupAcceleratorCost returns this type's registered AccH rate and whether it was found.
func LookupAcceleratorCost(g AcceleratorType) (float64, bool) {
	r, ok := acceleratorRateRegistry[string(g)]
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
