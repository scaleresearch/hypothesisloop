package cloudexec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
)

// wrapperCommand runs the job's command under hl exec, which the job image carries. stopGrace is
// how long the wrapper lets the command run after SIGTERM.
func wrapperCommand(stopGrace int64, command []string) []string {
	return append([]string{"hl", "exec", "--stop-grace", strconv.FormatInt(stopGrace, 10), "--"}, command...)
}

// isEnvironment says image is one of the environments repository's, named by its digest alone.
func (e *Executor) isEnvironment(image string) bool {
	reference, found := strings.CutPrefix(image, e.environments+"@")
	if e.environments == "" || !found {
		return false
	}
	parsed, err := digest.Parse(reference)
	return err == nil && parsed.Algorithm() == digest.SHA256
}

// buildSpec compiles exp into the instance to rent, without its name.
func (e *Executor) buildSpec(exp *domain.Experiment) (provider.Spec, error) {
	// EffectiveJob, never exp.Job: an admitted experiment's literal quantities and graces are in
	// exp.ResolvedJob.
	spec := exp.EffectiveJob()
	if spec.Nodes() > 1 {
		return provider.Spec{}, &agentexec.Unrunnable{Why: fmt.Sprintf("the job asks for %d nodes, and a job on this cluster is one rented instance", spec.Nodes())}
	}
	// An instance boots the image desired state names: the job image this cluster reports
	// (OnlyImage), or an environment the control plane built on it, which carries the wrapper
	// as well. Any other is an image this cluster does not run.
	if spec.Image != e.jobImage && !e.isEnvironment(spec.Image) {
		return provider.Spec{}, &agentexec.Unrunnable{Why: fmt.Sprintf("the job is to run in image %q, and this cluster runs only %q and the environments in %q by digest", spec.Image, e.jobImage, e.environments)}
	}
	group := spec.NodeGroups()[0]
	for name, quantity := range map[string]string{"cpu": group.CPU, "memory": group.Memory, "storage": group.Storage} {
		if quantity == "" {
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
		return provider.Spec{}, &agentexec.Unrunnable{Why: "the job asks for host_mounts, and a rented instance has no host of the platform's to mount from"}
	}

	command, args := workloadenv.Command(spec, group)
	// The image's own entrypoint cannot be wrapped from outside, so there has to be a command.
	if len(command) == 0 {
		return provider.Spec{}, &agentexec.Unrunnable{Why: "the job has no command, and a job on a rented instance needs one: the image's own entrypoint cannot be run under the platform's wrapper"}
	}
	env, err := workloadenv.Merged(exp, spec, group, e.apiURL, func(url string) string { return url })
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: %w", err)
	}

	// The wrapper is only sent SIGTERM for a termination the control plane granted the checkpoint
	// window, so the window is the one grace compiled into it. A granted window only ever
	// lengthens the ordinary grace.
	grace, checkpointGrace, err := spec.ResolvedGrace()
	if err != nil {
		return provider.Spec{}, fmt.Errorf("cloudexec: experiment %s: %w", exp.ID, err)
	}

	argv := append(append([]string(nil), command...), args...)
	return provider.Spec{
		Image:            spec.Image,
		Command:          wrapperCommand(max(grace, checkpointGrace), argv),
		Env:              env,
		AcceleratorType:  string(exp.AcceleratorType),
		AcceleratorCount: group.AcceleratorCount,
		CPUMillis:        cpu.MilliValue(),
		MemoryBytes:      memory.Value(),
		DiskBytes:        storage.Value(),
	}, nil
}
