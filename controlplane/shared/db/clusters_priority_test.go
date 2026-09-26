package db

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// An org orders the clusters it registered in one step, a refused order changes nothing, and a
// shared cluster holds the platform's priority, after every org's own.
func TestAnOrgOrdersItsClustersAndASharedOneComesLast(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	second, third := "a-second-"+w.suffix, "z-third-"+w.suffix
	for _, name := range []string{second, third} {
		if _, err := w.clusters.RegisterCluster(ctx, name, w.orgA); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofB, true); err != nil {
		t.Fatal(err)
	}
	listed := func() []string {
		t.Helper()
		clusters, err := w.clusters.ClustersHosting(ctx, w.orgA)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range clusters {
			if c.OrgID == w.orgA || c.Name == w.ofB {
				names = append(names, c.Name)
			}
		}
		return names
	}
	if got, want := listed(), []string{second, w.ofA, third, w.ofB}; !slices.Equal(got, want) {
		t.Fatalf("before any order: listed %v, want by name with the shared one last, %v", got, want)
	}
	if err := w.clusters.SetClusterOrder(ctx, w.orgA, []string{third, w.ofA, second}); err != nil {
		t.Fatal(err)
	}
	ordered := []string{third, w.ofA, second, w.ofB}
	for what, order := range map[string][]string{
		"one left out": {third, w.ofA}, "one twice": {third, third, w.ofA}, "the shared cluster": {third, w.ofA, second, w.ofB}, "another org's order": nil,
	} {
		org := w.orgA
		if order == nil {
			org, order = w.orgB, []string{third}
		}
		if err := w.clusters.SetClusterOrder(ctx, org, order); !errors.Is(err, ErrClusterOrder) {
			t.Errorf("%s: got = %v, want = %v", what, err, ErrClusterOrder)
		}
	}
	if got := listed(); !slices.Equal(got, ordered) {
		t.Fatalf("listed %v, want %v", got, ordered)
	}
	// A cluster registered after the order comes last among the org's own, and registering a name
	// again keeps its place.
	late := "a-late-" + w.suffix
	for _, name := range []string{late, third} {
		if _, err := w.clusters.RegisterCluster(ctx, name, w.orgA); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := listed(), []string{third, w.ofA, second, late, w.ofB}; !slices.Equal(got, want) {
		t.Fatalf("with a cluster registered later: listed %v, want %v", got, want)
	}
	registered, err := w.clusters.RegisteredClusters(ctx)
	if err != nil || registered[third].Priority <= registered[second].Priority || registered[w.ofB].Priority != domain.SharedClusterPriority {
		t.Fatalf("priorities as admission reads them: %d, %d and %d, %v", registered[third].Priority, registered[second].Priority, registered[w.ofB].Priority, err)
	}
	// Ending a sharing puts the cluster back among its org's own, at the default.
	if unshared, _, err := w.clusters.SetClusterShared(ctx, w.ofB, false); err != nil || unshared.Priority != domain.DefaultClusterPriority {
		t.Fatalf("once the sharing ended: %+v, %v, want priority %d", unshared, err, domain.DefaultClusterPriority)
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
