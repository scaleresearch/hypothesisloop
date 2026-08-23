package domain

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// This file defines canonical multi-dimensional resource primitives for physical fit checks.
// They're separate from the hours-based billing dimensions in accelerator.go
// (ResourceType/CapacityTier): a Footprint answers "does this fit right now" in canonical
// integer units on one cluster, with no guaranteed/burst or time dimension.

// ResourceKindCPU/Memory/Storage are the fixed physical dimensions every job participates in
// (CPU may be zero for an accelerator-only job, but the key always exists).
// ResourceKindAccelerator is used once per distinct accelerator type requested (Flavor is the
// accelerator type, e.g. "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"). ResourceKindExtended covers uncatalogued k8s extended
// resources (Flavor is the resource name, e.g. "google.com/tpu") — physical-fit-only, never
// billed (see JobSpec.ExtraResources's doc comment).
type ResourceKind string

const (
	ResourceKindCPU         ResourceKind = "cpu"
	ResourceKindMemory      ResourceKind = "memory"
	ResourceKindStorage     ResourceKind = "storage"
	ResourceKindAccelerator ResourceKind = "accelerator"
	ResourceKindExtended    ResourceKind = "extended"
)

// ResourceKey identifies one dimension of a Footprint. Flavor disambiguates within a Kind: the
// accelerator type ID for ResourceKindAccelerator, the raw k8s extended-resource name for
// ResourceKindExtended, and always empty for CPU/Memory/Storage (one dimension each).
type ResourceKey struct {
	Kind   ResourceKind
	Flavor string
}

// Footprint is a job's (or a cluster's capacity) resource vector in canonical integer units:
// millicores for CPU, bytes for memory/storage, whole units for accelerators and extended
// resources. Integer units avoid fractional-CPU precision loss and let dimensions be
// compared/subtracted with plain integer arithmetic.
type Footprint map[ResourceKey]int64

// NewFootprint returns an empty, ready-to-populate Footprint.
func NewFootprint() Footprint {
	return make(Footprint)
}

// Add adds amount to key's entry (creating it if absent). Zero/negative amounts are still
// recorded — explicitly asking for 0 differs from never having asked.
func (f Footprint) Add(key ResourceKey, amount int64) {
	f[key] += amount
}

// Scale returns a new Footprint with every dimension multiplied by n — expands a per-node
// footprint into a whole job's footprint (per-node x NumNodes), since a distributed job's true
// footprint is per-node, not per-pod.
func (f Footprint) Scale(n int64) Footprint {
	out := make(Footprint, len(f))
	for k, v := range f {
		out[k] = v * n
	}
	return out
}

// AddFootprint adds every dimension of other into f in place — accumulates a running "freed" or
// "occupied" total across several experiments' footprints.
func (f Footprint) AddFootprint(other Footprint) {
	for k, v := range other {
		f[k] += v
	}
}

// Sub returns a new Footprint of f minus other, dimension by dimension. Dimensions present only
// in other appear as negative entries — capacity going negative is how a caller detects
// "doesn't fit".
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

// Fits reports whether every dimension footprint requests is available in capacity — a job fits
// only if ALL requested dimensions fit jointly on that one cluster, not each checked
// independently against a possibly-different cluster. A dimension not requested is never a
// blocker. A dimension requested but not reported by capacity is treated as zero available:
// missing capacity data must fail the fit check, not be silently ignored.
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

// CapacityFootprint builds a per-cluster capacity Footprint from live CPU-core,
// per-accelerator-flavor, and RAM/ephemeral-storage availability — the shape
// workload.Backend.GetFlavorCapacity implementations report. cpuCores is floored to whole
// millicores (never rounded up in the admission loop's favor), preserving fractional-CPU
// precision. ramBytes/storageBytes are already integer bytes — pass 0 if a cluster has no fresh
// report for that dimension so Fits() fails closed. acceleratorByFlavor keys must already be
// accelerator type strings, matching JobSpec.Footprint()/Experiment.Footprint()'s
// accelerator key convention, or Fits() will never match them.
func CapacityFootprint(cpuCores float64, acceleratorByFlavor map[string]int64, ramBytes, storageBytes int64) Footprint {
	fp := NewFootprint()
	fp.Add(ResourceKey{Kind: ResourceKindCPU}, int64(cpuCores*1000))
	// Lowercased so Fits()'s exact-match join works regardless of casing (see AcceleratorType.MatchesLabels).
	for flavor, n := range acceleratorByFlavor {
		fp.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: strings.ToLower(flavor)}, n)
	}
	fp.Add(ResourceKey{Kind: ResourceKindMemory}, ramBytes)
	fp.Add(ResourceKey{Kind: ResourceKindStorage}, storageBytes)
	return fp
}

// FairShare returns a node's per-accelerator entitlement for heldAccelerators of its
// installedAccelerators, in the same canonical integer unit totalCanonical is expressed in.
// Floored, never ceiling: a "max" resolution built from this value must never be judged as
// exceeding its own fair share later by loop_disbalance.go's evictor, which compares with a
// strict > — a floored exact share is always <= itself, so it is never flagged as disbalanced
// due to rounding up.
//
// installedAccelerators <= 0 is a hard error, never a silent 0: per important.md's "one path or
// error, fail fast" rule, a zero/negative denominator is an invalid call, not a legitimate
// "nothing installed" answer with its own defined result — a node genuinely reporting no
// accelerators of a flavor is a case every caller must recognize and skip BEFORE calling this
// (see loop_disbalance.go/loop_resolve.go's own `if installed <= 0 { continue }` guards), not one
// this function should paper over by returning a number that looks like a valid share.
func FairShare(totalCanonical int64, heldAccelerators, installedAccelerators int64) (int64, error) {
	if installedAccelerators <= 0 {
		return 0, fmt.Errorf("domain.FairShare: installedAccelerators must be positive, got %d", installedAccelerators)
	}
	if heldAccelerators <= 0 || totalCanonical <= 0 {
		return 0, nil
	}
	// totalCanonical*heldAccelerators before dividing, not (totalCanonical/installedAccelerators)
	// *heldAccelerators — the latter floors twice and can under-report a multi-accelerator holder's
	// exact share (e.g. total=5, installed=2, held=3: true share floor(15/2)=7, but
	// floor(5/2)*3=6). Canonical units (millicores/bytes) times a per-job accelerator count never
	// approach int64 overflow in practice.
	return (totalCanonical * heldAccelerators) / installedAccelerators, nil
}

// NodeShape is what exactly one node of a job takes from the node it lands on: its accelerator
// count and its fungible resources in canonical units, keyed by NodeResource*. A dimension the
// node does not ask for is absent rather than zero.
type NodeShape struct {
	// Group is the group this node belongs to, "" for an ungrouped job's identical nodes.
	Group            string
	AcceleratorCount int64
	Resources        map[string]int64
}

// NodeShapes expands a job into one entry per node, in group order. This is the placement view
// of a job: admission walks it to prove every node has somewhere to land, which for an ungrouped
// job is Nodes() copies of one shape and for a grouped job is the groups' real, differing shapes.
//
// A malformed quantity string yields an absent dimension rather than an error, matching
// Experiment.Footprint: submission already parsed all three (see ValidateExperiment), and every
// caller here treats a job's shape as unconditional.
func (j JobSpec) NodeShapes() []NodeShape {
	out := make([]NodeShape, 0, j.Nodes())
	for _, group := range j.NodeGroups() {
		resources := map[string]int64{}
		for _, dimension := range []struct {
			key       string
			quantity  string
			canonical func(resource.Quantity) int64
		}{
			{NodeResourceCPUMillicores, group.CPU, func(q resource.Quantity) int64 { return q.MilliValue() }},
			{NodeResourceMemoryBytes, group.Memory, func(q resource.Quantity) int64 { return q.Value() }},
			{NodeResourceStorageBytes, group.Storage, func(q resource.Quantity) int64 { return q.Value() }},
		} {
			if dimension.quantity == "" {
				continue
			}
			q, err := resource.ParseQuantity(dimension.quantity)
			if err != nil {
				continue
			}
			if value := dimension.canonical(q); value > 0 {
				resources[dimension.key] = value
			}
		}
		for replica := 0; replica < group.Replicas; replica++ {
			out = append(out, NodeShape{Group: group.Name, AcceleratorCount: int64(group.AcceleratorCount), Resources: resources})
		}
	}
	return out
}

// Footprint computes j's total resource footprint in canonical units: millicores for CPU,
// bytes for memory/storage, whole units for the accelerator type (if any) and every
// ExtraResources entry.
//
// It is the SUM over j.NodeGroups() of each group's per-replica shape scaled by its replica
// count. For an ungrouped job that is one group of Nodes() identical replicas, i.e. exactly the
// per-node footprint x NumNodes it has always been; for a heterogeneous job it is the sum of the
// groups, which is what makes a learner plus 64 actors cost 8 accelerators rather than 520.
//
// ExtraResources stays job-level and is therefore charged once per node of every group, matching
// how each backend compiles it onto every pod.
//
// Returns an error if any quantity string fails to parse — callers must treat that as a
// rejection, not a zero-footprint dimension.
func (j JobSpec) Footprint(acceleratorType AcceleratorType) (Footprint, error) {
	total := NewFootprint()
	for _, group := range j.NodeGroups() {
		perNode := NewFootprint()
		// A "max" sentinel is always resolved to a plain literal once an experiment is admitted (see
		// scheduler.Loop.resolveClusterLocalResources; JobSpec.Footprint is called with the already-
		// resolved copy — see domain.Experiment.EffectiveJob), so it should never reach here in that
		// case — but resource.ParseQuantity("max") fails with a generic
		// "quantities must match the regular expression" error that names nothing about the real
		// cause, so name it explicitly instead.
		if group.CPU == MaxResourceSentinel || group.Memory == MaxResourceSentinel || group.Storage == MaxResourceSentinel {
			return nil, fmt.Errorf("job.cpu/memory/storage is still %q — it should have been resolved to a literal quantity at submission", MaxResourceSentinel)
		}
		if group.CPU != "" {
			q, err := resource.ParseQuantity(group.CPU)
			if err != nil {
				return nil, fmt.Errorf("job.cpu: %w", err)
			}
			perNode.Add(ResourceKey{Kind: ResourceKindCPU}, q.MilliValue())
		}
		if group.Memory != "" {
			q, err := resource.ParseQuantity(group.Memory)
			if err != nil {
				return nil, fmt.Errorf("job.memory: %w", err)
			}
			perNode.Add(ResourceKey{Kind: ResourceKindMemory}, q.Value())
		}
		if group.Storage != "" {
			q, err := resource.ParseQuantity(group.Storage)
			if err != nil {
				return nil, fmt.Errorf("job.storage: %w", err)
			}
			perNode.Add(ResourceKey{Kind: ResourceKindStorage}, q.Value())
		}
		if group.AcceleratorCount > 0 {
			if acceleratorType == "" {
				return nil, fmt.Errorf("job.accelerator_type is required when job.accelerator_count > 0")
			}
			perNode.Add(ResourceKey{Kind: ResourceKindAccelerator, Flavor: strings.ToLower(string(acceleratorType))}, int64(group.AcceleratorCount))
		}
		for name, qty := range j.ExtraResources {
			q, err := resource.ParseQuantity(qty)
			if err != nil {
				return nil, fmt.Errorf("job.extra_resources[%q]: %w", name, err)
			}
			perNode.Add(ResourceKey{Kind: ResourceKindExtended, Flavor: name}, q.Value())
		}
		total.AddFootprint(perNode.Scale(int64(group.Replicas)))
	}
	return total, nil
}
