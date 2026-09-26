package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	gi           = int64(1) << 30
	rented       = "rented"
	h100LabelKey = "nvidia.com/gpu.product"
)

// slotFixture is what a runtime that rents a machine per job reports: every machine it may rent
// is a node carrying domain.NodeSlotLabel, at its whole size while free and at nothing once taken.
type slotFixture struct {
	accelerators map[string]map[string]int64
	free         map[string]map[string]int64
	sizes        map[string]map[string]int64
	labels       map[string]map[string]string
}

func size(cpuMillis, memoryGi, storageGi int64) map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: cpuMillis, domain.NodeResourceMemoryBytes: memoryGi * gi, domain.NodeResourceStorageBytes: storageGi * gi,
	}
}

func newSlots() *slotFixture {
	return &slotFixture{accelerators: map[string]map[string]int64{}, free: map[string]map[string]int64{}, sizes: map[string]map[string]int64{}, labels: map[string]map[string]string{}}
}

func (c *slotFixture) add(node string, accelerators int64, whole map[string]int64) *slotFixture {
	c.sizes[node] = whole
	c.free[node] = cloneNodeCapacity(map[string]map[string]int64{node: whole})[node]
	c.labels[node] = map[string]string{domain.NodeSlotLabel: node}
	if accelerators > 0 {
		c.accelerators[node] = map[string]int64{h100: accelerators}
		c.labels[node][domain.NodeSlotAcceleratorsLabel] = fmt.Sprint(accelerators)
		c.labels[node][h100LabelKey] = "NVIDIA-H100-80GB-HBM3"
	}
	return c
}

// take is what the runtime reports once an instance is in the slot.
func (c *slotFixture) take(node string) *slotFixture {
	holdSlot(c.accelerators, c.free, c.labels, node)
	return c
}

// twoH100AndACPU is two 8-cpu slots with one H100 each and one 4-cpu slot without.
func twoH100AndACPU() *slotFixture {
	return newSlots().add("h100-0", 1, size(8000, 32, 100)).add("h100-1", 1, size(8000, 32, 100)).add("cpu-0", 0, size(4000, 16, 50))
}

// capacity is the cluster-level view of what is free.
func (c *slotFixture) capacity() domain.Footprint {
	out := domain.NewFootprint()
	for node, free := range c.free {
		out.Add(domain.ResourceKey{Kind: domain.ResourceKindCPU}, free[domain.NodeResourceCPUMillicores])
		out.Add(domain.ResourceKey{Kind: domain.ResourceKindMemory}, free[domain.NodeResourceMemoryBytes])
		out.Add(domain.ResourceKey{Kind: domain.ResourceKindStorage}, free[domain.NodeResourceStorageBytes])
		for flavor, count := range c.accelerators[node] {
			out.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(flavor)}, count)
		}
	}
	return out
}

func cpuJob(id, cpu string) *domain.Experiment {
	return &domain.Experiment{ID: id, AgentID: "agent-1", CapacityTier: domain.CapacityBurst, EstimatedDurationHours: 1,
		Job: domain.JobSpec{Image: "job", CPU: cpu, Memory: "1Gi", Storage: "1Gi"}}
}

func h100Job(id, cpu string) *domain.Experiment {
	exp := cpuJob(id, cpu)
	exp.AcceleratorType, exp.AcceleratorCount = h100, 1
	exp.Job.AcceleratorType, exp.Job.AcceleratorCount = h100, 1
	return exp
}

// sized is exp as the tick resolves it on the cluster.
func sized(t *testing.T, c *slotFixture, exp *domain.Experiment) *domain.Experiment {
	t.Helper()
	resolved, misfit := resolveOntoSlot(exp, rented, c.sizes, c.free, c.labels)
	if misfit != "" {
		t.Fatalf("%s: %s", exp.ID, misfit)
	}
	exp.ResolvedJob = resolved
	return exp
}

func pinnedTo(exp *domain.Experiment, slot string) *domain.Experiment {
	resolved := exp.EffectiveJob()
	resolved.NodeSelector = map[string]string{domain.NodeSlotLabel: slot}
	exp.ResolvedJob = &resolved
	return exp
}

func slotOf(job domain.JobSpec) string { return job.NodeSelector[domain.NodeSlotLabel] }

// A job on a cluster of slots holds a whole slot, so that is what it is resolved to, whatever it
// asked for. Everything that counts what a job holds then counts the slot.
func TestAJobIsSizedToTheSlotItHolds(t *testing.T) {
	c := twoH100AndACPU()
	small := sized(t, c, cpuJob("small", "2"))
	if job := small.EffectiveJob(); job.CPU != "4" || job.Memory != "16Gi" || job.Storage != "50Gi" {
		t.Fatalf("a 2-cpu job resolved to %s cpu, %s, %s, want the cpu slot's 4, 16Gi, 50Gi", job.CPU, job.Memory, job.Storage)
	}
	if small.Job.CPU != "2" || small.Job.Memory != "1Gi" {
		t.Fatalf("the submitted job was rewritten: %+v", small.Job)
	}
	whole := domain.Footprint{
		{Kind: domain.ResourceKindCPU}: 4000, {Kind: domain.ResourceKindMemory}: 16 * gi, {Kind: domain.ResourceKindStorage}: 50 * gi,
	}
	if got := small.Footprint(); !reflect.DeepEqual(got, whole) {
		t.Fatalf("footprint = %v, want the slot's %v", got, whole)
	}

	// "max" is the slot's size like any other request.
	greedy := h100Job("greedy", domain.MaxResourceSentinel)
	greedy.Job.Memory = domain.MaxResourceSentinel
	if job := sized(t, c, greedy).EffectiveJob(); job.CPU != "8" || job.Memory != "32Gi" || job.Storage != "100Gi" || jobNeedsMaxResolution(job) {
		t.Fatalf("a \"max\" job resolved to %s cpu, %s, %s", job.CPU, job.Memory, job.Storage)
	}
	// A grouped job of one node is resolved on its group.
	grouped := h100Job("grouped", "")
	grouped.Job = domain.JobSpec{Image: "registry.test/job:1", Groups: []domain.JobGroup{{Name: "only", Replicas: 1, CPU: "2", AcceleratorCount: 1, AcceleratorType: h100}}}
	if job := sized(t, c, grouped).EffectiveJob(); job.Groups[0].CPU != "8" || job.Groups[0].Memory != "32Gi" || grouped.Job.Groups[0].CPU != "2" {
		t.Fatalf("grouped job resolved to %+v, submitted %+v", job.Groups[0], grouped.Job.Groups[0])
	}
}

func TestOnlyASlotOfTheJobsOwnShapeTakesIt(t *testing.T) {
	c := twoH100AndACPU()
	otherType := h100Job("other-type", "2")
	otherType.AcceleratorType = "nvidia.com/gpu.product=NVIDIA-L40"
	twoDevices := h100Job("two-devices", "2")
	twoDevices.AcceleratorCount, twoDevices.Job.AcceleratorCount = 2, 2
	twoNodes := cpuJob("two-nodes", "2")
	twoNodes.Job.NumNodes = 2
	elsewhere := cpuJob("elsewhere", "2")
	elsewhere.Job.NodeSelector = map[string]string{"zone": "b"}
	for _, exp := range []*domain.Experiment{
		// Larger than the only slot without accelerators. The H100 slots are large enough and
		// are not its to take.
		cpuJob("six-cpu", "6"),
		h100Job("sixteen-cpu", "16"),
		otherType, twoDevices, twoNodes, elsewhere,
	} {
		if resolved, misfit := resolveOntoSlot(exp, rented, c.sizes, c.free, c.labels); misfit == "" || !strings.HasPrefix(misfit, domain.NotAdmittedCapacityUnavailable) {
			t.Errorf("%s: resolved to %+v, misfit %q", exp.ID, resolved, misfit)
		}
	}
	// The type is the one the planner would match: the value folds case, as node labels do.
	lowercase := h100Job("lowercase", "2")
	lowercase.AcceleratorType = domain.AcceleratorType(strings.ToLower(h100))
	if _, misfit := resolveOntoSlot(lowercase, rented, c.sizes, c.free, c.labels); misfit != "" {
		t.Errorf("a type in another casing: %s", misfit)
	}
}

// A job without accelerators waits for a slot without them. With the only such slot taken and
// both H100 slots free, it is still sized to the taken one, which nothing places it on.
func TestAJobWithoutAcceleratorsNeverHoldsASlotThatHasThem(t *testing.T) {
	c := twoH100AndACPU().take("cpu-0")
	waiting := sized(t, c, cpuJob("waiting", "2"))
	if waiting.EffectiveJob().CPU != "4" {
		t.Fatalf("resolved to %s cpu", waiting.EffectiveJob().CPU)
	}
	if topologyFits(c.accelerators, c.free, c.labels, waiting) {
		t.Fatal("a job without accelerators was placed with only H100 slots free")
	}
	// Sized like an H100 slot it still is not placed on one.
	asLarge := cpuJob("as-large", "8")
	resolved := asLarge.Job
	resolved.CPU, resolved.Memory, resolved.Storage = "8", "32Gi", "100Gi"
	asLarge.ResolvedJob = &resolved
	if topologyFits(c.accelerators, c.free, c.labels, asLarge) {
		t.Fatal("a job without accelerators was placed on a slot of its size that has one")
	}
}

// A free slot is taken before a taken one, and of several the smallest: a job that has to wait or
// preempt is sized to the least it needs.
func TestTheSlotAJobIsSizedToIsAFreeOneAndTheSmallest(t *testing.T) {
	c := newSlots().add("small-0", 0, size(4000, 16, 50)).add("large-0", 0, size(16000, 64, 200))
	if cpu := sized(t, c, cpuJob("a", "2")).EffectiveJob().CPU; cpu != "4" {
		t.Errorf("both free: sized to %s cpu, want the small slot's 4", cpu)
	}
	if cpu := sized(t, c, cpuJob("b", "8")).EffectiveJob().CPU; cpu != "16" {
		t.Errorf("an 8-cpu job: sized to %s cpu, want the only slot that holds it", cpu)
	}
	c.take("small-0")
	if cpu := sized(t, c, cpuJob("c", "2")).EffectiveJob().CPU; cpu != "16" {
		t.Errorf("small taken, large free: sized to %s cpu, want the free slot's 16", cpu)
	}
	c.take("large-0")
	if cpu := sized(t, c, cpuJob("d", "2")).EffectiveJob().CPU; cpu != "4" {
		t.Errorf("both taken: sized to %s cpu, want the smallest that holds it", cpu)
	}
}

// The defect this closes: four 2-cpu jobs fit one 8-cpu node by the sum of their requests, so one
// tick admitted all four onto one slot of a runtime that holds one job per slot.
func TestASlotIsFullOnceOneJobIsOnIt(t *testing.T) {
	c := newSlots().add("only-0", 0, size(8000, 32, 100))
	admitted := 0
	for i := 0; i < 4; i++ {
		probe, ok := onSlots(cpuJob(fmt.Sprintf("exp-%d", i), "2"), rented, c.free, c.labels)
		if ok && reservePlacement(c.accelerators, c.free, c.labels, probe) {
			admitted++
		}
	}
	if admitted != 1 || c.free["only-0"][domain.NodeResourceCPUMillicores] != 0 || c.free["only-0"][domain.NodeResourceStorageBytes] != 0 {
		t.Fatalf("%d jobs placed on one slot, left free %v", admitted, c.free["only-0"])
	}
	// A job that was not sized to the slot does not go on it at all.
	fresh := newSlots().add("only-0", 0, size(8000, 32, 100))
	if reservePlacement(fresh.accelerators, fresh.free, fresh.labels, cpuJob("unsized", "2")) {
		t.Fatal("a 2-cpu job was placed on an 8-cpu slot as a 2-cpu job")
	}
}

// The same node without the slot label is an ordinary node and packs by request, as before.
func TestANodeThatIsNotASlotPacksByRequest(t *testing.T) {
	c := newSlots().add("node-a", 0, size(8000, 32, 100))
	delete(c.labels["node-a"], domain.NodeSlotLabel)
	for i := 0; i < 4; i++ {
		exp := cpuJob(fmt.Sprintf("exp-%d", i), "2")
		probe, ok := onSlots(exp, "cluster-a", c.free, c.labels)
		if !ok || probe != exp || !reservePlacement(c.accelerators, c.free, c.labels, probe) {
			t.Fatalf("job %d was not placed on an ordinary 8-cpu node", i)
		}
	}
	if left := c.free["node-a"][domain.NodeResourceCPUMillicores]; left != 0 {
		t.Fatalf("%d millicores left", left)
	}
}

// Admission writes the slot into the resolved job, and only there: the runtime rents that slot
// and the submitted job still reads as it was written.
func TestTheClaimPinsTheJobToTheSlotItWasPlacedOn(t *testing.T) {
	c := twoH100AndACPU()
	for node := range c.labels {
		c.labels[node]["zone"] = "a"
	}
	candidate := h100Job("exp-1", "6")
	candidate.Job.NodeSelector = map[string]string{"zone": "a"}
	sized(t, c, candidate)

	if !desiredPlacementFits(c.accelerators, c.free, c.labels, nil, candidate, candidate.ResolvedJob) {
		t.Fatal("a 6-cpu H100 job was refused with two free H100 slots")
	}
	resolved := candidate.ResolvedJob
	if slot := slotOf(*resolved); slot != "h100-0" && slot != "h100-1" {
		t.Fatalf("resolved selector = %v, want one of the H100 slots", resolved.NodeSelector)
	}
	if resolved.NodeSelector["zone"] != "a" || !reflect.DeepEqual(candidate.Job.NodeSelector, map[string]string{"zone": "a"}) {
		t.Fatalf("resolved %v, submitted %v", resolved.NodeSelector, candidate.Job.NodeSelector)
	}
	if c.free["h100-0"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatal("proving the fit consumed the caller's capacity")
	}
}

// An ordinary node decides nothing for the runtime, which stays free to place the job.
func TestAJobOnAnOrdinaryNodeIsNotPinned(t *testing.T) {
	c := newSlots().add("node-a", 0, size(8000, 32, 100))
	delete(c.labels["node-a"], domain.NodeSlotLabel)
	candidate := cpuJob("exp-1", "2")
	resolved := candidate.Job
	if !desiredPlacementFits(c.accelerators, c.free, c.labels, nil, candidate, &resolved) || resolved.NodeSelector != nil {
		t.Fatalf("resolved selector = %v", resolved.NodeSelector)
	}
}

// A job admitted a moment ago holds its slot before the runtime has rented it, and keeps holding
// it, without being counted twice, once the runtime reports the slot taken.
func TestAPinnedDesiredJobHoldsItsSlotBeforeAndAfterItIsRented(t *testing.T) {
	for name, isRented := range map[string]bool{"not yet rented": false, "rented and reported taken": true} {
		t.Run(name, func(t *testing.T) {
			c := twoH100AndACPU()
			desired := []*domain.Experiment{pinnedTo(sized(t, c, h100Job("earlier", "6")), "h100-0")}
			if isRented {
				c.take("h100-0")
			}

			second := sized(t, c, h100Job("second", "6"))
			if !desiredPlacementFits(c.accelerators, c.free, c.labels, desired, second, second.ResolvedJob) {
				t.Fatal("the second job was refused with a slot free")
			}
			if slot := slotOf(*second.ResolvedJob); slot != "h100-1" {
				t.Fatalf("second job pinned to %q, want the slot the earlier job does not hold", slot)
			}

			desired = append(desired, second)
			third := sized(t, c, h100Job("third", "6"))
			if desiredPlacementFits(c.accelerators, c.free, c.labels, desired, third, third.ResolvedJob) {
				t.Fatalf("a third H100 job was admitted with both H100 slots held (pinned to %q)", slotOf(*third.ResolvedJob))
			}
		})
	}
}

// The tick reserves an admitted job against the pass's own view of the cluster. A pinned job
// takes its slot there, by the ordinary subtraction of what it holds, and no other.
func TestReservingAPinnedJobTakesItsSlot(t *testing.T) {
	c := twoH100AndACPU()
	if !reservePlacement(c.accelerators, c.free, c.labels, pinnedTo(sized(t, c, h100Job("exp-1", "1")), "h100-1")) {
		t.Fatal("a pinned job was refused on its free slot")
	}
	if c.accelerators["h100-1"][h100] != 0 || c.free["h100-1"][domain.NodeResourceMemoryBytes] != 0 || c.free["h100-0"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatalf("after reserving: %v %v", c.accelerators, c.free)
	}
	if reservePlacement(c.accelerators, c.free, c.labels, pinnedTo(sized(t, twoH100AndACPU(), h100Job("exp-2", "1")), "h100-1")) {
		t.Fatal("a second job was placed on a slot that is taken")
	}
}

// pinningStore runs the claim's capacity check the way the real store does, inside the claim and
// before the resolved job is written, and keeps what would have been written.
type pinningStore struct {
	LoopStore
	desired []*domain.Experiment
	written domain.JobSpec
}

func (s *pinningStore) ClaimSubmitted(ctx context.Context, _, _ string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	ok, err := capacityAvailable(ctx, s.desired)
	if err != nil || !ok {
		return false, err
	}
	s.written = *resolvedJob
	return true, nil
}

// Through the real submit path: the slot reaches the resolved job the store writes, which is the
// desired state the runtime is handed, and the experiment the tick goes on to reserve.
func TestSubmittingToASlotClusterWritesTheSlotIntoDesiredState(t *testing.T) {
	c := twoH100AndACPU()
	exp := sized(t, c, h100Job("exp-1", "6"))
	live := &liveWorkload{
		guaranteed: map[string]domain.Footprint{rented: c.capacity()},
		burst:      map[string]domain.Footprint{rented: domain.NewFootprint()},
		nodeAccel:  map[string]map[string]map[string]int64{rented: c.accelerators},
		nodeRes:    map[string]map[string]map[string]int64{rented: c.free},
		nodeLabels: map[string]map[string]map[string]string{rented: c.labels},
	}
	store := &pinningStore{desired: []*domain.Experiment{pinnedTo(sized(t, c, h100Job("earlier", "6")), "h100-0")}}
	l := NewLoop(store, submitQuota{}, live, testGraceLimits, zap.NewNop())

	if err := l.submitJob(context.Background(), exp, rented, ""); err != nil {
		t.Fatal(err)
	}
	if slot := slotOf(store.written); slot != "h100-1" || store.written.CPU != "8" {
		t.Fatalf("resolved job written with selector %v and %s cpu, want slot h100-1 and its 8", store.written.NodeSelector, store.written.CPU)
	}
	if slotOf(exp.EffectiveJob()) != "h100-1" || exp.Job.NodeSelector != nil || exp.Job.CPU != "6" || !live.created {
		t.Fatalf("effective %v, submitted %+v, created %v", exp.EffectiveJob().NodeSelector, exp.Job, live.created)
	}
}

// slotStore is the control plane's rows for a tick over the rented cluster. ClaimSubmitted runs
// the capacity check the way the real store does, against the rows already SUBMITTED there, and
// keeps what would have been written.
type slotStore struct {
	LoopStore
	queued, running, submitted []*domain.Experiment
	written                    map[string]domain.JobSpec
	reasons                    map[string]string
	requeued                   []string
}

func (s *slotStore) ListQueuedExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.queued, nil
}
func (s *slotStore) ListRunningExperiments(context.Context) ([]*domain.Experiment, error) {
	return s.running, nil
}
func (s *slotStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	return &domain.PlatformExperiment{ID: id}, nil
}
func (s *slotStore) IsAgentCut(context.Context, string, string) (bool, error) { return false, nil }
func (s *slotStore) HasUnsummarizedCompleted(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *slotStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return map[string]domain.Cluster{rented: {Name: rented}}, nil
}
func (s *slotStore) UpdateNotAdmittedReason(_ context.Context, id, reason string) error {
	if s.reasons == nil {
		s.reasons = map[string]string{}
	}
	s.reasons[id] = reason
	return nil
}
func (s *slotStore) ClaimSubmitted(ctx context.Context, id, _ string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	ok, err := capacityAvailable(ctx, s.submitted)
	if err != nil || !ok {
		return false, err
	}
	if s.written == nil {
		s.written = map[string]domain.JobSpec{}
	}
	s.written[id] = *resolvedJob
	row := &domain.Experiment{ID: id, ClusterName: rented, Status: domain.StatusSubmitted, ResolvedJob: resolvedJob}
	s.submitted = append(s.submitted, row)
	return true, nil
}
func (s *slotStore) RequeuePreempted(_ context.Context, id string, _, _ float64) (bool, error) {
	s.requeued = append(s.requeued, id)
	return true, nil
}

type slotQuota struct{ LoopQuotaStore }

func (slotQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{}, nil
}
func (slotQuota) ReserveAdmittedFlavor(context.Context, string, domain.AcceleratorType, float64, int) error {
	return nil
}

// slotWorkload reports the fixture as the one connected cluster. desiredFree is what the queue
// backend derives: the cluster's whole size minus the footprint of every desired job.
type slotWorkload struct {
	LoopWorkloadClient
	c       *slotFixture
	desired []*domain.Experiment
	created []string
}

func (w *slotWorkload) whole() domain.Footprint {
	all := newSlots()
	for node, whole := range w.c.sizes {
		all.add(node, int64(slotAccelerators(w.c.labels[node])), whole)
	}
	return all.capacity()
}
func (w *slotWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	desiredFree := w.whole()
	for _, exp := range w.desired {
		desiredFree = desiredFree.Sub(exp.Footprint())
	}
	return map[string]domain.Footprint{rented: w.c.capacity()}, map[string]domain.Footprint{rented: desiredFree}, nil
}
func (w *slotWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{rented: w.whole()}, nil
}
func (w *slotWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{rented: cloneNodeCapacity(w.c.accelerators)}, nil
}
func (w *slotWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{rented: cloneNodeCapacity(w.c.free)}, nil
}
func (w *slotWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{rented: cloneNodeCapacity(w.c.sizes)}, nil
}
func (w *slotWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{rented: w.c.labels}, nil
}
func (w *slotWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{rented: false}, nil
}
func (w *slotWorkload) CreateWorkload(_ context.Context, exp *domain.Experiment) error {
	w.created = append(w.created, exp.ID)
	return nil
}

func slotLoop(store *slotStore, c *slotFixture, observed ObservedState) (*Loop, *slotWorkload) {
	workload := &slotWorkload{c: c, desired: append(append([]*domain.Experiment(nil), store.running...), store.submitted...)}
	l := NewLoop(store, slotQuota{}, workload, testGraceLimits, zap.NewNop())
	l.evictor = noopEvictor{}
	l.disbalanceTolerance = testDisbalanceTolerance
	l.reprioritizer = noopReprioritizer{}
	l.observed = observed
	l.settler = &recordingSettler{}
	return l, workload
}

// One whole tick over a cluster of slots. Four 2-cpu jobs and three H100 jobs are queued against
// one slot without accelerators and two with one H100 each: one job per slot is admitted, each
// sized to its slot and pinned to it, no two to the same slot, and the rest wait.
func TestATickAdmitsOneJobPerSlotSizedAndPinned(t *testing.T) {
	c := twoH100AndACPU()
	store := &slotStore{}
	for i := 0; i < 4; i++ {
		store.queued = append(store.queued, cpuJob(fmt.Sprintf("cpu-job-%d", i), "2"))
	}
	for i := 0; i < 3; i++ {
		store.queued = append(store.queued, h100Job(fmt.Sprintf("h100-job-%d", i), "2"))
	}
	l, workload := slotLoop(store, c, fakeObserved{})

	if err := l.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots := map[string]string{}
	for id, job := range store.written {
		if other, taken := slots[slotOf(job)]; taken || slotOf(job) == "" {
			t.Fatalf("%s written with slot %q, which %s holds", id, slotOf(job), other)
		}
		slots[slotOf(job)] = id
		whole := c.sizes[slotOf(job)]
		if job.CPU != canonicalQuantityString(domain.NodeResourceCPUMillicores, float64(whole[domain.NodeResourceCPUMillicores])) ||
			job.Memory != canonicalQuantityString(domain.NodeResourceMemoryBytes, float64(whole[domain.NodeResourceMemoryBytes])) {
			t.Errorf("%s on %s written with %s cpu and %s", id, slotOf(job), job.CPU, job.Memory)
		}
		if strings.HasPrefix(id, "cpu-job") != (slotOf(job) == "cpu-0") {
			t.Errorf("%s was admitted to %s", id, slotOf(job))
		}
	}
	if len(store.written) != 3 || len(workload.created) != 3 {
		t.Fatalf("admitted %v, created %v, want one job per slot", slots, workload.created)
	}
	for _, exp := range store.queued {
		if _, admitted := store.written[exp.ID]; admitted {
			continue
		}
		if reason := store.reasons[exp.ID]; reason == "" || strings.HasPrefix(reason, domain.NotAdmittedExceedsClusterCapacity) {
			t.Errorf("%s waits with reason %q", exp.ID, reason)
		}
	}
}

// A job that was pinned to a slot and requeued comes back with nothing resolved, and the next
// tick puts it on whichever slot is free: here the other one, since an instance still holds the
// slot it had.
func TestARequeuedJobIsAdmittedToAnotherSlot(t *testing.T) {
	c := twoH100AndACPU().take("h100-0")
	requeued := h100Job("requeued", "2")
	requeued.AttemptCount, requeued.EvictionReason = 1, string(domain.EvictionStuckPending)
	store := &slotStore{queued: []*domain.Experiment{requeued}}
	l, _ := slotLoop(store, c, fakeObserved{})

	if err := l.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slot := slotOf(store.written["requeued"]); slot != "h100-1" {
		t.Fatalf("readmitted to %q (reason %q), want the free slot h100-1", slot, store.reasons["requeued"])
	}
}

// Preemption on slots. Both H100 slots are held by burst jobs that asked for 2 cpu of the 8, and
// a guaranteed job that needs an H100 and 4 cpu is queued. The victim gives back the slot it
// holds, which is what the guaranteed job is sized to, so one burst job is preempted. Counted by
// request, the victim freed 2 cpu on a slot that read as empty and the plan never committed.
func TestAGuaranteedJobPreemptsABurstJobForItsWholeSlot(t *testing.T) {
	c := twoH100AndACPU()
	var running []*domain.Experiment
	nodes := map[string]string{}
	for i, node := range []string{"h100-0", "h100-1"} {
		victim := pinnedTo(sized(t, c, h100Job(fmt.Sprintf("burst-%d", i), "2")), node)
		victim.Status, victim.ClusterName, victim.EstimatedCostAccH = domain.StatusRunning, rented, 1
		victim.CreatedAt = time.Now().Add(-time.Hour)
		running = append(running, victim)
		nodes[victim.ID] = node
	}
	c.take("h100-0").take("h100-1")
	guaranteed := h100Job("guaranteed", "4")
	guaranteed.CapacityTier = domain.CapacityGuaranteed
	store := &slotStore{queued: []*domain.Experiment{guaranteed}, running: running}
	l, _ := slotLoop(store, c, fakeObserved{node: nodes, elapsed: map[string]float64{"burst-0": 5, "burst-1": 1}})

	if err := l.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.requeued, []string{"burst-1"}) {
		t.Fatalf("preempted %v, want the burst job that has run least", store.requeued)
	}
	if len(store.written) != 0 {
		t.Fatalf("the guaranteed job was admitted while its slot is still held: %v", store.written)
	}

	// Next tick the victim is QUEUED and its instance still holds the slot. The capacity is
	// promised, so nothing more is preempted for the same job.
	store.running, store.requeued = running[:1], nil
	l, _ = slotLoop(store, c, fakeObserved{node: nodes})
	if err := l.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.requeued) != 0 || len(store.written) != 0 {
		t.Fatalf("while the slot is being freed: preempted %v, admitted %v", store.requeued, store.written)
	}

	// The instance is gone and the slot reads free: the job is admitted to it.
	c.free["h100-1"] = size(8000, 32, 100)
	c.accelerators["h100-1"] = map[string]int64{h100: 1}
	l, _ = slotLoop(store, c, fakeObserved{node: nodes})
	if err := l.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job := store.written["guaranteed"]; slotOf(job) != "h100-1" || job.CPU != "8" {
		t.Fatalf("admitted as %+v (reason %q)", job, store.reasons["guaranteed"])
	}
}

// With every slot held by a job smaller than its slot, nothing is free and nothing is being
// freed. Counted by request, the cluster's size minus what the jobs asked for left room for the
// next job on paper every tick, which read as capacity already on its way and skipped preemption
// for good.
func TestSlotsHeldBySmallJobsArePromisedToNoOne(t *testing.T) {
	c := twoH100AndACPU()
	var running []*domain.Experiment
	for i, node := range []string{"h100-0", "h100-1"} {
		holder := pinnedTo(sized(t, c, h100Job(fmt.Sprintf("holder-%d", i), "2")), node)
		holder.Status, holder.ClusterName = domain.StatusRunning, rented
		running = append(running, holder)
	}
	c.take("h100-0").take("h100-1")
	workload := &slotWorkload{c: c, desired: running}
	_, desiredFree, _ := workload.GetFlavorCapacity(context.Background())

	waiting := sized(t, c, h100Job("waiting", "4"))
	if domain.Fits(desiredFree[rented], waiting.Footprint()) {
		t.Fatalf("desired-free %v reads as room for %v", desiredFree[rented], waiting.Footprint())
	}
	accelerator := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}
	if got := desiredFree[rented]; got[accelerator] != 0 || got[domain.ResourceKey{Kind: domain.ResourceKindCPU}] != 4000 {
		t.Fatalf("desired-free = %v, want only the free cpu slot", got)
	}
}
