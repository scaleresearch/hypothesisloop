package compute

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
)

// buildSpec compiles exp into the instance to rent, without its name.
func (e *Executor) buildSpec(exp *clusterapi.DesiredJob) (provider.Spec, error) {
	// EffectiveJob, never exp.Job: an admitted experiment's literal quantities and graces are in
	// exp.ResolvedJob.
	spec := exp.EffectiveJob()
	if spec.Nodes() > 1 {
		return provider.Spec{}, &agentexec.Unrunnable{Why: fmt.Sprintf("the job asks for %d nodes, and a job on this cluster is one rented instance", spec.Nodes())}
	}
	group := spec.NodeGroups()[0]
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
	// A job that expects a dataset at a host path must not run without it, and a rented instance
	// has no host of ours to mount from.
	if len(spec.HostMounts) > 0 {
		return provider.Spec{}, &agentexec.Unrunnable{Why: "the job asks for host_mounts, and a rented instance has no host of the platform's to mount from"}
	}

	arch := spec.NodeSelector[clusterapi.NodeArchitectureLabel]
	if arch == "" {
		return provider.Spec{}, fmt.Errorf("cloudcompute: experiment %s names no architecture in its node selector (%s)", exp.ID, clusterapi.NodeArchitectureLabel)
	}

	env, err := workloadenv.Merged(exp, spec, group, e.apiURL, func(url string) string { return url })
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
		Command:          workloadenv.Command(exp, spec, group),
		Env:              env,
		AcceleratorType:  acceleratorType,
		AcceleratorCount: accelerators,
		CPUMillis:        cpu.MilliValue(),
		MemoryBytes:      memory.Value(),
		DiskBytes:        storage.Value(),
	}, nil
}
