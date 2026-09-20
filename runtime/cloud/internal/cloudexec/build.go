package cloudexec

import (
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
)

// wrapperCommand runs the job's command under hl exec, which the job image carries. stopGrace is
// how long the wrapper lets the command run after SIGTERM.
func wrapperCommand(stopGrace int64, command []string) []string {
	return append([]string{"hl", "exec", "--stop-grace", strconv.FormatInt(stopGrace, 10), "--"}, command...)
}

// buildSpec compiles exp into the instance to rent, without its name: the name needs a slot,
// which is chosen against what is rented right now.
func (e *Executor) buildSpec(exp *domain.Experiment) (provider.Spec, error) {
	// EffectiveJob, never exp.Job: an admitted experiment's literal quantities and graces are in
	// exp.ResolvedJob.
	spec := exp.EffectiveJob()
	if spec.Nodes() > 1 {
		return provider.Spec{}, fmt.Errorf("cloudexec: num_nodes=%d requested but a job here is one instance", spec.Nodes())
	}
	group := spec.NodeGroups()[0]
	for name, quantity := range map[string]string{"cpu": group.CPU, "memory": group.Memory, "storage": group.Storage} {
		if quantity == "" || quantity == domain.MaxResourceSentinel {
			return provider.Spec{}, fmt.Errorf("cloudexec: experiment %s has no resolved %s in desired state", exp.ID, name)
		}
	}
	cpu, err := resource.ParseQuantity(group.CPU)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: parse cpu %q: %w", group.CPU, err)
	}
	memory, err := resource.ParseQuantity(group.Memory)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: parse memory %q: %w", group.Memory, err)
	}
	storage, err := resource.ParseQuantity(group.Storage)
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: parse storage %q: %w", group.Storage, err)
	}
	// A job that expects a dataset at a host path must not run without it, and a rented instance
	// has no host of ours to mount from.
	if len(spec.HostMounts) > 0 {
		return provider.Spec{}, fmt.Errorf("cloudexec: experiment %s requests host_mounts, which a rented instance cannot provide", exp.ID)
	}

	command, args := group.Command, group.Args
	if len(command) == 0 {
		command, args = spec.Command, spec.Args
	}
	// The instance runs the command under hl exec, so there has to be one: the image's own
	// entrypoint cannot be wrapped from outside.
	if len(command) == 0 {
		return provider.Spec{}, fmt.Errorf("cloudexec: experiment %s has no command, and a job on a rented instance needs one", exp.ID)
	}

	platform, err := workloadenv.Platform(exp, group, e.apiURL, func(url string) string { return url })
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: %w", err)
	}
	env := workloadenv.Job(spec, group)
	for _, v := range platform {
		if _, set := env[v.Name]; !set {
			env[v.Name] = v.Value
		}
	}

	// The wrapper is only sent SIGTERM for a termination the control plane granted the checkpoint
	// window, so the window is the one grace compiled into it. A granted window only ever
	// lengthens the ordinary grace. Every other stop destroys the instance, see DeleteWorkload.
	grace, checkpointGrace, err := spec.ResolvedGrace()
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: experiment %s: %w", exp.ID, err)
	}

	return provider.Spec{
		Image:            e.jobImage,
		Command:          wrapperCommand(max(grace, checkpointGrace), append(append([]string(nil), command...), args...)),
		Env:              env,
		AcceleratorType:  string(exp.AcceleratorType),
		AcceleratorCount: group.AcceleratorCount,
		CPUMillis:        cpu.MilliValue(),
		MemoryBytes:      memory.Value(),
		DiskBytes:        storage.Value(),
	}, nil
}
