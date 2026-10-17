package db

import (
	"context"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A platform experiment's budget counts CPU core-hours, without a proposal, only once it has jobs
// and every one of them runs on cores: none with no job, none with one job on an accelerator.
// One query answers the whole page.
func TestOnCoresOnlyNamesThoseWithJobsAllOnCores(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	store := NewStore(pool, 3)
	suffix := uuid.New().String()[:8]
	agent := "cores-agent-" + suffix
	createTestAgent(t, pool, agent)
	pe := func(name string) string {
		id := "pe-" + name + "-" + suffix
		createTestPE(t, pool, id, 10)
		return id
	}
	noJobs, onCores, mixed, onAccelerator := pe("none"), pe("cores"), pe("mixed"), pe("gpu")
	job := func(peID string, n int, acceleratorType domain.AcceleratorType) {
		t.Helper()
		count := 1
		if acceleratorType == domain.CPUType {
			count = 0
		}
		exp := &domain.Experiment{
			ID: peID + "-job-" + string(rune('a'+n)), AgentID: agent, Actor: domain.Actor{Kind: domain.ActorAgent, ID: agent}, PlatformExperimentID: peID,
			HypothesisID: "h-" + peID, Hypothesis: "h", AcceleratorType: acceleratorType, AcceleratorCount: count,
			EstimatedDurationHours: 1, EstimatedCostAccH: 1, CapacityTier: domain.CapacityGuaranteed, Status: domain.StatusQueued,
			Job:     domain.JobSpec{Image: "registry.test/job:1", AcceleratorType: acceleratorType, AcceleratorCount: count, CPU: "1", Memory: "1Gi", Storage: "1Gi"},
			DataKey: domain.DataKey{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}
		if err := insertExperiment(ctx, pool, exp); err != nil {
			t.Fatalf("job of %s: %v", peID, err)
		}
		t.Cleanup(func() { _, _ = pool.pool.Exec(context.Background(), `DELETE FROM experiments WHERE id = $1`, exp.ID) })
	}
	const gpu = domain.AcceleratorType("nvidia.com/gpu.product=NVIDIA-L40")
	job(onCores, 0, domain.CPUType)
	job(onCores, 1, domain.CPUType)
	job(mixed, 0, domain.CPUType)
	job(mixed, 1, gpu)
	job(onAccelerator, 0, gpu)

	got, err := store.OnCoresOnly(ctx, []string{noJobs, onCores, mixed, onAccelerator})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{onCores: true}; !maps.Equal(got, want) {
		t.Fatalf("on cores only = %v, want %v", got, want)
	}
	if got, err := store.OnCoresOnly(ctx, []string{onCores}); err != nil || !got[onCores] {
		t.Fatalf("asked of the one on cores: %v, %v", got, err)
	}
	if got, err := store.OnCoresOnly(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("asked of none: %v, %v, want an empty answer", got, err)
	}
}
