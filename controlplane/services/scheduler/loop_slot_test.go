package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const gi = int64(1) << 30

// slotCluster is what a runtime that rents a machine per job reports: every machine it may rent
// is a node carrying domain.NodeSlotLabel. Here two 8-cpu slots with one H100 each and one 4-cpu
// slot without.
func slotCluster(slots bool) (byNode, nodeResources map[string]map[string]int64, labels map[string]map[string]string) {
	byNode = map[string]map[string]int64{"h100-0": {h100: 1}, "h100-1": {h100: 1}}
	nodeResources = map[string]map[string]int64{}
	labels = map[string]map[string]string{}
	for node, cpu := range map[string]int64{"h100-0": 8000, "h100-1": 8000, "cpu-0": 4000} {
		nodeResources[node] = map[string]int64{
			domain.NodeResourceCPUMillicores: cpu, domain.NodeResourceMemoryBytes: 32 * gi, domain.NodeResourceStorageBytes: 100 * gi,
		}
		labels[node] = map[string]string{}
		if slots {
			labels[node][domain.NodeSlotLabel] = node
		}
	}
	return byNode, nodeResources, labels
}

func cpuJob(id, cpu string) *domain.Experiment {
	return &domain.Experiment{ID: id, Job: domain.JobSpec{CPU: cpu, Memory: "1Gi", Storage: "1Gi"}}
}

func pinnedTo(exp *domain.Experiment, slot string) *domain.Experiment {
	resolved := exp.Job
	resolved.NodeSelector = map[string]string{domain.NodeSlotLabel: slot}
	exp.ResolvedJob = &resolved
	return exp
}

// The defect this closes: four 2-cpu jobs fit one 8-cpu node by the sum of their requests, so one
// tick admitted all four onto one slot of a runtime that holds one job per slot, and three of
// them could never be created.
func TestASlotIsFullOnceOneJobIsOnIt(t *testing.T) {
	for _, slots := range []bool{true, false} {
		byNode, nodeResources, labels := slotCluster(slots)
		delete(nodeResources, "h100-1")
		delete(nodeResources, "cpu-0")
		admitted := 0
		for i := 0; i < 4; i++ {
			if reservePlacement(byNode, nodeResources, labels, cpuJob(fmt.Sprintf("exp-%d", i), "2")) {
				admitted++
			}
		}
		// Without the label the node is an ordinary one and packs by request, as before.
		if want := map[bool]int{true: 1, false: 4}[slots]; admitted != want {
			t.Errorf("slots=%v: %d jobs placed on one 8-cpu node, want %d", slots, admitted, want)
		}
		if left := nodeResources["h100-0"][domain.NodeResourceCPUMillicores]; left != 0 {
			t.Errorf("slots=%v: %d millicores left on the node", slots, left)
		}
		if slots && byNode["h100-0"][h100] != 0 {
			t.Errorf("a job without accelerators left the slot's accelerator free for another job")
		}
	}
}

func TestSlotsHoldAsManyJobsAsThereAreSlots(t *testing.T) {
	byNode, nodeResources, labels := slotCluster(true)
	jobs := []*domain.Experiment{cpuJob("a", "2"), cpuJob("b", "2"), cpuJob("c", "2")}
	if !topologyFits(byNode, nodeResources, labels, jobs[0]) {
		t.Fatal("a job that fits a free slot was refused")
	}
	if _, ok := planPlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labels, jobs); !ok {
		t.Fatal("three jobs were refused on three free slots")
	}
	if _, ok := planPlacement(cloneNodeCapacity(byNode), cloneNodeCapacity(nodeResources), labels, append(jobs, cpuJob("d", "2"))); ok {
		t.Fatal("four jobs were placed on three slots")
	}
}

// Admission writes the slot into the resolved job, and only there: the runtime rents that slot
// and the submitted job still reads as it was written.
func TestTheClaimPinsTheJobToTheSlotItWasPlacedOn(t *testing.T) {
	byNode, nodeResources, labels := slotCluster(true)
	candidate := cpuJob("exp-1", "6")
	candidate.Job.NodeSelector = map[string]string{"zone": "a"}
	for node := range labels {
		labels[node]["zone"] = "a"
	}
	resolved := candidate.Job

	if !desiredPlacementFits(byNode, nodeResources, labels, nil, candidate, &resolved) {
		t.Fatal("a 6-cpu job was refused with two free 8-cpu slots")
	}
	slot := resolved.NodeSelector[domain.NodeSlotLabel]
	if slot != "h100-0" && slot != "h100-1" {
		t.Fatalf("resolved selector = %v, want one of the slots the job fits", resolved.NodeSelector)
	}
	if resolved.NodeSelector["zone"] != "a" || !reflect.DeepEqual(candidate.Job.NodeSelector, map[string]string{"zone": "a"}) {
		t.Fatalf("resolved %v, submitted %v", resolved.NodeSelector, candidate.Job.NodeSelector)
	}
	if nodeResources["h100-0"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatal("proving the fit consumed the caller's capacity")
	}
}

// An ordinary node decides nothing for the runtime, which stays free to place the job.
func TestAJobOnAnOrdinaryNodeIsNotPinned(t *testing.T) {
	byNode, nodeResources, labels := slotCluster(false)
	candidate := cpuJob("exp-1", "2")
	resolved := candidate.Job
	if !desiredPlacementFits(byNode, nodeResources, labels, nil, candidate, &resolved) || resolved.NodeSelector != nil {
		t.Fatalf("resolved selector = %v", resolved.NodeSelector)
	}
}

// A job admitted a moment ago holds its slot before the runtime has rented it, and keeps holding
// it, without being counted twice, once the runtime reports the slot taken.
func TestAPinnedDesiredJobHoldsItsSlotBeforeAndAfterItIsRented(t *testing.T) {
	for name, rented := range map[string]bool{"not yet rented": false, "rented and reported taken": true} {
		t.Run(name, func(t *testing.T) {
			byNode, nodeResources, labels := slotCluster(true)
			desired := []*domain.Experiment{pinnedTo(cpuJob("earlier", "6"), "h100-0")}
			if rented {
				occupyNode(byNode, nodeResources, "h100-0")
			}

			second := cpuJob("second", "6")
			resolved := second.Job
			if !desiredPlacementFits(byNode, nodeResources, labels, desired, second, &resolved) {
				t.Fatal("the second job was refused with a slot free")
			}
			if slot := resolved.NodeSelector[domain.NodeSlotLabel]; slot != "h100-1" {
				t.Fatalf("second job pinned to %q, want the slot the earlier job does not hold", slot)
			}

			desired = append(desired, pinnedTo(second, "h100-1"))
			third := cpuJob("third", "6")
			resolved = third.Job
			if desiredPlacementFits(byNode, nodeResources, labels, desired, third, &resolved) {
				t.Fatalf("a third 6-cpu job was admitted with both 8-cpu slots held (pinned to %q)", resolved.NodeSelector[domain.NodeSlotLabel])
			}
		})
	}
}

// Two free slots of one size are not interchangeable when a job planned later is pinned to one
// of them. The search must be able to move the first job to the other.
func TestAnUnpinnedJobMovesAsideForOnePinnedToTheSlotItTook(t *testing.T) {
	byNode, nodeResources, labels := slotCluster(true)
	delete(nodeResources, "cpu-0")
	// Hardest first puts the 6-cpu job before the pinned 2-cpu one, on the slot that sorts first.
	jobs := []*domain.Experiment{cpuJob("large", "6"), pinnedTo(cpuJob("pinned", "2"), "h100-0")}
	plan, ok := planPlacement(byNode, nodeResources, labels, jobs)
	if !ok {
		t.Fatal("no placement found, though large fits h100-1 and pinned holds h100-0")
	}
	nodes := map[int]string{}
	for _, p := range plan {
		nodes[p.job] = p.node
	}
	if nodes[0] != "h100-1" || nodes[1] != "h100-0" {
		t.Fatalf("placed %v", nodes)
	}
}

// The tick reserves an admitted job against the pass's own view of the cluster. A pinned job
// takes its slot there too, so the next job in the same tick sees it gone.
func TestReservingAPinnedJobTakesItsSlot(t *testing.T) {
	byNode, nodeResources, labels := slotCluster(true)
	if !reservePlacement(byNode, nodeResources, labels, pinnedTo(cpuJob("exp-1", "1"), "h100-1")) {
		t.Fatal("a pinned job was refused on its free slot")
	}
	if byNode["h100-1"][h100] != 0 || nodeResources["h100-1"][domain.NodeResourceMemoryBytes] != 0 || nodeResources["h100-0"][domain.NodeResourceCPUMillicores] != 8000 {
		t.Fatalf("after reserving: %v %v", byNode, nodeResources)
	}
	if reservePlacement(byNode, nodeResources, labels, pinnedTo(cpuJob("exp-2", "1"), "h100-1")) {
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
	byNode, nodeResources, labels := slotCluster(true)
	exp := cpuJob("exp-1", "6")
	live := &liveWorkload{
		guaranteed: map[string]domain.Footprint{"rented": exp.Footprint()},
		burst:      map[string]domain.Footprint{"rented": domain.NewFootprint()},
		nodeAccel:  map[string]map[string]map[string]int64{"rented": byNode},
		nodeRes:    map[string]map[string]map[string]int64{"rented": nodeResources},
		nodeLabels: map[string]map[string]map[string]string{"rented": labels},
	}
	store := &pinningStore{desired: []*domain.Experiment{pinnedTo(cpuJob("earlier", "6"), "h100-0")}}
	l := NewLoop(store, submitQuota{}, live, testGraceLimits, zap.NewNop())

	if err := l.submitJob(context.Background(), exp, "rented", ""); err != nil {
		t.Fatal(err)
	}
	if slot := store.written.NodeSelector[domain.NodeSlotLabel]; slot != "h100-1" {
		t.Fatalf("resolved job written with selector %v, want slot h100-1", store.written.NodeSelector)
	}
	if exp.EffectiveJob().NodeSelector[domain.NodeSlotLabel] != "h100-1" || exp.Job.NodeSelector != nil || !live.created {
		t.Fatalf("effective %v, submitted %v, created %v", exp.EffectiveJob().NodeSelector, exp.Job.NodeSelector, live.created)
	}
}
