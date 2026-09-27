package podexec

import (
	"testing"
)

const gib = int64(1) << 30

func diskWithFree(t *testing.T, free *int64) {
	t.Helper()
	previous := storageFreeBytes
	storageFreeBytes = func(string) (int64, error) { return *free, nil }
	t.Cleanup(func() { storageFreeBytes = previous })
}

func storageOf(t *testing.T, e *Executor) storageReading {
	t.Helper()
	available, total, err := e.GetLiveStorageCapacity(t.Context())
	if err != nil {
		t.Fatalf("GetLiveStorageCapacity: %v", err)
	}
	return storageReading{available: available, total: total}
}

// A node's storage total is what a job can be given when none runs, which is what the disk has
// free and never its size. It stays where it was while a job writes: what the job wrote comes
// back with its container. What is available is less what the job has reserved and not written.
func TestStorageTotalIsWhatJobsCanBeGivenWhenNoneRuns(t *testing.T) {
	free := 40 * gib
	diskWithFree(t, &free)
	f := newFakeEngine()
	e := newFakeExecutor(f, "/dev/fake/0")
	ctx := t.Context()

	if got, want := storageOf(t, e), (storageReading{available: 40 * gib, total: 40 * gib}); got != want {
		t.Fatalf("idle: storage = %+v, want %+v", got, want)
	}

	job := fakeJob("writer", "fast", 1)
	job.Job.Storage = "10Gi"
	pass(t, ctx, e, job)
	eventually(t, "the job runs", running(f, "writer"))
	eventually(t, "its start has returned", idle(e, "writer"))
	if got, want := storageOf(t, e), (storageReading{available: 30 * gib, total: 40 * gib}); got != want {
		t.Fatalf("a job that wrote nothing: storage = %+v, want %+v", got, want)
	}

	f.mu.Lock()
	f.containers[containerName("writer")].written = 4 * gib
	f.mu.Unlock()
	free = 36 * gib
	if got, want := storageOf(t, e), (storageReading{available: 30 * gib, total: 40 * gib}); got != want {
		t.Fatalf("a job that wrote 4 GiB of its 10: storage = %+v, want %+v", got, want)
	}

	// Its container has exited and still holds what it wrote, and something else took 3 GiB.
	f.mu.Lock()
	f.containers[containerName("writer")].state = "exited"
	f.mu.Unlock()
	free = 33 * gib
	if got, want := storageOf(t, e), (storageReading{available: 33 * gib, total: 37 * gib}); got != want {
		t.Fatalf("an exited job and a fuller disk: storage = %+v, want %+v", got, want)
	}
}

// Free space moves between two looks at the disk, so one pass answers every storage question
// from one look: a report whose available exceeds its total is refused.
func TestOnePassReadsTheDiskOnce(t *testing.T) {
	free := 40 * gib
	diskWithFree(t, &free)
	e := newFakeExecutor(newFakeEngine())
	ctx := t.Context()

	scoped, err := e.ForPass(ctx)
	if err != nil {
		t.Fatalf("ForPass: %v", err)
	}
	available, _, err := scoped.GetLiveStorageCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveStorageCapacity: %v", err)
	}
	free = 39 * gib
	_, total, err := scoped.GetLiveStorageCapacity(ctx)
	if err != nil {
		t.Fatalf("GetLiveStorageCapacity: %v", err)
	}
	if available != 40*gib || total != 40*gib {
		t.Fatalf("available = %d, total = %d, want both from the first look, 40 GiB", available, total)
	}
	if got := storageOf(t, e).total; got != 39*gib {
		t.Fatalf("the next pass: total = %d, want a new look, 39 GiB", got)
	}
}
