package cloudexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/exitline"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

type pushedReport struct {
	ExperimentID string   `json:"experiment_id"`
	Phase        string   `json:"phase"`
	LogTail      []string `json:"log_tail"`
	Message      string   `json:"message"`
	Attempt      *int     `json:"attempt"`
}

// Destroying an instance destroys the only copy of why its job ended. An exited instance stays
// rented, and in every status snapshot, for as long as its attempt is desired.
func TestAnExitedInstanceKeepsItsOutcomeAndOutputUntilTheControlPlaneLetsGo(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 0, 0)
	mustCreate(t, e, exp)
	p.Start("hl-job-t1-exp-1-a0-cpu-0")
	p.Exit("hl-job-t1-exp-1-a0-cpu-0", "RuntimeError: device crashed", exitline.Format(1))

	desired := map[string]*clusterapi.DesiredJob{"exp-1": exp}
	for pass := 0; pass < 2; pass++ {
		mustCreate(t, e, exp)
		if err := e.ReapTerminal(ctx, desired); err != nil {
			t.Fatal(err)
		}
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-cpu-0"}) {
		t.Fatalf("rented %v, want the exited instance kept and no second one", names)
	}
	ids, err := e.ListManagedJobsForStatus(ctx)
	tail, tailErr := e.FetchLogTail(ctx, "exp-1", 100)
	if err != nil || tailErr != nil || !reflect.DeepEqual(ids, []string{"exp-1"}) || tail[0] != "RuntimeError: device crashed" {
		t.Fatalf("status set %v, tail %q, %v %v", ids, tail, err, tailErr)
	}
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseFailed {
		t.Fatalf("phase = %v", phase)
	}

	if err := e.ReapTerminal(ctx, nil); err != nil || len(p.Names()) != 0 {
		t.Fatalf("no longer desired: %v, still rented %v", err, p.Names())
	}
}

// The shared loop pushes one last status before it deletes a workload that is no longer desired.
// Against this executor that push has to carry the running instance's output, and has to arrive
// while the instance is still rented.
func TestTheSharedLoopCapturesARunningInstancesOutputBeforeDestroyingIt(t *testing.T) {
	e, p := newTestExecutor(t)
	mustCreate(t, e, job(t, "exp-gone", 3, 0))
	p.Start("hl-job-t1-exp-gone-a3-cpu-0")
	p.Print("hl-job-t1-exp-gone-a3-cpu-0", "epoch 7 loss 0.31")

	var mu sync.Mutex
	var captured []pushedReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_, _ = w.Write([]byte(`{"experiments":[]}`))
			return
		}
		var body struct {
			Reports []pushedReport `json:"reports"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		if len(p.Names()) == 1 {
			captured = append(captured, body.Reports...)
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	agent := &agentloop.Agent{
		ClusterName:       "test",
		APIURL:            server.URL,
		Executor:          e,
		HTTPClient:        server.Client(),
		ReconcileInterval: 10 * time.Millisecond,
		StatusInterval:    time.Hour,
		Log:               func(string, ...any) {},
	}
	done := make(chan struct{})
	go func() { agent.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(p.Names()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if names := p.Names(); len(names) != 0 {
		t.Fatalf("the undesired instance was not destroyed: %v", names)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("pushes while the instance was still rented: %+v", captured)
	}
	got := captured[0]
	if got.ExperimentID != "exp-gone" || got.Phase != clusterapi.JobPhaseRunning.String() || got.Attempt == nil || *got.Attempt != 3 ||
		!reflect.DeepEqual(got.LogTail, []string{"epoch 7 loss 0.31"}) {
		t.Fatalf("final capture = %+v", got)
	}
}
