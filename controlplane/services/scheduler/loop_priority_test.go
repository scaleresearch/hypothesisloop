package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// An org's two clusters and the platform's shared one. The names sort against the priorities, so
// a search that still went by name would answer differently.
func rankedClusters() map[string]domain.Cluster {
	return map[string]domain.Cluster{
		"z-first":  {Name: "z-first", OrgID: "org-a", Priority: 500},
		"m-second": {Name: "m-second", OrgID: "org-a", Priority: 100},
		"a-shared": {Name: "a-shared", OrgID: "org-staff", Shared: true, Priority: domain.SharedClusterPriority},
	}
}

func accelerators(flavors map[string]int64) domain.Footprint {
	return domain.CapacityFootprint(16, flavors, 64<<30, 64<<30)
}

// free is clusters of one node each, holding the given accelerators free: what the clusters have
// in total and what their one node has.
func free(byCluster map[string]map[string]int64) (avail map[string]domain.Footprint, nodes, resources map[string]map[string]map[string]int64) {
	avail, nodes, resources = map[string]domain.Footprint{}, map[string]map[string]map[string]int64{}, map[string]map[string]map[string]int64{}
	for cluster, flavors := range byCluster {
		avail[cluster] = accelerators(flavors)
		nodes[cluster] = map[string]map[string]int64{"node-" + cluster: flavors}
		resources[cluster] = map[string]map[string]int64{"node-" + cluster: holdNodeTotal()}
	}
	return avail, nodes, resources
}

func TestPlacementTiersGoByPriorityThenName(t *testing.T) {
	registered := rankedClusters()
	registered["b-second"] = domain.Cluster{Name: "b-second", OrgID: "org-a", Priority: 100}
	candidates := map[string]domain.Footprint{"a-shared": nil, "m-second": nil, "z-first": nil, "b-second": nil, "unregistered": nil}

	got := placementTiers(candidates, registered)

	want := [][]string{{"z-first"}, {"b-second", "m-second"}, {"a-shared", "unregistered"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placementTiers = %v, want %v", got, want)
	}
	if order := placementOrder(candidates, registered); !reflect.DeepEqual(order, []string{"z-first", "b-second", "m-second", "a-shared", "unregistered"}) {
		t.Fatalf("placementOrder = %v", order)
	}
	if got := placementTiers(nil, registered); len(got) != 0 {
		t.Fatalf("placementTiers of no candidates = %v, want none", got)
	}
}

// The priority decides where a job that fits everywhere goes, and a full cluster passes the job
// down: it never waits for a higher cluster while a lower one has room.
func TestTickPlacesAJobOnTheHighestPriorityClusterWithRoom(t *testing.T) {
	for name, tc := range map[string]struct {
		clusters map[string]orgCluster
		want     string
	}{
		"all have room": {map[string]orgCluster{
			"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4}}, "z-first"},
		"the first is full": {map[string]orgCluster{
			"z-first": {free: 0, installed: 4}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4}}, "m-second"},
		"both of the org's own are full": {map[string]orgCluster{
			"z-first": {free: 0, installed: 4}, "m-second": {free: 0, installed: 4}, "a-shared": {free: 2, installed: 4}}, "a-shared"},
		"the first is away": {map[string]orgCluster{
			"m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4}}, "m-second"},
	} {
		for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
			t.Run(name+"/"+string(tier), func(t *testing.T) {
				store := &orgStore{registered: rankedClusters()}
				l := orgLoop(store, tc.clusters, orgJob("a1", "org-a", tier, 1))

				tickOnce(t, l)

				if got := store.submitted["a1"]; got != tc.want {
					t.Fatalf("placed on %q, want %q (reason %q)", got, tc.want, store.notAdmitted["a1"])
				}
			})
		}
	}
}

// A tick fills the higher cluster and spills the rest to the lower one, job by job.
func TestTickSpillsToTheNextPriorityOnceTheHigherIsFull(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: rankedClusters()}
			l := orgLoop(store, map[string]orgCluster{
				"z-first": {free: 2, installed: 2}, "m-second": {free: 1, installed: 1}, "a-shared": {free: 8, installed: 8},
			}, orgJob("a1", "org-a", tier, 1), orgJob("a2", "org-a", tier, 1), orgJob("a3", "org-a", tier, 1), orgJob("a4", "org-a", tier, 1))

			tickOnce(t, l)

			on := map[string]int{}
			for _, id := range []string{"a1", "a2", "a3", "a4"} {
				cluster, admitted := store.submitted[id]
				if !admitted {
					t.Fatalf("%s was not placed with room on the shared cluster (reason %q)", id, store.notAdmitted[id])
				}
				on[cluster]++
			}
			if want := map[string]int{"z-first": 2, "m-second": 1, "a-shared": 1}; !reflect.DeepEqual(on, want) {
				t.Fatalf("jobs per cluster = %v, want %v", on, want)
			}
		})
	}
}

// Another org has no say in the order: its jobs see the shared cluster alone, and an org's own
// priority is its own.
func TestPriorityOrdersOnlyTheClustersThatTakeTheJob(t *testing.T) {
	store := &orgStore{registered: rankedClusters()}
	l := orgLoop(store, map[string]orgCluster{
		"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4},
	}, orgJob("b1", "org-b", domain.CapacityBurst, 1))

	tickOnce(t, l)

	if got := store.submitted["b1"]; got != "a-shared" {
		t.Fatalf("org b's job was placed on %q, want the shared cluster", got)
	}
}

// The search is tier, then type, then cluster. A higher cluster at an acceptable alternative
// comes before a lower cluster at the type asked for, and inside one tier the type asked for
// comes before the name.
func TestTheClusterTierIsSearchedBeforeTheAcceleratorType(t *testing.T) {
	job := func() *domain.Experiment {
		exp := holdJob("a1", domain.CapacityBurst, h100, 1, "1")
		exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40}
		return exp
	}
	for name, tc := range map[string]struct {
		free        map[string]map[string]int64
		wantCluster string
		wantFlavor  domain.AcceleratorType
	}{
		"the higher cluster has only the alternative": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 1}, "m-second": map[string]int64{h100: 1}},
			"z-first", l40},
		"the higher cluster has both": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 1, h100: 1}, "m-second": map[string]int64{h100: 1}},
			"z-first", h100},
		"the higher cluster has neither free": {
			map[string]map[string]int64{"z-first": map[string]int64{l40: 0, h100: 0}, "m-second": map[string]int64{l40: 1}},
			"m-second", l40},
		"one tier, the type asked for is on the later name": {
			map[string]map[string]int64{"b-second": map[string]int64{l40: 1}, "m-second": map[string]int64{h100: 1}},
			"m-second", h100},
	} {
		t.Run(name, func(t *testing.T) {
			registered := rankedClusters()
			registered["b-second"] = domain.Cluster{Name: "b-second", OrgID: "org-a", Priority: 100}
			exp := job()

			avail, nodes, resources := free(tc.free)

			cluster, fp := resolveClusterAndFootprint(avail, registered, nodes, resources, nil, exp)

			if cluster != tc.wantCluster || exp.AcceleratorType != tc.wantFlavor {
				t.Fatalf("resolved to %q at %q, want %q at %q", cluster, exp.AcceleratorType, tc.wantCluster, tc.wantFlavor)
			}
			if !domain.Fits(avail[cluster], fp) {
				t.Fatalf("the footprint returned does not fit the cluster chosen: %v", fp)
			}
		})
	}
}

// A job that already ran keeps its flavor (candidateAcceleratorTypes), and is placed again by the
// same order as any other.
func TestARequeuedJobIsPlacedAgainByPriority(t *testing.T) {
	exp := holdJob("a1", domain.CapacityBurst, l40, 1, "1")
	exp.Job.AcceleratorType = h100
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40}
	exp.EvictionReason = string(domain.EvictionPreemptedForGuaranteed)
	avail, nodes, resources := free(map[string]map[string]int64{"z-first": {h100: 1}, "m-second": {l40: 1}, "a-shared": {l40: 1}})

	cluster, _ := resolveClusterAndFootprint(avail, rankedClusters(), nodes, resources, nil, exp)

	if cluster != "m-second" || exp.AcceleratorType != l40 {
		t.Fatalf("resolved to %q at %q, want the highest cluster with the flavor it ran on, m-second at %q", cluster, exp.AcceleratorType, l40)
	}
}

// A job reverted to the flavor it asked for, because its quota does not cover the alternative,
// is placed by the same order.
func TestPlaceAtFlavorTakesTheHighestPriorityClusterThatFits(t *testing.T) {
	exp := holdJob("a1", domain.CapacityBurst, h100, 1, "1")
	avail, nodes, resources := free(map[string]map[string]int64{"z-first": {h100: 1}, "m-second": {h100: 1}, "a-shared": {h100: 1}})
	if cluster, ok := placeAtFlavor(avail, rankedClusters(), nodes, resources, nil, exp); !ok || cluster != "z-first" {
		t.Fatalf("placeAtFlavor = %q, %v, want z-first", cluster, ok)
	}
	avail, nodes, resources = free(map[string]map[string]int64{"z-first": {h100: 0}, "m-second": {h100: 1}, "a-shared": {h100: 1}})
	if cluster, ok := placeAtFlavor(avail, rankedClusters(), nodes, resources, nil, exp); !ok || cluster != "m-second" {
		t.Fatalf("placeAtFlavor with the first full = %q, %v, want m-second", cluster, ok)
	}
}

// With no cluster fitting, room is made on the highest-priority cluster that has the hardware,
// whatever a lower one is short of.
func TestThePreemptionTargetIsTheHighestPriorityClusterWithTheHardware(t *testing.T) {
	need := holdJob("a1", domain.CapacityGuaranteed, h100, 2, "1").Footprint()
	registered := rankedClusters()
	for name, tc := range map[string]struct {
		avail map[string]domain.Footprint
		want  string
	}{
		"both are full, the lower is short of less": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{h100: 0}), "m-second": accelerators(map[string]int64{h100: 1})}, "z-first"},
		"the higher has no such hardware": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{l40: 4}), "m-second": accelerators(map[string]int64{h100: 0})}, "m-second"},
		"a lower one fits outright": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{h100: 1}), "a-shared": accelerators(map[string]int64{h100: 2})}, "a-shared"},
		"nobody has the hardware": {
			map[string]domain.Footprint{"z-first": accelerators(map[string]int64{l40: 4}), "m-second": accelerators(map[string]int64{l40: 4})}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clusterWithBestFit(tc.avail, registered, need); got != tc.want {
				t.Fatalf("clusterWithBestFit = %q, want %q", got, tc.want)
			}
		})
	}
}

// A guaranteed job with nowhere to fit takes its room from the burst work on the cluster its org
// ranked first, and leaves the lower ones alone.
func TestTickPreemptsOnTheHighestPriorityCluster(t *testing.T) {
	high, low := runningBurst("on-first", "org-a", "z-first"), runningBurst("on-second", "org-a", "m-second")
	store := &orgStore{registered: rankedClusters()}
	store.running = []*domain.Experiment{low, high}
	l := orgLoop(store, map[string]orgCluster{"z-first": {free: 0, installed: 1}, "m-second": {free: 0, installed: 1}},
		orgJob("a1", "org-a", domain.CapacityGuaranteed, 1))
	l.observed = fakeObserved{node: map[string]string{"on-first": "node-z-first", "on-second": "node-m-second"}}

	tickOnce(t, l)

	if !reflect.DeepEqual(store.requeued, []string{"on-first"}) {
		t.Fatalf("preempted %v, want the burst job on the highest-priority cluster alone", store.requeued)
	}
}

// A guaranteed job asks a scaling cluster its org ranked higher to scale up before it takes room
// on a lower cluster. A burst job never bets, and takes the room.
func TestAHigherPriorityScalingClusterGetsItsBetBeforeALowerClusterIsUsed(t *testing.T) {
	clusters := map[string]orgCluster{
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4},
	}
	for tier, want := range map[domain.CapacityTier]string{domain.CapacityGuaranteed: "z-first", domain.CapacityBurst: "m-second"} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: rankedClusters()}
			l := orgLoop(store, clusters, orgJob("a1", "org-a", tier, 1))

			tickOnce(t, l)

			if got := store.submitted["a1"]; got != want {
				t.Fatalf("placed on %q, want %q (reason %q)", got, want, store.notAdmitted["a1"])
			}
		})
	}
}

// The bet is bounded: once it timed out the cluster is in the job's tried list, and the job takes
// the room the lower cluster has.
func TestAJobWhoseBetOnTheHigherClusterTimedOutTakesTheLowerCluster(t *testing.T) {
	exp := orgJob("a1", "org-a", domain.CapacityGuaranteed, 1)
	exp.TriedClusters = []domain.TriedCluster{{ClusterID: "id-z-first", At: time.Now()}}
	store := &orgStore{registered: rankedClusters()}
	l := orgLoop(store, map[string]orgCluster{
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4},
	}, exp)

	tickOnce(t, l)

	if got := store.submitted["a1"]; got != "m-second" {
		t.Fatalf("placed on %q, want m-second (reason %q)", got, store.notAdmitted["a1"])
	}
}

// Inside one priority a cluster with room is used before another is asked to scale, and a scaling
// cluster ranked below the one with room is never asked.
func TestALiveFitIsTakenBeforeABetOfTheSameOrALowerPriority(t *testing.T) {
	for name, registered := range map[string]map[string]domain.Cluster{
		"the same priority": {
			"a-scaling": {Name: "a-scaling", OrgID: "org-a", Priority: 100},
			"z-room":    {Name: "z-room", OrgID: "org-a", Priority: 100},
		},
		"the scaling one is lower": {
			"a-scaling": {Name: "a-scaling", OrgID: "org-a", Priority: 50},
			"z-room":    {Name: "z-room", OrgID: "org-a", Priority: 100},
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &orgStore{registered: registered}
			l := orgLoop(store, map[string]orgCluster{
				"a-scaling": {free: 0, installed: 4, autoscaler: true}, "z-room": {free: 2, installed: 4},
			}, orgJob("a1", "org-a", domain.CapacityGuaranteed, 1))

			tickOnce(t, l)

			if got := store.submitted["a1"]; got != "z-room" {
				t.Fatalf("placed on %q, want the cluster with room (reason %q)", got, store.notAdmitted["a1"])
			}
		})
	}
}

// With nothing fitting anywhere, the bet goes to the highest-priority scaling cluster.
func TestTheBetGoesToTheHighestPriorityScalingCluster(t *testing.T) {
	store := &orgStore{registered: rankedClusters()}
	l := orgLoop(store, map[string]orgCluster{
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 0, installed: 4, autoscaler: true}, "a-shared": {free: 0, installed: 4, autoscaler: true},
	}, orgJob("a1", "org-a", domain.CapacityGuaranteed, 1))

	tickOnce(t, l)

	if got := store.submitted["a1"]; got != "z-first" {
		t.Fatalf("the bet went to %q, want z-first (reason %q)", got, store.notAdmitted["a1"])
	}
}

func failedOn(exp *domain.Experiment, ago time.Duration, clusterIDs ...string) *domain.Experiment {
	for _, id := range clusterIDs {
		exp.TriedClusters = append(exp.TriedClusters, domain.TriedCluster{ClusterID: id, At: time.Now().Add(-ago), Reason: string(domain.EvictionStuckPending)})
	}
	return exp
}

// A cluster that just failed a job is left out of its next placement, so the job goes to a lower
// one, both tiers alike. After the TTL the cluster is a candidate again.
func TestAJobIsNotPlacedBackOnTheClusterThatJustFailedIt(t *testing.T) {
	clusters := map[string]orgCluster{
		"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-shared": {free: 2, installed: 4},
	}
	for name, tc := range map[string]struct {
		ago    time.Duration
		failed []string
		want   string
	}{
		"failed on the first a moment ago": {time.Minute, []string{"id-z-first"}, "m-second"},
		"failed on both of the org's own":  {time.Minute, []string{"id-z-first", "id-m-second"}, "a-shared"},
		"failed on the first an hour ago":  {time.Hour, []string{"id-z-first"}, "z-first"},
		"failed on a cluster that is gone": {time.Minute, []string{"id-removed"}, "z-first"},
	} {
		for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
			t.Run(name+"/"+string(tier), func(t *testing.T) {
				store := &orgStore{registered: rankedClusters()}
				l := orgLoop(store, clusters, failedOn(orgJob("a1", "org-a", tier, 1), tc.ago, tc.failed...))

				tickOnce(t, l)

				if got := store.submitted["a1"]; got != tc.want {
					t.Fatalf("placed on %q, want %q (reason %q)", got, tc.want, store.notAdmitted["a1"])
				}
			})
		}
	}
}

// The other jobs of the queue are not held off the cluster: the failure is that job's own.
func TestAClusterThatFailedOneJobStillTakesTheOthers(t *testing.T) {
	store := &orgStore{registered: rankedClusters()}
	l := orgLoop(store, map[string]orgCluster{"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}},
		failedOn(orgJob("a1", "org-a", domain.CapacityBurst, 1), time.Minute, "id-z-first"), orgJob("a2", "org-a", domain.CapacityBurst, 1))

	tickOnce(t, l)

	if store.submitted["a1"] != "m-second" || store.submitted["a2"] != "z-first" {
		t.Fatalf("placed %v, want the failed job on m-second and the other on z-first", store.submitted)
	}
}

// A cluster that is the only one able to run the job stays a candidate however recently it
// failed it: leaving it out would leave the job with nowhere to go for the whole TTL.
func TestTheOnlyCompatibleClusterIsTriedAgainAtOnce(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: rankedClusters()}
			l := orgLoop(store, map[string]orgCluster{"z-first": {free: 2, installed: 4}},
				failedOn(orgJob("a1", "org-a", tier, 1), time.Minute, "id-z-first"))

			tickOnce(t, l)

			if got := store.submitted["a1"]; got != "z-first" {
				t.Fatalf("placed on %q, want z-first, its only cluster (reason %q)", got, store.notAdmitted["a1"])
			}
		})
	}
}

func TestWithoutFailedKeepsAFailedClusterOnlyWhenNothingElseCouldHostTheJob(t *testing.T) {
	now := time.Now()
	ids := map[string]string{"h": "id-h", "h2": "id-h2", "l": "id-l", "unbound": ""}
	job := func(acceptable ...domain.AcceleratorType) *domain.Experiment {
		exp := failedOn(holdJob("a1", domain.CapacityBurst, h100, 1, "1"), time.Minute, "id-h")
		exp.Job.AcceptableAcceleratorTypes = acceptable
		return exp
	}
	hasH100, hasL40 := accelerators(map[string]int64{h100: 0}), accelerators(map[string]int64{l40: 4})
	for name, tc := range map[string]struct {
		candidates map[string]domain.Footprint
		exp        *domain.Experiment
		ttl        time.Duration
		want       []string
	}{
		"another cluster has the hardware, full or not": {map[string]domain.Footprint{"h": hasH100, "h2": hasH100}, job(), time.Hour, []string{"h2"}},
		"the other cluster has other hardware":          {map[string]domain.Footprint{"h": hasH100, "l": hasL40}, job(), time.Hour, []string{"h", "l"}},
		"the other cluster has a type the job accepts":  {map[string]domain.Footprint{"h": hasH100, "l": hasL40}, job(l40), time.Hour, []string{"l"}},
		"it is the only cluster":                        {map[string]domain.Footprint{"h": hasH100}, job(), time.Hour, []string{"h"}},
		"the failure is older than the TTL":             {map[string]domain.Footprint{"h": hasH100, "h2": hasH100}, job(), 30 * time.Second, []string{"h", "h2"}},
		"no TTL is configured":                          {map[string]domain.Footprint{"h": hasH100, "h2": hasH100}, job(), 0, []string{"h", "h2"}},
		"a cluster with no identity is never left out":  {map[string]domain.Footprint{"unbound": hasH100, "h2": hasH100}, job(), time.Hour, []string{"h2", "unbound"}},
	} {
		t.Run(name, func(t *testing.T) {
			kept := withoutFailed(tc.candidates, ids, tc.exp, tc.ttl, now)
			if got := placementOrder(kept, nil); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
	}
}
