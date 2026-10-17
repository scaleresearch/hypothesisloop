package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func sharedAs(shared bool) *bool { return &shared }

// Sharing and settings were two writes: a change of both whose second write failed had already
// ended the sharing and requeued the other orgs' jobs.
func TestAClusterUpdateThatFailsLeavesNothingChanged(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	timeout, other, negative := 600, 900, -1
	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofA, sharedAs(true), domain.ClusterSettings{StuckPendingTimeoutSeconds: &timeout}); err != nil {
		t.Fatal(err)
	}
	guest := w.placed(t, "b-running", w.peB, w.ofA, domain.StatusRunning)
	unchanged := func(after string) {
		t.Helper()
		byName, err := w.clusters.RegisteredClusters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c := byName[w.ofA]
		if !c.Shared || c.Priority != domain.SharedClusterPriority || c.StuckPendingTimeoutSeconds == nil || *c.StuckPendingTimeoutSeconds != timeout {
			t.Errorf("after %s: cluster = %+v, want it shared at the platform's priority with a timeout of %d", after, c, timeout)
		}
		if job := w.get(t, guest); job.Status != domain.StatusRunning || job.ClusterName != w.ofA {
			t.Errorf("after %s: the other org's job is %s on %q, want it running on %s", after, job.Status, job.ClusterName, w.ofA)
		}
	}

	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofA, sharedAs(false), domain.ClusterSettings{StuckPendingTimeoutSeconds: &negative}); err == nil {
		t.Fatal("an invalid setting: got = nil, want an error")
	}
	unchanged("a refused setting")

	// The database refuses the requeue of the other org's job, the last write of the transaction.
	refuse := "refuse_requeue_" + w.suffix
	t.Cleanup(func() {
		_, _ = w.pool.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+refuse+` ON experiments`)
		_, _ = w.pool.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+refuse+`()`)
	})
	for _, ddl := range []string{
		`CREATE FUNCTION ` + refuse + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.id = '` + guest + `' AND NEW.status = 'QUEUED' THEN RAISE EXCEPTION 'requeue refused'; END IF;
	RETURN NEW;
END $$`,
		`CREATE TRIGGER ` + refuse + ` BEFORE UPDATE ON experiments FOR EACH ROW EXECUTE FUNCTION ` + refuse + `()`,
	} {
		if _, err := w.pool.pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofA, sharedAs(false), domain.ClusterSettings{StuckPendingTimeoutSeconds: &other}); err == nil {
		t.Fatal("a requeue the database refused: got = nil, want an error")
	}
	unchanged("a failed requeue")
}

func TestAClusterBelongsToTheOrgThatRegisteredIt(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	clusters := NewClustersStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)
	name := "cluster-" + uuid.New().String()[:8]

	if _, found, err := clusters.ClusterOrg(ctx, name); err != nil || found {
		t.Fatalf("org of an unregistered cluster: got found = %v, %v, want false", found, err)
	}
	first, err := clusters.RegisterCluster(ctx, name, orgA)
	if err != nil || first.OrgID != orgA {
		t.Fatalf("register: got = %+v, %v, want it owned by %s", first, err, orgA)
	}
	again, err := clusters.RegisterCluster(ctx, name, orgA)
	if err != nil || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("register again by its owner: got = %+v, %v, want the same row", again, err)
	}
	if _, err := clusters.RegisterCluster(ctx, name, orgB); !errors.Is(err, ErrClusterNameTaken) {
		t.Fatalf("register a taken name from another org: got = %v, want = %v", err, ErrClusterNameTaken)
	}
	if _, err := clusters.RegisterCluster(ctx, "Not A Name", orgA); err == nil {
		t.Fatal("register an invalid name: got = nil, want an error")
	}
	if org, found, err := clusters.ClusterOrg(ctx, name); err != nil || !found || org != orgA {
		t.Fatalf("cluster org: got = %q, %v, %v, want %s", org, found, err, orgA)
	}
	byName, err := clusters.RegisteredClusters(ctx)
	if err != nil || byName[name].OrgID != orgA || byName[name].Shared {
		t.Fatalf("registered clusters: got = %+v, %v, want %s owned by %s and not shared", byName[name], err, name, orgA)
	}
	if _, err := clusters.RegisterCluster(ctx, "cluster-"+uuid.New().String()[:8], "org-missing"); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("register for an org that does not exist: got = %v, want = %v", err, ErrOrgNotFound)
	}
	if listed, err := clusters.ClustersHosting(ctx, orgA); err != nil || len(listed) != 1 || listed[0].Name != name {
		t.Fatalf("clusters of the owner: got = %+v, %v, want only %s", listed, err, name)
	}
	if listed, err := clusters.ClustersHosting(ctx, orgB); err != nil || len(listed) != 0 {
		t.Fatalf("clusters of another org: got = %+v, %v, want none", listed, err)
	}
	if _, _, err := clusters.UpdateCluster(ctx, name, sharedAs(true), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	for _, orgID := range []string{orgA, orgB} {
		if listed, err := clusters.ClustersHosting(ctx, orgID); err != nil || len(listed) != 1 || listed[0].Name != name || !listed[0].Shared {
			t.Fatalf("clusters hosting %s once %s is shared: got = %+v, %v, want only it", orgID, name, listed, err)
		}
	}
	if _, _, err := clusters.UpdateCluster(ctx, name, sharedAs(false), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	if listed, err := clusters.ClustersHosting(ctx, orgB); err != nil || len(listed) != 0 {
		t.Fatalf("clusters of another org once sharing ended: got = %+v, %v, want none", listed, err)
	}
}

func TestSharingAClusterIsStoredAndSurvivesRegisteringAgain(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	clusters := NewClustersStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	name := "cluster-" + uuid.New().String()[:8]
	if _, _, err := clusters.UpdateCluster(ctx, name, sharedAs(true), domain.ClusterSettings{}); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("share an unregistered cluster: got = %v, want = %v", err, ErrClusterNotFound)
	}
	if _, err := clusters.RegisterCluster(ctx, name, orgA); err != nil {
		t.Fatalf("register: got = %v, want = nil", err)
	}

	shared, _, err := clusters.UpdateCluster(ctx, name, sharedAs(true), domain.ClusterSettings{})
	if err != nil || !shared.Shared || shared.OrgID != orgA {
		t.Fatalf("share: got = %+v, %v, want it shared and still owned by %s", shared, err, orgA)
	}
	if !shared.Hosts("org-other") || !shared.Hosts(orgA) {
		t.Fatal("a shared cluster does not host every org")
	}
	again, err := clusters.RegisterCluster(ctx, name, orgA)
	if err != nil || !again.Shared {
		t.Fatalf("register again: got = %+v, %v, want it still shared", again, err)
	}
	unshared, _, err := clusters.UpdateCluster(ctx, name, sharedAs(false), domain.ClusterSettings{})
	if err != nil || unshared.Shared || unshared.Hosts("org-other") || !unshared.Hosts(orgA) {
		t.Fatalf("stop sharing: got = %+v, %v, want it to host %s alone", unshared, err, orgA)
	}
}

func TestAReportedClusterIDIsBoundToOneRegisteredName(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	clusters := NewClustersStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)
	suffix := uuid.New().String()[:8]
	ofA, ofB := "cluster-a-"+suffix, "cluster-b-"+suffix
	idA, idB := "id-a-"+suffix, "id-b-"+suffix
	for name, org := range map[string]string{ofA: orgA, ofB: orgB} {
		if _, err := clusters.RegisterCluster(ctx, name, org); err != nil {
			t.Fatalf("register %s: got = %v, want = nil", name, err)
		}
	}

	if err := clusters.BindReportedID(ctx, "cluster-missing-"+suffix, idA); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("bind for an unregistered name: got = %v, want = %v", err, ErrClusterNotFound)
	}
	if got, err := clusters.ClusterByReportedID(ctx, idA); err != nil || got != nil {
		t.Fatalf("cluster of an unbound id: got = %+v, %v, want nil, nil", got, err)
	}
	for i := 0; i < 2; i++ {
		if err := clusters.BindReportedID(ctx, ofA, idA); err != nil {
			t.Fatalf("bind, attempt %d: got = %v, want = nil", i+1, err)
		}
	}
	if err := clusters.BindReportedID(ctx, ofB, idA); !errors.Is(err, ErrClusterIDBound) {
		t.Fatalf("another cluster reporting a bound id: got = %v, want = %v", err, ErrClusterIDBound)
	}
	if got, err := clusters.ClusterByReportedID(ctx, idA); err != nil || got == nil || got.Name != ofA || got.OrgID != orgA {
		t.Fatalf("cluster of the id: got = %+v, %v, want %s of %s", got, err, ofA, orgA)
	}
	if err := clusters.BindReportedID(ctx, ofB, idB); err != nil {
		t.Fatalf("bind the second cluster to its own id: got = %v, want = nil", err)
	}
}
