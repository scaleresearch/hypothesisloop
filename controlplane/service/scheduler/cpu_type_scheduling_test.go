package scheduler

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Jobs on cores beside jobs on accelerators on one node, through the scheduler's own passes: what
// a job costs, where it is placed, who goes first, and who is preempted for whom. The last test
// runs one accelerator-only workload with and without cores priced and offered, and wants every
// outcome equal.

// mixedWorkload is cluster-a with one node of 16 cores holding four h100 and, when it offers
// them, its free cores as units of the CPU type.
type mixedWorkload struct {
	holdWorkload
	h100Free  int64
	cpuFree   float64
	published bool
}

func (w mixedWorkload) acceleratorTypes(h100s, cores int64) map[string]int64 {
	if !w.published {
		return map[string]int64{h100: h100s}
	}
	return map[string]int64{h100: h100s, string(cpuType): cores}
}
func (w mixedWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func() map[string]domain.Footprint {
		return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(w.cpuFree, w.acceleratorTypes(w.h100Free, int64(w.cpuFree)), 64<<30, 64<<30)}
	}
	return free(), free(), nil
}
func (w mixedWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": w.acceleratorTypes(w.h100Free, int64(w.cpuFree))}}, nil
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
	return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(16, w.acceleratorTypes(4, 16), 64<<30, 64<<30)}, nil
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
	AcceleratorType domain.AcceleratorType
	Cost            float64
	Count           int
}

// mixedQuota grants every agent ample hours and records what each admitted job was reserved at.
type mixedQuota struct {
	tickQuota
	reserved map[string]reservation
}

func (*mixedQuota) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return &domain.AgentQuota{GuaranteedAcceleratorHours: 100, BurstAcceleratorHours: 100}, nil
}
func (q *mixedQuota) ReserveAdmittedAcceleratorType(_ context.Context, id string, acceleratorType domain.AcceleratorType, cost float64, count int) error {
	q.reserved[id] = reservation{acceleratorType, cost, count}
	return nil
}

func mixedLoop(store LoopStore, workload ClusterCapacity, observed ObservedState) (*Loop, *mixedQuota, *recordingEvictor) {
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
	held, err := exp.EstimateHeld()
	if err != nil {
		panic(err)
	}
	exp.EstimatedCostAccH = held
	return exp
}

func coresJob(id string, tier domain.CapacityTier, cores int) *domain.Experiment {
	return holdJob(id, tier, cpuType, cores, strconv.Itoa(cores))
}

func running(exp *domain.Experiment) *domain.Experiment {
	exp.ClusterName, exp.Status, exp.CreatedAt = "cluster-a", domain.StatusRunning, time.Now().Add(-time.Hour)
	exp.Job.NumNodes = 1
	return exp
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAJobIsCostedAtTheRateOfItsOwnType(t *testing.T) {
	withCPUType(t)
	for name, tc := range map[string]struct {
		exp  *domain.Experiment
		want float64
	}{
		"4 cores for an hour":           {priced(coresJob("c", domain.CapacityBurst, 4), 1), 4},
		"1 core for 6 minutes":          {priced(coresJob("c", domain.CapacityBurst, 1), 0.1), 0.1},
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
	withCPUType(t)
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			cores := priced(coresJob("cores", tier, 4), 2)
			gpu := priced(holdJob("gpu", tier, h100, 1, "2"), 2)
			store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{cores, gpu}}}}
			l, quota, evictor := mixedLoop(store, mixedWorkload{h100Free: 4, cpuFree: 16, published: true}, nil)

			tickOnce(t, l)

			if store.submitted["cores"] != "cluster-a" || store.submitted["gpu"] != "cluster-a" {
				t.Fatalf("submitted = %v, want both on cluster-a (reasons %v)", store.submitted, store.notAdmitted)
			}
			want := map[string]reservation{"cores": {cpuType, 4 * 2, 4}, "gpu": {h100, 2, 1}}
			for id, w := range want {
				if got := quota.reserved[id]; got.AcceleratorType != w.AcceleratorType || got.Count != w.Count || !near(got.Cost, w.Cost) {
					t.Errorf("%s reserved at %+v, want %+v", id, got, w)
				}
			}
			if got := store.resolved["cores"]; got.CPU != "4" || got.Memory != "1Gi" || got.AcceleratorCount != 4 {
				t.Errorf("the core job runs as cpu %q, memory %q, count %d; want the 4 cores and 1Gi it stated", got.CPU, got.Memory, got.AcceleratorCount)
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

func TestPreemptionFreesUnitsOfTheTypeThatIsShortAndLeavesTheOtherAlone(t *testing.T) {
	withCPUType(t)
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
			mixedWorkload{h100Free: 0, cpuFree: 4, published: true},
			[]string{"burst-cores"},
		},
		"a guaranteed accelerator job short of an h100": {
			priced(holdJob("guaranteed", domain.CapacityGuaranteed, h100, 1, "2"), 1),
			mixedWorkload{h100Free: 0, cpuFree: 4, published: true},
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

// Fairness reads one number per agent, hours used over hours granted, and one per job, its cost
// over the grant. Both are in accelerator-hours whatever the type, so an agent that spent its
// grant on cores stands behind one that spent less on accelerators, and the reverse.
func TestFairnessOrdersCoreAndAcceleratorJobsByTheSameRatio(t *testing.T) {
	withCPUType(t)
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
			job("gpu-of-fresh", "fresh", holdJob("", domain.CapacityBurst, h100, 2, "1")),
			job("cores-of-fresh", "fresh", coresJob("", domain.CapacityBurst, 1)),
		}
	}
	// Among the fresh agent's own two jobs the smaller bite of the grant goes first: a core for an
	// hour costs 1, two h100 cost 2.
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

type tickOutcome struct {
	Submitted   map[string]string
	NotAdmitted map[string]string
	Reserved    map[string]reservation
	Resolved    map[string]domain.JobSpec
	Requeued    []string
	Evicted     []string
}

// One accelerator workload, decided twice: with a catalog and a node that know of no cores, and
// with cores priced, offered by the node and a core job in the queue. A burst tick
// has a job that fits, one at its share, one over its share and one too large for the node. A
// guaranteed tick has a job that preempts a burst one. Every outcome of every accelerator job
// must be the same.
func TestAnAcceleratorWorkloadIsDecidedTheSameBesideJobsOnCPU(t *testing.T) {
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
		l, quota, evictor := mixedLoop(store, mixedWorkload{h100Free: 2, cpuFree: 14, published: withClass},
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
			priced(holdJob("at-share", domain.CapacityBurst, h100, 1, "4"), 1),
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
	withCPUType(t)
	afterBurst, afterGuaranteed := tick(t, true, burst()), tick(t, true, guaranteed())

	if !reflect.DeepEqual(beforeBurst, afterBurst) {
		t.Errorf("a burst tick of accelerator jobs was decided differently beside jobs on CPU:\nwithout %+v\nwith    %+v", beforeBurst, afterBurst)
	}
	if !reflect.DeepEqual(beforeGuaranteed, afterGuaranteed) {
		t.Errorf("a guaranteed tick of accelerator jobs was decided differently beside jobs on CPU:\nwithout %+v\nwith    %+v", beforeGuaranteed, afterGuaranteed)
	}
	// The comparison is of ticks that did something of each kind.
	if len(beforeBurst.Submitted) != 2 || beforeBurst.Resolved["at-share"].CPU != "4" || len(beforeBurst.NotAdmitted) != 2 || !near(beforeBurst.Reserved["fits"].Cost, 2) {
		t.Errorf("the burst tick exercised too little to compare: %+v", beforeBurst)
	}
	if !reflect.DeepEqual(beforeGuaranteed.Requeued, []string{"victim"}) || !strings.HasPrefix(beforeGuaranteed.NotAdmitted["behind"], domain.NotAdmittedOutranked+": ") {
		t.Errorf("the guaranteed tick exercised too little to compare: %+v", beforeGuaranteed)
	}
}

func (*mixedStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }

// coreBoxes is one idle node per cluster offering its cores as units of the CPU type, with the
// cores and memory named here.
type coreBoxes struct {
	ClusterCapacity
	cores  map[string]int64
	memory map[string]int64
}

func (w coreBoxes) node(cluster string) map[string]int64 {
	return map[string]int64{
		domain.NodeResourceCPUMillicores: w.cores[cluster] * 1000,
		domain.NodeResourceMemoryBytes:   w.memory[cluster],
		domain.NodeResourceStorageBytes:  64 << 30,
	}
}
func (w coreBoxes) footprints() map[string]domain.Footprint {
	out := map[string]domain.Footprint{}
	for c, n := range w.cores {
		out[c] = domain.CapacityFootprint(float64(n), map[string]int64{string(cpuType): n}, w.memory[c], 64<<30)
	}
	return out
}
func (w coreBoxes) perNode(value func(string) map[string]int64) map[string]map[string]map[string]int64 {
	out := map[string]map[string]map[string]int64{}
	for c := range w.cores {
		out[c] = map[string]map[string]int64{"node-" + c: value(c)}
	}
	return out
}
func (w coreBoxes) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	return w.footprints(), w.footprints(), nil
}
func (w coreBoxes) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return w.footprints(), nil
}
func (w coreBoxes) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(func(c string) map[string]int64 { return map[string]int64{string(cpuType): w.cores[c]} }), nil
}
func (w coreBoxes) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(w.node), nil
}
func (w coreBoxes) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(w.node), nil
}
func (w coreBoxes) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	out := map[string]map[string]map[string]string{}
	for c := range w.cores {
		out[c] = map[string]map[string]string{"node-" + c: {}}
	}
	return out, nil
}
func (coreBoxes) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// A job above its cores' part of one cluster's nodes goes to another cluster whose nodes hold it,
// and is refused only when no cluster's do, in cores and Gi, naming every cluster tried.
func TestAJobOnCPUAboveItsShareOfOneClusterIsPlacedOnAnotherThatHoldsIt(t *testing.T) {
	withCPUType(t)
	const small = 16471307056 // 2 of its 8 cores get 4117826764 bytes, 3.83Gi
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			for name, tc := range map[string]struct {
				bigMemory int64
				placed    string
				reason    string
			}{
				"another cluster holds it": {bigMemory: 250 << 30, placed: "cluster-b"},
				"no cluster holds it": {bigMemory: small, reason: domain.NotAdmittedExceedsAcceleratorShare +
					": memory 8Gi is above the 3.8Gi that 2 core(s) get on cluster cluster-a (each node's memory split evenly across its installed cores); request at most 3.8Gi" +
					"; memory 8Gi is above the 3.8Gi that 2 core(s) get on cluster cluster-b (each node's memory split evenly across its installed cores); request at most 3.8Gi"},
			} {
				job := coresJob("j", tier, 2)
				job.Job.Memory = "8Gi"
				store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{job}}}}
				l, _, _ := mixedLoop(store, coreBoxes{cores: map[string]int64{"cluster-a": 8, "cluster-b": 8}, memory: map[string]int64{"cluster-a": small, "cluster-b": tc.bigMemory}}, nil)

				tickOnce(t, l)

				if got := store.submitted["j"]; got != tc.placed {
					t.Errorf("%s: submitted on %q, want %q (reason %q)", name, got, tc.placed, store.notAdmitted["j"])
				}
				if got := store.notAdmitted["j"]; tc.reason != "" && got != tc.reason {
					t.Errorf("%s: reason\n %q, want\n %q", name, got, tc.reason)
				}
			}
		})
	}
}

func (coreBoxes) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (coreBoxes) GetClusterIDs(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
