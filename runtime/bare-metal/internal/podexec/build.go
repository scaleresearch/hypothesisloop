package podexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// hugepageMountPoints maps a k8s-style hugepage resource key ("hugepages-1Gi", "hugepages-2Mi")
// to the hugetlbfs mount point the kernel already exposes for that page size on any Linux host
// (confirmed against this node's own /proc/mounts: "hugetlbfs on /dev/hugepages type hugetlbfs
// ...pagesize=2M", "hugetlbfs on /dev/hugepages-1G type hugetlbfs ...pagesize=1024M"). Unlike
// k8s, which reserves+mounts a per-pod quota via the hugepages resource request, a bare node has
// no per-container hugepage accounting to offer — but the pages are still reserved node-wide (an
// operator concern, same as k8s expects the node's boot params to already provide), so passing
// the real mount through via a bind mount is what actually lets tt-metal's UMD driver find its
// host DMA channel, not just accounting metadata to ignore.
var hugepageMountPoints = map[string]string{
	"hugepages-2Mi": "/dev/hugepages",
	"hugepages-1Gi": "/dev/hugepages-1G",
}

// hugepageMountsFor resolves accelerator_pod_resources's hugepage keys to real, existing
// hugetlbfs mount points on this node, logging (not failing) anything it can't resolve — an
// unrecognized size or a node that was never configured with that size reserved is a real gap
// the workload will likely hit itself (as a driver error), not something worth failing job
// admission over.
func hugepageMountsFor(experimentID string, podResources map[string]string) []string {
	var mounts []string
	for key := range podResources {
		mount, known := hugepageMountPoints[key]
		if !known {
			log.Printf("podexec: experiment %s requests accelerator_pod_resources[%s], which this bare node does not recognize as a hugepage size — ignoring", experimentID, key)
			continue
		}
		if _, err := os.Stat(mount); err != nil {
			log.Printf("podexec: experiment %s requests %s but this node has no %s hugetlbfs mount (%v) — ignoring, the workload may fail to initialize its device", experimentID, key, mount, err)
			continue
		}
		mounts = append(mounts, mount)
	}
	return mounts
}

// resolveHostMounts validates JobSpec.HostMounts against this node's actual filesystem —
// unlike hugepageMountsFor (which logs and drops an unresolvable request, since hugepages are a
// soft prerequisite some workloads tolerate missing), a requested host mount that doesn't exist
// on this node fails admission outright: a job silently running without the dataset it expects
// at a given path is a correctness bug that looks like a working job, not a visible failure.
func resolveHostMounts(hostMounts map[string]string) (map[string]string, error) {
	if len(hostMounts) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(hostMounts))
	for containerPath, hostPath := range hostMounts {
		info, err := os.Stat(hostPath)
		if err != nil {
			return nil, fmt.Errorf("podexec: host_mounts[%s]=%q: %w (this node does not have the requested dataset/directory — either it was never populated here, or the job should target a node that has it via node_selector)", containerPath, hostPath, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("podexec: host_mounts[%s]=%q is not a directory", containerPath, hostPath)
		}
		resolved[containerPath] = hostPath
	}
	return resolved, nil
}

// Placement is the bare-node analogue of k8sexec.AcceleratorPlacement: which specific local
// devices satisfy exp's AcceleratorType/AcceleratorCount, resolved once against live inventory
// (see resolvePlacementFor in lifecycle.go) and passed in so BuildContainerSpec stays pure.
type Placement struct {
	// DevicePaths are host device nodes (e.g. "/dev/nvidia0") to pass through with --device.
	// Empty when the experiment requests no accelerator.
	DevicePaths []string
}

// BuildContainerSpec deterministically compiles exp into a podman invocation — the podman
// analogue of k8sexec.BuildJob. Pure: same desired state plus same placement always compiles
// to the same spec, since its output feeds the desired-spec hash reconcile compares against.
//
// The control plane picks which runtime (k8s or bare-metal) a job lands on; the job spec itself
// is written against neither in particular. Per different-backend.md §1 coupling 2,
// accelerator_tolerations/extra_resources only have meaning under a k8s taint/device-plugin
// model — there is nothing on a bare node to enforce them against, so they are logged and
// ignored rather than rejecting a spec that happens to carry them (which would make a job work
// or fail to submit purely based on which runtime it happens to be admitted onto).
// accelerator_pod_resources (hugepages) is different: the *quota* it requests has no bare-node
// enforcement either, but the underlying hugetlbfs mount it names is a real, physical
// prerequisite some workloads (e.g. tt-metal's UMD driver) fail without — so that part is
// honored via a bind mount (see hugepageMountPoints), not silently dropped. NumNodes > 1 is
// still a hard error: distributed placement across multiple bare nodes is deliberately out of
// scope for a single-node runtime (different-backend.md §4.4) and there is no way to honor it
// partially.
func (e *Executor) BuildContainerSpec(exp *domain.Experiment, placement Placement) (containerSpec, error) {
	spec := exp.Job
	if spec.CPU == "" || spec.Memory == "" || spec.Storage == "" || spec.MaxRetries == nil || *spec.MaxRetries < 0 {
		return containerSpec{}, fmt.Errorf("podexec: CPU, memory, storage, and non-negative max_retries are required desired state")
	}
	if spec.Nodes() > 1 {
		return containerSpec{}, fmt.Errorf("podexec: num_nodes=%d requested but this bare-node runtime executes single-node jobs only", spec.Nodes())
	}
	if len(spec.AcceleratorTolerations) > 0 {
		log.Printf("podexec: experiment %s requests accelerator_tolerations, which has no meaning on a bare node — ignoring", exp.ID)
	}
	if len(spec.ExtraResources) > 0 {
		log.Printf("podexec: experiment %s requests extra_resources, which requires a k8s device plugin — ignoring on this bare node", exp.ID)
	}
	hugepageMounts := hugepageMountsFor(exp.ID, spec.AcceleratorPodResources)
	readOnlyMounts, err := resolveHostMounts(spec.HostMounts)
	if err != nil {
		return containerSpec{}, err
	}
	if spec.AcceleratorCount > 0 && len(placement.DevicePaths) != spec.AcceleratorCount {
		return containerSpec{}, fmt.Errorf("podexec: accelerator %q requested %d device(s) but placement resolved %d", exp.AcceleratorType, spec.AcceleratorCount, len(placement.DevicePaths))
	}

	cpuQty, err := resource.ParseQuantity(spec.CPU)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse cpu %q: %w", spec.CPU, err)
	}
	memQty, err := resource.ParseQuantity(spec.Memory)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse memory %q: %w", spec.Memory, err)
	}
	storageQty, err := resource.ParseQuantity(spec.Storage)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse storage %q: %w", spec.Storage, err)
	}
	// The Engine API's NanoCPUs is CPU quota in units of 1e-9 cores; MilliValue is 1e-3 cores,
	// so scale by 1e6 to convert one to the other without losing fractional-core precision.
	nanoCPUs := cpuQty.MilliValue() * 1_000_000
	var shmSizeBytes int64
	if spec.ShmSize != "" {
		shmQty, err := resource.ParseQuantity(spec.ShmSize)
		if err != nil {
			return containerSpec{}, fmt.Errorf("podexec: parse shm_size %q: %w", spec.ShmSize, err)
		}
		shmSizeBytes = shmQty.Value()
	}

	env := map[string]string{
		"HYPOTHESISLOOP_EXPERIMENT_ID":     exp.ID,
		"HYPOTHESISLOOP_AGENT_ID":          exp.AgentID,
		"HYPOTHESISLOOP_PROJECT_ID":        exp.ProjectID,
		"HYPOTHESISLOOP_CODE_REF":          exp.CodeRef,
		"HYPOTHESISLOOP_CONFIG_HASH":       exp.ConfigHash,
		"HYPOTHESISLOOP_DATA_REF":          exp.DataRef,
		"HYPOTHESISLOOP_API_URL":           e.apiURL,
		"HYPOTHESISLOOP_ACCELERATOR_TYPE":  string(exp.AcceleratorType),
		// Per node (spec), never the job total (exp) — see the same variable in the k8s
		// runtime's job_build.go for the failure this caused. Single-node-only today makes the
		// two equal in practice; taking it from the spec is what keeps that an accident rather
		// than something the next multi-node change has to remember.
		"HYPOTHESISLOOP_ACCELERATOR_COUNT": fmt.Sprintf("%d", spec.AcceleratorCount),
		"HYPOTHESISLOOP_DURATION_SECONDS":  fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600)),
		"TRACEPARENT":                      traceparentFromID(exp.ID),
	}
	for k, v := range spec.Env {
		env[k] = v
	}

	cs := containerSpec{
		Name:           containerName(exp.ID),
		Image:          spec.Image,
		Command:        spec.Command,
		Args:           spec.Args,
		Env:            env,
		NanoCPUs:       nanoCPUs,
		MemoryBytes:    memQty.Value(),
		ShmSizeBytes:   shmSizeBytes,
		Devices:        placement.DevicePaths,
		Mounts:         hugepageMounts,
		ReadOnlyMounts: readOnlyMounts,
		Labels: map[string]string{
			LabelManagedBy:       ManagedByValue,
			LabelExperimentID:    exp.ID,
			LabelAgentID:         exp.AgentID,
			LabelCapacityTier:    string(exp.CapacityTier),
			LabelAcceleratorType: string(exp.AcceleratorType),
			LabelAcceleratorCnt:  fmt.Sprintf("%d", spec.AcceleratorCount),
			LabelCPUCores:        fmt.Sprintf("%d", cpuQty.MilliValue()),
			LabelMemoryBytes:     fmt.Sprintf("%d", memQty.Value()),
			LabelStorageBytes:    fmt.Sprintf("%d", storageQty.Value()),
		},
	}

	if e.storageQuotaEnforced {
		cs.StorageOptSize = spec.Storage
	}

	grace := e.defaultTerminationGracePeriodSeconds
	if spec.TerminationGracePeriodSeconds != nil {
		grace = *spec.TerminationGracePeriodSeconds
	}
	if grace > e.maxTerminationGracePeriodSeconds {
		grace = e.maxTerminationGracePeriodSeconds
	}
	cs.Labels[LabelGraceSeconds] = fmt.Sprintf("%d", grace)

	hash, err := hashContainerSpec(cs)
	if err != nil {
		return containerSpec{}, err
	}
	cs.Labels[LabelDesiredSpecHash] = hash
	return cs, nil
}

// hashContainerSpec hashes every field that determines runtime behavior. Go's encoding/json
// sorts map keys alphabetically, so this is deterministic across calls with the same input —
// no manual key sorting needed, unlike k8sexec's env-var slice (which stays order-sensitive,
// hence sorted separately in BuildContainerSpec... actually env is a map here since podman -e
// order is irrelevant to behavior).
func hashContainerSpec(cs containerSpec) (string, error) {
	// Exclude the hash label itself (not yet set) and copy so this pass never mutates cs.
	labels := make(map[string]string, len(cs.Labels))
	for k, v := range cs.Labels {
		labels[k] = v
	}
	sortedDevices := append([]string(nil), cs.Devices...)
	sort.Strings(sortedDevices)
	sortedMounts := append([]string(nil), cs.Mounts...)
	sort.Strings(sortedMounts)
	payload := struct {
		Name           string
		Image          string
		Command        []string
		Args           []string
		Env            map[string]string
		NanoCPUs       int64
		MemoryBytes    int64
		ShmSizeBytes   int64
		StorageOptSize string
		Devices        []string
		Mounts         []string
		ReadOnlyMounts map[string]string
		Labels         map[string]string
	}{cs.Name, cs.Image, cs.Command, cs.Args, cs.Env, cs.NanoCPUs, cs.MemoryBytes, cs.ShmSizeBytes, cs.StorageOptSize, sortedDevices, sortedMounts, cs.ReadOnlyMounts, labels}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("podexec: hash desired spec: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}
