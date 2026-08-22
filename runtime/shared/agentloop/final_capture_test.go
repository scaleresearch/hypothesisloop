package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// captureExecutor is the narrow slice of agentexec.Executor these tests drive: which workloads
// exist, what their logs say, and which ones were deleted. Everything else panics if called, so a
// test that starts depending on more says so instead of silently passing.
type captureExecutor struct {
	agentexec.Executor
	managed []string
	logTail []string
	// unpollable names a job whose phase cannot be read, standing in for a transient API failure.
	unpollable string
	mu         sync.Mutex
	deleted    []string
}

func (e *captureExecutor) ListManagedJobs(context.Context) ([]string, error) { return e.managed, nil }

// The same set as ListManagedJobs here: these tests never exercise the delete-and-recreate window
// the two lists exist to differ across, and leaving it on the embedded nil Executor made every
// reconcile pass nil-panic before it reached the behaviour under test.
func (e *captureExecutor) ListManagedJobsForStatus(context.Context) ([]string, error) {
	return e.managed, nil
}
func (e *captureExecutor) ListManagedAuxiliaryWorkloads(context.Context) ([]string, error) {
	return nil, nil
}

// Nothing to prune is a valid implementation, not a missing one — see agentexec.Executor.
func (e *captureExecutor) ReapTerminal(context.Context, map[string]*domain.Experiment) error {
	return nil
}
func (e *captureExecutor) WorkloadMatchesDesired(context.Context, *domain.Experiment) (bool, error) {
	return true, nil
}
func (e *captureExecutor) PollJobPhaseAndUID(_ context.Context, id string) (workload.JobPhase, string, error) {
	if id != "" && id == e.unpollable {
		return workload.JobPhasePending, "", fmt.Errorf("transient poll failure for %s", id)
	}
	return workload.JobPhaseFailed, "uid", nil
}
func (e *captureExecutor) ResolveAdmittedAcceleratorType(context.Context, string) (domain.AcceleratorType, string, bool, error) {
	return "nvidia.com/gpu.product=NVIDIA-L40", "node-a", true, nil
}
func (e *captureExecutor) FetchLogTail(context.Context, string, int) ([]string, error) {
	return e.logTail, nil
}
func (e *captureExecutor) PollPhaseDetail(context.Context, string) (string, string, int32, error) {
	return domain.PhaseReasonContainerFailed, "container exited with code 1", 0, nil
}
func (e *captureExecutor) GetLiveCPUCapacity(context.Context) (float64, float64, error) {
	return 8, 16, nil
}
func (e *captureExecutor) GetLiveRAMCapacity(context.Context) (int64, int64, error) {
	return 1 << 34, 1 << 35, nil
}
func (e *captureExecutor) GetLiveNodeResourceCapacity(context.Context) (map[string]map[string]int64, error) {
	return map[string]map[string]int64{}, nil
}

func (e *captureExecutor) GetLiveStorageCapacity(context.Context) (int64, int64, error) {
	return 1 << 36, 1 << 37, nil
}
func (e *captureExecutor) GetLiveAcceleratorCapacitySnapshot(context.Context) (map[string]int64, map[string]int64, map[string]map[string]int64, map[string]map[string]string, error) {
	return map[string]int64{}, map[string]int64{}, map[string]map[string]int64{}, map[string]map[string]string{}, nil
}
func (e *captureExecutor) DeleteWorkload(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleted = append(e.deleted, id)
	return nil
}

// A workload that is no longer desired is about to be deleted, and deletion destroys the only
// copy of why it ended. Its log tail and termination reason must reach the control plane first,
// or a job that died between two status pushes is recorded as FAILED with nothing attached.
func TestReconcilePushesFinalOutputBeforeDeletingAnUndesiredWorkload(t *testing.T) {
	var mu sync.Mutex
	var pushed [][]statusReportWire
	var deletedWhenPushed []string

	exec := &captureExecutor{managed: []string{"exp-gone"}, logTail: []string{"Traceback", "RuntimeError: device crashed"}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/clusters/test/reconcile" {
			// Desired state is empty: exp-gone is terminal control-plane-side.
			_, _ = w.Write([]byte(`{"experiments":[]}`))
			return
		}
		var body struct {
			Reports []statusReportWire `json:"reports"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		pushed = append(pushed, body.Reports)
		exec.mu.Lock()
		deletedWhenPushed = append([]string{}, exec.deleted...)
		exec.mu.Unlock()
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	agent := &Agent{
		ClusterName:     "test",
		APIURL:          server.URL,
		Executor:        exec,
		HTTPClient:      server.Client(),
		MaxLogLineChars: 200,
		Log:             func(string, ...any) {},
	}
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	if !reflect.DeepEqual(exec.deleted, []string{"exp-gone"}) {
		t.Fatalf("deleted = %v, want [exp-gone] — the undesired workload must still be deleted", exec.deleted)
	}
	if len(pushed) != 1 || len(pushed[0]) != 1 {
		t.Fatalf("got %d push(es) %v, want exactly one report for exp-gone", len(pushed), pushed)
	}
	report := pushed[0][0]
	if report.ExperimentID != "exp-gone" {
		t.Errorf("pushed report for %q, want exp-gone", report.ExperimentID)
	}
	if len(report.LogTail) != 2 || report.LogTail[1] != "RuntimeError: device crashed" {
		t.Errorf("final report log tail = %v, want the crashed job's output", report.LogTail)
	}
	if report.Reason != domain.PhaseReasonContainerFailed {
		t.Errorf("final report reason = %q, want %q", report.Reason, domain.PhaseReasonContainerFailed)
	}
	// Ordering is the whole point: the push must happen while the pod still exists.
	if len(deletedWhenPushed) != 0 {
		t.Errorf("workloads %v were already deleted when the final status was pushed", deletedWhenPushed)
	}
}

// The rule that makes every push safe: a status push IS a complete cluster snapshot, and a job
// missing from the newest one reads as JobPhaseGone — which the controller evicts. So a job that
// cannot be observed must cost the whole push, never be quietly omitted: reporting nothing leaves
// the control plane saying "cannot tell", while reporting the others says "that one vanished".
func TestReportChangedStatusesPushesNothingRatherThanAPartialSnapshot(t *testing.T) {
	var mu sync.Mutex
	var pushed int

	exec := &captureExecutor{managed: []string{"exp-ok", "exp-unobservable"}, unpollable: "exp-unobservable"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/clusters/test/reconcile" {
			_, _ = w.Write([]byte(`{"experiments":[]}`))
			return
		}
		mu.Lock()
		pushed++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	agent := &Agent{
		ClusterName:     "test",
		APIURL:          server.URL,
		Executor:        exec,
		HTTPClient:      server.Client(),
		MaxLogLineChars: 200,
		Log:             func(string, ...any) {},
	}
	agent.reportChangedStatuses(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if pushed != 0 {
		t.Fatalf("pushed %d snapshot(s) with one job unobservable — a partial snapshot reports every omitted job as gone", pushed)
	}
}

// A control plane that never answers must not hold the deletion: the pass's actual job is
// removing workloads that are no longer desired, and a log line is not worth leaving one running.
func TestReconcileDeletesUndesiredWorkloadEvenWhenTheFinalPushHangs(t *testing.T) {
	exec := &captureExecutor{managed: []string{"exp-gone"}}

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/clusters/test/reconcile" {
			_, _ = w.Write([]byte(`{"experiments":[]}`))
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Release the hung handler before closing the server: Close waits for in-flight requests, so
	// the reverse order deadlocks the test rather than the code under test.
	defer func() {
		close(release)
		server.Close()
	}()

	agent := &Agent{
		ClusterName:     "test",
		APIURL:          server.URL,
		Executor:        exec,
		HTTPClient:      server.Client(),
		MaxLogLineChars: 200,
		Log:             func(string, ...any) {},
	}

	// The capture budget is what bounds this; the test's own ceiling is well above it so a
	// regression shows up as a failure here rather than as a hung test binary.
	ctx, cancel := context.WithTimeout(context.Background(), finalStatusCaptureBudget+30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- agent.reconcileOnce(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("reconcileOnce did not return: a hanging control plane blocked the deletion pass")
	}
	if !reflect.DeepEqual(exec.deleted, []string{"exp-gone"}) {
		t.Fatalf("deleted = %v, want [exp-gone] despite the final push never completing", exec.deleted)
	}
}
