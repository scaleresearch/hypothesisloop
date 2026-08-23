package podexec

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// containerInfo is the subset of a container.Summary this runtime reads. The engine is the
// actual-state record (important.md #4): every call re-derives this from the live daemon,
// nothing is cached between reconcile ticks.
type containerInfo struct {
	ID     string
	Names  []string
	Labels map[string]string
	State  string // "running", "exited", "created", ...
}

func (c containerInfo) name() string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return ""
}

// listManagedContainers returns every container (any state) carrying LabelManagedBy —
// including stopped ones, since a stopped container is itself the "gone" record (see
// different-backend.md §4.6) until it's pruned by reapTerminal.
func (e *Executor) listManagedContainers(ctx context.Context) ([]containerInfo, error) {
	summaries, err := e.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", LabelManagedBy+"="+ManagedByValue),
	})
	if err != nil {
		return nil, fmt.Errorf("podexec: list containers: %w", err)
	}
	out := make([]containerInfo, 0, len(summaries.Items))
	for _, s := range summaries.Items {
		out = append(out, containerInfo{
			ID:     s.ID,
			Names:  trimSlashPrefix(s.Names),
			Labels: s.Labels,
			State:  string(s.State),
		})
	}
	return out, nil
}

// trimSlashPrefix strips the leading "/" the engine API prefixes container names with.
func trimSlashPrefix(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if len(n) > 0 && n[0] == '/' {
			n = n[1:]
		}
		out[i] = n
	}
	return out
}

// exitCode inspects id and returns its exit code — a separate call from listManagedContainers
// because container.Summary (the list response) does not carry it, only container.InspectResponse
// does. Called only for containers already known to be exited, not on every list.
func (e *Executor) exitCode(ctx context.Context, id string) (int, error) {
	resp, err := e.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return 0, fmt.Errorf("podexec: inspect container %s: %w", id, err)
	}
	if resp.Container.State == nil {
		return 0, nil
	}
	return resp.Container.State.ExitCode, nil
}

// containerSpec is the fully-resolved container configuration for one experiment, produced by
// BuildContainerSpec — the podman/docker analogue of a compiled batchv1.Job.
type containerSpec struct {
	Name           string
	Image          string
	Command        []string
	Args           []string
	Env            map[string]string
	NanoCPUs       int64
	MemoryBytes    int64
	ShmSizeBytes   int64
	StorageOptSize string   // storage-opt "size" value, e.g. "10Gi"; empty when not enforced
	Devices        []string // host device paths, mapped straight through to the container
	Mounts         []string // host paths bind-mounted read-write at the same path in the container (e.g. a hugetlbfs mount)
	// ReadOnlyMounts is JobSpec.HostMounts, resolved and validated (see BuildContainerSpec):
	// container path -> host path, mounted read-only. Distinct from Mounts because a host mount
	// generally has a different container path than its host path (unlike hugepages) and must
	// never be writable, since it's shared read-only across concurrent jobs.
	ReadOnlyMounts map[string]string
	Labels         map[string]string
}

// startContainer creates and starts spec detached. Podman/Docker tolerate neither AlreadyExists
// nor NotFound the way k8s Jobs do, so callers (CreateWorkload) check listManagedContainers
// themselves first.
func (e *Executor) startContainer(ctx context.Context, spec containerSpec) error {
	cmd := append(append([]string{}, spec.Command...), spec.Args...)
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	// NVIDIA devices go through DeviceRequests (--gpus), not a raw DeviceMapping, so the
	// nvidia-container-toolkit hook injects the host driver libraries CUDA images need.
	devices := make([]container.DeviceMapping, 0, len(spec.Devices))
	var nvidiaDeviceIDs []string
	for _, d := range spec.Devices {
		if id, ok := strings.CutPrefix(d, "/dev/nvidia"); ok && id != "" && id[0] >= '0' && id[0] <= '9' {
			nvidiaDeviceIDs = append(nvidiaDeviceIDs, id)
			continue
		}
		devices = append(devices, container.DeviceMapping{PathOnHost: d, PathInContainer: d, CgroupPermissions: "rwm"})
	}
	var deviceRequests []container.DeviceRequest
	if len(nvidiaDeviceIDs) > 0 {
		deviceRequests = append(deviceRequests, container.DeviceRequest{
			Driver:       "nvidia",
			DeviceIDs:    nvidiaDeviceIDs,
			Capabilities: [][]string{{"gpu"}},
		})
	}
	binds := make([]string, 0, len(spec.Mounts)+len(spec.ReadOnlyMounts))
	for _, m := range spec.Mounts {
		binds = append(binds, m+":"+m)
	}
	containerPaths := make([]string, 0, len(spec.ReadOnlyMounts))
	for containerPath := range spec.ReadOnlyMounts {
		containerPaths = append(containerPaths, containerPath)
	}
	sort.Strings(containerPaths)
	for _, containerPath := range containerPaths {
		binds = append(binds, spec.ReadOnlyMounts[containerPath]+":"+containerPath+":ro")
	}
	hostConfig := &container.HostConfig{
		ShmSize: spec.ShmSizeBytes,
		Binds:   binds,
		// Without this the engine keeps every byte a training job ever wrote, on the same
		// filesystem whose free space is reported as storage capacity — a long run fills the node
		// and takes every other job on it down. Only the tail is ever read (FetchLogTail), so
		// keeping more than a couple of files serves nothing.
		LogConfig: container.LogConfig{
			Type:   "json-file",
			Config: map[string]string{"max-size": "64m", "max-file": "2"},
		},
		// Bridge networking, not host: the engine masks most of /sys/kernel/mm under host
		// networking (a host-wide-tunable path it hides from containers by default even though
		// it doesn't namespace it) -- tt-metal's UMD driver reads
		// /sys/kernel/mm/hugepages/*/nr_hugepages directly to find its host DMA channel and fails
		// outright without it, and no narrower unmask/security-opt/capability restores just that
		// one path; only Privileged does, and that also drops the Devices cgroup allow-list
		// below, exposing every accelerator on the host instead of just the one this experiment
		// was placed on. Loopback URLs a workload needs (API_URL, the object-store endpoint) are
		// rewritten to containerHostAlias (see containerReachable) and resolved via ExtraHosts.
		ExtraHosts: []string{containerHostAlias + ":host-gateway"},
		Resources: container.Resources{
			NanoCPUs:       spec.NanoCPUs,
			Memory:         spec.MemoryBytes,
			Devices:        devices,
			DeviceRequests: deviceRequests,
		},
	}
	if spec.StorageOptSize != "" {
		hostConfig.StorageOpt = map[string]string{"size": spec.StorageOptSize}
	}

	if err := e.pullImage(ctx, spec.Image); err != nil {
		return err
	}
	result, err := e.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    cmd,
			Env:    env,
			Labels: spec.Labels,
		},
		HostConfig: hostConfig,
	})
	if err != nil {
		return fmt.Errorf("podexec: create container %s: %w", spec.Name, err)
	}
	if _, err := e.docker.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("podexec: start container %s: %w", spec.Name, err)
	}
	return nil
}

// stopContainer sends SIGTERM then SIGKILL after graceSeconds — the engine's own stop-timeout
// semantics, the direct analogue of TerminationGracePeriodSeconds. The container is left in
// place (not removed): a stopped container is the "gone" record (§4.6), pruned separately.
func (e *Executor) stopContainer(ctx context.Context, name string, graceSeconds int64) error {
	timeout := int(graceSeconds)
	_, err := e.docker.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout})
	if err != nil && errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *Executor) removeContainer(ctx context.Context, name string) error {
	_, err := e.docker.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	if err != nil && errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// pullImage fetches image before the container is created. Without it a job whose image is not
// already on the host never gets a container at all — create fails every reconcile, and with no
// container there is nothing for PollPhaseDetail to inspect, so the failure reaches an operator as
// an unexplained stuck-pending job.
//
// Checked against the local store first: a bare image ref with no registry host (e.g. a locally
// built "localhost/hypothesisloop-workload:latest") makes the engine treat "localhost" as an
// actual registry host and dial it over the network -- there is nothing listening there, so that
// pull always fails, even though the image already sits in the local store from the build step
// this ran right after. Skipping the network round-trip when the image is already present is also
// just the IfNotPresent policy every other image in this repo already runs under.
func (e *Executor) pullImage(ctx context.Context, image string) error {
	if _, err := e.docker.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("podexec: inspect image %s: %w", image, err)
	}
	body, err := e.docker.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("podexec: pull image %s: %w", image, err)
	}
	defer body.Close()
	// The engine streams progress and reports a mid-stream failure only in the body, so the pull
	// is not complete until the stream is drained.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("podexec: pull image %s: %w", image, err)
	}
	return nil
}
