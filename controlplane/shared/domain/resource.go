package domain

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// This file introduces the canonical multi-dimensional resource primitives used for physical
// fit checks (see SCHEDULING_GENERALIZATION_PLAN.md, Class A step 3). They are intentionally
// separate from the hours-based billing dimensions in gpu.go (ResourceType/CapacityTier): a
// Footprint answers "does this fit right now," in canonical integer units, on one cluster; it
// carries no notion of guaranteed/burst or a time dimension at all.

// ResourceKindCPU/Memory/Storage are the fixed, always-meaningful physical dimensions every job
// participates in (CPU may be zero for an accelerator-only job, but the key always exists).
// ResourceKindAccelerator is used once per distinct accelerator type a job requests (Flavor is
// the accelerator type ID, e.g. "T4"/"H100"). ResourceKindExtended covers any uncatalogued k8s
// extended resource (Flavor is the resource name, e.g. "google.com/tpu") — physical-fit-only,
// same as RAM/storage, never billed (see JobSpec.ExtraResources's doc comment).
type ResourceKind string

const (
	ResourceKindCPU         ResourceKind = "cpu"
	ResourceKindMemory      ResourceKind = "memory"
	ResourceKindStorage     ResourceKind = "storage"
	ResourceKindAccelerator ResourceKind = "accelerator"
	ResourceKindExtended    ResourceKind = "extended"
)

// ResourceKey identifies one dimension of a Footprint. Flavor disambiguates within a Kind: for
// ResourceKindAccelerator it's the accelerator type ID; for ResourceKindExtended it's the raw
// k8s extended-resource name; for CPU/Memory/Storage it's always empty (one dimension each).
type ResourceKey struct {
	Kind   ResourceKind
	Flavor string
}

// Footprint is a job's (or a cluster's available capacity's) resource vector in canonical
// integer units: millicores for CPU, bytes for memory/storage, whole units for accelerators and
// extended resources. Canonical units avoid the fractional-CPU precision loss a plain float or
// whole-core rounding would introduce (see the plan's "fractional CPU precision" acceptance
// test) and let every dimension be compared/subtracted with plain integer arithmetic.
type Footprint map[ResourceKey]int64

// NewFootprint returns an empty, ready-to-populate Footprint.
func NewFootprint() Footprint {
	return make(Footprint)
}

// Add adds amount to key's entry (creating it if absent). Zero/negative amounts are still
// recorded — a caller asking for 0 of something explicitly is different from never having asked.
func (f Footprint) Add(key ResourceKey, amount int64) {
	f[key] += amount
}

// Scale returns a new Footprint with every dimension multiplied by n — used to expand a
// per-node footprint into a whole job's footprint (per-node x NumNodes), per the plan's Class A
// step 3 correction that a distributed job's true footprint is per-node, not per-pod.
func (f Footprint) Scale(n int64) Footprint {
	out := make(Footprint, len(f))
	for k, v := range f {
		out[k] = v * n
	}
	return out
}

// AddFootprint adds every dimension of other into f in place — used to accumulate a running
// "freed so far" or "occupied so far" total across several experiments' individual footprints.
func (f Footprint) AddFootprint(other Footprint) {
	for k, v := range other {
		f[k] += v
	}
}

// Sub returns a new Footprint of f minus other, dimension by dimension. Dimensions present only
// in other are included as negative entries in the result (capacity going negative is exactly
// what a caller needs to detect "doesn't fit").
func (f Footprint) Sub(other Footprint) Footprint {
	out := make(Footprint, len(f)+len(other))
	for k, v := range f {
		out[k] = v
	}
	for k, v := range other {
		out[k] -= v
	}
	return out
}

// Fits reports whether every dimension footprint requests is available in capacity — a job
// fits a cluster only if ALL of its requested dimensions fit jointly on that one cluster, not
// merely each dimension checked independently against a possibly-different cluster. A dimension
// footprint does not request at all is never a blocker (capacity's exact size in a dimension the
// job doesn't need is irrelevant). A dimension footprint requests but capacity does not report at
// all is treated as zero available (missing capacity data for a dimension a job actually needs
// must fail the fit check, not be silently ignored — see the plan's "fail closed on stale/missing
// capacity" cross-cutting fix).
func Fits(capacity, footprint Footprint) bool {
	for k, need := range footprint {
		if need <= 0 {
			continue
		}
		have := capacity[k] // zero value if absent — fails closed
		if have < need {
			return false
		}
	}
	return true
}

// CapacityFootprint builds a per-cluster capacity Footprint from live CPU-core and
// per-accelerator-flavor availability — the shape every workload.Backend.GetFlavorCapacity
// implementation reports today (RAM/storage are not part of this yet — see the plan's Class B
// step 2, not landed). cpuCores is floored to whole millicores (never rounded up in the
// admission loop's favor, same directionality the old whole-core truncation used, but with
// millicore granularity so a capacity check against a fractional-CPU job doesn't lose
// precision). gpuByFlavor keys must already be GPUType.FlavorName()-shaped, matching
// JobSpec.Footprint()/Experiment.Footprint()'s accelerator key convention, or Fits() will never
// match them.
func CapacityFootprint(cpuCores float64, gpuByFlavor map[string]int64) Footprint {
	fp := NewFootprint()
	fp.Add(ResourceKey{Kind: ResourceKindCPU}, int64(cpuCores*1000))
	for flavor, n := range gpuByFlavor {
		fp.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: flavor}, n)
	}
	return fp
}

// Footprint computes j's total resource footprint in canonical units: millicores for CPU,
// bytes for memory/storage, whole units for the accelerator type (if any) and every
// ExtraResources entry. Per the plan's Class A step 3 correction, this is the PER-NODE
// footprint scaled by Nodes() — a distributed (NumNodes > 1) job's true demand is per-node x
// NumNodes, not per-pod, matching how BuildJob compiles NumNodes into an Indexed Job with one
// full-sized pod per rank.
//
// Returns an error if any quantity string fails to parse — callers must treat that as a
// rejection, not a zero-footprint dimension (a malformed "job.cpu" must never be silently
// treated as "requests no CPU").
func (j JobSpec) Footprint() (Footprint, error) {
	perNode := NewFootprint()

	if j.CPU != "" {
		q, err := resource.ParseQuantity(j.CPU)
		if err != nil {
			return nil, fmt.Errorf("job.cpu: %w", err)
		}
		perNode.Add(ResourceKey{Kind: ResourceKindCPU}, q.MilliValue())
	}
	if j.Memory != "" {
		q, err := resource.ParseQuantity(j.Memory)
		if err != nil {
			return nil, fmt.Errorf("job.memory: %w", err)
		}
		perNode.Add(ResourceKey{Kind: ResourceKindMemory}, q.Value())
	}
	if j.Storage != "" {
		q, err := resource.ParseQuantity(j.Storage)
		if err != nil {
			return nil, fmt.Errorf("job.storage: %w", err)
		}
		perNode.Add(ResourceKey{Kind: ResourceKindStorage}, q.Value())
	}
	if j.GPUCount > 0 {
		if j.GPUType == "" {
			return nil, fmt.Errorf("job.gpu_type is required when job.gpu_count > 0")
		}
		// Flavor uses the same GPUType.FlavorName() convention capacity reporting keys on
		// (e.g. "flavor-t4", not "T4") so Fits() actually matches a job's accelerator dimension
		// against the right capacity entry.
		perNode.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: j.GPUType.FlavorName()}, int64(j.GPUCount))
	}
	for name, qty := range j.ExtraResources {
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return nil, fmt.Errorf("job.extra_resources[%q]: %w", name, err)
		}
		perNode.Add(ResourceKey{Kind: ResourceKindExtended, Flavor: name}, q.Value())
	}

	return perNode.Scale(int64(j.Nodes())), nil
}
