package podexec

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// runningRequests sums the resource requests recorded (as labels, at creation time) on every
// currently running/pending managed container — the bare-node analogue of k8sexec summing pod
// Resources.Requests for pods already assigned to a node. There is no separate bookkeeping
// store: the containers themselves are the record (important.md #4).
type runningRequests struct {
	cpuMilli     int64
	memoryBytes  int64
	storageBytes int64
	accelerators map[string]int64 // "key=value" -> count in use
}

func (e *Executor) sumRunningRequests(ctx context.Context) (runningRequests, error) {
	return e.sumRunningRequestsExcluding(ctx, "")
}

// sumRunningRequestsExcluding is sumRunningRequests but skips excludeExperimentID's own
// container(s) — used by resolvePlacementFor so an experiment's already-running container never
// counts as "in use" against its own placement recomputation. Without this, re-resolving
// placement for an already-running job (every reconcile tick) would see its own device claim in
// the running-requests tally and, whenever more than one device of that type exists, pick a
// *different* free device each time — flipping the desired spec hash and looping
// delete-then-recreate forever (see CreateWorkload's drift check).
func (e *Executor) sumRunningRequestsExcluding(ctx context.Context, excludeExperimentID string) (runningRequests, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return runningRequests{}, err
	}
	out := runningRequests{accelerators: map[string]int64{}}
	for _, c := range containers {
		if excludeExperimentID != "" && c.Labels[LabelExperimentID] == excludeExperimentID {
			continue
		}
		if c.State != "running" && c.State != "created" {
			continue
		}
		if v, err := strconv.ParseInt(c.Labels[LabelCPUCores], 10, 64); err == nil {
			out.cpuMilli += v
		}
		if v, err := strconv.ParseInt(c.Labels[LabelMemoryBytes], 10, 64); err == nil {
			out.memoryBytes += v
		}
		if v, err := strconv.ParseInt(c.Labels[LabelStorageBytes], 10, 64); err == nil {
			out.storageBytes += v
		}
		acceleratorType := c.Labels[LabelAcceleratorType]
		count, _ := strconv.ParseInt(c.Labels[LabelAcceleratorCnt], 10, 64)
		if acceleratorType != "" && count > 0 {
			out.accelerators[acceleratorType] += count
		}
	}
	return out, nil
}

// GetLiveCPUCapacity reports this node's real, current CPU-core capacity: total logical cores
// minus every running/pending managed container's recorded request.
func (e *Executor) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	requests, err := e.sumRunningRequests(ctx)
	if err != nil {
		return 0, 0, err
	}
	total = totalCPUCores()
	availMilli := int64(total*1000) - requests.cpuMilli
	if availMilli < 0 {
		availMilli = 0
	}
	return float64(availMilli) / 1000.0, total, nil
}

func (e *Executor) GetLiveRAMCapacity(ctx context.Context) (available, total int64, err error) {
	requests, err := e.sumRunningRequests(ctx)
	if err != nil {
		return 0, 0, err
	}
	total, err = totalMemoryBytes()
	if err != nil {
		return 0, 0, err
	}
	available = total - requests.memoryBytes
	if available < 0 {
		available = 0
	}
	return available, total, nil
}

func (e *Executor) GetLiveStorageCapacity(ctx context.Context) (available, total int64, err error) {
	requests, err := e.sumRunningRequests(ctx)
	if err != nil {
		return 0, 0, err
	}
	total, free, err := scratchCapacityBytes(e.scratchDir)
	if err != nil {
		return 0, 0, err
	}
	// Two independent limits, so the smaller is what is actually available: what nothing has
	// reserved yet, and what the filesystem physically still has. Reporting only the first
	// ignores everything running jobs write — image layers, checkpoints, container logs — so a
	// node with a full disk kept advertising room and admitted jobs that could not write.
	available = total - requests.storageBytes
	if free < available {
		available = free
	}
	if available < 0 {
		available = 0
	}
	return available, total, nil
}

// GetLiveAcceleratorCapacitySnapshot returns aggregate and per-node actual accelerator state.
// There is exactly one node (this one), so byNode/nodeLabels each carry a single entry keyed by
// e.nodeName — the bare-node shape of the same cluster->node->flavor->count contract k8sexec
// reports, restricted (like k8sexec) to the priced catalog.
func (e *Executor) GetLiveAcceleratorCapacitySnapshot(ctx context.Context) (available, total map[string]int64, byNode map[string]map[string]int64, nodeLabels map[string]map[string]string, err error) {
	devices, err := probeAccelerators(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	requests, err := e.sumRunningRequests(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	total = map[string]int64{}
	inUse := map[string]int64{}
	for _, d := range devices {
		for _, label := range d.Labels {
			if !e.pricedAcceleratorTypes[strings.ToLower(label)] {
				continue
			}
			total[label]++
		}
	}
	// Folded: total's label casing and a running job's requested casing can differ (see
	// domain.AcceleratorType.MatchesLabels); joining on exact match previously undercounted
	// in-use devices, letting a second job double-book an occupied one.
	for label, count := range requests.accelerators {
		inUse[strings.ToLower(label)] += count
	}
	available = map[string]int64{}
	for label, t := range total {
		free := t - inUse[strings.ToLower(label)]
		if free < 0 {
			free = 0
		}
		available[label] = free
	}
	byNode = map[string]map[string]int64{e.nodeName: available}
	labels := make(map[string]string, len(e.nodeLabels))
	for k, v := range e.nodeLabels {
		labels[k] = v
	}
	for _, d := range devices {
		for _, label := range d.Labels {
			key, value, ok := splitLabel(label)
			if ok {
				labels[key] = value
			}
		}
	}
	nodeLabels = map[string]map[string]string{e.nodeName: labels}
	return available, total, byNode, nodeLabels, nil
}

func splitLabel(s string) (key, value string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// GetFlavorCapacity reports this node's capacity as a single canonical domain.Footprint under
// its own cluster_name — the workload.Backend contract's admission-facing view. Every job here
// is guaranteed-tier eligible capacity; burst capacity is reported as the same numbers, since a
// single bare node has no separate burst pool the way k8s PriorityClasses model preemption
// (there is nothing here to preempt into — this runtime runs at most one workload's containers
// concurrently within whatever fits).
func (e *Executor) GetFlavorCapacity(ctx context.Context, clusterName string) (guaranteed, burst map[string]domain.Footprint, err error) {
	cpuAvail, _, err := e.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	ramAvail, _, err := e.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	acceleratorAvail, _, _, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	fp := domain.NewFootprint()
	fp.Add(domain.ResourceKey{Kind: domain.ResourceKindCPU}, int64(cpuAvail*1000))
	fp.Add(domain.ResourceKey{Kind: domain.ResourceKindMemory}, ramAvail)
	for acceleratorType, count := range acceleratorAvail {
		fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: acceleratorType}, count)
	}
	guaranteed = map[string]domain.Footprint{clusterName: fp}
	burst = map[string]domain.Footprint{clusterName: fp}
	return guaranteed, burst, nil
}

// GetLiveNodeResourceCapacity reports this node's free CPU/memory/storage. There is exactly one
// node, so this is the per-node view of the same numbers the cluster-wide collectors report.
func (e *Executor) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	cpuAvail, _, err := e.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, err
	}
	ramAvail, _, err := e.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, err
	}
	storageAvail, _, err := e.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]int64{e.nodeName: {
		domain.NodeResourceCPUMillicores: int64(cpuAvail * 1000),
		domain.NodeResourceMemoryBytes:   ramAvail,
		domain.NodeResourceStorageBytes:  storageAvail,
	}}, nil
}

// GetAcceleratorCapacityByNode/GetNodeLabels give the plain cluster->node->... shape
// workload.Backend expects, wrapping GetLiveAcceleratorCapacitySnapshot's single-node result.
func (e *Executor) GetAcceleratorCapacityByNode(ctx context.Context, clusterName string) (map[string]map[string]map[string]int64, error) {
	_, _, byNode, _, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]map[string]int64{clusterName: byNode}, nil
}

func (e *Executor) GetNodeLabels(ctx context.Context, clusterName string) (map[string]map[string]map[string]string, error) {
	_, _, _, nodeLabels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]map[string]string{clusterName: nodeLabels}, nil
}

// devicesInUseExcluding returns the set of host device paths (case-folded, matching the
// case-insensitive comparison already used by GetLiveAcceleratorCapacitySnapshot above) actually
// bound to a running/pending managed container, skipping excludeExperimentID's own container(s)
// for the same reason sumRunningRequestsExcluding does.
//
// resolvePlacementFor used to infer "in use" positionally — treating the first N devices (in
// probe order) matching a type as claimed, where N was just a per-type running count. That
// assumes in-use devices are always a prefix of probe order, which doesn't hold once a
// mid-probe-order device is freed while an earlier or later one stays busy: the next placement
// could then hand out an already-claimed device. Inspecting each container's actual
// HostConfig.Devices instead ties "in use" to the specific device path really passed through.
func (e *Executor) devicesInUseExcluding(ctx context.Context, excludeExperimentID string) (map[string]bool, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	inUse := map[string]bool{}
	for _, c := range containers {
		if excludeExperimentID != "" && c.Labels[LabelExperimentID] == excludeExperimentID {
			continue
		}
		if c.State != "running" && c.State != "created" {
			continue
		}
		if c.Labels[LabelAcceleratorType] == "" {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		if resp.Container.HostConfig == nil {
			continue
		}
		for _, d := range resp.Container.HostConfig.Devices {
			inUse[strings.ToLower(d.PathOnHost)] = true
		}
		// NVIDIA devices are never in HostConfig.Devices: startContainer passes them through
		// DeviceRequests so the nvidia-container-toolkit hook runs. Reading only Devices meant
		// every NVIDIA GPU always looked free, and placement handed the same chip to job after
		// job while the rest of the node sat idle.
		for _, req := range resp.Container.HostConfig.DeviceRequests {
			if req.Driver != "nvidia" {
				continue
			}
			if len(req.DeviceIDs) == 0 {
				// startContainer always names the exact devices. A count-only request would let
				// the runtime pick GPUs we cannot identify, so we could not tell which are taken.
				log.Printf("podexec: container %s requests %d nvidia devices without naming them; occupancy is unreliable", c.name(), req.Count)
				continue
			}
			for _, id := range req.DeviceIDs {
				inUse[strings.ToLower(nvidiaDevicePath(id))] = true
			}
		}
	}
	return inUse, nil
}

// nvidiaDevicePath reverses startContainer's split of a device path into a DeviceRequest ID.
func nvidiaDevicePath(deviceID string) string {
	return "/dev/nvidia" + deviceID
}

// currentDevicesFor returns the device paths the experiment's live container already holds, in
// the order they were assigned, or nil if it has no live container.
func (e *Executor) currentDevicesFor(ctx context.Context, experimentID string) ([]string, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		if c.State != "running" && c.State != "created" {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		if resp.Container.HostConfig == nil {
			continue
		}
		var devices []string
		for _, d := range resp.Container.HostConfig.Devices {
			devices = append(devices, d.PathOnHost)
		}
		for _, req := range resp.Container.HostConfig.DeviceRequests {
			if req.Driver != "nvidia" {
				continue
			}
			for _, id := range req.DeviceIDs {
				devices = append(devices, nvidiaDevicePath(id))
			}
		}
		return devices, nil
	}
	return nil, nil
}

// resolvePlacementFor picks which free local devices satisfy exp's AcceleratorType/Count —
// the single-node analogue of k8sexec's ResolveAcceleratorPlacement. Placement is a pure
// function of live device probes plus current container labels, recomputed every call: no
// reservation ledger (different-backend.md §4.4).
func (e *Executor) resolvePlacementFor(ctx context.Context, exp *domain.Experiment) (Placement, error) {
	// The job's TOTAL, not its top-level count: a grouped job states its accelerators per group
	// and leaves the top-level count at zero, so keying on that resolved no devices and then
	// failed the job in BuildContainerSpec for a placement nobody had been asked to make. This
	// runtime only ever runs a one-node job, so the total is that node's own count.
	count := exp.Job.TotalAccelerators()
	if count <= 0 {
		return Placement{}, nil
	}
	devices, err := probeAccelerators(ctx)
	if err != nil {
		return Placement{}, err
	}
	// A job that already has a container keeps the devices it is running on. Re-resolving
	// first-free would move a healthy job onto a different chip the moment a neighbour finished,
	// which changes its spec hash, makes reconcile drift-delete it, and terminalizes real work.
	if current, err := e.currentDevicesFor(ctx, exp.ID); err != nil {
		return Placement{}, err
	} else if int64(len(current)) == int64(count) {
		return Placement{DevicePaths: current}, nil
	}

	inUseDevices, err := e.devicesInUseExcluding(ctx, exp.ID)
	if err != nil {
		return Placement{}, err
	}

	candidateTypes := append([]domain.AcceleratorType{exp.AcceleratorType}, exp.Job.AcceptableAcceleratorTypes...)
	for _, candidate := range candidateTypes {
		if candidate == "" {
			continue
		}
		var freeDevices []string
		for _, d := range devices {
			if !hasLabel(d.Labels, string(candidate)) {
				continue
			}
			if inUseDevices[strings.ToLower(d.DevicePath)] {
				continue // already claimed by a running container
			}
			freeDevices = append(freeDevices, d.DevicePath)
			if int64(len(freeDevices)) == int64(count) {
				break
			}
		}
		if int64(len(freeDevices)) == int64(count) {
			return Placement{DevicePaths: freeDevices}, nil
		}
	}
	return Placement{}, fmt.Errorf("podexec: no free devices match accelerator type %q (or acceptable alternatives) for %d requested", exp.AcceleratorType, count)
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}
