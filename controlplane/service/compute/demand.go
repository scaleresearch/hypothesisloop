package compute

import (
	"context"
	"slices"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// Demand is what waits to be placed on the cluster: the queued jobs it may run. A provider's
// market is read only for what they could be placed on.
type Demand interface {
	Waiting(ctx context.Context) ([]Waiting, error)
}

// Waiting is one queued job: its accelerator type, the architecture of its image and the job as
// submitted.
type Waiting struct {
	AcceleratorType clusterapi.AcceleratorType
	Architecture    string
	Job             clusterapi.JobSpec
}

// demandMargin is how many machines beyond the jobs waiting for a shape on an architecture its
// market is asked about, for the jobs queued between two passes.
const demandMargin = 2

// demand is one read of what waits for the cluster, by marketKey: how many nodes of waiting jobs
// each shape on each architecture could take, and in gangs the most nodes of it one waiting job
// that spans several needs on one private network. err says why it could not be read.
type demand struct {
	wanted map[string]int
	gangs  map[string]int
	err    error
}

// demanded reads what waits once per view. A job counts on every shape that takes it, as
// admission may place it on any of them, with the nodes of each group the shape takes.
func (e *Executor) demanded(ctx context.Context, v *view) *demand {
	if v.demand != nil {
		return v.demand
	}
	d := &demand{wanted: map[string]int{}, gangs: map[string]int{}}
	v.demand = d
	waiting, err := e.demand.Waiting(ctx)
	if err != nil {
		d.err = err
		return d
	}
	for _, w := range waiting {
		for _, shape := range e.shapes {
			key := marketKey(shape.Name, w.Architecture)
			nodes := e.takes(shape, w.Architecture, w)
			d.wanted[key] += nodes
			if nodes > 0 && w.Job.Nodes() > 1 {
				d.gangs[key] = max(d.gangs[key], nodes)
			}
		}
	}
	return d
}

// takes is how many of w's nodes admission could place on slots of shape on arch, by the rule it
// places by: the slot's labels under the job's node selector and clusterapi.SlotTakes for each
// group. A job that spans several nodes is taken only where a private network can join them as
// near as it asks.
func (e *Executor) takes(shape Shape, arch string, w Waiting) int {
	if (w.Job.Nodes() > 1 && (e.network == nil || !clusterapi.NodesLevel(e.network.Within()).AsNearAs(w.Job.NodesWithin()))) || !slices.Contains(e.architectures, arch) {
		return 0
	}
	labels := slot{shape: shape, architecture: arch}.labels(e.imageNeeds)
	for key, value := range w.Job.NodeSelector {
		if labels[key] != value {
			return 0
		}
	}
	size := map[string]int64{
		clusterapi.NodeResourceCPUMillicores: shape.CPUMillis,
		clusterapi.NodeResourceMemoryBytes:   shape.MemoryBytes,
		clusterapi.NodeResourceStorageBytes:  shape.StorageBytes,
	}
	nodes := 0
	for _, group := range w.Job.NodeGroups() {
		if clusterapi.SlotTakes(w.AcceleratorType, group, shape.Accelerators, labels, size) {
			nodes += group.Replicas
		}
	}
	return nodes
}
