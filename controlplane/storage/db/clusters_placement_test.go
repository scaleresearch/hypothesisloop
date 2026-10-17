package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// twoOrgs is two orgs, each with a cluster and a platform experiment, and an agent to submit.
type twoOrgs struct {
	pool             *Pool
	clusters         *Store
	exps             *Store
	orgA, orgB       string
	ofA, ofB         string
	peA, peB, agent  string
	suffix           string
	unregisteredName string
}

func newTwoOrgs(t *testing.T) *twoOrgs {
	t.Helper()
	pool := eventsTestDB(t)
	accounts := NewStore(pool, 3)
	w := &twoOrgs{pool: pool, clusters: NewStore(pool, 3), exps: NewStore(pool, 3), suffix: uuid.New().String()[:8]}
	_, w.orgA = newTestUserHome(t, accounts)
	_, w.orgB = newTestUserHome(t, accounts)
	w.ofA, w.ofB, w.unregisteredName = "of-a-"+w.suffix, "of-b-"+w.suffix, "nobodys-"+w.suffix
	w.peA, w.peB, w.agent = "pe-a-"+w.suffix, "pe-b-"+w.suffix, "agent-"+w.suffix
	createTestAgent(t, pool, w.agent)
	createTestPEIn(t, pool, w.peA, defaultProjectOf(t, accounts, w.orgA).ID)
	createTestPEIn(t, pool, w.peB, defaultProjectOf(t, accounts, w.orgB).ID)
	for name, org := range map[string]string{w.ofA: w.orgA, w.ofB: w.orgB} {
		if _, err := w.clusters.RegisterCluster(context.Background(), name, org); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return w
}

// platformCluster registers a cluster of the platform's org, which serves every org. The
// database is shared by the package's tests, so it is removed when the test ends.
func (w *twoOrgs) platformCluster(t *testing.T) string {
	t.Helper()
	name := "of-platform-" + w.suffix
	if _, err := w.clusters.RegisterCluster(context.Background(), name, domain.PlatformOrgID); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	t.Cleanup(func() { _, _ = w.clusters.RemoveCluster(context.Background(), name) })
	return name
}

func (w *twoOrgs) queued(t *testing.T, id, peID string) string {
	t.Helper()
	seedExperimentForCapTest(t, w.pool, id+"-"+w.suffix, w.agent, peID, 1, domain.StatusQueued)
	return id + "-" + w.suffix
}

func (w *twoOrgs) claim(t *testing.T, id, cluster string) bool {
	t.Helper()
	refused, err := w.exps.ClaimSubmitted(context.Background(), id, cluster, &domain.JobSpec{AcceleratorCount: 1}, nil)
	if err != nil {
		t.Fatalf("ClaimSubmitted(%s, %s): %v", id, cluster, err)
	}
	return refused == nil
}

// placed puts a job of peID on cluster in the given placed status.
func (w *twoOrgs) placed(t *testing.T, id, peID, cluster string, status domain.ExperimentStatus) string {
	t.Helper()
	id = w.queued(t, id, peID)
	if !w.claim(t, id, cluster) {
		t.Fatalf("job %s was not placed on %s", id, cluster)
	}
	if status == domain.StatusRunning {
		if started, err := w.exps.MarkStarted(context.Background(), id); err != nil || !started {
			t.Fatalf("MarkStarted(%s) = %v, %v", id, started, err)
		}
	}
	return id
}

func (w *twoOrgs) get(t *testing.T, id string) *domain.Experiment {
	t.Helper()
	exp, err := w.exps.GetExperiment(context.Background(), id)
	if err != nil || exp == nil {
		t.Fatalf("GetExperiment(%s) = %+v, %v", id, exp, err)
	}
	return exp
}

func ids(exps []*domain.Experiment) map[string]bool {
	out := map[string]bool{}
	for _, exp := range exps {
		out[exp.ID] = true
	}
	return out
}

// The rule is held by the statement that places the job, whoever calls it and whatever they
// checked first.
func TestAClaimPlacesAJobOnlyOnARegisteredClusterThatTakesItsOrg(t *testing.T) {
	w := newTwoOrgs(t)
	job := w.queued(t, "job-a", w.peA)

	for _, cluster := range []string{w.ofB, w.unregisteredName} {
		if w.claim(t, job, cluster) {
			t.Fatalf("a job of org a was placed on %s", cluster)
		}
		if got := w.get(t, job); got.Status != domain.StatusQueued || got.ClusterName != "" {
			t.Fatalf("after a refused claim on %s: status %s on %q, want it still queued", cluster, got.Status, got.ClusterName)
		}
	}
	if !w.claim(t, job, w.platformCluster(t)) {
		t.Fatal("a job was refused on a cluster of the platform's org")
	}
	own := w.queued(t, "own-a", w.peA)
	if !w.claim(t, own, w.ofA) {
		t.Fatal("a job was refused on its own org's cluster")
	}
	if got := w.get(t, own); got.Status != domain.StatusSubmitted || got.ClusterName != w.ofA {
		t.Fatalf("a claimed job: status %s on %q, want SUBMITTED on %s", got.Status, got.ClusterName, w.ofA)
	}
}

// One org and its registered clusters, which is every deployment before tenancy: every claim a
// tick makes lands.
func TestAClaimInASingleOrgDeploymentLandsOnEveryRegisteredCluster(t *testing.T) {
	pool := eventsTestDB(t)
	clusters, exps := NewStore(pool, 3), NewStore(pool, 3)
	suffix := uuid.New().String()[:8]
	agent, pe := "agent-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, agent)
	createTestPE(t, pool, pe, 10)
	for i, name := range []string{"local-" + suffix, "second-" + suffix} {
		if _, err := clusters.RegisterCluster(context.Background(), name, "default"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = clusters.RemoveCluster(context.Background(), name) })
		id := "job-" + suffix + "-" + string(rune('a'+i))
		seedExperimentForCapTest(t, pool, id, agent, pe, 1, domain.StatusQueued)
		refused, err := exps.ClaimSubmitted(context.Background(), id, name, &domain.JobSpec{AcceleratorCount: 1}, nil)
		if err != nil || refused != nil {
			t.Fatalf("claim on %s = %v, %v, want it placed", name, refused, err)
		}
	}
}

func TestRemovingAClusterRequeuesItsJobsAndForgetsItsIdentityAndSettings(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	id := "id-a-" + w.suffix
	if err := w.clusters.BindReportedID(ctx, w.ofA, id); err != nil {
		t.Fatal(err)
	}
	timeout := 600
	if _, err := w.clusters.UpdateCluster(ctx, w.ofA, domain.ClusterSettings{StuckPendingTimeoutSeconds: &timeout}); err != nil {
		t.Fatal(err)
	}
	submitted := w.placed(t, "a-submitted", w.peA, w.ofA, domain.StatusSubmitted)
	running := w.placed(t, "a-running", w.peA, w.ofA, domain.StatusRunning)
	elsewhere := w.placed(t, "b-at-home", w.peB, w.ofB, domain.StatusRunning)

	if _, err := w.clusters.RemoveCluster(ctx, w.unregisteredName); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("remove a cluster nobody registered: got = %v, want = %v", err, ErrClusterNotFound)
	}
	requeued, err := w.clusters.RemoveCluster(ctx, w.ofA)
	if err != nil || len(requeued) != 2 {
		t.Fatalf("remove: requeued %v, %v, want the two jobs placed there", requeued, err)
	}
	for _, job := range []string{submitted, running} {
		if got := w.get(t, job); got.Status != domain.StatusQueued || got.ClusterName != "" || got.EvictionReason != string(domain.EvictionClusterWithdrawn) || got.RetriesUsed() != 0 {
			t.Errorf("%s after its cluster was removed: %s on %q, eviction %q, retries used %d", job, got.Status, got.ClusterName, got.EvictionReason, got.RetriesUsed())
		}
		if w.claim(t, job, w.ofA) {
			t.Errorf("%s was placed on a removed cluster", job)
		}
	}
	if got := w.get(t, elsewhere); got.Status != domain.StatusRunning || got.ClusterName != w.ofB {
		t.Errorf("a job on another cluster: %s on %q, want it untouched", got.Status, got.ClusterName)
	}
	if got, err := w.clusters.GetCluster(ctx, w.ofA); err != nil || got != nil {
		t.Fatalf("the removed cluster: got = %+v, %v, want no row, so its runtime token names nothing", got, err)
	}
	if got, err := w.clusters.GetClusterSettings(ctx, id); err != nil || got != nil {
		t.Fatalf("settings of the removed cluster: got = %+v, %v, want none", got, err)
	}
	if got, err := w.clusters.ClusterByReportedID(ctx, id); err != nil || got != nil {
		t.Fatalf("the identity of the removed cluster: bound to %+v, %v, want nothing", got, err)
	}

	// The name is free, and whoever takes it starts with nothing of the old cluster.
	if _, err := w.clusters.RegisterCluster(ctx, w.ofA, w.orgB); err != nil {
		t.Fatalf("register the freed name: %v", err)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, id); err != nil {
		t.Fatalf("bind the old identity to the new cluster: %v", err)
	}
	if got, err := w.clusters.GetClusterSettings(ctx, id); err != nil || got != nil {
		t.Fatalf("settings under the old identity: got = %+v, %v, want none inherited", got, err)
	}
}

func TestAClusterIdentityIsBoundOnceAndOnlyClearingReleasesIt(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	idA, rebuilt := "id-a-"+w.suffix, "id-a-rebuilt-"+w.suffix

	for _, invalid := range []string{"", "has/slash", "has space", "nul\x00byte", "../up", string(make([]byte, domain.MaxClusterIDLength+1))} {
		if err := w.clusters.BindReportedID(ctx, w.ofA, invalid); err == nil {
			t.Errorf("bind identity %q: got = nil, want it refused", invalid)
		}
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, idA); err != nil {
		t.Fatal(err)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, idA); err != nil {
		t.Fatalf("the same identity again: got = %v, want = nil", err)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofB, idA); !errors.Is(err, ErrClusterIDBound) {
		t.Fatalf("another cluster stating a bound identity: got = %v, want = %v", err, ErrClusterIDBound)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, rebuilt); !errors.Is(err, ErrClusterIDChanged) {
		t.Fatalf("a bound cluster stating another identity: got = %v, want = %v", err, ErrClusterIDChanged)
	}
	if got, _ := w.clusters.GetCluster(ctx, w.ofA); got.ReportedID != idA {
		t.Fatalf("after the refusals the cluster is bound to %q, want %q", got.ReportedID, idA)
	}

	// A cluster's settings are set by name, before any agent connects too, and read by the
	// scheduler under the identity bound to it. A field left out stays, and 0 clears one.
	timeout, limit, zero := 600, 8, 0
	if _, err := w.clusters.UpdateCluster(ctx, w.ofB, domain.ClusterSettings{MaxSpeculativeAccelerators: &limit}); err != nil {
		t.Fatalf("settings of a cluster no agent has connected to: %v", err)
	}
	if _, err := w.clusters.UpdateCluster(ctx, w.ofA, domain.ClusterSettings{StuckPendingTimeoutSeconds: &timeout, MaxSpeculativeAccelerators: &limit}); err != nil {
		t.Fatal(err)
	}
	changed, err := w.clusters.UpdateCluster(ctx, w.ofA, domain.ClusterSettings{MaxSpeculativeAccelerators: &zero})
	if err != nil || changed.MaxSpeculativeAccelerators != nil || changed.StuckPendingTimeoutSeconds == nil || *changed.StuckPendingTimeoutSeconds != timeout {
		t.Fatalf("clearing one setting: got = %+v, %v, want the cap gone and the timeout kept", changed, err)
	}
	if got, err := w.clusters.GetClusterSettings(ctx, idA); err != nil || got == nil || *got.StuckPendingTimeoutSeconds != timeout || got.MaxSpeculativeAccelerators != nil {
		t.Fatalf("settings under the bound identity: got = %+v, %v, want the timeout alone", got, err)
	}
	// The silence ceiling is read by name: a cluster that stopped reporting has no fresh identity.
	ceiling := 21600
	if _, err := w.clusters.UpdateCluster(ctx, w.ofA, domain.ClusterSettings{StatusSilenceCeilingSeconds: &ceiling}); err != nil {
		t.Fatal(err)
	}
	if got, err := w.clusters.StatusSilenceCeiling(ctx, w.ofA); err != nil || got == nil || *got != ceiling {
		t.Fatalf("silence ceiling by name: got = %v, %v, want %d", got, err, ceiling)
	}
	if got, err := w.clusters.StatusSilenceCeiling(ctx, w.unregisteredName); err != nil || got != nil {
		t.Fatalf("silence ceiling of an unregistered name: got = %v, %v, want none", got, err)
	}
	if _, err := w.clusters.UpdateCluster(ctx, w.ofA, domain.ClusterSettings{StatusSilenceCeilingSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.clusters.ClearReportedID(ctx, w.unregisteredName); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("clear for an unregistered name: got = %v, want = %v", err, ErrClusterNotFound)
	}
	cleared, err := w.clusters.ClearReportedID(ctx, w.ofA)
	if err != nil || cleared.ReportedID != "" || cleared.OrgID != w.orgA {
		t.Fatalf("clear: got = %+v, %v, want the cluster with no identity", cleared, err)
	}
	if cleared.StuckPendingTimeoutSeconds == nil {
		t.Fatal("clearing the identity took the settings of the named cluster with it")
	}
	// The identity the cluster gave up is free. Whoever states it next is read with its own
	// settings, never the ones of the cluster that held the identity before.
	if err := w.clusters.BindReportedID(ctx, w.ofB, idA); err != nil {
		t.Fatalf("bind a released identity: %v", err)
	}
	if got, err := w.clusters.GetClusterSettings(ctx, idA); err != nil || got == nil || got.StuckPendingTimeoutSeconds != nil || *got.MaxSpeculativeAccelerators != limit {
		t.Fatalf("settings under a released identity: got = %+v, %v, want those of the cluster it is bound to now", got, err)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, rebuilt); err != nil {
		t.Fatalf("bind the rebuilt cluster after clearing: %v", err)
	}
}
