package compute

import (
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
)

// rank is one node of a job as it is rented: the instance to rent, without its name or where it
// meets the others, and where it stands among them, the node at index of group, whose first rank
// is offset.
type rank struct {
	spec   provider.Spec
	group  clusterapi.JobGroup
	offset int
	index  int
}

// buildRanks compiles exp into one rank per node, in rank order.
func (e *Executor) buildRanks(exp *clusterapi.DesiredJob) ([]rank, error) {
	// EffectiveJob, never exp.Job: an admitted experiment's literal quantities and graces are in
	// exp.ResolvedJob.
	job := exp.EffectiveJob()
	arch := job.NodeSelector[clusterapi.NodeArchitectureLabel]
	if arch == "" {
		return nil, fmt.Errorf("cloudcompute: experiment %s names no architecture in its node selector (%s)", exp.ID, clusterapi.NodeArchitectureLabel)
	}
	var ranks []rank
	for _, group := range job.NodeGroups() {
		spec, err := e.buildSpec(exp, job, group, arch)
		if err != nil {
			return nil, err
		}
		offset := len(ranks)
		for index := range group.Replicas {
			ranks = append(ranks, rank{spec: spec, group: group, offset: offset, index: index})
		}
	}
	return ranks, nil
}

// buildSpec compiles one node of group into the instance to rent.
func (e *Executor) buildSpec(exp *clusterapi.DesiredJob, job clusterapi.JobSpec, group clusterapi.JobGroup, arch string) (provider.Spec, error) {
	for name, quantity := range map[string]string{"cpu": group.CPU, "memory": group.Memory, "storage": group.Storage} {
		if quantity == "" {
			return provider.Spec{}, fmt.Errorf("cloudcompute: experiment %s has no resolved %s in desired state", exp.ID, name)
		}
	}
	cpu, err := resource.ParseQuantity(group.CPU)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudcompute: parse cpu %q: %w", group.CPU, err)
	}
	memory, err := resource.ParseQuantity(group.Memory)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudcompute: parse memory %q: %w", group.Memory, err)
	}
	storage, err := resource.ParseQuantity(group.Storage)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudcompute: parse storage %q: %w", group.Storage, err)
	}

	env, err := workloadenv.Merged(exp, job, group, e.apiURL, func(url string) string { return url })
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudcompute: %w", err)
	}

	// A job on CPU counts its cores as units of the CPU type, which is no device to rent.
	acceleratorType, accelerators := string(exp.AcceleratorType), group.AcceleratorCount
	if exp.AcceleratorType.Kind() == clusterapi.TypeKindCPU {
		acceleratorType, accelerators = "", 0
	}
	var login *provider.ImageLogin
	if pull := exp.ImagePull; pull != nil {
		login = &provider.ImageLogin{Registry: pull.Registry, Username: pull.Username, Password: pull.Password}
	}
	// The launch script is only sent SIGTERM for a termination the control plane granted the
	// checkpoint window, and keeps the command running for the job's stop grace after it.
	return provider.Spec{
		Image:            exp.Image,
		ImageLogin:       login,
		Architecture:     arch,
		Command:          workloadenv.Command(exp, job, group),
		Env:              env,
		AcceleratorType:  acceleratorType,
		AcceleratorCount: accelerators,
		CPUMillis:        cpu.MilliValue(),
		MemoryBytes:      memory.Value(),
		DiskBytes:        storage.Value(),
	}, nil
}

// onNetwork is r's spec on network, where it meets the others at masterAddr over the network's
// interface, as a node of a job that spans several on k8s is told.
func (r rank) onNetwork(job clusterapi.JobSpec, network, masterAddr, iface string) provider.Spec {
	spec := r.spec
	spec.Network = network
	spec.Env = maps.Clone(r.spec.Env)
	for _, v := range workloadenv.Gang(job, r.group, r.offset, r.index, masterAddr) {
		spec.Env[v.Name] = v.Value
	}
	spec.Env[jobenv.NCCLSocketInterface] = iface
	spec.Env[jobenv.GlooSocketInterface] = iface
	return spec
}

// spreads says whether no two of job's nodes may share a host, which they may not unless the job
// says otherwise.
func spreads(job clusterapi.JobSpec) bool {
	return job.Topology == nil || job.Topology.SpreadAcrossHosts == nil || *job.Topology.SpreadAcrossHosts
}
