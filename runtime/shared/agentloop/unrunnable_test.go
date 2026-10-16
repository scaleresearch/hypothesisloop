package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// controlPlane answers reconcile with the desired jobs it holds and keeps what was sent to it.
type controlPlane struct {
	mu      sync.Mutex
	desired string
	// registryPull is the answer's registry_pull, as JSON. Empty sends none.
	registryPull string
	reconcile    []map[string]any
	statuses     [][]clusterapi.StatusReport
}

func (c *controlPlane) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			var sent map[string]any
			_ = json.Unmarshal(body, &sent)
			c.reconcile = append(c.reconcile, sent)
			answer := `{"experiments":` + c.desired
			if c.registryPull != "" {
				answer += `,"registry_pull":` + c.registryPull
			}
			_, _ = w.Write([]byte(answer + `}`))
			return
		}
		var sent struct {
			Reports []clusterapi.StatusReport `json:"reports"`
		}
		_ = json.Unmarshal(body, &sent)
		c.statuses = append(c.statuses, sent.Reports)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func (c *controlPlane) lastStatus() []clusterapi.StatusReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.statuses) == 0 {
		return nil
	}
	return c.statuses[len(c.statuses)-1]
}

// A job the runtime can never create is said to the control plane, in the runtime's words, with
// the one reason the control plane ends a job for without waiting. A create that failed otherwise
// is said as create_failed, which ends nothing, so its owner reads why it waits. Neither fails
// the pass, and each is logged when it changes.
func TestAJobTheRuntimeDidNotCreateIsReportedWithItsReason(t *testing.T) {
	exec := &captureExecutor{managed: []string{"exp-running"}, uncreatable: map[string]error{
		"exp-refused": fmt.Errorf("build: %w", &agentexec.Unrunnable{Why: "the job asks for host_mounts, and a rented instance has none"}),
		"exp-later":   errors.New(`jobs.batch is forbidden: User "system:serviceaccount:hl:agent" cannot create resource "jobs"`),
	}}
	cp := &controlPlane{desired: `[{"id":"exp-running"},{"id":"exp-refused","attempt_count":2},{"id":"exp-later","attempt_count":1},{"id":"exp-fine"}]`}
	agent := newTestAgent(cp.serve(t), exec)
	var logged agentLog
	agent.Log = logged.log
	ctx := context.Background()

	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatalf("pass error = %v, want none: every refused create is the control plane's to read", err)
	}
	if exec.created != 1 {
		t.Fatalf("created %d workloads, want the one that could be", exec.created)
	}
	exec.managed = append(exec.managed, "exp-fine")
	for range 2 {
		if err := agent.reconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(logged.lines); n != 2 {
		t.Fatalf("logged %q over three passes, want each refused create once", logged.lines)
	}
	agent.reportStatuses(ctx, exec)
	reports := map[string]clusterapi.StatusReport{}
	for _, report := range cp.lastStatus() {
		reports[report.ExperimentID] = report
	}
	refused, reported := reports["exp-refused"]
	if !reported || refused.Phase != "pending" || refused.Reason != clusterapi.PhaseReasonConfigError ||
		refused.Message != "the job asks for host_mounts, and a rented instance has none" || refused.Attempt == nil || *refused.Attempt != 2 {
		t.Fatalf("the refused job was reported as %+v", refused)
	}
	failed, reported := reports["exp-later"]
	if !reported || failed.Phase != "pending" || failed.Reason != clusterapi.PhaseReasonCreateFailed ||
		failed.Message != exec.uncreatable["exp-later"].Error() || failed.Attempt == nil || *failed.Attempt != 1 {
		t.Fatalf("the job whose create failed was reported as %+v", failed)
	}
	if len(reports) != 4 {
		t.Fatalf("reported %v, want the two running jobs and the two not created", reports)
	}

	// The control plane took the jobs away: nothing is said of them any more.
	cp.mu.Lock()
	cp.desired = `[{"id":"exp-running"},{"id":"exp-fine"}]`
	cp.mu.Unlock()
	if err := agent.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	agent.reportStatuses(ctx, exec)
	if got := cp.lastStatus(); len(got) != 2 {
		t.Fatalf("after the jobs were withdrawn the status still holds %+v", got)
	}
}

// A refusal is no status of a job that has a workload: what exists is what is reported.
func TestARefusedCreateIsNotReportedOverAWorkloadThatExists(t *testing.T) {
	exec := &captureExecutor{uncreatable: map[string]error{"exp-a": &agentexec.Unrunnable{Why: "refused"}}}
	cp := &controlPlane{desired: `[{"id":"exp-a"}]`}
	agent := newTestAgent(cp.serve(t), exec)
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	exec.managed = []string{"exp-a"}
	agent.reportStatuses(context.Background(), exec)
	if got := cp.lastStatus(); len(got) != 1 || got[0].Reason == clusterapi.PhaseReasonConfigError {
		t.Fatalf("reported %+v, want the workload's own status once", got)
	}
}

// A runtime that runs any image reports none, and no runtime says an image is only a default.
func TestTheReconcileReportCarriesTheOneImageTheRuntimeRuns(t *testing.T) {
	for _, image := range []string{"registry.test/platform-job:7", ""} {
		cp := &controlPlane{desired: `[]`}
		if err := newTestAgent(cp.serve(t), &captureExecutor{onlyImage: image}).reconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := cp.reconcile[0]["job_image"]; got != image {
			t.Fatalf("job_image = %v, want %q", got, image)
		}
		if _, sent := cp.reconcile[0]["job_image_default"]; sent {
			t.Fatal("the report carries job_image_default")
		}
	}
}

// The answer's registry_pull is what every job created in the pass is handed, and a pass whose
// answer has none hands none.
func TestEveryJobCreatedIsHandedTheAnswersPullLogin(t *testing.T) {
	exec := &captureExecutor{}
	cp := &controlPlane{
		desired:      `[{"id":"exp-env","environment_key":"k1"},{"id":"exp-plain"}]`,
		registryPull: `{"registry":"europe-west1-docker.pkg.dev","username":"oauth2accesstoken","password":"pull-token","expires_at":"2030-01-01T00:00:00Z"}`,
	}
	agent := newTestAgent(cp.serve(t), exec)
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exp-env", "exp-plain"} {
		if pull := exec.pulls[id]; pull == nil || pull.Registry != "europe-west1-docker.pkg.dev" || pull.Username != "oauth2accesstoken" || pull.Password != "pull-token" {
			t.Fatalf("%s was handed %+v, want the answer's registry_pull", id, pull)
		}
	}

	cp.mu.Lock()
	cp.desired, cp.registryPull = `[{"id":"exp-next"}]`, ""
	cp.mu.Unlock()
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pull, created := exec.pulls["exp-next"]; !created || pull != nil {
		t.Fatalf("exp-next was handed %+v, want no login from an answer that has none", pull)
	}
}

type refreshingExecutor struct {
	*captureExecutor
	refreshed map[string]string
}

func (e *refreshingExecutor) RefreshPull(_ context.Context, exp *clusterapi.DesiredJob, pull *clusterapi.RegistryLogin) error {
	e.refreshed[exp.ID] = pull.Password
	return nil
}

// Every pass hands the answer's registry_pull to each live job that runs in an environment, so a
// pod that pulls again after its first login expired pulls with the current one.
func TestEveryLiveJobInAnEnvironmentIsHandedTheCurrentPullLogin(t *testing.T) {
	exec := &refreshingExecutor{captureExecutor: &captureExecutor{managed: []string{"exp-env", "exp-plain"}}, refreshed: map[string]string{}}
	cp := &controlPlane{
		desired:      `[{"id":"exp-env","environment_key":"k1"},{"id":"exp-plain"}]`,
		registryPull: `{"registry":"europe-west1-docker.pkg.dev","username":"oauth2accesstoken","password":"pull-token","expires_at":"2030-01-01T00:00:00Z"}`,
	}
	agent := newTestAgent(cp.serve(t), nil)
	agent.Executor = exec
	if err := agent.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(exec.refreshed) != 1 || exec.refreshed["exp-env"] != "pull-token" {
		t.Fatalf("refreshed %v, want exp-env alone with the answer's login", exec.refreshed)
	}
	if exec.created != 0 {
		t.Fatalf("created %d live jobs again", exec.created)
	}
}
