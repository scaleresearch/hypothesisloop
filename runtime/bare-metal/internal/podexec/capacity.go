package podexec

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// holds says a container in this engine state still holds what its labels request. Every
// container that exists does, paused, restarting, dead or in a state nobody thought of, except
// one whose command has ended: that one has given back its memory and its devices, and what it
// wrote is counted from the filesystem.
func holds(state string) bool {
	return state != "exited" && state != "stopped"
}

// live says the container's command runs or can still run, so the reconcile pass compares it
// with desired state and stops it when it is no longer wanted. Any other container is only
// reported and, once undesired, removed.
func live(state string) bool {
	switch state {
	case "running", "created", "paused", "restarting":
		return true
	}
	return false
}

// runningRequests sums the resource requests recorded (as labels, at creation time) on every
// managed container that holds them — the bare-node analogue of k8sexec summing pod
// Resources.Requests for pods already assigned to a node. There is no separate bookkeeping
// store: the containers themselves are the record (important.md #4).
type runningRequests struct {
	cpuMilli     int64
	memoryBytes  int64
	storageBytes int64
	accelerators map[string]int64 // "key=value" -> count in use
	// unreadable is set when a container's requests cannot be read. What it holds is then
	// unknown, so nothing of the node is reported free until it is gone: left out, its share read
	// as free while it was held and a second job was booked onto it.
	unreadable bool
}

// left is what of total no container holds, given taken is what the readable ones hold.
func (r runningRequests) left(total, taken int64) int64 {
	if r.unreadable {
		return 0
	}
	return max(total-taken, 0)
}

// A job with an operation in flight is counted from the operation: its requests are taken from
// before its pull starts until its stop returns, whatever state the engine shows meanwhile.
func (e *Executor) sumRunningRequests(ctx context.Context) (runningRequests, error) {
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return runningRequests{}, err
	}
	return sumRequests(ops, containers), nil
}

func sumRequests(ops map[string]operation, containers []containerInfo) runningRequests {
	claims := make([]containerInfo, 0, len(ops)+len(containers))
	for id, op := range ops {
		if op.labels != nil {
			claims = append(claims, containerInfo{Names: []string{containerName(id)}, Labels: op.labels})
		}
	}
	for _, c := range containers {
		if _, busy := ops[c.Labels[LabelExperimentID]]; busy {
			continue
		}
		if !holds(c.State) {
			continue
		}
		claims = append(claims, c)
	}
	out := runningRequests{accelerators: map[string]int64{}}
	for _, c := range claims {
		count, countErr := requestLabel(c, LabelAcceleratorCnt)
		acceleratorType := c.Labels[LabelAcceleratorType]
		var typeErr error
		if count > 0 && acceleratorType == "" {
			typeErr = fmt.Errorf("podexec: container %s holds %d accelerators and names no type in label %s", c.name(), count, LabelAcceleratorType)
		}
		cpuMilli, cpuErr := requestLabel(c, LabelCPUCores)
		memoryBytes, memoryErr := requestLabel(c, LabelMemoryBytes)
		storageBytes, storageErr := requestLabel(c, LabelStorageBytes)
		if err := errors.Join(countErr, typeErr, cpuErr, memoryErr, storageErr); err != nil {
			log.Printf("podexec: this node is reported with nothing free until the container is gone, what it holds cannot be read: %v", err)
			out.unreadable = true
			continue
		}
		if count > 0 {
			out.accelerators[acceleratorType] += count
		}
		out.cpuMilli += cpuMilli
		out.memoryBytes += memoryBytes
		out.storageBytes += storageBytes
	}
	return out
}

// requestLabel parses a resource request recorded on a managed container.
func requestLabel(c containerInfo, key string) (int64, error) {
	v, err := strconv.ParseInt(c.Labels[key], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("podexec: container %s label %s=%q: %w", c.name(), key, c.Labels[key], err)
	}
	return v, nil
}

// GetLiveCPUCapacity reports this node's real, current CPU-core capacity: total logical cores
// minus the recorded request of every managed container that holds it.
func (e *Executor) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	requests, err := e.sumRunningRequests(ctx)
	if err != nil {
		return 0, 0, err
	}
	total = totalCPUCores()
	return float64(requests.left(int64(total*1000), requests.cpuMilli)) / 1000.0, total, nil
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
	return requests.left(total, requests.memoryBytes), total, nil
}

// storageReading is one look at the disk jobs write to.
type storageReading struct{ available, total int64 }

// writtenRefresh is how long a measurement of the containers' writable layers is used. The
// engine walks every file of a layer to size it, 1.7 s for a layer of 300 000 files, so the walk
// is not made on every pass. It is the time the walk itself is allowed.
const writtenRefresh = engineCallTimeout

// writtenBytes is what each container has written, by container id, as of the last walk.
func (e *Executor) writtenBytes(ctx context.Context) (map[string]int64, error) {
	e.mu.Lock()
	written, fresh := e.written, time.Since(e.writtenAt) < writtenRefresh
	e.mu.Unlock()
	if fresh {
		return written, nil
	}
	at := time.Now()
	containers, err := e.listContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	written = make(map[string]int64, len(containers))
	for _, c := range containers {
		written[c.ID] = c.WrittenBytes
	}
	e.mu.Lock()
	e.written, e.writtenAt = written, at
	e.mu.Unlock()
	return written, nil
}

// readStorage reports total as what jobs can be given when none runs: what the filesystem has
// free now plus what this cluster's containers have written, which comes back when they are
// removed. The filesystem's own size is never free, so a job sized between the two was accepted
// and queued for good. Free space and the containers there are come from this reading, and only
// what each has written is as old as writtenRefresh. available is the smaller of two independent
// limits: what no running job has reserved, and what the filesystem physically still has, which
// image layers and exited containers also take from.
func (e *Executor) readStorage(ctx context.Context) (storageReading, error) {
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return storageReading{}, err
	}
	written, err := e.writtenBytes(ctx)
	if err != nil {
		return storageReading{}, err
	}
	free, err := storageFreeBytes(e.storageDir)
	if err != nil {
		return storageReading{}, err
	}
	total := free
	for _, c := range containers {
		total += written[c.ID]
	}
	requests := sumRequests(ops, containers)
	available := min(free, requests.left(total, requests.storageBytes))
	return storageReading{available: available, total: total}, nil
}

// passExecutor answers every storage question of one pass from one reading. Free space moves
// between two readings, and the control plane refuses a report whose available exceeds its total.
type passExecutor struct {
	*Executor
	storage func() (storageReading, error)
}

func (e *Executor) ForPass(ctx context.Context) (agentexec.Executor, error) {
	return e.forPass(ctx), nil
}

func (e *Executor) forPass(ctx context.Context) *passExecutor {
	return &passExecutor{Executor: e, storage: sync.OnceValues(func() (storageReading, error) { return e.readStorage(ctx) })}
}

func (e *Executor) GetLiveStorageCapacity(ctx context.Context) (available, total int64, err error) {
	return e.forPass(ctx).GetLiveStorageCapacity(ctx)
}

func (e *Executor) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	return e.forPass(ctx).GetLiveNodeResourceCapacity(ctx)
}

func (e *Executor) GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	return e.forPass(ctx).GetNodeTotalCapacity(ctx)
}

func (p *passExecutor) GetLiveStorageCapacity(context.Context) (available, total int64, err error) {
	storage, err := p.storage()
	return storage.available, storage.total, err
}

// GetLiveAcceleratorCapacitySnapshot returns aggregate and per-node actual accelerator state.
// There is exactly one node (this one), so byNode/nodeLabels each carry a single entry keyed by
// e.nodeName — the bare-node shape of the same cluster->node->flavor->count contract k8sexec
// reports. Every type the hardware publishes is reported; the control plane keeps the priced ones.
func (e *Executor) GetLiveAcceleratorCapacitySnapshot(ctx context.Context) (available, total map[string]int64, byNode map[string]map[string]int64, nodeLabels map[string]map[string]string, err error) {
	devices, err := e.probe(ctx)
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
			total[label]++
		}
	}
	cores := string(domain.CPUType)
	total[cores] = int64(totalCPUCores())
	// Folded: total's label casing and a running job's requested casing can differ (see
	// domain.AcceleratorType.MatchesLabels); joining on exact match previously undercounted
	// in-use devices, letting a second job double-book an occupied one.
	for label, count := range requests.accelerators {
		inUse[strings.ToLower(label)] += count
	}
	available = map[string]int64{}
	for label, t := range total {
		available[label] = requests.left(t, inUse[strings.ToLower(label)])
	}
	// Every job holds cores, an accelerator job too, so the cores free are those no running job
	// was given, whatever its type.
	available[cores] = requests.left(total[cores]*1000, requests.cpuMilli) / 1000
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

// GetLiveNodeResourceCapacity reports this node's free CPU/memory/storage. There is exactly one
// node, so this is the per-node view of the same numbers the cluster-wide collectors report.
func (p *passExecutor) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	cpuAvail, _, err := p.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, err
	}
	ramAvail, _, err := p.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, err
	}
	storageAvail, _, err := p.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]int64{p.nodeName: {
		domain.NodeResourceCPUMillicores: int64(cpuAvail * 1000),
		domain.NodeResourceMemoryBytes:   ramAvail,
		domain.NodeResourceStorageBytes:  storageAvail,
	}}, nil
}

// GetNodeTotalCapacity reports this node's capacity available to platform-scheduled jobs — the
// same per-node shape GetLiveNodeResourceCapacity returns, but total rather than free. There is
// exactly one node here too.
//
// Cores and memory are the installed total with nothing subtracted: a bare-metal node onboarded
// via the tt-bare-metal-node flow is dedicated to HypothesisLoop and runs no permanently-resident
// non-platform process. Storage is what readStorage finds jobs can be given.
func (p *passExecutor) GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	_, cpuTotal, err := p.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, err
	}
	_, ramTotal, err := p.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, err
	}
	_, storageTotal, err := p.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]map[string]int64{p.nodeName: {
		domain.NodeResourceCPUMillicores: int64(cpuTotal * 1000),
		domain.NodeResourceMemoryBytes:   ramTotal,
		domain.NodeResourceStorageBytes:  storageTotal,
	}}, nil
}

// devicesInUseExcluding returns the set of host device paths (case-folded, matching the
// case-insensitive comparison already used by GetLiveAcceleratorCapacitySnapshot above) actually
// bound to a managed container that holds them (holds) or held by an operation in flight, skipping
// excludeExperimentID's own: counting a job's devices against its own placement would move it to
// a different device every pass, flip its spec hash and recreate it forever.
//
// resolvePlacementFor used to infer "in use" positionally — treating the first N devices (in
// probe order) matching a type as claimed, where N was just a per-type running count. That
// assumes in-use devices are always a prefix of probe order, which doesn't hold once a
// mid-probe-order device is freed while an earlier or later one stays busy: the next placement
// could then hand out an already-claimed device. Inspecting each container's actual
// HostConfig.Devices instead ties "in use" to the specific device path really passed through.
func (e *Executor) devicesInUseExcluding(ctx context.Context, excludeExperimentID string) (map[string]bool, error) {
	ops := e.operations()
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	inUse := map[string]bool{}
	for id, op := range ops {
		if id == excludeExperimentID {
			continue
		}
		for _, d := range op.devices {
			inUse[strings.ToLower(d)] = true
		}
	}
	for _, c := range containers {
		if excludeExperimentID != "" && c.Labels[LabelExperimentID] == excludeExperimentID {
			continue
		}
		if !holds(c.State) {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		if resp.Container.HostConfig == nil {
			continue
		}
		for _, d := range devicesPassed(resp.Container.HostConfig) {
			inUse[strings.ToLower(d)] = true
		}
		for _, req := range resp.Container.HostConfig.DeviceRequests {
			if len(req.DeviceIDs) == 0 {
				// startContainer always names the exact devices. A count-only request would let
				// the engine pick devices we cannot identify, so we could not tell which are taken.
				log.Printf("podexec: container %s requests %d %s devices without naming them; occupancy is unreliable", c.name(), req.Count, req.Driver)
			}
		}
	}
	return inUse, nil
}

// currentDevicesFor returns the device paths the experiment's container was created with, in the
// order they were assigned, or nil if it has no container. An exited container counts: placing
// it afresh would change its spec hash, so reconcile would remove it and run the job again. A job
// still pulling has no container yet and keeps the devices its operation was started with.
func (e *Executor) currentDevicesFor(ctx context.Context, experimentID string) ([]string, error) {
	if op, busy := e.operations()[experimentID]; busy && op.devices != nil {
		return op.devices, nil
	}
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if c.Labels[LabelExperimentID] != experimentID {
			continue
		}
		resp, err := e.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("podexec: inspect container %s: %w", c.ID, err)
		}
		if resp.Container.HostConfig == nil {
			continue
		}
		return devicesPassed(resp.Container.HostConfig), nil
	}
	return nil, nil
}

// resolvePlacementFor picks which free local devices satisfy exp's AcceleratorType/Count —
// the single-node analogue of k8sexec's ResolveAcceleratorPlacement. Placement is a function of
// live device probes, current containers and the operations in flight, recomputed every call.
// Only the reconcile pass calls it, so two jobs are never placed at the same time.
func (e *Executor) resolvePlacementFor(ctx context.Context, exp *domain.Experiment) (Placement, error) {
	// The job's TOTAL, not its top-level count: a grouped job states its accelerators per group
	// and leaves the top-level count at zero, so keying on that resolved no devices and then
	// failed the job in BuildContainerSpec for a placement nobody had been asked to make. This
	// runtime only ever runs a one-node job, so the total is that node's own count. Read from
	// EffectiveJob, as BuildContainerSpec reads the count it checks this placement against.
	count := exp.EffectiveJob().TotalAccelerators()
	if count <= 0 || onCores(exp.AcceleratorType) {
		return Placement{}, nil
	}
	devices, err := e.probe(ctx)
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

	var freeDevices []string
	for _, d := range devices {
		if !hasLabel(d.Labels, string(exp.AcceleratorType)) {
			continue
		}
		if inUseDevices[strings.ToLower(d.DevicePath)] {
			continue // already claimed by a running container
		}
		freeDevices = append(freeDevices, d.DevicePath)
		if int64(len(freeDevices)) == int64(count) {
			return Placement{DevicePaths: freeDevices}, nil
		}
	}
	return Placement{}, fmt.Errorf("podexec: no free devices match accelerator type %q for %d requested", exp.AcceleratorType, count)
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}
