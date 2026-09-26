package scheduler

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A CPU class beside an accelerator type on one node, through the scheduler's own passes: what a
// job costs, where it is placed and at which type, who goes first, and who is preempted for whom.
// The last test runs one accelerator-only workload with and without a CPU class in the catalog
// and on the node, and wants every outcome equal.

// mixedWorkload is cluster-a with one node of 16 cores holding four h100 and, with a class
// published, 16 units of cpuClass and 8 of otherCPUClass.
type mixedWorkload struct {
	holdWorkload
	h100Free, coresFree, otherCoresFree int64
	cpuFree                             float64
	published                           bool
}

func (w mixedWorkload) flavors(h100s, cores, otherCores int64) map[string]int64 {
	if !w.published {
		return map[string]int64{h100: h100s}
	}
	return map[string]int64{h100: h100s, cpuClass: cores, otherCPUClass: otherCores}
}
func (w mixedWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func() map[string]domain.Footprint {
		return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(w.cpuFree, w.flavors(w.h100Free, w.coresFree, w.otherCoresFree), 64<<30, 64<<30)}
	}
	return free(), free(), nil
}
func (w mixedWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": w.flavors(w.h100Free, w.coresFree, w.otherCoresFree)}}, nil
}
func (w mixedWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	node := holdNodeTotal()
	node[domain.NodeResourceCPUMillicores] = int64(w.cpuFree * 1000)
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": node}}, nil
}
func (mixedWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (mixedWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}}, nil
}
func (w mixedWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(16, w.flavors(4, 16, 8), 64<<30, 64<<30)}, nil
}

// mixedStore records where each job was claimed, the spec it was claimed with, and who was
// requeued for another job.
type mixedStore struct {
	holdStore
	resolved map[string]domain.JobSpec
	requeued []string
}

func (s *mixedStore) ClaimSubmitted(ctx context.Context, id, cluster string, resolved *domain.JobSpec, fits func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	if s.resolved == nil {
		s.resolved = map[string]domain.JobSpec{}
	}
	if resolved != nil {
		s.resolved[id] = *resolved
	}
	return s.holdStore.ClaimSubmitted(ctx, id, cluster, resolved, fits)
}
func (s *mixedStore) RequeuePreempted(_ context.Context, id string, _, _ float64) (bool, error) {
	s.requeued = append(s.requeued, id)
	return true, nil
}

type reservation struct {
	Flavor domain.AcceleratorType
	Cost   float64
	Count  int
}

// mixedQuota grants every agent ample hours and records what each admitted job was reserved at.
type mixedQuota struct {
	tickQuota
	reserved map[string]reservation
}

func (*mixedQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{GuaranteedAcceleratorHours: 100, BurstAcceleratorHours: 100}, nil
}
func (q *mixedQuota) ReserveAdmittedFlavor(_ context.Context, id string, flavor domain.AcceleratorType, cost float64, count int) error {
	q.reserved[id] = reservation{flavor, cost, count}
	return nil
}

func mixedLoop(store LoopStore, workload LoopWorkloadClient, observed ObservedState) (*Loop, *mixedQuota, *recordingEvictor) {
	evictor := &recordingEvictor{}
	l := holdLoop(store, workload, evictor)
	quota := &mixedQuota{reserved: map[string]reservation{}}
	l.quota = quota
	l.settler = &recordingSettler{}
	l.observed = fakeObserved{}
	if observed != nil {
		l.observed = observed
	}
	return l, quota, evictor
}

// priced is a job as submission leaves it: its cost is rate x count x hours of its own type.
func priced(exp *domain.Experiment, hours float64) *domain.Experiment {
	exp.EstimatedDurationHours = hours
	exp.EstimatedCostAccH = estimatedAcceleratorCost(exp)
	return exp
}

func coresJob(id string, tier domain.CapacityTier, cores int) *domain.Experiment {
	exp := holdJob(id, tier, cpuClass, cores, "")
	if err := countSetsCores(&exp.Job); err != nil {
		panic(err)
	}
	return exp
}

func running(exp *domain.Experiment) *domain.Experiment {
	exp.ClusterName, exp.Status, exp.CreatedAt = "cluster-a", domain.StatusRunning, time.Now().Add(-time.Hour)
	exp.Job.NumNodes = 1
	return exp
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAJobIsCostedAtTheRateOfItsOwnType(t *testing.T) {
	withCPUClasses(t)
	for name, tc := range map[string]struct {
		exp  *domain.Experiment
		want float64
	}{
		"8 cores for 3 hours":           {priced(coresJob("c", domain.CapacityBurst, 8), 3), 8 * 0.01 * 3},
		"1 core for 6 minutes":          {priced(coresJob("c", domain.CapacityBurst, 1), 0.1), 0.001},
		"8 cores of the dearer class":   {priced(holdJob("c", domain.CapacityBurst, otherCPUClass, 8, "8"), 3), 8 * 0.02 * 3},
		"1 h100 for 3 hours":            {priced(holdJob("g", domain.CapacityBurst, h100, 1, "8"), 3), 3},
		"2 l40 for 3 hours":             {priced(holdJob("g", domain.CapacityBurst, l40, 2, "8"), 3), 1.5},
		"an h100 job holding 8 cores":   {priced(holdJob("g", domain.CapacityBurst, h100, 1, "8"), 1), 1},
		"an h100 job holding 0.5 cores": {priced(holdJob("g", domain.CapacityBurst, h100, 1, "500m"), 1), 1},
	} {
		if !near(tc.exp.EstimatedCostAccH, tc.want) {
			t.Errorf("%s: estimated cost = %v, want %v", name, tc.exp.EstimatedCostAccH, tc.want)
		}
	}
}

func TestATickAdmitsACoreJobAndAnAcceleratorJobSideBySide(t *testing.T) {
	withCPUClasses(t)
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			cores := priced(coresJob("cores", tier, 4), 2)
			gpu := priced(holdJob("gpu", tier, h100, 1, "2"), 2)
			store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{cores, gpu}}}}
			l, quota, evictor := mixedLoop(store, mixedWorkload{h100Free: 4, coresFree: 16, otherCoresFree: 8, cpuFree: 16, published: true}, nil)

			tickOnce(t, l)

			if store.submitted["cores"] != "cluster-a" || store.submitted["gpu"] != "cluster-a" {
				t.Fatalf("submitted = %v, want both on cluster-a (reasons %v)", store.submitted, store.notAdmitted)
			}
			want := map[string]reservation{"cores": {cpuClass, 4 * 0.01 * 2, 4}, "gpu": {h100, 2, 1}}
			for id, w := range want {
				if got := quota.reserved[id]; got.Flavor != w.Flavor || got.Count != w.Count || !near(got.Cost, w.Cost) {
					t.Errorf("%s reserved at %+v, want %+v", id, got, w)
				}
			}
			if got := store.resolved["cores"]; got.CPU != "4" || got.Memory != "16Gi" || got.AcceleratorCount != 4 {
				t.Errorf("the core job runs as cpu %q, memory %q, count %d; want 4 cores and a quarter of the node's 64Gi", got.CPU, got.Memory, got.AcceleratorCount)
			}
			if got := store.resolved["gpu"]; got.CPU != "2" || got.AcceleratorCount != 1 {
				t.Errorf("the accelerator job runs as cpu %q, count %d; want what it asked for", got.CPU, got.AcceleratorCount)
			}
			if len(evictor.evicted) != 0 || len(store.requeued) != 0 {
				t.Errorf("evicted %v, requeued %v with room for both", evictor.evicted, store.requeued)
			}
		})
	}
}

// Core units and real cores are two counts. A core job needs both, and free units alone admit
// nothing once accelerator jobs hold the node's cores.
func TestACoreJobWaitsWhenTheNodesCoresAreHeldThoughUnitsAreFree(t *testing.T) {
	withCPUClasses(t)
	cores := priced(coresJob("cores", domain.CapacityBurst, 4), 1)
	store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{cores}}}}
	l, _, evictor := mixedLoop(store, mixedWorkload{h100Free: 0, coresFree: 16, otherCoresFree: 8, cpuFree: 2, published: true}, nil)

	tickOnce(t, l)

	if _, admitted := store.submitted["cores"]; admitted {
		t.Fatal("4 cores were admitted onto a node with 2 free")
	}
	if len(evictor.evicted) != 0 {
		t.Errorf("evicted %v with no running job to blame", evictor.evicted)
	}
}

func TestACoreJobTakesAnotherCPUClassAtThatClassesRate(t *testing.T) {
	withCPUClasses(t)
	cores := coresJob("cores", domain.CapacityBurst, 4)
	cores.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{otherCPUClass}
	priced(cores, 2)
	holder := running(priced(coresJob("holder", domain.CapacityGuaranteed, 14), 10))
	store := &mixedStore{holdStore: holdStore{
		noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{cores}},
		running:            []*domain.Experiment{holder},
	}}
	l, quota, _ := mixedLoop(store, mixedWorkload{h100Free: 4, coresFree: 2, otherCoresFree: 8, cpuFree: 16, published: true},
		fakeObserved{node: map[string]string{"holder": "node-a"}})

	tickOnce(t, l)

	if store.submitted["cores"] != "cluster-a" {
		t.Fatalf("not placed with 8 units of the other class free (reason %q)", store.notAdmitted["cores"])
	}
	if got := quota.reserved["cores"]; got.Flavor != otherCPUClass || got.Count != 4 || !near(got.Cost, 4*0.02*2) {
		t.Errorf("reserved at %+v, want 4 cores of the other class at its rate", got)
	}
	// 4 of the other class's 8 units: half the node, where 4 of the first class's 16 are a quarter.
	if got := store.resolved["cores"]; got.CPU != "4" || got.Memory != "32Gi" {
		t.Errorf("runs as cpu %q with memory %q; want 4 cores and its share of the class it landed on", got.CPU, got.Memory)
	}
	if cores.AcceleratorType != otherCPUClass {
		t.Errorf("admitted as %q, want the class it landed on", cores.AcceleratorType)
	}
}

func TestPreemptionFreesUnitsOfTheTypeThatIsShortAndLeavesTheOtherAlone(t *testing.T) {
	withCPUClasses(t)
	observed := fakeObserved{
		node:    map[string]string{"burst-cores": "node-a", "burst-gpu": "node-a"},
		elapsed: map[string]float64{"burst-cores": 0.5, "burst-gpu": 0.5},
	}
	for name, tc := range map[string]struct {
		preemptor *domain.Experiment
		free      mixedWorkload
		want      []string
	}{
		"a guaranteed core job short of core units": {
			priced(coresJob("guaranteed", domain.CapacityGuaranteed, 8), 1),
			mixedWorkload{h100Free: 0, coresFree: 4, otherCoresFree: 8, cpuFree: 4, published: true},
			[]string{"burst-cores"},
		},
		"a guaranteed accelerator job short of an h100": {
			priced(holdJob("guaranteed", domain.CapacityGuaranteed, h100, 1, "2"), 1),
			mixedWorkload{h100Free: 0, coresFree: 4, otherCoresFree: 8, cpuFree: 4, published: true},
			[]string{"burst-gpu"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			burstCores := running(priced(coresJob("burst-cores", domain.CapacityBurst, 8), 10))
			burstGPU := running(priced(holdJob("burst-gpu", domain.CapacityBurst, h100, 1, "2"), 10))
			store := &mixedStore{holdStore: holdStore{
				noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{tc.preemptor}},
				running:            []*domain.Experiment{burstCores, burstGPU},
			}}
			l, _, evictor := mixedLoop(store, tc.free, observed)

			tickOnce(t, l)

			if !reflect.DeepEqual(store.requeued, tc.want) {
				t.Fatalf("requeued = %v, want %v (reason %q)", store.requeued, tc.want, store.notAdmitted["guaranteed"])
			}
			if len(evictor.evicted) != 0 {
				t.Errorf("the disbalance pass evicted %v", evictor.evicted)
			}
		})
	}
}

// A burst job never takes from guaranteed work, of either kind.
func TestABurstCoreJobPreemptsNothing(t *testing.T) {
	withCPUClasses(t)
	held := running(priced(coresJob("held", domain.CapacityGuaranteed, 16), 10))
	other := running(priced(coresJob("burst-running", domain.CapacityBurst, 4), 10))
	burst := priced(coresJob("burst", domain.CapacityBurst, 8), 1)
	store := &mixedStore{holdStore: holdStore{
		noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{burst}},
		running:            []*domain.Experiment{held, other},
	}}
	l, _, evictor := mixedLoop(store, mixedWorkload{h100Free: 4, coresFree: 0, otherCoresFree: 8, cpuFree: 0, published: true},
		fakeObserved{node: map[string]string{"held": "node-a", "burst-running": "node-a"}})

	tickOnce(t, l)

	if len(store.requeued) != 0 || len(evictor.evicted) != 0 || store.submitted["burst"] != "" {
		t.Fatalf("requeued %v, evicted %v, submitted %v; want a burst job with no free units to wait", store.requeued, evictor.evicted, store.submitted)
	}
}

// Fairness reads one number per agent, hours used over hours granted, and one per job, its cost
// over the grant. Both are in accelerator-hours whatever the type, so an agent that spent its
// grant on cores stands behind one that spent less on accelerators, and the reverse.
func TestFairnessOrdersCoreAndAcceleratorJobsByTheSameRatio(t *testing.T) {
	withCPUClasses(t)
	at := time.Now()
	job := func(id, agent string, exp *domain.Experiment) *domain.Experiment {
		exp.ID, exp.AgentID, exp.QueuedAt = id, agent, &at
		return priced(exp, 1)
	}
	quotas := map[string]*domain.AgentQuota{
		quotaKey("spent-on-cores", "pe-1"): {GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 6},
		quotaKey("spent-on-gpus", "pe-1"):  {GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 2},
		quotaKey("fresh", "pe-1"):          {GuaranteedAcceleratorHours: 10},
	}
	order := func(sorted []*domain.Experiment) []string {
		ids := make([]string, 0, len(sorted))
		for _, exp := range sorted {
			ids = append(ids, exp.ID)
		}
		return ids
	}
	build := func() []*domain.Experiment {
		return []*domain.Experiment{
			job("cores-of-heavy-user", "spent-on-cores", coresJob("", domain.CapacityBurst, 1)),
			job("gpu-of-light-user", "spent-on-gpus", holdJob("", domain.CapacityBurst, h100, 1, "1")),
			job("gpu-of-fresh", "fresh", holdJob("", domain.CapacityBurst, h100, 1, "1")),
			job("cores-of-fresh", "fresh", coresJob("", domain.CapacityBurst, 16)),
		}
	}
	// Among the fresh agent's own two jobs the smaller bite of the grant goes first: 16 cores for
	// an hour are 0.16 accelerator-hours, an h100 is 1.
	want := []string{"cores-of-fresh", "gpu-of-fresh", "gpu-of-light-user", "cores-of-heavy-user"}

	burst := build()
	sortBurst(burst, quotas, nil)
	if got := order(burst); !reflect.DeepEqual(got, want) {
		t.Errorf("burst order = %v, want %v", got, want)
	}
	guaranteed := build()
	sortGuaranteed(guaranteed, quotas, nil, time.Hour)
	if got := order(guaranteed); !reflect.DeepEqual(got, want) {
		t.Errorf("guaranteed order = %v, want %v", got, want)
	}
}

// A dearer alternative is tried only when the quota left covers the difference, for a CPU class
// as for an accelerator. The job's estimate at the class it asked for is already in what is used.
func TestTheQuotaLeftDecidesWhetherADearerClassIsTried(t *testing.T) {
	withCPUClasses(t)
	// 10 cores for 10 hours: 1 accelerator-hour at the class asked for, 2 at the dearer one.
	exp := priced(coresJob("cores", domain.CapacityBurst, 10), 10)
	exp.AcceleratorType = otherCPUClass
	for used, want := range map[float64]bool{1: true, 9: true, 9.5: false, 10: false} {
		quota := &domain.AgentQuota{BurstAcceleratorHours: 10, UsedBurstAccH: used}
		if got := quotaCanCoverFlavor(quota, exp); got != want {
			t.Errorf("with %v of 10 used: the dearer class is covered = %v, want %v", used, got, want)
		}
	}
	gpu := priced(holdJob("gpu", domain.CapacityBurst, l40, 1, "1"), 4)
	gpu.AcceleratorType = h100
	for used, want := range map[float64]bool{7: true, 7.5: false} {
		quota := &domain.AgentQuota{BurstAcceleratorHours: 10, UsedBurstAccH: used}
		if got := quotaCanCoverFlavor(quota, gpu); got != want {
			t.Errorf("an l40 job moving to an h100 with %v of 10 used: covered = %v, want %v", used, got, want)
		}
	}
}

type tickOutcome struct {
	Submitted   map[string]string
	NotAdmitted map[string]string
	Reserved    map[string]reservation
	Resolved    map[string]domain.JobSpec
	Requeued    []string
	Evicted     []string
}

// One accelerator workload, decided twice: with a catalog and a node that know of no CPU class,
// and with both classes priced, published on the node and a core job in the queue. A burst tick
// has a job that fits, one sized by "max", one over its share and one too large for the node. A
// guaranteed tick has a job that preempts a burst one. Every outcome of every accelerator job
// must be the same.
func TestAnAcceleratorWorkloadIsDecidedTheSameBesideACPUClass(t *testing.T) {
	tick := func(t *testing.T, withClass bool, queued []*domain.Experiment) tickOutcome {
		victim := running(priced(holdJob("victim", domain.CapacityBurst, h100, 1, "1"), 10))
		keeper := running(priced(holdJob("keeper", domain.CapacityGuaranteed, h100, 1, "1"), 10))
		if withClass {
			queued = append(queued, priced(coresJob("cores", domain.CapacityBurst, 2), 1))
		}
		store := &mixedStore{holdStore: holdStore{
			noRunningListStore: noRunningListStore{t: t, queued: queued},
			running:            []*domain.Experiment{victim, keeper},
		}}
		store.submitted, store.notAdmitted, store.resolved = map[string]string{}, map[string]string{}, map[string]domain.JobSpec{}
		l, quota, evictor := mixedLoop(store, mixedWorkload{h100Free: 2, coresFree: 16, otherCoresFree: 8, cpuFree: 14, published: withClass},
			fakeObserved{node: map[string]string{"victim": "node-a", "keeper": "node-a"}, elapsed: map[string]float64{"victim": 1, "keeper": 1}})

		tickOnce(t, l)

		out := tickOutcome{store.submitted, store.notAdmitted, quota.reserved, store.resolved, store.requeued, evictor.evicted}
		for _, m := range []map[string]string{out.Submitted, out.NotAdmitted} {
			delete(m, "cores")
		}
		delete(out.Reserved, "cores")
		delete(out.Resolved, "cores")
		return out
	}
	burst := func() []*domain.Experiment {
		return []*domain.Experiment{
			priced(holdJob("fits", domain.CapacityBurst, h100, 1, "1"), 2),
			priced(holdJob("max", domain.CapacityBurst, h100, 1, domain.MaxResourceSentinel), 1),
			priced(holdJob("over-share", domain.CapacityBurst, h100, 1, "6"), 1),
			priced(holdJob("too-large", domain.CapacityBurst, h100, 5, "1"), 1),
		}
	}
	guaranteed := func() []*domain.Experiment {
		return []*domain.Experiment{
			priced(holdJob("guaranteed", domain.CapacityGuaranteed, h100, 3, "3"), 1),
			priced(holdJob("behind", domain.CapacityBurst, h100, 1, "1"), 1),
		}
	}

	beforeBurst, beforeGuaranteed := tick(t, false, burst()), tick(t, false, guaranteed())
	withCPUClasses(t)
	afterBurst, afterGuaranteed := tick(t, true, burst()), tick(t, true, guaranteed())

	if !reflect.DeepEqual(beforeBurst, afterBurst) {
		t.Errorf("a burst tick of accelerator jobs was decided differently beside a CPU class:\nwithout %+v\nwith    %+v", beforeBurst, afterBurst)
	}
	if !reflect.DeepEqual(beforeGuaranteed, afterGuaranteed) {
		t.Errorf("a guaranteed tick of accelerator jobs was decided differently beside a CPU class:\nwithout %+v\nwith    %+v", beforeGuaranteed, afterGuaranteed)
	}
	// The comparison is of ticks that did something of each kind.
	if len(beforeBurst.Submitted) != 2 || beforeBurst.Resolved["max"].CPU != "4" || len(beforeBurst.NotAdmitted) != 2 || !near(beforeBurst.Reserved["fits"].Cost, 2) {
		t.Errorf("the burst tick exercised too little to compare: %+v", beforeBurst)
	}
	if !reflect.DeepEqual(beforeGuaranteed.Requeued, []string{"victim"}) || beforeGuaranteed.NotAdmitted["behind"] != domain.NotAdmittedOutranked {
		t.Errorf("the guaranteed tick exercised too little to compare: %+v", beforeGuaranteed)
	}
}
