package compute

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// Demand is what waits to be placed on the cluster: the queued jobs it may run. A provider's
// market is read only for what they could be placed on.
type Demand interface {
	Waiting(ctx context.Context) ([]Waiting, error)
}

// Waiting is one queued job: its accelerator type and the job as submitted.
type Waiting struct {
	AcceleratorType clusterapi.AcceleratorType
	Job             clusterapi.JobSpec
}

// demandMargin is how many machines beyond the jobs waiting for a shape on an architecture its
// market is asked about, for the jobs queued between two passes.
const demandMargin = 2

// wanted is how many waiting jobs each shape on each architecture could take, by marketKey. A job
// counts on every shape that takes it, as admission may place it on any of them.
func (e *Executor) wanted(ctx context.Context) (map[string]int, error) {
	waiting, err := e.demand.Waiting(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]int{}
	for _, w := range waiting {
		arch := w.Job.NodeSelector[clusterapi.NodeArchitectureLabel]
		for _, shape := range e.shapes {
			if e.takes(shape, arch, w) {
				wanted[marketKey(shape.Name, arch)]++
			}
		}
	}
	return wanted, nil
}

// takes says a slot of shape on arch is one admission could place w on, by the rule it places
// by: one node, exactly the shape's accelerators of its type (none for a job on cores), the slot's
// labels under the job's node selector, and every quantity the job names within the shape.
func (e *Executor) takes(shape Shape, arch string, w Waiting) bool {
	if w.Job.Nodes() != 1 || !slices.Contains(e.architectures, arch) {
		return false
	}
	group := w.Job.NodeGroups()[0]
	devices := group.AcceleratorCount
	if w.AcceleratorType.Kind() == clusterapi.TypeKindCPU {
		devices = 0
	}
	labels := slot{shape: shape, architecture: arch}.labels(e.imageNeeds)
	if shape.Accelerators != devices || (devices > 0 && !w.AcceleratorType.MatchesLabels(labels)) {
		return false
	}
	for key, value := range w.Job.NodeSelector {
		if labels[key] != value {
			return false
		}
	}
	for _, q := range []struct {
		quantity string
		within   int64
		milli    bool
	}{{group.CPU, shape.CPUMillis, true}, {group.Memory, shape.MemoryBytes, false}, {group.Storage, shape.StorageBytes, false}} {
		parsed, err := resource.ParseQuantity(q.quantity)
		if err != nil {
			return false
		}
		amount := parsed.Value()
		if q.milli {
			amount = parsed.MilliValue()
		}
		if amount > q.within {
			return false
		}
	}
	return true
}
