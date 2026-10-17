package db

import (
	"context"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A job keeps the architecture of its image beside the image, and its job_spec as submitted.
func TestAJobKeepsItsImageArchitectureAndItsSubmittedSpec(t *testing.T) {
	pool := eventsTestDB(t)
	store := NewStore(pool, 3)
	agentID, peID, hypothesisID := seedRun(t, pool, "image-arch", true)
	exp := jobOf(agentID, peID, hypothesisID, "image-arch-job", domain.QueueReasonAwaitingAdmission)
	exp.Job.NodeSelector = map[string]string{"zone": "b"}
	exp.Image, exp.ImageArchitecture = "registry.test/pe@sha256:ab", "arm64"
	queueJob(t, pool, exp)

	got, err := store.GetExperiment(context.Background(), exp.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v, %v", got, err)
	}
	if got.ImageArchitecture != "arm64" || got.Image != exp.Image {
		t.Errorf("read back image %q on %q, want %q on arm64", got.Image, got.ImageArchitecture, exp.Image)
	}
	if len(got.Job.NodeSelector) != 1 || got.Job.NodeSelector["zone"] != "b" {
		t.Errorf("job_spec node selector = %v, want it as submitted", got.Job.NodeSelector)
	}
}

// The data-key sweep reads the ended jobs still holding a key through their partial index, not
// by scanning every job.
func TestTheDataKeySweepReadsItsIndex(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT id FROM experiments WHERE `+endedJobKeySQL("60"))
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_experiments_data_key") {
		t.Errorf("the sweep does not read idx_experiments_data_key:\n%s", strings.Join(plan, "\n"))
	}
}
