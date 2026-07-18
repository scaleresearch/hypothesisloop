package workload

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// DefaultClusterName is the cluster name used when only a single cluster is configured
// (the historical single-cluster behavior) or when an experiment has no ClusterName set
// (old rows, single-cluster mode).
const DefaultClusterName = "default"

// ClusterSet holds one *JobWorkloadClient per configured target Kubernetes cluster, keyed
// by cluster name. It is the multi-cluster-aware entry point that call sites should use
// instead of holding a single *JobWorkloadClient directly.
type ClusterSet struct {
	clients map[string]*JobWorkloadClient
	order   []string
}

// NewClusterSet builds a ClusterSet from a map of per-cluster Config, calling the existing
// New() for each entry. The map keys are cluster names (e.g. "default").
func NewClusterSet(configs map[string]Config) (*ClusterSet, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("workload: NewClusterSet: no cluster configs provided")
	}
	cs := &ClusterSet{clients: make(map[string]*JobWorkloadClient, len(configs))}
	// Sort names for deterministic iteration order (Names(), first-cluster policies, etc).
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		client, err := New(configs[name])
		if err != nil {
			return nil, fmt.Errorf("workload: NewClusterSet: cluster %q: %w", name, err)
		}
		cs.clients[name] = client
		cs.order = append(cs.order, name)
	}
	return cs, nil
}

// Get returns the client for the named cluster, if configured.
func (cs *ClusterSet) Get(name string) (*JobWorkloadClient, bool) {
	c, ok := cs.clients[name]
	return c, ok
}

// Default returns the client to use when a call site has no explicit cluster routing
// (backward compatibility for single-cluster deployments and old rows without a
// ClusterName). If there is exactly one configured cluster, that one is returned
// regardless of its name. Otherwise the cluster named "default" is returned, if present.
func (cs *ClusterSet) Default() *JobWorkloadClient {
	if len(cs.clients) == 1 {
		for _, c := range cs.clients {
			return c
		}
	}
	return cs.clients[DefaultClusterName]
}

// Names returns the configured cluster names in stable (sorted) order.
func (cs *ClusterSet) Names() []string {
	out := make([]string, len(cs.order))
	copy(out, cs.order)
	return out
}

// forExperiment resolves the client to use for exp, routing by exp.ClusterName and
// falling back to Default() when it is empty (old rows / single-cluster mode) or unknown.
func (cs *ClusterSet) forExperiment(exp *domain.Experiment) *JobWorkloadClient {
	if exp != nil && exp.ClusterName != "" {
		if c, ok := cs.clients[exp.ClusterName]; ok {
			return c
		}
	}
	return cs.Default()
}

// CreateWorkload routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.CreateWorkload: no client for cluster %q", exp.ClusterName)
	}
	return c.CreateWorkload(ctx, exp)
}

// DeleteWorkload routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) DeleteWorkload(ctx context.Context, exp *domain.Experiment) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.DeleteWorkload: no client for cluster %q", exp.ClusterName)
	}
	return c.DeleteWorkload(ctx, exp.ID)
}

// PollJobPhase routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) PollJobPhase(ctx context.Context, exp *domain.Experiment) (JobPhase, error) {
	c := cs.forExperiment(exp)
	if c == nil {
		return JobPhasePending, fmt.Errorf("workload: ClusterSet.PollJobPhase: no client for cluster %q", exp.ClusterName)
	}
	return c.PollJobPhase(ctx, exp.ID)
}

// GetAdmittedGPUType routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType {
	c := cs.forExperiment(exp)
	if c == nil {
		return exp.GPUType
	}
	return c.GetAdmittedGPUType(ctx, exp)
}

// WaitForJobDeletion routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.WaitForJobDeletion: no client for cluster %q", exp.ClusterName)
	}
	return c.WaitForJobDeletion(ctx, exp.ID, timeout)
}

// GetFlavorCapacity returns nominal GPU slot capacity per flavor, broken out per configured
// cluster (guaranteed[cluster][flavor], burst[cluster][flavor]) — capacity is derived from
// static config (not live cluster state) and is identical across clusters today, but each
// cluster still gets its own entry so admission can reason about per-cluster room.
func (cs *ClusterSet) GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]map[string]int64, err error) {
	guaranteed = make(map[string]map[string]int64, len(cs.order))
	burst = make(map[string]map[string]int64, len(cs.order))
	for _, name := range cs.order {
		g, b, err := cs.clients[name].GetFlavorCapacity(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("workload: ClusterSet.GetFlavorCapacity: cluster %q: %w", name, err)
		}
		guaranteed[name] = g
		burst[name] = b
	}
	return guaranteed, burst, nil
}

// ClusterNames is an alias for Names(), satisfying call sites that pick a target cluster
// for admission (e.g. services/scheduler.WorkloadClient).
func (cs *ClusterSet) ClusterNames() []string {
	return cs.Names()
}

// ProvisionAgent runs on the default cluster: today's native backend has no per-agent
// object to create (see Backend.ProvisionAgent doc), so this is a no-op passthrough.
func (cs *ClusterSet) ProvisionAgent(ctx context.Context, agentID string) error {
	c := cs.Default()
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.ProvisionAgent: no default cluster configured")
	}
	return c.ProvisionAgent(ctx, agentID)
}

// SetupCluster runs SetupCluster on every configured cluster.
func (cs *ClusterSet) SetupCluster(ctx context.Context) error {
	for _, name := range cs.order {
		if err := cs.clients[name].SetupCluster(ctx); err != nil {
			return fmt.Errorf("workload: ClusterSet.SetupCluster: cluster %q: %w", name, err)
		}
	}
	return nil
}
