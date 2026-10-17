package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// An image is recorded on an open or running platform experiment and read back with it; a closed
// one keeps the image it had, and the write says it was not made.
func TestAnImageIsRecordedOnlyWhileThePlatformExperimentIsOpenOrRunning(t *testing.T) {
	pool := eventsTestDB(t)
	store := &Store{pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := eventsTestPrefix + "pe-image-" + time.Now().UTC().Format("150405.000000")
	if _, err := pool.pool.Exec(ctx, `INSERT INTO platform_experiments
		(id, project_id, name, description, budget_accelerator_hours, max_agents, metrics, report_interval_seconds,
		 starts_at, ends_at, status, stages, current_stage, created_at, updated_at, default_quota_tier, created_by_kind)
		VALUES ($1, 'default', $1, '', 1, 1, '[]'::jsonb, 10, NOW(), NOW() + interval '1 hour', 'open',
		        '[]'::jsonb, 1, NOW(), NOW(), 'guaranteed', 'staff')`, id); err != nil {
		t.Fatalf("seed platform experiment: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM platform_experiments WHERE id = $1`, id)
	})

	image := domain.ExperimentImage{Image: "registry.test/hl-org/pe@sha256:ab", Architecture: "arm64", SetAt: time.Now().UTC().Truncate(time.Microsecond)}
	if set, err := store.SetPlatformExperimentImage(ctx, id, image); err != nil || !set {
		t.Fatalf("SetPlatformExperimentImage on an open one: %v, %v", set, err)
	}
	pe, err := store.GetPlatformExperiment(ctx, id)
	if err != nil || pe.Image == nil || pe.Image.Image != image.Image || pe.Image.Architecture != "arm64" || !pe.Image.SetAt.Equal(image.SetAt) {
		t.Fatalf("read back %+v, %v, want %+v", pe.Image, err, image)
	}

	if err := store.ClosePlatformExperiment(ctx, id, nil, ""); err != nil {
		t.Fatal(err)
	}
	later := domain.ExperimentImage{Image: "registry.test/hl-org/pe@sha256:cd", Architecture: "amd64", SetAt: time.Now().UTC()}
	if set, err := store.SetPlatformExperimentImage(ctx, id, later); err != nil || set {
		t.Fatalf("SetPlatformExperimentImage on a closed one: %v, %v, want not set", set, err)
	}
	if pe, err := store.GetPlatformExperiment(ctx, id); err != nil || pe.Image == nil || pe.Image.Image != image.Image {
		t.Fatalf("a closed one's image is %+v, %v, want %s kept", pe.Image, err, image.Image)
	}
}
