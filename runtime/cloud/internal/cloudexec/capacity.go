package cloudexec

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// slot is room for one instance of one shape, reported as a node. It is taken while any instance
// carries its name, whatever that instance's state, since an instance is billed until it is
// destroyed. It is offerless when no instance holds it and the vendor has no machine to rent for
// it right now: the configuration allows the instance and the market does not.
type slot struct {
	shape     Shape
	name      string
	taken     bool
	offerless bool
}

// free says a job admitted to the slot now can be rented a machine.
func (s slot) free() bool { return !s.taken && !s.offerless }

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

// rentable is how many more instances of each shape the vendor has a machine for, at the size
// Create rents a slot at. It is nil for a provider with no market, where every slot no instance
// holds can be rented.
//
// The market is asked about every shape once per view, so a slot is reported free on what the
// vendor has right now. A count kept from an earlier pass reports a slot free after the market
// dried up, and the job placed on it waits and goes back to the queue.
func (e *Executor) rentable(ctx context.Context, v *view) (map[string]int, error) {
	if e.market == nil || v.rentable != nil {
		return v.rentable, nil
	}
	rentable := make(map[string]int, len(e.shapes))
	for _, shape := range e.shapes {
		count, err := e.market.Rentable(ctx, provider.Spec{
			AcceleratorType: shape.AcceleratorType, AcceleratorCount: shape.Accelerators,
			CPUMillis: shape.CPUMillis, MemoryBytes: shape.MemoryBytes, DiskBytes: shape.StorageBytes,
		}, shape.MaxInstances)
		if err != nil {
			return nil, fmt.Errorf("cloudexec: count what there is to rent for shape %s: %w", shape.Name, err)
		}
		rentable[shape.Name] = count
	}
	v.rentable = rentable
	return rentable, nil
}

// slotsAgainst marks the slots of v. rentable bounds how many of a shape's slots that no
// instance holds are free, lowest index first, and nil bounds none.
func (e *Executor) slotsAgainst(v *view, rentable map[string]int) []slot {
	taken := make(map[string]bool, len(v.instances))
	for _, i := range v.instances {
		taken[i.slot()] = true
	}
	var slots []slot
	for _, shape := range e.shapes {
		left := rentable[shape.Name]
		for n := 0; n < shape.MaxInstances; n++ {
			s := slot{shape: shape, name: slotName(shape.Name, n), taken: taken[slotName(shape.Name, n)]}
			if !s.taken && rentable != nil {
				s.offerless = left == 0
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
	labels := map[string]string{domain.NodeSlotLabel: s.name}
	if s.offerless {
		labels[domain.NodeSlotNoOfferLabel] = "true"
	}
	if key, value, ok := strings.Cut(s.shape.AcceleratorType, "="); ok {
		labels[key] = value
		labels[domain.NodeSlotAcceleratorsLabel] = strconv.Itoa(s.shape.Accelerators)
	}
	return labels
}

// holds says whether a slot of this shape may be asked for want. Admission chose the slot and
// sized the job to it; this keeps a job from renting more than the shape allows, whatever desired
// state says. The type is compared the way admission compares it (domain.AcceleratorType.Is), so
// a job admitted to this slot is never refused here for how it cased its type.
func (s Shape) holds(want provider.Spec) error {
	if want.AcceleratorCount > 0 && !domain.AcceleratorType(want.AcceleratorType).Is(domain.AcceleratorType(s.AcceleratorType)) {
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
		domain.NodeResourceCPUMillicores: s.CPUMillis,
		domain.NodeResourceMemoryBytes:   s.MemoryBytes,
		domain.NodeResourceStorageBytes:  s.StorageBytes,
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
		if s.shape.Accelerators == 0 {
			continue
		}
		free := int64(s.shape.Accelerators)
		if !s.free() {
			free = 0
		}
		total[s.shape.AcceleratorType] += int64(s.shape.Accelerators)
		available[s.shape.AcceleratorType] += free
		byNode[s.name] = map[string]int64{s.shape.AcceleratorType: free}
	}
	return available, total, byNode, nodeLabels, nil
}
