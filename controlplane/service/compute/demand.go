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
// each shape on each architecture could take, and the gangs among them. err says why it could
// not be read.
type demand struct {
	wanted map[string]int
	gangs  map[string]gang
	err    error
}

// gang is what the waiting jobs that span several nodes ask of one shape on one architecture: the
// most nodes of it one of them needs on one private network, and whether any of them needs each
// on a host of its own.
type gang struct {
	nodes  int
	spread bool
}

// demanded reads what waits once per view. A job counts on every shape that takes it, as
// admission may place it on any of them, with the nodes of each group the shape takes.
func (e *Executor) demanded(ctx context.Context, v *view) *demand {
	if v.demand != nil {
		return v.demand
	}
	d := &demand{wanted: map[string]int{}, gangs: map[string]gang{}}
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
				g := d.gangs[key]
				d.gangs[key] = gang{nodes: max(g.nodes, nodes), spread: g.spread || spreads(w.Job)}
			}
		}
	}
	return d
}

// takes is how many of w's nodes admission could place on slots of shape on arch, by the rule it
// places by: the nodes of every group with exactly the shape's accelerators of its type (none for
// a job on cores), the slot's labels under the job's node selector, and every quantity the group
// names within the shape. A job that spans several nodes is taken only where a private network
// can join them.
func (e *Executor) takes(shape Shape, arch string, w Waiting) int {
	if (w.Job.Nodes() > 1 && e.network == nil) || !slices.Contains(e.architectures, arch) {
		return 0
	}
	labels := slot{shape: shape, architecture: arch}.labels(e.imageNeeds)
	for key, value := range w.Job.NodeSelector {
		if labels[key] != value {
			return 0
		}
	}
	nodes := 0
	for _, group := range w.Job.NodeGroups() {
		if holdsGroup(shape, labels, w.AcceleratorType, group) {
			nodes += group.Replicas
		}
	}
	return nodes
}

func holdsGroup(shape Shape, labels map[string]string, acceleratorType clusterapi.AcceleratorType, group clusterapi.JobGroup) bool {
	devices := group.AcceleratorCount
	if acceleratorType.Kind() == clusterapi.TypeKindCPU {
		devices = 0
	}
	if shape.Accelerators != devices || (devices > 0 && !acceleratorType.MatchesLabels(labels)) {
		return false
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
