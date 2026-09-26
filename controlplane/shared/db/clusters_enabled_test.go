package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func (w *twoOrgs) setEnabled(t *testing.T, cluster, org string, enabled bool) *domain.Cluster {
	t.Helper()
	c, err := w.clusters.SetClusterEnabled(context.Background(), cluster, org, "user-test", enabled)
	if err != nil {
		t.Fatalf("SetClusterEnabled(%s, %s, %v): %v", cluster, org, enabled, err)
	}
	return c
}

func TestAClaimSkipsAClusterItsOrgTurnedOffAndTakesItAgainOnceOn(t *testing.T) {
	w := newTwoOrgs(t)
	running := w.placed(t, "a-running", w.peA, w.ofA, domain.StatusRunning)
	submitted := w.placed(t, "a-submitted", w.peA, w.ofA, domain.StatusSubmitted)

	off := w.setEnabled(t, w.ofA, w.orgA, false)
	if off.Hosts(w.orgA) || off.EnabledFor(w.orgA) || !off.OpenTo(w.orgA) {
		t.Fatalf("the cluster after its org turned it off: %+v, want it open to the org and not hosting it", off)
	}
	job := w.queued(t, "a-next", w.peA)
	if w.claim(t, job, w.ofA) {
		t.Fatal("a job was placed on a cluster its org turned off")
	}
	if got := w.get(t, job); got.Status != domain.StatusQueued || got.ClusterName != "" {
		t.Fatalf("after the refused claim: status %s on %q, want it still queued", got.Status, got.ClusterName)
	}
	for id, want := range map[string]domain.ExperimentStatus{running: domain.StatusRunning, submitted: domain.StatusSubmitted} {
		if got := w.get(t, id); got.Status != want || got.ClusterName != w.ofA {
			t.Fatalf("job %s already on the cluster: status %s on %q, want it left %s there", id, got.Status, got.ClusterName, want)
		}
	}

	// Turning it off twice is one decision, and the first one's author stays.
	w.setEnabled(t, w.ofA, w.orgA, false)
	var rows int
	if err := w.pool.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM org_disabled_clusters WHERE cluster_name = $1 AND org_id = $2 AND disabled_by = 'user-test'`, w.ofA, w.orgA).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows for the turned-off cluster = %d, %v, want one", rows, err)
	}

	on := w.setEnabled(t, w.ofA, w.orgA, true)
	if !on.Hosts(w.orgA) {
		t.Fatalf("the cluster after its org turned it on again: %+v, want it hosting the org", on)
	}
	w.setEnabled(t, w.ofA, w.orgA, true)
	if !w.claim(t, job, w.ofA) {
		t.Fatal("a job was refused on a cluster its org turned on again")
	}
}

func TestOneOrgTurningASharedClusterOffLeavesItOnForTheOther(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofA, true); err != nil {
		t.Fatal(err)
	}
	w.setEnabled(t, w.ofA, w.orgB, false)

	ofB, ofA := w.queued(t, "b-job", w.peB), w.queued(t, "a-job", w.peA)
	if w.claim(t, ofB, w.ofA) {
		t.Fatal("a job of the org that turned the shared cluster off was placed on it")
	}
	if !w.claim(t, ofA, w.ofA) {
		t.Fatal("a job of the org that left the shared cluster on was refused")
	}
	if !w.claim(t, ofB, w.ofB) {
		t.Fatal("a job was refused on its org's own cluster, which is on")
	}

	registered, err := w.clusters.RegisteredClusters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := registered[w.ofA]; c.Hosts(w.orgB) || !c.Hosts(w.orgA) || !slices.Equal(c.DisabledOrgs, []string{w.orgB}) {
		t.Fatalf("the shared cluster as admission reads it: %+v, want it off for org b alone", c)
	}
	hosting, err := w.clusters.ClustersHosting(ctx, w.orgB)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, c := range hosting {
		listed[c.Name] = c.EnabledFor(w.orgB)
	}
	if on, ok := listed[w.ofA]; !ok || on {
		t.Fatalf("the clusters listed to org b = %v, want the shared one among them and off", listed)
	}
	if !listed[w.ofB] {
		t.Fatalf("the clusters listed to org b = %v, want its own on", listed)
	}
}

func TestAClusterNotOpenToAnOrgIsNotItsToTurnOff(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	for _, name := range []string{w.ofB, w.unregisteredName} {
		for _, enabled := range []bool{false, true} {
			if _, err := w.clusters.SetClusterEnabled(ctx, name, w.orgA, "user-test", enabled); !errors.Is(err, ErrClusterNotFound) {
				t.Fatalf("SetClusterEnabled(%s, org a, %v) = %v, want ErrClusterNotFound", name, enabled, err)
			}
		}
	}
	var rows int
	if err := w.pool.pool.QueryRow(ctx, `SELECT count(*) FROM org_disabled_clusters WHERE org_id = $1`, w.orgA).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows written for clusters that are not the org's = %d, %v, want none", rows, err)
	}
}

func TestRemovingAClusterForgetsWhoTurnedItOff(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	w.setEnabled(t, w.ofA, w.orgA, false)
	if _, err := w.clusters.RemoveCluster(ctx, w.ofA); err != nil {
		t.Fatal(err)
	}
	again, err := w.clusters.RegisterCluster(ctx, w.ofA, w.orgA)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Hosts(w.orgA) {
		t.Fatalf("a cluster registered again under a removed name: %+v, want it on", again)
	}
}

// A claim and the decision to turn a cluster off take the same lock, so a claim that waits
// behind the decision reads it.
func TestAClaimWaitingBehindAClusterBeingTurnedOffIsRefused(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	job := w.queued(t, "a-late", w.peA)

	tx, err := w.pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, clusterAdmissionLock, w.ofA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO org_disabled_clusters (cluster_name, org_id, disabled_by) VALUES ($1, $2, 'user-test')`, w.ofA, w.orgA); err != nil {
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
		t.Fatalf("the claim finished (%v, %v) while the cluster was being turned off, want it to wait", got.claimed, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || got.claimed {
		t.Fatalf("the claim after the cluster was turned off = %v, %v, want it refused", got.claimed, got.err)
	}
}

// The other order: a claim already deciding holds the lock, the decision to turn the cluster off
// waits for it, and the job it placed stays.
func TestTurningAClusterOffWaitsForAClaimInFlightAndLeavesItsJob(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	job := w.queued(t, "a-first", w.peA)

	deciding, release := make(chan struct{}), make(chan struct{})
	claimed := make(chan error, 1)
	go func() {
		ok, err := w.exps.ClaimSubmitted(ctx, job, w.ofA, &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}, func(context.Context, []*domain.Experiment) (bool, error) {
			close(deciding)
			<-release
			return true, nil
		})
		if err == nil && !ok {
			err = errors.New("the claim was refused")
		}
		claimed <- err
	}()
	<-deciding
	off := make(chan error, 1)
	go func() {
		_, err := w.clusters.SetClusterEnabled(ctx, w.ofA, w.orgA, "user-test", false)
		off <- err
	}()
	select {
	case err := <-off:
		t.Fatalf("the cluster was turned off (%v) while a claim on it was deciding, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-claimed; err != nil {
		t.Fatalf("the claim in flight: %v", err)
	}
	if err := <-off; err != nil {
		t.Fatalf("turning the cluster off: %v", err)
	}
	if got := w.get(t, job); got.Status != domain.StatusSubmitted || got.ClusterName != w.ofA {
		t.Fatalf("the job placed before the cluster was turned off: status %s on %q, want it left there", got.Status, got.ClusterName)
	}
	if w.claim(t, w.queued(t, "a-second", w.peA), w.ofA) {
		t.Fatal("a job was placed after the cluster was turned off")
	}
}
