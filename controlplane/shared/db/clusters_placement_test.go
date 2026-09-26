package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// twoOrgs is two orgs, each with a cluster and a platform experiment, and an agent to submit.
type twoOrgs struct {
	pool             *Pool
	clusters         *ClustersStore
	exps             *ExperimentsStore
	orgA, orgB       string
	ofA, ofB         string
	peA, peB, agent  string
	suffix           string
	unregisteredName string
}

func newTwoOrgs(t *testing.T) *twoOrgs {
	t.Helper()
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	w := &twoOrgs{pool: pool, clusters: NewClustersStore(pool), exps: NewExperimentsStore(pool), suffix: uuid.New().String()[:8]}
	_, w.orgA = newTestUserHome(t, accounts)
	_, w.orgB = newTestUserHome(t, accounts)
	w.ofA, w.ofB, w.unregisteredName = "of-a-"+w.suffix, "of-b-"+w.suffix, "nobodys-"+w.suffix
	w.peA, w.peB, w.agent = "pe-a-"+w.suffix, "pe-b-"+w.suffix, "agent-"+w.suffix
	createTestAgent(t, pool, w.agent, domain.AgentKindHuman)
	createTestPEIn(t, pool, w.peA, defaultProjectOf(t, accounts, w.orgA).ID)
	createTestPEIn(t, pool, w.peB, defaultProjectOf(t, accounts, w.orgB).ID)
	for name, org := range map[string]string{w.ofA: w.orgA, w.ofB: w.orgB} {
		if _, err := w.clusters.RegisterCluster(context.Background(), name, org); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	// The database is shared by the package's tests, and a shared cluster is every org's.
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `UPDATE clusters SET shared = false WHERE name = ANY($1)`, []string{w.ofA, w.ofB})
	})
	return w
}

func (w *twoOrgs) queued(t *testing.T, id, peID string) string {
	t.Helper()
	seedExperimentForCapTest(t, w.pool, id+"-"+w.suffix, w.agent, peID, 1, domain.StatusQueued)
	return id + "-" + w.suffix
}

func (w *twoOrgs) claim(t *testing.T, id, cluster string) bool {
	t.Helper()
	claimed, err := w.exps.ClaimSubmitted(context.Background(), id, cluster, &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}, nil)
	if err != nil {
		t.Fatalf("ClaimSubmitted(%s, %s): %v", id, cluster, err)
	}
	return claimed
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
	if _, _, err := w.clusters.SetClusterShared(context.Background(), w.ofB, true); err != nil {
		t.Fatal(err)
	}
	if !w.claim(t, job, w.ofB) {
		t.Fatal("a job was refused on a shared cluster")
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
	clusters, exps := NewClustersStore(pool), NewExperimentsStore(pool)
	suffix := uuid.New().String()[:8]
	agent, pe := "agent-"+suffix, "pe-"+suffix
	createTestAgent(t, pool, agent, domain.AgentKindHuman)
	createTestPE(t, pool, pe, 10)
	for i, name := range []string{"local-" + suffix, "second-" + suffix} {
		if _, err := clusters.RegisterCluster(context.Background(), name, "default"); err != nil {
			t.Fatal(err)
		}
		id := "job-" + suffix + "-" + string(rune('a'+i))
		seedExperimentForCapTest(t, pool, id, agent, pe, 1, domain.StatusQueued)
		claimed, err := exps.ClaimSubmitted(context.Background(), id, name, &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}, nil)
		if err != nil || !claimed {
			t.Fatalf("claim on %s = %v, %v, want it placed", name, claimed, err)
		}
	}
}

func TestEndingTheSharingOfAClusterRequeuesTheOtherOrgsJobsPlacedOnIt(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	queue := NewClusterQueueStore(w.pool)
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofA, true); err != nil {
		t.Fatal(err)
	}
	submitted := w.placed(t, "b-submitted", w.peB, w.ofA, domain.StatusSubmitted)
	running := w.placed(t, "b-running", w.peB, w.ofA, domain.StatusRunning)
	owners := w.placed(t, "a-running", w.peA, w.ofA, domain.StatusRunning)
	elsewhere := w.placed(t, "b-at-home", w.peB, w.ofB, domain.StatusRunning)
	before := map[string]*domain.Experiment{submitted: w.get(t, submitted), running: w.get(t, running)}

	if _, requeued, err := w.clusters.SetClusterShared(ctx, w.ofA, true); err != nil || len(requeued) != 0 {
		t.Fatalf("sharing a shared cluster again requeued %v, %v, want nothing", requeued, err)
	}
	cluster, requeued, err := w.clusters.SetClusterShared(ctx, w.ofA, false)
	if err != nil || cluster.Shared {
		t.Fatalf("stop sharing: got = %+v, %v", cluster, err)
	}
	if got := map[string]bool{}; len(requeued) != 2 {
		for _, id := range requeued {
			got[id] = true
		}
		t.Fatalf("requeued %v, want exactly the two jobs of org b placed on %s", got, w.ofA)
	}

	for id, was := range before {
		got := w.get(t, id)
		if got.Status != domain.StatusQueued || got.ClusterName != "" || got.ResolvedJob != nil ||
			got.NotAdmittedReason != domain.NotAdmittedAwaitingAdmission || got.EvictionReason != string(domain.EvictionClusterWithdrawn) {
			t.Errorf("%s after the sharing ended: %s on %q, reason %q, eviction %q, want it queued for a new placement", id, got.Status, got.ClusterName, got.NotAdmittedReason, got.EvictionReason)
		}
		if got.AttemptCount != was.AttemptCount+1 || got.InfraRequeueCount != was.InfraRequeueCount+1 || got.RetriesUsed() != was.RetriesUsed() {
			t.Errorf("%s: attempt %d, infra requeues %d, retries used %d, want the attempt advanced and no retry spent", id, got.AttemptCount, got.InfraRequeueCount, got.RetriesUsed())
		}
	}
	for id, cluster := range map[string]string{owners: w.ofA, elsewhere: w.ofB} {
		if got := w.get(t, id); got.Status != domain.StatusRunning || got.ClusterName != cluster {
			t.Errorf("%s: %s on %q, want it left running on %s", id, got.Status, got.ClusterName, cluster)
		}
	}

	desired, err := queue.ListDesiredWorkloads(ctx, w.ofA)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(desired); len(got) != 1 || !got[owners] {
		t.Fatalf("desired on %s = %v, want only its own org's job, so its runtime is handed no other org's token", w.ofA, got)
	}
	undesired, err := queue.ListRecentlyUndesiredWorkloads(ctx, w.ofA, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(undesired); len(got) != 2 || !got[submitted] || !got[running] {
		t.Fatalf("recently undesired on %s = %v, want the two requeued jobs", w.ofA, got)
	}
	if grants := domain.CheckpointWindowGrants(undesired); len(grants) != 2 {
		t.Fatalf("checkpoint windows = %v, want one for each job the platform withdrew", grants)
	}
	if other, err := queue.ListRecentlyUndesiredWorkloads(ctx, w.ofB, time.Minute); err != nil || len(other) != 0 {
		t.Fatalf("recently undesired on %s = %v, %v, want nothing: none of them ran there", w.ofB, ids(other), err)
	}

	if w.claim(t, submitted, w.ofA) {
		t.Fatal("a requeued job of org b was placed back on the cluster that stopped being shared")
	}
	if !w.claim(t, submitted, w.ofB) {
		t.Fatal("a requeued job was refused on its own org's cluster")
	}
}

// A claim and the end of sharing take the same lock, so a claim that waits behind the decision
// reads it.
func TestAClaimWaitingBehindTheEndOfSharingIsRefused(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofA, true); err != nil {
		t.Fatal(err)
	}
	job := w.queued(t, "b-late", w.peB)

	tx, err := w.pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, clusterAdmissionLock, w.ofA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE clusters SET shared = false WHERE name = $1`, w.ofA); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		claimed bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		claimed, err := w.exps.ClaimSubmitted(ctx, job, w.ofA, &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}, nil)
		done <- outcome{claimed, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the claim finished (%v, %v) while the cluster was being withdrawn, want it to wait", got.claimed, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || got.claimed {
		t.Fatalf("the claim after the sharing ended = %v, %v, want it refused", got.claimed, got.err)
	}
}

func TestRemovingAClusterRequeuesItsJobsAndForgetsItsIdentityAndSettings(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	settings := NewClusterSettingsStore(w.pool)
	id := "id-a-" + w.suffix
	if err := w.clusters.BindReportedID(ctx, w.ofA, id); err != nil {
		t.Fatal(err)
	}
	timeout := 600
	if err := settings.PutClusterSettings(ctx, &domain.ClusterSettings{ClusterID: id, StuckPendingTimeoutSeconds: &timeout}); err != nil {
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
	if got, err := settings.GetClusterSettings(ctx, id); err != nil || got != nil {
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
	if got, err := settings.GetClusterSettings(ctx, id); err != nil || got != nil {
		t.Fatalf("settings under the old identity: got = %+v, %v, want none inherited", got, err)
	}
}

func TestAClusterIdentityIsBoundOnceAndOnlyClearingReleasesIt(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	settings := NewClusterSettingsStore(w.pool)
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

	timeout := 600
	if err := settings.PutClusterSettings(ctx, &domain.ClusterSettings{ClusterID: idA, StuckPendingTimeoutSeconds: &timeout}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.clusters.ClearReportedID(ctx, w.unregisteredName); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("clear for an unregistered name: got = %v, want = %v", err, ErrClusterNotFound)
	}
	cleared, err := w.clusters.ClearReportedID(ctx, w.ofA)
	if err != nil || cleared.ReportedID != "" || cleared.OrgID != w.orgA {
		t.Fatalf("clear: got = %+v, %v, want the cluster with no identity", cleared, err)
	}
	if got, err := settings.GetClusterSettings(ctx, idA); err != nil || got != nil {
		t.Fatalf("settings under a cleared identity: got = %+v, %v, want them deleted", got, err)
	}
	// The identity the cluster gave up is free. Whoever states it next gets no settings with it.
	if err := w.clusters.BindReportedID(ctx, w.ofB, idA); err != nil {
		t.Fatalf("bind a released identity: %v", err)
	}
	if got, err := settings.GetClusterSettings(ctx, idA); err != nil || got != nil {
		t.Fatalf("settings inherited with a released identity: got = %+v, %v, want none", got, err)
	}
	if err := w.clusters.BindReportedID(ctx, w.ofA, rebuilt); err != nil {
		t.Fatalf("bind the rebuilt cluster after clearing: %v", err)
	}
}

// The write that places a job holds the image rule too (domain.Cluster.Runs): what a tick
// filtered in memory is checked again against the cluster's row as it stands.
func TestAClaimPlacesAJobOnlyWhereItsImageRuns(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	const platformImage = "registry.test/platform-job:7"
	setImage := func(id, image string) {
		t.Helper()
		if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET job_spec = job_spec - 'image' || CASE WHEN $2 = '' THEN '{}'::jsonb ELSE jsonb_build_object('image', $2::text) END WHERE id = $1`, id, image); err != nil {
			t.Fatal(err)
		}
	}
	named, bare, platform := w.queued(t, "named", w.peA), w.queued(t, "bare", w.peA), w.queued(t, "platform", w.peA)
	setImage(bare, "")
	setImage(platform, platformImage)

	// The cluster runs each job in its own image: a job that names none has nothing to run in.
	if w.claim(t, bare, w.ofA) {
		t.Fatal("a job with no image was placed on a cluster that has none of its own")
	}
	if got, err := w.clusters.GetCluster(ctx, w.ofA); err != nil || got.JobImage != "" {
		t.Fatalf("a cluster nothing was reported for: job_image %q, %v", got.JobImage, err)
	}

	// Its runtime reports one image for every job.
	for range 2 {
		if err := w.clusters.RecordJobImage(ctx, w.ofA, platformImage); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := w.clusters.GetCluster(ctx, w.ofA); got.JobImage != platformImage {
		t.Fatalf("job_image = %q, want the one reported", got.JobImage)
	}
	if registered, _ := w.clusters.RegisteredClusters(ctx); registered[w.ofA].JobImage != platformImage {
		t.Fatalf("the registered clusters do not carry the image: %+v", registered[w.ofA])
	}
	if w.claim(t, named, w.ofA) {
		t.Fatal("a job naming its own image was placed on a cluster that runs every job in another")
	}
	if got := w.get(t, named); got.Status != domain.StatusQueued || got.ClusterName != "" {
		t.Fatalf("after the refused claim: %s on %q, want still queued", got.Status, got.ClusterName)
	}
	for _, id := range []string{bare, platform} {
		if !w.claim(t, id, w.ofA) {
			t.Fatalf("%s was refused on the cluster whose image it runs in", id)
		}
	}

	// The runtime is replaced by one that runs a job in the image it names.
	if err := w.clusters.RecordJobImage(ctx, w.ofA, ""); err != nil {
		t.Fatal(err)
	}
	if !w.claim(t, named, w.ofA) {
		t.Fatal("a job naming an image was refused on a cluster that runs each job in its own")
	}
}
