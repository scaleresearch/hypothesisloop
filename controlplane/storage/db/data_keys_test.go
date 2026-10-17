package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const testGrace = 10 * time.Minute

func endedKeysOf(t *testing.T, store *Store) []EndedDataKey {
	t.Helper()
	keys, err := store.EndedDataKeys(context.Background(), testGrace)
	if err != nil {
		t.Fatalf("EndedDataKeys: %v", err)
	}
	return keys
}

// A job's key outlives its end by the grace its workload has to save and shut down, then
// leaves the store and the row; a deletion that fails keeps it for the next pass.
func TestDropDataKeyTakesAnEndedJobsKeyOutOfTheStore(t *testing.T) {
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	key := domain.DataKey{AccessKeyID: "AKIDROPTEST", SecretAccessKey: "secret"}
	exp.DataKey = key
	if _, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("admit: %v", err)
	}
	want := EndedDataKey{ExperimentID: exp.ID, AccessKeyID: key.AccessKeyID}

	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a queued job's key is listed as ended")
	}
	if _, err := store.pool.pool.Exec(ctx, `UPDATE experiments SET status = 'EVICTED', eviction_reason = 'preempted', not_admitted_reason = NULL, ended_at = now() WHERE id = $1`, exp.ID); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a job that ended within its grace has its key listed")
	}
	if _, err := store.pool.pool.Exec(ctx, `UPDATE experiments SET ended_at = now() - interval '11 minutes' WHERE id = $1`, exp.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("the key of a job past its grace is not listed")
	}

	if err := store.DropDataKey(ctx, want, testGrace, func(context.Context, string) error { return errors.New("store down") }); err == nil {
		t.Fatal("DropDataKey with a failing deletion: got nil, want the error")
	}
	if !slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a key whose deletion failed is no longer listed")
	}

	stale := want
	stale.AccessKeyID = "other"
	deleted := 0
	if err := store.DropDataKey(ctx, stale, testGrace, func(context.Context, string) error { deleted++; return nil }); err != nil || deleted != 0 {
		t.Fatalf("DropDataKey of a key the row no longer holds: err %v, %d deletions, want none", err, deleted)
	}

	var dropped []string
	if err := store.DropDataKey(ctx, want, testGrace, func(_ context.Context, accessKeyID string) error {
		dropped = append(dropped, accessKeyID)
		return nil
	}); err != nil {
		t.Fatalf("DropDataKey: %v", err)
	}
	if !slices.Equal(dropped, []string{key.AccessKeyID}) {
		t.Fatalf("DropDataKey deleted %v from the store, want %s", dropped, key.AccessKeyID)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataKey != (domain.DataKey{}) {
		t.Fatalf("row keeps key %+v after the drop", got.DataKey)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a dropped key is still listed")
	}
}
