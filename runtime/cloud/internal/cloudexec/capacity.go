package cloudexec

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// slot is one synthetic node: room for one instance of one shape. It is taken while any instance
// carries its name, whatever that instance's state, since an instance is billed until it is
// destroyed.
type slot struct {
	shape Shape
	node  string
	taken bool
}

// slots reads the account once and lays every shape's slots against what is rented. Nothing is
// remembered from the previous read.
func (e *Executor) slots(ctx context.Context) ([]slot, error) {
	instances, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	return e.slotsAgainst(instances), nil
}

func (e *Executor) slotsAgainst(instances []instance) []slot {
	taken := make(map[string]bool, len(instances))
	for _, i := range instances {
		taken[i.node()] = true
	}
	var slots []slot
	for _, shape := range e.shapes {
		for n := 0; n < shape.MaxInstances; n++ {
			node := nodeName(shape.Name, n)
			slots = append(slots, slot{shape: shape, node: node, taken: taken[node]})
		}
	}
	return slots
}

// labels is the node label a shape's accelerator type stands for, as a device plugin would
// publish it. Admission matches a job's type against these.
func (s Shape) labels() map[string]string {
	key, value, ok := strings.Cut(s.AcceleratorType, "=")
	if !ok {
		return map[string]string{}
	}
	return map[string]string{key: value}
}

func (s Shape) fits(want provider.Spec) bool {
	if want.AcceleratorCount > 0 {
		if !domain.AcceleratorType(want.AcceleratorType).MatchesLabels(s.labels()) || want.AcceleratorCount > s.Accelerators {
			return false
		}
	}
	return want.CPUMillis <= s.CPUMillis && want.MemoryBytes <= s.MemoryBytes && want.DiskBytes <= s.StorageBytes
}

// freeSlot picks the slot a job with these requests is rented into: a free one of the shape with
// the fewest accelerators that fits, so a job that needs none leaves the accelerator shapes to
// the jobs that do. Admission already found a node with room; this is the same answer read from
// the account as it is now.
func (e *Executor) freeSlot(instances []instance, want provider.Spec) (shape string, index int, err error) {
	var fitting []Shape
	for _, s := range e.shapes {
		if s.fits(want) {
			fitting = append(fitting, s)
		}
	}
	if len(fitting) == 0 {
		return "", 0, fmt.Errorf("cloudexec: no shape fits accelerator %q x%d, cpu %dm, memory %d, storage %d",
			want.AcceleratorType, want.AcceleratorCount, want.CPUMillis, want.MemoryBytes, want.DiskBytes)
	}
	sort.SliceStable(fitting, func(a, b int) bool { return fitting[a].Accelerators < fitting[b].Accelerators })
	taken := make(map[string]bool, len(instances))
	for _, i := range instances {
		taken[i.node()] = true
	}
	for _, s := range fitting {
		for n := 0; n < s.MaxInstances; n++ {
			if !taken[nodeName(s.Name, n)] {
				return s.Name, n, nil
			}
		}
	}
	return "", 0, fmt.Errorf("cloudexec: every instance of a shape that fits is already rented")
}

func (e *Executor) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return 0, 0, err
	}
	var availableMillis, totalMillis int64
	for _, s := range slots {
		totalMillis += s.shape.CPUMillis
		if !s.taken {
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
		if !s.taken {
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
		if !s.taken {
			available += s.shape.StorageBytes
		}
	}
	return available, total, nil
}

// GetLiveNodeResourceCapacity reports a free slot at its shape's full size and a taken one at
// zero: a slot is one instance, however small the job in it.
func (e *Executor) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	slots, err := e.slots(ctx)
	if err != nil {
		return nil, err
	}
	byNode := make(map[string]map[string]int64, len(slots))
	for _, s := range slots {
		if s.taken {
			byNode[s.node] = nodeResources(Shape{})
			continue
		}
		byNode[s.node] = nodeResources(s.shape)
	}
	return byNode, nil
}

// GetNodeTotalCapacity is configuration alone. Nothing but our own instances is ever in a slot.
func (e *Executor) GetNodeTotalCapacity(context.Context) (map[string]map[string]int64, error) {
	byNode := map[string]map[string]int64{}
	for _, s := range e.slotsAgainst(nil) {
		byNode[s.node] = nodeResources(s.shape)
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
		nodeLabels[s.node] = s.shape.labels()
		if s.shape.Accelerators == 0 {
			continue
		}
		free := int64(s.shape.Accelerators)
		if s.taken {
			free = 0
		}
		total[s.shape.AcceleratorType] += int64(s.shape.Accelerators)
		available[s.shape.AcceleratorType] += free
		byNode[s.node] = map[string]int64{s.shape.AcceleratorType: free}
	}
	return available, total, byNode, nodeLabels, nil
}
