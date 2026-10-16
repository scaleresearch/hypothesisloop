package cloudcompute

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
)

// slot is room for one instance of one shape, reported as a node. It is taken while any instance
// carries its name, whatever that instance's state, since an instance is billed until it is
// destroyed. It is offerless when no instance holds it and the vendor has no machine to rent for
// it right now: the configuration allows the instance and the market does not. noOffer then says
// why, for whoever waits for the slot.
type slot struct {
	shape   Shape
	name    string
	taken   bool
	noOffer string
}

func (s slot) offerless() bool { return s.noOffer != "" }

// free says a job admitted to the slot now can be rented a machine.
func (s slot) free() bool { return !s.taken && !s.offerless() }

// slots lays every shape's slots against what the account holds and what the vendor has to rent.
// An instance of a shape or an index that is no longer configured is in no slot: it is still
// managed, and counted nowhere.
func (e *Executor) slots(ctx context.Context) ([]slot, error) {
	v, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	rentable, err := e.rentable(ctx, v)
	if err != nil {
		return nil, err
	}
	return e.slotsAgainst(v, rentable), nil
}

// onOffer is how many more instances of a shape the vendor has a machine for, and why a slot of
// the shape beyond them has none.
type onOffer struct {
	left int
	why  string
}

// rentable is what the vendor has to rent of each shape, at the size Create rents a slot at, asked
// once per shape and view. It is nil for a provider with no market, where every slot no instance
// holds can be rented.
func (e *Executor) rentable(ctx context.Context, v *view) (map[string]onOffer, error) {
	if e.market == nil || v.rentable != nil {
		return v.rentable, nil
	}
	rentable := make(map[string]onOffer, len(e.shapes))
	for _, shape := range e.shapes {
		supply, err := e.market.Rentable(ctx, provider.Spec{
			AcceleratorType: shape.AcceleratorType, AcceleratorCount: shape.Accelerators,
			CPUMillis: shape.CPUMillis, MemoryBytes: shape.MemoryBytes, DiskBytes: shape.StorageBytes,
		}, shape.MaxInstances)
		if err != nil {
			return nil, fmt.Errorf("cloudcompute: count what there is to rent for shape %s: %w", shape.Name, err)
		}
		why := "shape " + shape.Name + ": " + supply.Why
		if supply.Count == 0 {
			log.Printf("cloudcompute: shape %s has nothing to rent: %s (%s)", shape.Name, supply.Why, supply.Detail)
		} else {
			why = "shape " + shape.Name + ": the provider has fewer machines on offer for it than max_instances, and the shape's lower slots take them"
		}
		rentable[shape.Name] = onOffer{left: supply.Count, why: why}
	}
	v.rentable = rentable
	return rentable, nil
}

// slotsAgainst marks the slots of v. rentable bounds how many of a shape's slots that no
// instance holds are free, lowest index first, and nil bounds none.
func (e *Executor) slotsAgainst(v *view, rentable map[string]onOffer) []slot {
	taken := make(map[string]bool, len(v.instances))
	for _, i := range v.instances {
		taken[i.slot()] = true
	}
	var slots []slot
	for _, shape := range e.shapes {
		supply := rentable[shape.Name]
		left := supply.left
		for n := 0; n < shape.MaxInstances; n++ {
			s := slot{shape: shape, name: slotName(shape.Name, n), taken: taken[slotName(shape.Name, n)]}
			if !s.taken && rentable != nil {
				if left == 0 {
					s.noOffer = supply.why
				}
				left = max(0, left-1)
			}
			slots = append(slots, s)
		}
	}
	return slots
}

// configured returns the shape of the slot called name, if the configuration has such a slot.
func (e *Executor) configured(name string) (shape Shape, index int, ok bool) {
	for _, s := range e.shapes {
		for n := 0; n < s.MaxInstances; n++ {
			if slotName(s.Name, n) == name {
				return s, n, true
			}
		}
	}
	return Shape{}, 0, false
}

// labels are the node labels of a slot: the one that makes it a slot and names it, so admission
// sizes one job to it and says which slot it placed the job on, for a slot with accelerators
// how many it has and the label its type stands for, as a device plugin would publish it, and
// for an offerless slot that it is one, so a job waiting for it is told what it waits for.
func (s slot) labels() map[string]string {
	labels := map[string]string{clusterapi.NodeSlotLabel: s.name}
	if s.offerless() {
		labels[clusterapi.NodeSlotNoOfferLabel] = s.noOffer
	}
	if key, value, ok := strings.Cut(s.shape.AcceleratorType, "="); ok {
		labels[key] = value
		labels[clusterapi.NodeSlotAcceleratorsLabel] = strconv.Itoa(s.shape.Accelerators)
	}
	return labels
}

// holds says whether a slot of this shape may be asked for want. Admission chose the slot and
// sized the job to it; this keeps a job from renting more than the shape allows, whatever desired
// state says. The type is compared the way admission compares it (clusterapi.AcceleratorType.Is), so
// a job admitted to this slot is never refused here for how it cased its type.
func (s Shape) holds(want provider.Spec) error {
	if want.AcceleratorCount > 0 && !clusterapi.AcceleratorType(want.AcceleratorType).Is(clusterapi.AcceleratorType(s.AcceleratorType)) {
		return fmt.Errorf("accelerator type %q is not the slot's %q", want.AcceleratorType, s.AcceleratorType)
	}
	if want.AcceleratorCount > s.Accelerators || want.CPUMillis > s.CPUMillis || want.MemoryBytes > s.MemoryBytes || want.DiskBytes > s.StorageBytes {
		return fmt.Errorf("accelerators %d, cpu %dm, memory %d, storage %d exceed the slot's shape", want.AcceleratorCount, want.CPUMillis, want.MemoryBytes, want.DiskBytes)
	}
	return nil
}

func (e *Executor) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return 0, 0, err
	}
	var availableMillis, totalMillis int64
	for _, s := range slots {
		totalMillis += s.shape.CPUMillis
		if s.free() {
			availableMillis += s.shape.CPUMillis
		}
	}
	return float64(availableMillis) / 1000, float64(totalMillis) / 1000, nil
}

func (e *Executor) GetLiveRAMCapacity(ctx context.Context) (available, total int64, err error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range slots {
		total += s.shape.MemoryBytes
		if s.free() {
			available += s.shape.MemoryBytes
		}
	}
	return available, total, nil
}

func (e *Executor) GetLiveStorageCapacity(ctx context.Context) (available, total int64, err error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range slots {
		total += s.shape.StorageBytes
		if s.free() {
			available += s.shape.StorageBytes
		}
	}
	return available, total, nil
}

// GetLiveNodeResourceCapacity reports a free slot at its shape's full size and any other at
// zero: a slot is one instance, however small the job in it.
func (e *Executor) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return nil, err
	}
	byNode := make(map[string]map[string]int64, len(slots))
	for _, s := range slots {
		if !s.free() {
			byNode[s.name] = nodeResources(Shape{})
			continue
		}
		byNode[s.name] = nodeResources(s.shape)
	}
	return byNode, nil
}

// GetNodeTotalCapacity is configuration alone. Nothing but our own instances is ever in a slot.
func (e *Executor) GetNodeTotalCapacity(context.Context) (map[string]map[string]int64, error) {
	byNode := map[string]map[string]int64{}
	for _, s := range e.slotsAgainst(&view{}, nil) {
		byNode[s.name] = nodeResources(s.shape)
	}
	return byNode, nil
}

func nodeResources(s Shape) map[string]int64 {
	return map[string]int64{
		clusterapi.NodeResourceCPUMillicores: s.CPUMillis,
		clusterapi.NodeResourceMemoryBytes:   s.MemoryBytes,
		clusterapi.NodeResourceStorageBytes:  s.StorageBytes,
	}
}

func (e *Executor) GetLiveAcceleratorCapacitySnapshot(ctx context.Context) (available, total map[string]int64, byNode map[string]map[string]int64, nodeLabels map[string]map[string]string, err error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	available, total = map[string]int64{}, map[string]int64{}
	byNode = map[string]map[string]int64{}
	nodeLabels = make(map[string]map[string]string, len(slots))
	for _, s := range slots {
		nodeLabels[s.name] = s.labels()
		unit, units := s.shape.units()
		free := units
		if !s.free() {
			free = 0
		}
		total[unit] += units
		available[unit] += free
		byNode[s.name] = map[string]int64{unit: free}
	}
	return available, total, byNode, nodeLabels, nil
}

// units is what a slot of the shape offers a job, as the type it is priced under and how many:
// its accelerators, or for a shape with none its whole cores as units of clusterapi.CPUType, which a
// job on CPU holds all of. A shape with accelerators offers no cores: a job on CPU never holds a
// slot with a device (clusterapi.NodeSlotLabel).
func (s Shape) units() (unit string, count int64) {
	if s.Accelerators == 0 {
		return string(clusterapi.CPUType), s.CPUMillis / 1000
	}
	return s.AcceleratorType, int64(s.Accelerators)
}
