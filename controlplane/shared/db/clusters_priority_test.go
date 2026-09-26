package db

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAClusterIsRegisteredAtTheDefaultPriorityAndItsOrgChangesIt(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	registered, err := w.clusters.GetCluster(ctx, w.ofA)
	if err != nil || registered.Priority != domain.DefaultClusterPriority {
		t.Fatalf("a registered cluster: priority %d, %v, want %d", registered.Priority, err, domain.DefaultClusterPriority)
	}
	for _, priority := range []int{domain.ClusterPriorityMin, 250, domain.ClusterPriorityMax} {
		set, err := w.clusters.SetClusterPriority(ctx, w.ofA, priority)
		if err != nil || set.Priority != priority {
			t.Fatalf("SetClusterPriority(%d) = %+v, %v", priority, set, err)
		}
	}
	again, err := w.clusters.RegisterCluster(ctx, w.ofA, w.orgA)
	if err != nil || again.Priority != domain.ClusterPriorityMax {
		t.Fatalf("registering the name again: priority %d, %v, want the one its org set kept", again.Priority, err)
	}
	for _, priority := range []int{domain.SharedClusterPriority, -1, domain.ClusterPriorityMax + 1} {
		if _, err := w.clusters.SetClusterPriority(ctx, w.ofA, priority); err == nil {
			t.Fatalf("SetClusterPriority(%d) was accepted", priority)
		}
	}
	if _, err := w.clusters.SetClusterPriority(ctx, w.unregisteredName, 5); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("a priority for a name nobody registered: %v, want ErrClusterNotFound", err)
	}
	if kept, _ := w.clusters.GetCluster(ctx, w.ofA); kept.Priority != domain.ClusterPriorityMax {
		t.Fatalf("after the refused priorities: %d, want the last accepted one", kept.Priority)
	}
}

// A shared cluster is the platform's: it holds the priority below every org's own, whatever its
// org had set, and no org sets another on it.
func TestASharedClusterHoldsThePlatformsPriorityUntilTheSharingEnds(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	if _, err := w.clusters.SetClusterPriority(ctx, w.ofA, 700); err != nil {
		t.Fatal(err)
	}
	shared, _, err := w.clusters.SetClusterShared(ctx, w.ofA, true)
	if err != nil || shared.Priority != domain.SharedClusterPriority {
		t.Fatalf("a cluster once shared: priority %d, %v, want %d", shared.Priority, err, domain.SharedClusterPriority)
	}
	if _, err := w.clusters.SetClusterPriority(ctx, w.ofA, 700); !errors.Is(err, ErrClusterShared) {
		t.Fatalf("a priority for a shared cluster: %v, want ErrClusterShared", err)
	}
	if still, _ := w.clusters.GetCluster(ctx, w.ofA); still.Priority != domain.SharedClusterPriority {
		t.Fatalf("after the refused priority: %d, want %d", still.Priority, domain.SharedClusterPriority)
	}
	if again, _, err := w.clusters.SetClusterShared(ctx, w.ofA, true); err != nil || again.Priority != domain.SharedClusterPriority {
		t.Fatalf("sharing it again: %+v, %v", again, err)
	}
	unshared, _, err := w.clusters.SetClusterShared(ctx, w.ofA, false)
	if err != nil || unshared.Priority != domain.DefaultClusterPriority {
		t.Fatalf("once the sharing ended: priority %d, %v, want %d", unshared.Priority, err, domain.DefaultClusterPriority)
	}
	// Ending a sharing that is not on leaves what the org set.
	if _, err := w.clusters.SetClusterPriority(ctx, w.ofA, 300); err != nil {
		t.Fatal(err)
	}
	if kept, _, err := w.clusters.SetClusterShared(ctx, w.ofA, false); err != nil || kept.Priority != 300 {
		t.Fatalf("ending a sharing that was not on: priority %d, %v, want 300", kept.Priority, err)
	}
}

func TestClustersAreListedInTheOrderPlacementTriesThem(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	second, third := "a-second-"+w.suffix, "z-first-"+w.suffix
	for _, name := range []string{second, third} {
		if _, err := w.clusters.RegisterCluster(ctx, name, w.orgA); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.clusters.SetClusterPriority(ctx, third, 500); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofB, true); err != nil {
		t.Fatal(err)
	}
	listed, err := w.clusters.ClustersHosting(ctx, w.orgA)
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, c := range listed {
		if c.OrgID == w.orgA || c.Name == w.ofB {
			mine = append(mine, c.Name)
		}
	}
	// 500 first, then the two at the default by name, then the shared one at 0.
	want := []string{third, second, w.ofA, w.ofB}
	if len(mine) != len(want) {
		t.Fatalf("listed %v, want %v", mine, want)
	}
	for i := range want {
		if mine[i] != want[i] {
			t.Fatalf("listed %v, want %v", mine, want)
		}
	}
	registered, err := w.clusters.RegisteredClusters(ctx)
	if err != nil || registered[third].Priority != 500 || registered[w.ofB].Priority != domain.SharedClusterPriority {
		t.Fatalf("priorities as admission reads them: %d and %d, %v", registered[third].Priority, registered[w.ofB].Priority, err)
	}
}

// A database from before priorities has shared clusters at the column's default. Applying the
// schema again puts them at the platform's priority.
func TestTheSchemaPutsAnAlreadySharedClusterAtThePlatformsPriority(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	t.Setenv("HYPOTHESISLOOP_SCHEMA_SQL", "schema.sql")
	if _, err := w.pool.pool.Exec(ctx, `ALTER TABLE clusters DROP CONSTRAINT clusters_priority`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.pool.Exec(context.Background(), `UPDATE clusters SET shared = false, priority = 100 WHERE name = $1`, w.ofA)
		if err := ApplySchema(context.Background(), w.pool); err != nil {
			t.Errorf("restore the schema: %v", err)
		}
	})
	if _, err := w.pool.pool.Exec(ctx, `UPDATE clusters SET shared = true, priority = 100 WHERE name = $1`, w.ofA); err != nil {
		t.Fatal(err)
	}
	if err := ApplySchema(ctx, w.pool); err != nil {
		t.Fatalf("apply the schema again: %v", err)
	}
	if c, _ := w.clusters.GetCluster(ctx, w.ofA); c.Priority != domain.SharedClusterPriority {
		t.Fatalf("an already shared cluster after the schema is applied: priority %d, want %d", c.Priority, domain.SharedClusterPriority)
	}
	if _, err := w.pool.pool.Exec(ctx, `UPDATE clusters SET priority = 100 WHERE name = $1`, w.ofA); err == nil {
		t.Fatal("the schema lets a shared cluster hold an org's priority")
	}
}
