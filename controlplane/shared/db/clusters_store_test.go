package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

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
	byName, err := clusters.ClusterOrgs(ctx)
	if err != nil || byName[name] != orgA {
		t.Fatalf("cluster to org map: got = %q, %v, want %s", byName[name], err, orgA)
	}
	if listed, err := clusters.ListClusters(ctx, []string{orgA}); err != nil || len(listed) != 1 || listed[0].Name != name {
		t.Fatalf("clusters of the owner: got = %+v, %v, want only %s", listed, err, name)
	}
	if listed, err := clusters.ListClusters(ctx, []string{orgB}); err != nil || len(listed) != 0 {
		t.Fatalf("clusters of another org: got = %+v, %v, want none", listed, err)
	}
}
