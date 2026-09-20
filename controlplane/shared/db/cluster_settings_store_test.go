package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func intPtr(v int) *int { return &v }

func TestClusterSettingsGetAndPutRoundTrip(t *testing.T) {
	pool := eventsTestDB(t)
	store := &ClusterSettingsStore{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := eventsTestPrefix + "cluster-" + time.Now().UTC().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM cluster_settings WHERE cluster_id = $1`, id)
	})

	got, err := store.GetClusterSettings(ctx, id)
	if err != nil {
		t.Fatalf("get before put: got = %v, want = nil", err)
	}
	if got != nil {
		t.Fatalf("get before put: got = %+v, want = nil (no row yet)", got)
	}

	in := &domain.ClusterSettings{
		ClusterID:                  id,
		ScaleUpTimeoutSeconds:      intPtr(300),
		MaxSpeculativeAccelerators: intPtr(8),
		StuckPendingTimeoutSeconds: intPtr(900),
	}
	if err := store.PutClusterSettings(ctx, in); err != nil {
		t.Fatalf("put: got = %v, want = nil", err)
	}
	got, err = store.GetClusterSettings(ctx, id)
	if err != nil {
		t.Fatalf("get after put: got = %v, want = nil", err)
	}
	if got == nil || got.ScaleUpTimeoutSeconds == nil || *got.ScaleUpTimeoutSeconds != 300 ||
		got.MaxSpeculativeAccelerators == nil || *got.MaxSpeculativeAccelerators != 8 ||
		got.StuckPendingTimeoutSeconds == nil || *got.StuckPendingTimeoutSeconds != 900 {
		t.Fatalf("get after put: got = %+v, want ScaleUpTimeoutSeconds=300 MaxSpeculativeAccelerators=8 StuckPendingTimeoutSeconds=900", got)
	}

	// Upsert clears a field back to nil (global default / no cap) rather than leaving it stuck.
	in2 := &domain.ClusterSettings{ClusterID: id, ScaleUpTimeoutSeconds: intPtr(120)}
	if err := store.PutClusterSettings(ctx, in2); err != nil {
		t.Fatalf("re-put: got = %v, want = nil", err)
	}
	got, err = store.GetClusterSettings(ctx, id)
	if err != nil {
		t.Fatalf("get after re-put: got = %v, want = nil", err)
	}
	if got == nil || got.ScaleUpTimeoutSeconds == nil || *got.ScaleUpTimeoutSeconds != 120 || got.MaxSpeculativeAccelerators != nil || got.StuckPendingTimeoutSeconds != nil {
		t.Fatalf("get after re-put: got = %+v, want ScaleUpTimeoutSeconds=120 and the other two nil", got)
	}

	// The stuck-pending timeout alone, which is the row a cluster of rented instances has.
	if err := store.PutClusterSettings(ctx, &domain.ClusterSettings{ClusterID: id, StuckPendingTimeoutSeconds: intPtr(900)}); err != nil {
		t.Fatalf("put stuck-pending only: %v", err)
	}
	got, err = store.GetClusterSettings(ctx, id)
	if err != nil || got == nil || got.StuckPendingTimeoutSeconds == nil || *got.StuckPendingTimeoutSeconds != 900 ||
		got.ScaleUpTimeoutSeconds != nil || got.MaxSpeculativeAccelerators != nil {
		t.Fatalf("get after stuck-pending only: got = %+v, %v", got, err)
	}
	if err := store.PutClusterSettings(ctx, &domain.ClusterSettings{ClusterID: id, StuckPendingTimeoutSeconds: intPtr(0)}); err == nil {
		t.Fatal("a zero stuck-pending timeout was stored")
	}
}
