package db

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// An org orders the clusters it registered in one step, a refused order changes nothing, and a
// cluster of the platform's org comes after every one of the org's own.
func TestAnOrgOrdersItsClustersAndAPlatformOneComesLast(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	second, third := "a-second-"+w.suffix, "z-third-"+w.suffix
	for _, name := range []string{second, third} {
		if _, err := w.clusters.RegisterCluster(ctx, name, w.orgA); err != nil {
			t.Fatal(err)
		}
	}
	platform := w.platformCluster(t)
	listed := func() []string {
		t.Helper()
		clusters, err := w.clusters.ClustersHosting(ctx, w.orgA)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range clusters {
			if c.OrgID == w.orgA || c.Name == platform {
				names = append(names, c.Name)
			}
		}
		return names
	}
	if got, want := listed(), []string{second, w.ofA, third, platform}; !slices.Equal(got, want) {
		t.Fatalf("before any order: listed %v, want by name with the platform one last, %v", got, want)
	}
	if err := w.clusters.SetClusterOrder(ctx, w.orgA, []string{third, w.ofA, second}); err != nil {
		t.Fatal(err)
	}
	ordered := []string{third, w.ofA, second, platform}
	for what, order := range map[string][]string{
		"one left out": {third, w.ofA}, "one twice": {third, third, w.ofA}, "the platform cluster": {third, w.ofA, second, platform}, "another org's order": nil,
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
	if got, want := listed(), []string{third, w.ofA, second, late, platform}; !slices.Equal(got, want) {
		t.Fatalf("with a cluster registered later: listed %v, want %v", got, want)
	}
	registered, err := w.clusters.RegisteredClusters(ctx)
	if err != nil || registered[third].Rank(w.orgA) <= registered[second].Rank(w.orgA) || registered[platform].Rank(w.orgA) >= registered[late].Rank(w.orgA) {
		t.Fatalf("ranks as admission reads them: %d, %d, %d and %d, %v", registered[third].Rank(w.orgA), registered[second].Rank(w.orgA), registered[late].Rank(w.orgA), registered[platform].Rank(w.orgA), err)
	}
}
