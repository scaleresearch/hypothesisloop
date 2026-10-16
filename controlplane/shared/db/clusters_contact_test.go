package db

import (
	"context"
	"testing"
)

// What a cluster's reconcile was last refused with is kept, with when, until one is accepted.
func TestTheLastRefusalOfAClustersReconcileIsKeptUntilOneIsAccepted(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	if got, err := w.clusters.GetCluster(ctx, w.ofA); err != nil || got.LastError != "" || got.LastErrorAt != nil {
		t.Fatalf("a cluster never refused: last error %q at %v, %v", got.LastError, got.LastErrorAt, err)
	}
	const refusal = "cluster of-a is bound to another cluster_id"
	if err := w.clusters.RecordContactRefusal(ctx, w.ofA, refusal); err != nil {
		t.Fatal(err)
	}
	got, err := w.clusters.GetCluster(ctx, w.ofA)
	if err != nil || got.LastError != refusal || got.LastErrorAt == nil {
		t.Fatalf("after a refusal: last error %q at %v, %v, want it kept with its time", got.LastError, got.LastErrorAt, err)
	}
	if other, _ := w.clusters.GetCluster(ctx, w.ofB); other.LastError != "" {
		t.Fatalf("another cluster got the refusal: %q", other.LastError)
	}
	if err := w.clusters.RecordContactRefusal(ctx, w.ofA, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.clusters.GetCluster(ctx, w.ofA); got.LastError != "" || got.LastErrorAt != nil {
		t.Fatalf("after an accepted reconcile: last error %q at %v, want none", got.LastError, got.LastErrorAt)
	}
}
