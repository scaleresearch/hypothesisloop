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
	registered := threeOrgClusters()
	registered["of-a-auto"] = domain.Cluster{Name: "of-a-auto", OrgID: "org-a"}
	for name, clusters := range map[string]map[string]orgCluster{
		"only other orgs' clusters are connected":   {"of-a": {free: 4, installed: 4}, "of-b": {free: 4, installed: 4}},
		"another org's cluster scales up":           {"of-a-auto": {installed: 4, autoscaler: true}},
		"the shared cluster is registered and away": {"of-b": {free: 4, installed: 4}},
		"a cluster nobody registered has room":      {"phantom": {free: 4, installed: 4, autoscaler: true}},
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
			store := &orgStore{registered: threeOrgClusters()}
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
	exp := &domain.Experiment{Job: domain.JobSpec{CPU: "1", Memory: "1Gi", Storage: "1Gi"}}

	candidates, ineligible := eligibleClusters(avail, map[string]bool{}, singleOrg, exp)
	if ineligible != "" || len(candidates) != len(avail) {
		t.Fatalf("got %v, %q, want all of %v", candidates, ineligible, avail)
	}
	candidates["cluster-a"][cpuKey] = 0
	if avail["cluster-a"][cpuKey] != 0 {
		t.Fatal("the offered clusters do not share their footprints with the tick's own view, so a reservation made through one is lost")
	}
}
