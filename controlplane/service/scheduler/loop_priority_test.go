package scheduler

import (
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// An org's two clusters and one of the platform's, ranked above them by the platform's org. The
// names sort against the priorities, so a search that still went by name would answer differently.
func rankedClusters() map[string]domain.Cluster {
	return map[string]domain.Cluster{
		"z-first":    {Name: "z-first", OrgID: "org-a", Priority: 500},
		"m-second":   {Name: "m-second", OrgID: "org-a", Priority: 100},
		"a-platform": {Name: "a-platform", OrgID: domain.PlatformOrgID, Priority: 900},
	}
}

func accelerators(acceleratorTypes map[string]int64) domain.Footprint {
	return domain.CapacityFootprint(16, acceleratorTypes, 64<<30, 64<<30)
}

// The priority decides where a job that fits everywhere goes, and a full cluster passes the job
// down: it never waits for a higher cluster while a lower one has room.
func TestTickPlacesAJobOnTheHighestPriorityClusterWithRoom(t *testing.T) {
	for name, tc := range map[string]struct {
		clusters map[string]orgCluster
		want     string
	}{
		"all have room": {map[string]orgCluster{
			"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4}}, "z-first"},
		"the first is full": {map[string]orgCluster{
			"z-first": {free: 0, installed: 4}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4}}, "m-second"},
		"both of the org's own are full": {map[string]orgCluster{
			"z-first": {free: 0, installed: 4}, "m-second": {free: 0, installed: 4}, "a-platform": {free: 2, installed: 4}}, "a-platform"},
		"the first is away": {map[string]orgCluster{
			"m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4}}, "m-second"},
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
				"z-first": {free: 2, installed: 2}, "m-second": {free: 1, installed: 1}, "a-platform": {free: 8, installed: 8},
			}, orgJob("a1", "org-a", tier, 1), orgJob("a2", "org-a", tier, 1), orgJob("a3", "org-a", tier, 1), orgJob("a4", "org-a", tier, 1))

			tickOnce(t, l)

			on := map[string]int{}
			for _, id := range []string{"a1", "a2", "a3", "a4"} {
				cluster, admitted := store.submitted[id]
				if !admitted {
					t.Fatalf("%s was not placed with room on the platform cluster (reason %q)", id, store.notAdmitted[id])
				}
				on[cluster]++
			}
			if want := map[string]int{"z-first": 2, "m-second": 1, "a-platform": 1}; !reflect.DeepEqual(on, want) {
				t.Fatalf("jobs per cluster = %v, want %v", on, want)
			}
		})
	}
}

// Another org has no say in the order: its jobs see the platform cluster alone, and an org's own
// priority is its own.
func TestPriorityOrdersOnlyTheClustersThatTakeTheJob(t *testing.T) {
	store := &orgStore{registered: rankedClusters()}
	l := orgLoop(store, map[string]orgCluster{
		"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4},
	}, orgJob("b1", "org-b", domain.CapacityBurst, 1))

	tickOnce(t, l)

	if got := store.submitted["b1"]; got != "a-platform" {
		t.Fatalf("org b's job was placed on %q, want the platform cluster", got)
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
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4},
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
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4},
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
		"z-first": {free: 0, installed: 4, autoscaler: true}, "m-second": {free: 0, installed: 4, autoscaler: true}, "a-platform": {free: 0, installed: 4, autoscaler: true},
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
		"z-first": {free: 2, installed: 4}, "m-second": {free: 2, installed: 4}, "a-platform": {free: 2, installed: 4},
	}
	for name, tc := range map[string]struct {
		ago    time.Duration
		failed []string
		want   string
	}{
		"failed on the first a moment ago": {time.Minute, []string{"id-z-first"}, "m-second"},
		"failed on both of the org's own":  {time.Minute, []string{"id-z-first", "id-m-second"}, "a-platform"},
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
			if got := slices.Sorted(maps.Keys(kept)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
	}
}
