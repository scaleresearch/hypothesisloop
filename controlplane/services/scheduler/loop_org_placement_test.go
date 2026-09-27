package scheduler

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ownClusters registers every cluster of avail for the org of a job that names none, which is
// every job of a deployment with one org.
func ownClusters(avail map[string]domain.Footprint) map[string]domain.Cluster {
	out := make(map[string]domain.Cluster, len(avail))
	for name := range avail {
		out[name] = domain.Cluster{Name: name}
	}
	return out
}

// singleOrg is a deployment with one org: every cluster these tests name is registered and is
// the job's own.
var singleOrg = func() map[string]domain.Cluster {
	out := map[string]domain.Cluster{}
	for _, name := range []string{
		"cluster-a", "cluster-b", "cluster-z", "cluster-idle", "cluster-busy", "cluster-h100", "cluster-amd",
		"c1", "c2", "a-cluster", "b-cluster", "a-no-l40-cluster", "z-saturated-l40-cluster", "cluster",
	} {
		out[name] = domain.Cluster{Name: name}
	}
	return out
}()

// Every store fake of these tests is a deployment with one org.
func (*disbalanceStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*preemptStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*resolveTestStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*speculationStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*nilCallbackClaimStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*pinningStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*resilienceStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*claimStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}
func (*noRunningListStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return singleOrg, nil
}

// orgCluster is one connected cluster of the placement tests: a single node holding h100
// accelerators, some of them free.
type orgCluster struct {
	free, installed int64
	autoscaler      bool
	multiNode       bool
}

type orgWorkload struct {
	LoopWorkloadClient
	clusters map[string]orgCluster
}

func (w orgWorkload) footprints(count func(orgCluster) int64) map[string]domain.Footprint {
	out := map[string]domain.Footprint{}
	for name, c := range w.clusters {
		out[name] = domain.CapacityFootprint(16, map[string]int64{h100: count(c)}, 64<<30, 64<<30)
	}
	return out
}

func (w orgWorkload) perNode(value func(orgCluster) map[string]int64) map[string]map[string]map[string]int64 {
	out := map[string]map[string]map[string]int64{}
	for name, c := range w.clusters {
		out[name] = map[string]map[string]int64{"node-" + name: value(c)}
	}
	return out
}

func (w orgWorkload) capability(has func(orgCluster) bool) map[string]bool {
	out := map[string]bool{}
	for name, c := range w.clusters {
		out[name] = has(c)
	}
	return out
}

func (w orgWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func(c orgCluster) int64 { return c.free }
	return w.footprints(free), w.footprints(free), nil
}
func (w orgWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return w.footprints(func(c orgCluster) int64 { return c.installed }), nil
}
func (w orgWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(func(c orgCluster) map[string]int64 { return map[string]int64{h100: c.free} }), nil
}
func (w orgWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(func(orgCluster) map[string]int64 { return holdNodeTotal() }), nil
}
func (w orgWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return w.perNode(func(orgCluster) map[string]int64 { return holdNodeTotal() }), nil
}
func (w orgWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	out := map[string]map[string]map[string]string{}
	for name := range w.clusters {
		out[name] = map[string]map[string]string{"node-" + name: {}}
	}
	return out, nil
}
func (w orgWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return w.capability(func(c orgCluster) bool { return c.multiNode }), nil
}
func (w orgWorkload) GetAutoscalerCapability(context.Context) (map[string]bool, error) {
	return w.capability(func(c orgCluster) bool { return c.autoscaler }), nil
}
func (w orgWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for name := range w.clusters {
		out[name] = "id-" + name
	}
	return out, nil
}
func (orgWorkload) CreateWorkload(context.Context, *domain.Experiment) error { return nil }

// orgStore is a deployment with several orgs: registered says whose each cluster is.
type orgStore struct {
	terminatingStore
	registered map[string]domain.Cluster
}

func (s *orgStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return s.registered, nil
}
func (s *orgStore) ListSubmittedExperiments(context.Context) ([]*domain.Experiment, error) {
	return nil, nil
}
func (s *orgStore) GetClusterSettings(context.Context, string) (*domain.ClusterSettings, error) {
	return nil, nil
}
func (s *orgStore) RecentlyTriedClusters(context.Context, time.Duration) (map[string]bool, error) {
	return nil, nil
}

func orgJob(id, orgID string, tier domain.CapacityTier, count int) *domain.Experiment {
	exp := holdJob(id, tier, h100, count, "1")
	exp.OrgID = orgID
	return exp
}

// The clusters of three orgs in one deployment: org-a's and org-b's own, and one that org-staff
// registered and the platform shares.
func threeOrgClusters() map[string]domain.Cluster {
	return map[string]domain.Cluster{
		"of-a":   {Name: "of-a", OrgID: "org-a"},
		"of-b":   {Name: "of-b", OrgID: "org-b"},
		"shared": {Name: "shared", OrgID: "org-staff", Shared: true},
	}
}

func orgLoop(store *orgStore, clusters map[string]orgCluster, queued ...*domain.Experiment) *Loop {
	store.queued = queued
	l := holdLoop(store, orgWorkload{clusters: clusters}, noopEvictor{})
	l.settler = &recordingSettler{}
	return l.WithSpeculation(15 * time.Minute)
}

func tickOnce(t *testing.T, l *Loop) {
	t.Helper()
	if err := l.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v, want nil", err)
	}
}

func TestTickPlacesAJobOnlyOnAClusterOfItsOrgOrASharedOne(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: threeOrgClusters()}
			clusters := map[string]orgCluster{
				"of-a":   {free: 4, installed: 4},
				"of-b":   {free: 4, installed: 4},
				"shared": {free: 8, installed: 8},
			}
			l := orgLoop(store, clusters,
				orgJob("a1", "org-a", tier, 1), orgJob("a2", "org-a", tier, 1),
				orgJob("b1", "org-b", tier, 1), orgJob("b2", "org-b", tier, 1),
				orgJob("c1", "org-c", tier, 1), orgJob("c2", "org-c", tier, 1))

			tickOnce(t, l)

			allowed := map[string][]string{
				"a1": {"of-a", "shared"}, "a2": {"of-a", "shared"},
				"b1": {"of-b", "shared"}, "b2": {"of-b", "shared"},
				"c1": {"shared"}, "c2": {"shared"},
			}
			for id, clusters := range allowed {
				got, admitted := store.submitted[id]
				if !admitted {
					t.Errorf("job %s was not admitted (reason %q) with room on %v", id, store.notAdmitted[id], clusters)
					continue
				}
				if !slices.Contains(clusters, got) {
					t.Errorf("job %s was placed on %s, want one of %v", id, got, clusters)
				}
			}
		})
	}
}

// The other org's cluster has all the room and the job's own has none: the job waits for its own.
func TestTickKeepsAJobWaitingForItsOwnClusterBesideAnotherOrgsIdleOne(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: threeOrgClusters()}
			l := orgLoop(store, map[string]orgCluster{"of-a": {free: 0, installed: 4}, "of-b": {free: 4, installed: 4}},
				orgJob("a1", "org-a", tier, 1))

			tickOnce(t, l)

			if cluster, admitted := store.submitted["a1"]; admitted {
				t.Fatalf("org-a's job was placed on %s", cluster)
			}
			if got := store.notAdmitted["a1"]; !strings.HasPrefix(got, domain.NotAdmittedCapacityUnavailable) {
				t.Fatalf("not_admitted_reason = %q, want %s: its own cluster is full", got, domain.NotAdmittedCapacityUnavailable)
			}
		})
	}
}

func TestTickTellsAJobWhoseOrgHasNoClusterSo(t *testing.T) {
	registered := map[string]domain.Cluster{
		"of-a":      {Name: "of-a", OrgID: "org-a"},
		"of-b":      {Name: "of-b", OrgID: "org-b"},
		"of-a-auto": {Name: "of-a-auto", OrgID: "org-a"},
	}
	for name, clusters := range map[string]map[string]orgCluster{
		"only other orgs' clusters are connected": {"of-a": {free: 4, installed: 4}, "of-b": {free: 4, installed: 4}},
		"another org's cluster scales up":         {"of-a-auto": {installed: 4, autoscaler: true}},
		"a cluster nobody registered has room":    {"phantom": {free: 4, installed: 4, autoscaler: true}},
		"nothing is connected":                    {},
	} {
		for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
			t.Run(name+"/"+string(tier), func(t *testing.T) {
				store := &orgStore{registered: registered}
				l := orgLoop(store, clusters, orgJob("c1", "org-c", tier, 1))

				tickOnce(t, l)

				if cluster, admitted := store.submitted["c1"]; admitted {
					t.Fatalf("a job of an org with no cluster was placed on %s", cluster)
				}
				if got := store.notAdmitted["c1"]; got != domain.NotAdmittedNoClusterForOrg {
					t.Fatalf("not_admitted_reason = %q, want %q", got, domain.NotAdmittedNoClusterForOrg)
				}
			})
		}
	}
}

// An org with a cluster registered has a cluster, connected or not. Its job waits for it, and the
// reason it reads is the same whatever other orgs have connected, so it learns nothing of them.
func TestTickDoesNotCallAnAwayClusterMissing(t *testing.T) {
	for name, registered := range map[string]map[string]domain.Cluster{
		"its own cluster is away":    {"of-a": {Name: "of-a", OrgID: "org-a"}, "of-c": {Name: "of-c", OrgID: "org-c"}},
		"the shared cluster is away": threeOrgClusters(),
	} {
		reasons := map[string]string{}
		for others, clusters := range map[string]map[string]orgCluster{
			"another org's cluster is connected": {"of-a": {free: 4, installed: 4}},
			"nothing is connected":               {},
		} {
			for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
				t.Run(name+"/"+others+"/"+string(tier), func(t *testing.T) {
					store := &orgStore{registered: registered}
					l := orgLoop(store, clusters, orgJob("c1", "org-c", tier, 1))

					tickOnce(t, l)

					if cluster, admitted := store.submitted["c1"]; admitted {
						t.Fatalf("the job was placed on %s, a cluster of another org", cluster)
					}
					got := store.notAdmitted["c1"]
					if got == domain.NotAdmittedNoClusterForOrg {
						t.Fatalf("not_admitted_reason = %q for an org whose cluster is registered and away", got)
					}
					if seen, ok := reasons[string(tier)]; ok && seen != got {
						t.Fatalf("not_admitted_reason = %q with %s and %q otherwise: the reason tells an org what others have connected", got, others, seen)
					}
					reasons[string(tier)] = got
				})
			}
		}
	}
}

// A name nobody registered reports capacity only when a staff token pushed it. It is no cluster:
// a job waits for its own full cluster beside it.
func TestTickNeverPlacesOnAClusterNobodyRegistered(t *testing.T) {
	store := &orgStore{registered: threeOrgClusters()}
	l := orgLoop(store, map[string]orgCluster{
		"of-a":    {free: 0, installed: 4},
		"phantom": {free: 4, installed: 4, autoscaler: true},
	}, orgJob("guaranteed", "org-a", domain.CapacityGuaranteed, 1), orgJob("burst", "org-a", domain.CapacityBurst, 1))

	tickOnce(t, l)

	if len(store.submitted) != 0 {
		t.Fatalf("placed %v with only an unregistered cluster free", store.submitted)
	}
}

// With no live room anywhere a guaranteed job bets on a cluster that scales up. The bet goes to
// its own org's cluster or a shared one, never to another org's.
func TestTickSpeculatesOnlyOnAClusterThatHostsTheJob(t *testing.T) {
	registered := threeOrgClusters()
	for name, tc := range map[string]struct {
		clusters map[string]orgCluster
		want     string
	}{
		"another org's cluster scales up, the job's own does not": {
			map[string]orgCluster{"of-a": {installed: 4}, "of-b": {installed: 4, autoscaler: true}}, ""},
		"another org's and a shared cluster scale up": {
			map[string]orgCluster{"of-a": {installed: 4}, "of-b": {installed: 4, autoscaler: true}, "shared": {installed: 4, autoscaler: true}}, "shared"},
		"the job's own cluster scales up": {
			map[string]orgCluster{"of-a": {installed: 4, autoscaler: true}, "of-b": {installed: 4, autoscaler: true}}, "of-a"},
	} {
		t.Run(name, func(t *testing.T) {
			store := &orgStore{registered: registered}
			l := orgLoop(store, tc.clusters, orgJob("a1", "org-a", domain.CapacityGuaranteed, 1))

			tickOnce(t, l)

			if got := store.submitted["a1"]; got != tc.want {
				t.Fatalf("speculative submit went to %q, want %q (reason %q)", got, tc.want, store.notAdmitted["a1"])
			}
		})
	}
}

// A job returned to the queue after an infrastructure fault on its own cluster carries that
// cluster in its tried list. Its next placement stays inside its org and the shared clusters.
func TestTickFailsOverOnlyToAClusterThatHostsTheJob(t *testing.T) {
	failedOver := func() *domain.Experiment {
		exp := orgJob("a1", "org-a", domain.CapacityGuaranteed, 1)
		exp.InfraRequeueCount = 1
		exp.TriedClusters = []domain.TriedCluster{{ClusterID: "id-of-a", At: time.Now()}}
		return exp
	}
	t.Run("only another org's cluster is left", func(t *testing.T) {
		store := &orgStore{registered: threeOrgClusters()}
		l := orgLoop(store, map[string]orgCluster{
			"of-a": {installed: 4, autoscaler: true},
			"of-b": {free: 4, installed: 4, autoscaler: true},
		}, failedOver())

		tickOnce(t, l)

		if cluster, admitted := store.submitted["a1"]; admitted {
			t.Fatalf("failed over to %s, a cluster of another org", cluster)
		}
		if got := store.notAdmitted["a1"]; got != domain.NotAdmittedNoScalableCapacity {
			t.Fatalf("not_admitted_reason = %q, want %q: the one cluster that hosts the job was just tried", got, domain.NotAdmittedNoScalableCapacity)
		}
	})
	t.Run("a shared cluster is left", func(t *testing.T) {
		store := &orgStore{registered: threeOrgClusters()}
		l := orgLoop(store, map[string]orgCluster{
			"of-a":   {installed: 4, autoscaler: true},
			"of-b":   {free: 4, installed: 4, autoscaler: true},
			"shared": {installed: 4, autoscaler: true},
		}, failedOver())

		tickOnce(t, l)

		if got := store.submitted["a1"]; got != "shared" {
			t.Fatalf("failed over to %q, want the shared cluster (reason %q)", got, store.notAdmitted["a1"])
		}
	})
}

func runningBurst(id, orgID, cluster string) *domain.Experiment {
	exp := orgJob(id, orgID, domain.CapacityBurst, 1)
	exp.ClusterName, exp.Status = cluster, domain.StatusRunning
	exp.EstimatedDurationHours, exp.CreatedAt = 10, time.Now().Add(-time.Hour)
	return exp
}

// Tiers are the platform's on a shared cluster: a guaranteed job of one org preempts a burst job
// of another there. On a cluster of another org it preempts nothing, because it cannot land there.
func TestTickPreemptsAcrossOrgsOnASharedClusterOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster   string
		preempted bool
	}{
		"a shared cluster":      {"shared", true},
		"another org's cluster": {"of-b", false},
	} {
		t.Run(name, func(t *testing.T) {
			victim := runningBurst("victim", "org-b", tc.cluster)
			registered := threeOrgClusters()
			delete(registered, "of-a")
			if !tc.preempted {
				delete(registered, "shared")
			}
			store := &orgStore{registered: registered}
			store.running = []*domain.Experiment{victim}
			l := orgLoop(store, map[string]orgCluster{tc.cluster: {free: 0, installed: 1}},
				orgJob("a1", "org-a", domain.CapacityGuaranteed, 1))
			l.observed = observedOnNode([]string{"victim"}, "node-"+tc.cluster)

			tickOnce(t, l)

			if got := len(store.requeued) == 1; got != tc.preempted {
				t.Fatalf("preempted %v on %s, want preempted = %v", store.requeued, tc.cluster, tc.preempted)
			}
			if _, admitted := store.submitted["a1"]; admitted {
				t.Fatal("the guaranteed job was admitted before its capacity was free")
			}
			if !tc.preempted && store.notAdmitted["a1"] != domain.NotAdmittedNoClusterForOrg {
				t.Fatalf("not_admitted_reason = %q, want %q", store.notAdmitted["a1"], domain.NotAdmittedNoClusterForOrg)
			}
		})
	}
}

// A pin is written by admission alone, so one naming a cluster that does not host the job was
// written around it. It is refused, where a pin used to be taken at its word.
func TestTickRefusesAPinOnAClusterThatDoesNotHostTheJob(t *testing.T) {
	for _, pinned := range []string{"of-b", "phantom"} {
		t.Run(pinned, func(t *testing.T) {
			exp := orgJob("a1", "org-a", domain.CapacityBurst, 1)
			exp.ClusterName = pinned
			store := &orgStore{registered: threeOrgClusters()}
			l := orgLoop(store, map[string]orgCluster{"of-a": {free: 4, installed: 4}, pinned: {free: 4, installed: 4}}, exp)

			tickOnce(t, l)

			if cluster, admitted := store.submitted["a1"]; admitted {
				t.Fatalf("a job pinned to %s was placed on %s", pinned, cluster)
			}
			if got := store.notAdmitted["a1"]; got != domain.NotAdmittedNoClusterForOrg {
				t.Fatalf("not_admitted_reason = %q, want %q", got, domain.NotAdmittedNoClusterForOrg)
			}
		})
	}
}

func TestAMultiNodeJobIsOfferedOnlyHostingClustersThatSpanNodes(t *testing.T) {
	avail := map[string]domain.Footprint{"of-a": {cpuKey: 100000}, "of-b": {cpuKey: 100000}, "shared": {cpuKey: 100000}}
	exp := groupedTwoNodeExperiment()
	exp.OrgID = "org-a"

	candidates, ineligible := eligibleClusters(avail, map[string]bool{"of-b": true}, threeOrgClusters(), exp)
	if len(candidates) != 0 || ineligible != domain.NotAdmittedNoMultiNodeCluster {
		t.Fatalf("got %v, %q, want no cluster and %q: the only one that spans nodes is another org's", candidates, ineligible, domain.NotAdmittedNoMultiNodeCluster)
	}
	candidates, ineligible = eligibleClusters(avail, map[string]bool{"of-b": true, "shared": true}, threeOrgClusters(), exp)
	if _, ok := candidates["shared"]; len(candidates) != 1 || !ok || ineligible != "" {
		t.Fatalf("got %v, %q, want only the shared cluster", candidates, ineligible)
	}
}

// One org, every cluster its own: the filter returns what it was given, value for value.
func TestADeploymentWithOneOrgSeesEveryConnectedCluster(t *testing.T) {
	avail := map[string]domain.Footprint{"cluster-a": {cpuKey: 1}, "cluster-b": {cpuKey: 2}, "c1": {cpuKey: 3}}
	exp := &domain.Experiment{Job: domain.JobSpec{Image: "registry.test/job:1", CPU: "1", Memory: "1Gi", Storage: "1Gi"}}

	candidates, ineligible := eligibleClusters(avail, map[string]bool{}, singleOrg, exp)
	if ineligible != "" || len(candidates) != len(avail) {
		t.Fatalf("got %v, %q, want all of %v", candidates, ineligible, avail)
	}
	candidates["cluster-a"][cpuKey] = 0
	if avail["cluster-a"][cpuKey] != 0 {
		t.Fatal("the offered clusters do not share their footprints with the tick's own view, so a reservation made through one is lost")
	}
}

// twoOrgsOnClusterA is cluster-a of the hold fixtures registered for two orgs' tests: shared by
// the platform, or org-b's own. cluster-b is org-b's in both.
func twoOrgsOnClusterA(shared bool) map[string]domain.Cluster {
	clusterA := domain.Cluster{Name: "cluster-a", OrgID: "org-b"}
	if shared {
		clusterA = domain.Cluster{Name: "cluster-a", OrgID: "org-staff", Shared: true}
	}
	return map[string]domain.Cluster{"cluster-a": clusterA, "cluster-b": {Name: "cluster-b", OrgID: "org-b"}}
}

// A job that strands idle accelerators is evicted for a job of another org only on a cluster
// that takes both. On a cluster of its own org it is not evicted for a job that cannot land
// there, however its requests look.
func TestTickEvictsADisbalancedJobForAnotherOrgOnASharedClusterOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		shared  bool
		evicted []string
	}{
		"a shared cluster":      {true, []string{"hog"}},
		"another org's cluster": {false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			// 14 cores for one of four accelerators leaves half a core and three idle accelerators.
			hog := holdJob("hog", domain.CapacityBurst, h100, 1, "14")
			hog.OrgID, hog.ClusterName, hog.Status = "org-b", "cluster-a", domain.StatusRunning
			hog.Job.Memory = "8Gi"
			blocked := holdJob("blocked", domain.CapacityBurst, h100, 1, "2")
			blocked.OrgID = "org-a"
			blocked.Job.Memory = "8Gi"

			store := &orgStore{registered: twoOrgsOnClusterA(tc.shared)}
			store.t, store.queued, store.running = t, []*domain.Experiment{blocked}, []*domain.Experiment{hog}
			evictor := &recordingEvictor{}
			l := holdLoop(store, holdWorkload{cpuCores: 0.5}, evictor)
			l.settler = &recordingSettler{}
			l.observed = observedOnNode([]string{"hog"}, "node-a")

			tickOnce(t, l)

			if !slices.Equal(evictor.evicted, tc.evicted) {
				t.Fatalf("evicted %v, want %v", evictor.evicted, tc.evicted)
			}
			if _, admitted := store.submitted["blocked"]; admitted {
				t.Fatalf("the blocked job was placed on %s, where nothing was free for it", store.submitted["blocked"])
			}
			if !tc.shared && store.notAdmitted["blocked"] != domain.NotAdmittedNoClusterForOrg {
				t.Fatalf("not_admitted_reason = %q, want %q", store.notAdmitted["blocked"], domain.NotAdmittedNoClusterForOrg)
			}
		})
	}
}

type twoOrgMaxStore struct {
	resolvedCPUStore
	registered map[string]domain.Cluster
}

func (s *twoOrgMaxStore) RegisteredClusters(context.Context) (map[string]domain.Cluster, error) {
	return s.registered, nil
}

// "max" is a share of the node per accelerator held, whoever holds it: two orgs' jobs on a
// shared cluster resolve to the same share, and it is the share a job gets on its org's own
// cluster, where the other org's job resolves to nothing because it is not placed.
func TestMaxResolvesToTheSameShareForTwoOrgsOnASharedCluster(t *testing.T) {
	resolve := func(registered map[string]domain.Cluster) map[string]string {
		ofA := holdJob("of-a", domain.CapacityBurst, h100, 1, domain.MaxResourceSentinel)
		ofB := holdJob("of-b", domain.CapacityBurst, h100, 1, domain.MaxResourceSentinel)
		ofA.OrgID, ofB.OrgID = "org-a", "org-b"
		ofA.ClusterName, ofB.ClusterName = "cluster-a", "cluster-a"
		store := &twoOrgMaxStore{registered: registered}
		store.t, store.queued = t, []*domain.Experiment{ofA, ofB}
		l := holdLoop(store, holdWorkload{cpuCores: 16}, noopEvictor{})
		tickOnce(t, l)
		return store.cpu
	}

	shared := resolve(twoOrgsOnClusterA(true))
	if shared["of-a"] == "" || shared["of-a"] != shared["of-b"] {
		t.Fatalf("resolved cpu on a shared cluster = %q for org a and %q for org b, want the same share for both", shared["of-a"], shared["of-b"])
	}

	own := resolve(twoOrgsOnClusterA(false))
	if own["of-b"] != shared["of-b"] {
		t.Fatalf("resolved cpu on the org's own cluster = %q, want the %q it gets on a shared one", own["of-b"], shared["of-b"])
	}
	if cpu, placed := own["of-a"]; placed {
		t.Fatalf("org a's job resolved %q on a cluster of org b, where it is never placed", cpu)
	}
}

// An org turns a cluster off for its own jobs, its own cluster or a shared one. Its jobs go to
// what it left on, whichever tier they are, and the other orgs' jobs are placed as before.
func TestTickNeverPlacesAJobOnAClusterItsOrgTurnedOff(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			registered := threeOrgClusters()
			shared := registered["shared"]
			shared.DisabledOrgs = []string{"org-a"}
			registered["shared"] = shared
			ofB := registered["of-b"]
			ofB.DisabledOrgs = []string{"org-b"}
			registered["of-b"] = ofB
			store := &orgStore{registered: registered}
			l := orgLoop(store, map[string]orgCluster{
				"of-a":   {free: 1, installed: 1},
				"of-b":   {free: 4, installed: 4},
				"shared": {free: 8, installed: 8},
			},
				orgJob("a1", "org-a", tier, 1), orgJob("a2", "org-a", tier, 1),
				orgJob("b1", "org-b", tier, 1), orgJob("c1", "org-c", tier, 1))

			tickOnce(t, l)

			if got := store.submitted["a1"]; got != "of-a" {
				t.Errorf("a1 was placed on %q, want of-a, the one cluster its org left on", got)
			}
			if cluster, admitted := store.submitted["a2"]; admitted {
				t.Errorf("a2 was placed on %s with its org's only enabled cluster full", cluster)
			}
			if got, _ := domain.SplitQueueReason(store.notAdmitted["a2"]); got != domain.NotAdmittedOutranked {
				t.Errorf("a2 not_admitted_reason = %q, want %q: a cluster its org left on exists and a1 took its room", got, domain.NotAdmittedOutranked)
			}
			for _, id := range []string{"b1", "c1"} {
				if got := store.submitted[id]; got != "shared" {
					t.Errorf("%s was placed on %q, want shared", id, got)
				}
			}
		})
	}
}

// A job waiting only because its org turned off every cluster it could use says so, and is told
// something else than an org that has no cluster at all.
func TestTickTellsAJobWhoseOrgTurnedEveryClusterOffSo(t *testing.T) {
	registered := map[string]domain.Cluster{
		"of-a":   {Name: "of-a", OrgID: "org-a", DisabledOrgs: []string{"org-a"}},
		"shared": {Name: "shared", OrgID: "org-staff", Shared: true, DisabledOrgs: []string{"org-a", "org-c"}},
	}
	for name, clusters := range map[string]map[string]orgCluster{
		"both connected with room": {"of-a": {free: 4, installed: 4}, "shared": {free: 4, installed: 4}},
		"one scales up":            {"shared": {installed: 4, autoscaler: true}},
		"nothing is connected":     {},
	} {
		for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
			t.Run(name+"/"+string(tier), func(t *testing.T) {
				store := &orgStore{registered: registered}
				l := orgLoop(store, clusters, orgJob("a1", "org-a", tier, 1), orgJob("c1", "org-c", tier, 1), orgJob("d1", "org-d", tier, 1))

				tickOnce(t, l)

				for _, id := range []string{"a1", "c1"} {
					if cluster, admitted := store.submitted[id]; admitted {
						t.Fatalf("%s was placed on %s, which its org turned off", id, cluster)
					}
					if got := store.notAdmitted[id]; got != domain.NotAdmittedClustersDisabled {
						t.Fatalf("%s not_admitted_reason = %q, want %q", id, got, domain.NotAdmittedClustersDisabled)
					}
				}
				if _, connected := clusters["shared"]; connected && !clusters["shared"].autoscaler {
					if got := store.submitted["d1"]; got != "shared" {
						t.Fatalf("d1 of an org that left the shared cluster on was placed on %q", got)
					}
				}
			})
		}
	}
}

// Nothing running is evicted to make room on a cluster the waiting job cannot land on: its org
// turned the cluster off, so it is no candidate for preemption either.
func TestTickPreemptsNothingOnAClusterTheJobsOrgTurnedOff(t *testing.T) {
	victim := runningBurst("victim", "org-b", "shared")
	registered := threeOrgClusters()
	delete(registered, "of-a")
	shared := registered["shared"]
	shared.DisabledOrgs = []string{"org-a"}
	registered["shared"] = shared
	store := &orgStore{registered: registered}
	store.running = []*domain.Experiment{victim}
	l := orgLoop(store, map[string]orgCluster{"shared": {free: 0, installed: 1}},
		orgJob("a1", "org-a", domain.CapacityGuaranteed, 1), orgJob("a2", "org-a", domain.CapacityBurst, 1))
	l.observed = observedOnNode([]string{"victim"}, "node-shared")

	tickOnce(t, l)

	if len(store.requeued) != 0 {
		t.Fatalf("preempted %v for a job that cannot land on the cluster", store.requeued)
	}
	for _, id := range []string{"a1", "a2"} {
		if got := store.notAdmitted[id]; got != domain.NotAdmittedClustersDisabled {
			t.Fatalf("%s not_admitted_reason = %q, want %q", id, got, domain.NotAdmittedClustersDisabled)
		}
	}
}

// A cluster that boots one image of its own runs the jobs that name none or exactly it, and
// every other cluster runs a job in the image the job names. A job is offered only the clusters
// that run it, and when none does the reason says which image runs where.
func TestAJobIsOfferedOnlyTheClustersThatRunItsImage(t *testing.T) {
	const platformImage = "registry.test/platform-job:7"
	registered := map[string]domain.Cluster{
		"rented": {Name: "rented", OrgID: "org-a", JobImage: platformImage},
		"k8s":    {Name: "k8s", OrgID: "org-a"},
	}
	avail := func() map[string]domain.Footprint {
		return map[string]domain.Footprint{"rented": {cpuKey: 100000}, "k8s": {cpuKey: 100000}}
	}
	job := func(image string) *domain.Experiment {
		return &domain.Experiment{OrgID: "org-a", Job: domain.JobSpec{Image: image, CPU: "1", Memory: "1Gi", Storage: "1Gi"}}
	}
	for name, c := range map[string]struct {
		image string
		want  []string
	}{
		"no image goes where one is fixed":            {"", []string{"rented"}},
		"its own image goes where jobs bring theirs":  {"registry.test/mine:1", []string{"k8s"}},
		"the fixed image named runs on both clusters": {platformImage, []string{"k8s", "rented"}},
	} {
		candidates, ineligible := eligibleClusters(avail(), map[string]bool{}, registered, job(c.image))
		var got []string
		for cluster := range candidates {
			got = append(got, cluster)
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) || ineligible != "" {
			t.Errorf("%s: offered %v (%q), want %v", name, got, ineligible, c.want)
		}
	}

	onlyRented := map[string]domain.Cluster{"rented": registered["rented"]}
	candidates, ineligible := eligibleClusters(avail(), map[string]bool{}, onlyRented, job("registry.test/mine:1"))
	want := "no_cluster_for_image: the job names image registry.test/mine:1, and rented runs every job in " + platformImage + ": leave job.image out to run there"
	if len(candidates) != 0 || ineligible != want {
		t.Errorf("an image no cluster runs: offered %v, reason %q\nwant %q", candidates, ineligible, want)
	}
	onlyK8s := map[string]domain.Cluster{"k8s": registered["k8s"]}
	candidates, ineligible = eligibleClusters(avail(), map[string]bool{}, onlyK8s, job(""))
	if len(candidates) != 0 || ineligible != "no_cluster_for_image: the job names no image, and every cluster that takes this org's jobs runs the image a job names: set job.image (hl capacity lists the images offered)" {
		t.Errorf("no image and no cluster with one: offered %v, reason %q", candidates, ineligible)
	}
	// A cluster the org turned off is no place the reason sends the job to.
	off := map[string]domain.Cluster{"rented": {Name: "rented", OrgID: "org-a", JobImage: platformImage, DisabledOrgs: []string{"org-a"}}, "k8s": registered["k8s"]}
	if _, ineligible = eligibleClusters(avail(), map[string]bool{}, off, job("")); strings.Contains(ineligible, "rented") {
		t.Errorf("the reason names a cluster the org turned off: %q", ineligible)
	}
}

// A queued job that exceeds what every cluster running its image choice has installed is told
// its size is the cause only where no other image choice would be served.
func TestAJobHeldByItsImageChoiceIsNotToldItAsksForTooMuch(t *testing.T) {
	const platformImage = "registry.test/platform-job:7"
	registered := map[string]domain.Cluster{
		"rented": {Name: "rented", OrgID: "org-a", JobImage: platformImage},
		"metal":  {Name: "metal", OrgID: "org-a"},
	}
	small := domain.CapacityFootprint(2, nil, 8<<30, 100<<30)
	large := domain.CapacityFootprint(16, nil, 64<<30, 500<<30)
	exp := &domain.Experiment{OrgID: "org-a", Job: domain.JobSpec{CPU: "8", Memory: "1Gi", Storage: "1Gi"}}

	got := exceedsInstalledReason(map[string]domain.Footprint{"rented": small, "metal": large}, nil, registered, exp)
	want := "no_cluster_for_image: the job names no image, so it runs only on a cluster with one job image (rented), and none has installed what it asks for, " +
		"8 cores, 1 GiB memory, 1 GiB storage. A cluster that runs the image a job names has (metal): set job.image (hl capacity lists the images offered)"
	if got != want {
		t.Errorf("a cluster the image choice rules out has it installed:\n got %q\nwant %q", got, want)
	}
	if got := exceedsInstalledReason(map[string]domain.Footprint{"rented": small, "metal": small}, nil, registered, exp); got != exceedsClusterCapacityReason(exp) {
		t.Errorf("no cluster has it installed: got %q, want %q", got, exceedsClusterCapacityReason(exp))
	}
}

func (*orgStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }

func (*twoOrgMaxStore) MarkHeldJobs(context.Context, time.Duration) error { return nil }
