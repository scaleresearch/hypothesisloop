package podexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// containerHostAlias is the podman/docker-assigned DNS name that resolves, from inside a
// container on its own bridge network, to whatever the host's own loopback serves -- the
// non-host-networking way to reach "the control plane / object store this node itself talks to
// as localhost". See dockerHostGateway in container.go for the ExtraHosts entry that backs it.
const containerHostAlias = "host.containers.internal"

// containerReachable rewrites a URL's loopback host (localhost/127.0.0.1) to containerHostAlias
// so a workload container -- which runs on its own network namespace, not the host's -- can
// still reach a control-plane/object-store endpoint this bare node itself resolves as loopback.
// Any other host (a real LAN/public address) is returned unchanged: those are already reachable
// the same way from inside the container as from the host.
func containerReachable(rawURL string) string {
	for _, loopback := range []string{"localhost", "127.0.0.1"} {
		for _, scheme := range []string{"http://", "https://"} {
			prefix := scheme + loopback
			if strings.HasPrefix(rawURL, prefix) {
				return scheme + containerHostAlias + strings.TrimPrefix(rawURL, prefix)
			}
		}
	}
	return rawURL
}

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
	// EffectiveJob, never exp.Job directly: an admitted experiment's literal CPU/memory/storage
	// live in exp.ResolvedJob (see domain.Experiment.ResolvedJob) — exp.Job may still carry the
	// "max" sentinel the agent submitted, and GET must keep returning exactly that.
	spec := exp.EffectiveJob()
	// Nodes() sums a grouped job's replicas, so a heterogeneous job is refused here by exactly the
	// same rule and the same error a num_nodes > 1 job already gets. Groups do not make this
	// runtime multi-node; they make its refusal consistent — a job it cannot run is rejected for
	// the reason it cannot run it, not for the syntax it happened to be written in.
	if spec.Nodes() > 1 {
		return containerSpec{}, fmt.Errorf("podexec: num_nodes=%d requested but this bare-node runtime executes single-node jobs only", spec.Nodes())
	}
	// A single-node job is one node group of one replica, whose shape is the top-level one for an
	// ungrouped job and the sole group's for a grouped one. Read through NodeGroups so this
	// runtime never has to know which form it was handed.
	group := spec.NodeGroups()[0]
	if group.CPU == "" || group.Memory == "" || group.Storage == "" || spec.MaxRetries == nil || *spec.MaxRetries < 0 {
		return containerSpec{}, fmt.Errorf("podexec: CPU, memory, storage, and non-negative max_retries are required desired state")
	}
	// Defense-in-depth: see k8sexec.BuildJobs' identical check. Reaching here with a literal
	// "max" means the scheduler admitted this experiment without resolving it — a control-plane
	// bug, not something this runtime should compile into a nonsense podman resource request.
	if group.CPU == domain.MaxResourceSentinel || group.Memory == domain.MaxResourceSentinel || group.Storage == domain.MaxResourceSentinel {
		return containerSpec{}, fmt.Errorf("podexec: experiment %s reached BuildContainerSpec with an unresolved %q sentinel still in its job spec — this is a control-plane bug, not a placement failure", exp.ID, domain.MaxResourceSentinel)
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
	if group.AcceleratorCount > 0 && len(placement.DevicePaths) != group.AcceleratorCount {
		return containerSpec{}, fmt.Errorf("podexec: accelerator %q requested %d device(s) but placement resolved %d", exp.AcceleratorType, group.AcceleratorCount, len(placement.DevicePaths))
	}

	// A "max" sentinel is always resolved to a plain literal once an experiment is admitted (see
	// scheduler.Loop.resolveClusterLocalResources; EffectiveJob above reads exp.ResolvedJob), so it should
	// never reach here — but resource.ParseQuantity("max") fails with a generic "quantities must
	// match the regular expression" error that names nothing about the real cause, so name it
	// explicitly instead.
	for name, qty := range map[string]string{"cpu": group.CPU, "memory": group.Memory, "storage": group.Storage} {
		if qty == domain.MaxResourceSentinel {
			return containerSpec{}, fmt.Errorf("podexec: job.%s is still %q — it should have been resolved to a literal quantity at submission", name, domain.MaxResourceSentinel)
		}
	}
	cpuQty, err := resource.ParseQuantity(group.CPU)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse cpu %q: %w", group.CPU, err)
	}
	memQty, err := resource.ParseQuantity(group.Memory)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse memory %q: %w", group.Memory, err)
	}
	storageQty, err := resource.ParseQuantity(group.Storage)
	if err != nil {
		return containerSpec{}, fmt.Errorf("podexec: parse storage %q: %w", group.Storage, err)
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

	// Same two variables, same source, as the k8s runtime: durable-data addressing arrives in
	// desired state so a job reads and writes the same prefixes wherever it was placed.
	if exp.Data == nil {
		return containerSpec{}, fmt.Errorf("podexec: experiment %s has no durable-data access in desired state", exp.ID)
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":               exp.Data.AccessKeyID,
		"AWS_ENDPOINT_URL":                containerReachable(exp.Data.Endpoint),
		"AWS_ENDPOINT_URL_S3":             containerReachable(exp.Data.Endpoint),
		"AWS_REGION":                      exp.Data.Region,
		"AWS_SECRET_ACCESS_KEY":           exp.Data.SecretAccessKey,
		"AWS_SESSION_TOKEN":               exp.Data.SessionToken,
		"HYPOTHESISLOOP_DATA_SHARED":      exp.Data.Shared,
		"HYPOTHESISLOOP_DATA_URI":         exp.Data.URI,
		"HYPOTHESISLOOP_EXPERIMENT_ID":    exp.ID,
		"HYPOTHESISLOOP_AGENT_ID":         exp.AgentID,
		"HYPOTHESISLOOP_PROJECT_ID":       exp.ProjectID,
		"HYPOTHESISLOOP_CODE_REF":         exp.CodeRef,
		"HYPOTHESISLOOP_CONFIG_HASH":      exp.ConfigHash,
		"HYPOTHESISLOOP_DATA_REF":         exp.DataRef,
		"HYPOTHESISLOOP_API_URL":          containerReachable(e.apiURL),
		"HYPOTHESISLOOP_ACCELERATOR_TYPE": string(exp.AcceleratorType),
		// Per node (spec), never the job total (exp) — see the same variable in the k8s
		// runtime's job_build.go for the failure this caused. Single-node-only today makes the
		// two equal in practice; taking it from the spec is what keeps that an accident rather
		// than something the next multi-node change has to remember.
		"HYPOTHESISLOOP_ACCELERATOR_COUNT": fmt.Sprintf("%d", group.AcceleratorCount),
		"HYPOTHESISLOOP_DURATION_SECONDS":  fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600)),
		// Always 0 here: this runtime is single-node, and a single-pod job's retries never reach
		// the control-plane gang retry that increments it. Injected anyway so both runtimes hand a
		// workload the same vocabulary, and so it lands in this executor's desired-spec hash for
		// the same delete-and-recreate reason the k8s runtime documents.
		"HYPOTHESISLOOP_ATTEMPT": fmt.Sprintf("%d", exp.AttemptCount),
		"TRACEPARENT":            traceparentFromID(exp.ID),
	}
	// Job-level environment first, then the group's own over it — the same layering the k8s
	// runtime applies. An ungrouped job's synthetic group carries the job-level map, so this is
	// spec.Env exactly as before.
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range group.Env {
		env[k] = v
	}

	// The group's own process when it names one, the job's shared entrypoint otherwise — same
	// rule as the k8s runtime.
	command, args := group.Command, group.Args
	if len(command) == 0 {
		command, args = spec.Command, spec.Args
	}
	cs := containerSpec{
		Name:           containerName(exp.ID),
		Image:          spec.Image,
		Command:        command,
		Args:           args,
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
			LabelAcceleratorCnt:  fmt.Sprintf("%d", group.AcceleratorCount),
			LabelCPUCores:        fmt.Sprintf("%d", cpuQty.MilliValue()),
			LabelMemoryBytes:     fmt.Sprintf("%d", memQty.Value()),
			LabelStorageBytes:    fmt.Sprintf("%d", storageQty.Value()),
			LabelAttempt:         fmt.Sprintf("%d", exp.AttemptCount),
		},
	}

	if e.storageQuotaEnforced {
		cs.StorageOptSize = group.Storage
	}

	grace := e.defaultTerminationGracePeriodSeconds
	if spec.TerminationGracePeriodSeconds != nil {
		grace = *spec.TerminationGracePeriodSeconds
	}
	if grace > e.maxTerminationGracePeriodSeconds {
		grace = e.maxTerminationGracePeriodSeconds
	}
	cs.Labels[LabelGraceSeconds] = fmt.Sprintf("%d", grace)

	// The checkpoint window, capped, recorded separately from the shutdown grace above rather
	// than folded into it: only a policy-class termination gets to spend it, and every other
	// stop of this container -- a drift replacement, an infrastructure failure -- must still
	// take the ordinary grace. DeleteWorkload picks between them from what the control plane
	// granted, and cannot pick a window the job never declared.
	checkpointGrace := int64(0)
	if spec.CheckpointGraceSeconds != nil {
		checkpointGrace = *spec.CheckpointGraceSeconds
		if checkpointGrace > e.maxCheckpointGraceSeconds {
			checkpointGrace = e.maxCheckpointGraceSeconds
		}
	}
	cs.Labels[LabelCheckpointGrace] = fmt.Sprintf("%d", checkpointGrace)

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
	// Exclude the durable-data session: the control plane mints a fresh one on every reconcile
	// pass, and hashing it would recreate every running container every few seconds. The prefixes
	// and endpoint it comes with are desired state and stay in.
	env := make(map[string]string, len(cs.Env))
	for k, v := range cs.Env {
		env[k] = v
	}
	for _, name := range domain.DataCredentialEnvNames {
		delete(env, name)
	}
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
	}{cs.Name, cs.Image, cs.Command, cs.Args, env, cs.NanoCPUs, cs.MemoryBytes, cs.ShmSizeBytes, cs.StorageOptSize, sortedDevices, sortedMounts, cs.ReadOnlyMounts, labels}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("podexec: hash desired spec: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// SupportsMultiNodeJobs is false: this runtime executes a job on the one bare node it runs on.
// Reported to the control plane alongside capacity so a job spanning several nodes is never
// placed here — before, such a job was admitted, held a reservation, and only then failed at
// BuildContainerSpec, which is an admission-time answer given at execution time.
func (e *Executor) SupportsMultiNodeJobs() bool { return false }
