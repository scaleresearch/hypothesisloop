package compute

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// slot is room for one instance of one shape, reported as a node. It is taken while any instance
// carries its name, whatever that instance's state, since an instance is billed until it is
// destroyed. It is offerless when no instance holds it and the vendor has no machine to rent for
// it right now, or its market was not read because no job waits for it. noOffer then says why,
// for whoever waits for the slot. architecture is that of the instance's machine, and for a slot
// no instance holds the one it is dealt (slotsAgainst).
//
// network is, for a free slot of a shape some waiting job that spans several nodes wants, how
// many instances of the shape one private network can hold now, and why none when it can hold
// none. It is nil for every other slot.
type slot struct {
	shape        Shape
	name         string
	taken        bool
	noOffer      string
	architecture string
	network      *onOffer
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
	return e.slotsAgainst(v, e.rentable(ctx, v), e.networked(ctx, v)), nil
}

// onOffer is how many more instances of a shape the vendor has a machine of one architecture for,
// and why a slot of the shape and the architecture beyond them has none.
type onOffer struct {
	left int
	why  string
}

// marketKey names the market of one shape on one architecture.
func marketKey(shape, arch string) string { return shape + " on " + arch }

// market is what the vendor has to rent of each shape on each architecture and how many waiting
// jobs could take each, by marketKey.
type market struct {
	offers map[string]onOffer
	wanted map[string]int
}

// rentable reads the market once per view, only for the shapes on the architectures some waiting
// job could take, asking for about as many machines as wait for each. With nothing waiting it
// reads nothing. It is nil for a provider with no market, where every slot no instance holds can
// be rented. A market that cannot be read has nothing to rent until it can, and says why.
func (e *Executor) rentable(ctx context.Context, v *view) *market {
	if e.market == nil || v.market != nil {
		return v.market
	}
	d := e.demanded(ctx, v)
	m := &market{offers: map[string]onOffer{}, wanted: d.wanted}
	taken := takenSlots(v)
	for _, shape := range e.shapes {
		open := 0
		for n := 0; n < shape.MaxInstances; n++ {
			if _, held := taken[slotName(shape.Name, n)]; !held {
				open++
			}
		}
		for _, arch := range e.architectures {
			key := marketKey(shape.Name, arch)
			switch {
			case d.err != nil:
				m.offers[key] = onOffer{why: "shape " + key + ": the jobs waiting for the cluster could not be read: " + d.err.Error()}
			case d.wanted[key] == 0 || open == 0:
				m.offers[key] = onOffer{why: "shape " + key + ": no job waits for it, and its market is read once one does"}
			default:
				m.offers[key] = e.onOffer(ctx, shape, arch, min(open, d.wanted[key]+demandMargin))
			}
		}
	}
	v.market = m
	return m
}

// onOffer asks the market of one shape on one architecture for up to atMost machines.
func (e *Executor) onOffer(ctx context.Context, shape Shape, arch string, atMost int) onOffer {
	key := marketKey(shape.Name, arch)
	supply, err := e.market.Rentable(ctx, shape.spec(arch), atMost)
	if err != nil {
		return onOffer{why: "shape " + key + ": the provider's market could not be read: " + err.Error()}
	}
	if supply.Count == 0 {
		return onOffer{why: "shape " + key + ": " + supply.Why}
	}
	return onOffer{left: supply.Count, why: "shape " + key + ": the shape's lower slots take the machines its market was last read to have"}
}

// spec is an instance of the shape on arch, as a market or a network is asked about it.
func (s Shape) spec(arch string) provider.Spec {
	return provider.Spec{
		Architecture:    arch,
		AcceleratorType: s.AcceleratorType, AcceleratorCount: s.Accelerators,
		CPUMillis: s.CPUMillis, MemoryBytes: s.MemoryBytes, DiskBytes: s.StorageBytes,
	}
}

// networked reads, once per view, how many instances one private network can hold of each shape
// on each architecture some waiting job that spans several nodes wants, asking for as many as the
// largest such job needs, by marketKey. It is nil for a provider with no network.
func (e *Executor) networked(ctx context.Context, v *view) map[string]onOffer {
	if e.network == nil || v.networks != nil {
		return v.networks
	}
	v.networks = map[string]onOffer{}
	for _, shape := range e.shapes {
		for _, arch := range e.architectures {
			key := marketKey(shape.Name, arch)
			g, wanted := e.demanded(ctx, v).gangs[key]
			if !wanted {
				continue
			}
			supply, err := e.network.Networked(ctx, shape.spec(arch), g.nodes, g.spread)
			switch {
			case err != nil:
				v.networks[key] = onOffer{why: "shape " + key + ": the provider's private networks could not be read: " + err.Error()}
			case supply.Count == 0:
				v.networks[key] = onOffer{why: "shape " + key + ": " + supply.Why}
			default:
				v.networks[key] = onOffer{left: supply.Count}
			}
		}
	}
	return v.networks
}

// takenSlots is the architecture of the machine in each slot an instance holds.
func takenSlots(v *view) map[string]string {
	taken := make(map[string]string, len(v.instances))
	for _, i := range v.instances {
		taken[i.slot()] = i.Architecture
	}
	return taken
}

// slotsAgainst marks the slots of v. Each shape's slots that no instance holds are dealt in turn,
// lowest index first, to every architecture the provider rents, whatever the market has: the
// architectures a cluster names do not wait on a read. Those some job waits for on the shape come
// first, so a shape with a single open slot names the one wanted. m bounds how many slots of
// each architecture are free, and nil bounds none. networks are what each free slot of a shape
// on an architecture is told of the provider's private networks.
func (e *Executor) slotsAgainst(v *view, m *market, networks map[string]onOffer) []slot {
	taken := takenSlots(v)
	var slots []slot
	for _, shape := range e.shapes {
		order := e.architectures
		left := map[string]int{}
		if m != nil {
			order = m.order(shape.Name, e.architectures)
			for _, arch := range order {
				left[arch] = m.offers[marketKey(shape.Name, arch)].left
			}
		}
		turn := 0
		for n := 0; n < shape.MaxInstances; n++ {
			s := slot{shape: shape, name: slotName(shape.Name, n)}
			if arch, held := taken[s.name]; held {
				s.taken, s.architecture = true, arch
				slots = append(slots, s)
				continue
			}
			s.architecture = order[turn%len(order)]
			turn++
			if m != nil {
				if left[s.architecture] > 0 {
					left[s.architecture]--
				} else {
					s.noOffer = m.offers[marketKey(shape.Name, s.architecture)].why
				}
			}
			if network, wanted := networks[marketKey(shape.Name, s.architecture)]; wanted && s.free() {
				s.network = &network
			}
			slots = append(slots, s)
		}
	}
	return slots
}

// order is architectures with those some job waits for on shape first.
func (m *market) order(shape string, architectures []string) []string {
	ordered := slices.Clone(architectures)
	slices.SortStableFunc(ordered, func(a, b string) int {
		wantA, wantB := m.wanted[marketKey(shape, a)] > 0, m.wanted[marketKey(shape, b)] > 0
		switch {
		case wantA == wantB:
			return 0
		case wantA:
			return -1
		}
		return 1
	})
	return ordered
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
// sizes one job to it and says which slot it placed the job on, its architecture where it has one
// (a vendor may not say that of a machine it rented), what a job's image needs there, for a slot with
// accelerators how many it has and the label its type stands for, as a device plugin would
// publish it, for an offerless slot that it is one, so a job waiting for it is told what it
// waits for, and what it is told of the provider's private networks.
func (s slot) labels(imageNeeds []string) map[string]string {
	labels := map[string]string{clusterapi.NodeSlotLabel: s.name}
	if s.architecture != "" {
		labels[clusterapi.NodeArchitectureLabel] = s.architecture
	}
	if len(imageNeeds) > 0 {
		labels[clusterapi.NodeImageNeedsLabel] = strings.Join(imageNeeds, ",")
	}
	if s.offerless() {
		labels[clusterapi.NodeSlotNoOfferLabel] = s.noOffer
	}
	if s.network != nil {
		labels[clusterapi.NodeSlotNetworkLabel] = strconv.Itoa(s.network.left)
		if s.network.left == 0 {
			labels[clusterapi.NodeSlotNoNetworkLabel] = s.network.why
		}
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
	for _, s := range e.slotsAgainst(&view{}, nil, nil) {
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
		nodeLabels[s.name] = s.labels(e.imageNeeds)
		nodeLabels[s.name][clusterapi.NodeSlotStartupLabel] = strconv.Itoa(int(e.startup.Seconds()))
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
