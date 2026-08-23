package agentloop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A reconcile pass is a loop over every workload on the cluster, and deletion is how a finished
// job gives its accelerators back. So the cost of one entry taking the pass down with it is not
// abstract: every other finished job on that cluster keeps holding capacity until the bad entry
// goes away by itself. These tests pin that one failure costs exactly its own workload.
//
// Written after a suite run measured the real reconcile cadence at a median 4s with a 52s tail
// against a 2s nominal interval — the pass is serial, so anything that makes a pass fail or run
// long is paid by every workload waiting behind it.

// newUndesiredStateServer serves an empty desired set, which makes every managed workload
// undesired and therefore a deletion candidate in this pass.
func newUndesiredStateServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_, _ = w.Write([]byte(`{"experiments":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
}

func newTestAgent(server *httptest.Server, exec *captureExecutor) *Agent {
	return &Agent{
		ClusterName:     "test",
		APIURL:          server.URL,
		Executor:        exec,
		HTTPClient:      server.Client(),
		MaxLogLineChars: 200,
		Log:             func(string, ...any) {},
	}
}

// One workload's delete failing must not strand the others. Before this was pinned, the loops
// already continued past a per-workload error, but nothing said so — and the two validation loops
// above them did not.
func TestOneWorkloadsFailedDeleteStillLetsTheOthersBeDeleted(t *testing.T) {
	exec := &captureExecutor{
		managed:     []string{"exp-a", "exp-broken", "exp-c"},
		undeletable: "exp-broken",
	}
	server := newUndesiredStateServer(t)
	defer server.Close()

	err := newTestAgent(server, exec).reconcileOnce(context.Background())
	if err == nil {
		t.Fatal("reconcileOnce returned nil — the failing workload must still be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "exp-broken") {
		t.Errorf("error %q does not name the workload that failed", err)
	}
	for _, unrelated := range []string{"exp-a", "exp-c"} {
		if strings.Contains(err.Error(), unrelated) {
			t.Errorf("error %q names %s, which reconciled fine", err, unrelated)
		}
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	deleted := strings.Join(exec.deleted, ",")
	for _, want := range []string{"exp-a", "exp-c"} {
		if !strings.Contains(deleted, want) {
			t.Errorf("deleted = %v, want %s among them — one workload's failure stranded the rest, so their accelerators stay held", exec.deleted, want)
		}
	}
}

// The dangerous half of surviving a bad row. Deletion here is decided by ABSENCE from the desired
// set, so a desired row whose identity was lost in transit does not read as "unusable row" — it
// reads as "that workload is no longer wanted", and its still-running job gets destroyed. An
// incomplete desired snapshot therefore withdraws the absence argument for this pass instead of
// acting on it.
func TestAnIncompleteDesiredSnapshotDeletesNothing(t *testing.T) {
	exec := &captureExecutor{managed: []string{"exp-running"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			// The identity-less row may well BE exp-running, with its id lost on the way here.
			_, _ = w.Write([]byte(`{"experiments":[{"id":""}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	err := newTestAgent(server, exec).reconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without identity") {
		t.Fatalf("err = %v, want the identity-less row reported rather than hidden", err)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing — a workload was destroyed on the strength of a desired snapshot known to be missing a row", exec.deleted)
	}
}

// The mirror image: creation is decided by absence from the ACTUAL set, so a workload the pass
// could not name must not be created a second time.
func TestAnIncompleteActualSnapshotCreatesNothing(t *testing.T) {
	exec := &captureExecutor{managed: []string{""}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			// exp-wanted is desired and appears absent — but the unnamed actual workload above
			// may be exactly it.
			_, _ = w.Write([]byte(`{"experiments":[{"id":"exp-wanted"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	err := newTestAgent(server, exec).reconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without experiment identity") {
		t.Fatalf("err = %v, want the unnamed actual workload reported", err)
	}
	if exec.created != 0 {
		t.Fatalf("created %d workload(s), want 0 — a running job may have been duplicated on an actual snapshot known to be incomplete", exec.created)
	}
}

// A duplicate desired id is ambiguous rather than unusable, and the copies may differ in placement,
// image or resources. Picking one would make the pass reconcile against whichever the feed listed
// first — deleting a correct workload as drifted on one pass and recreating it on the next. The id
// is left alone entirely, and because it was dropped the snapshot is incomplete, so nothing is
// deleted for merely appearing undesired either.
func TestADuplicateDesiredIDIsLeftUntouchedRatherThanGuessedAt(t *testing.T) {
	exec := &captureExecutor{managed: []string{"exp-dup", "exp-finished"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_, _ = w.Write([]byte(`{"experiments":[{"id":"exp-dup"},{"id":"exp-dup"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	err := newTestAgent(server, exec).reconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want the duplicate reported", err)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing — exp-dup must not be torn down over an ambiguity, and the snapshot that dropped it cannot convict exp-finished either", exec.deleted)
	}
}

// Quarantine must not reopen. With three copies of an id, removing it from the map on the second
// occurrence leaves the third free to install itself as though it were the only one — putting the
// pass back to reconciling against an arbitrary copy, which is the thing the quarantine exists to
// prevent. An id that was ever ambiguous stays ambiguous for the whole pass.
func TestAThriceDuplicatedDesiredIDStaysQuarantined(t *testing.T) {
	// drifted is what makes this test able to fail: if the third copy re-enters the desired set,
	// the pass compares exp-dup against it, sees a mismatch and DELETES a running workload on the
	// strength of a spec it had already ruled ambiguous. With comparison always reporting a match,
	// the re-entry is silent and the test passes either way.
	exec := &captureExecutor{managed: []string{"exp-dup"}, drifted: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_, _ = w.Write([]byte(`{"experiments":[{"id":"exp-dup"},{"id":"exp-dup"},{"id":"exp-dup"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	err := newTestAgent(server, exec).reconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want the duplicate reported", err)
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.deleted) != 0 || exec.created != 0 {
		t.Fatalf("deleted = %v, created = %d; want neither — the third copy re-entered the desired set and was acted on", exec.deleted, exec.created)
	}
}
