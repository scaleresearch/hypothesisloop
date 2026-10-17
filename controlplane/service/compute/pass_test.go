package compute

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
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

type vendorCalls struct{ list, logs int }

func callsOf(p *fake.Provider) vendorCalls {
	return vendorCalls{list: p.Calls("List"), logs: p.Calls("Logs")}
}

// fakeControlPlane answers the loop with desired and records what the vendor had been called by the
// time each status push arrived.
type fakeControlPlane struct {
	*httptest.Server
	mu      sync.Mutex
	pushes  []vendorCalls
	reports []pushedReport
}

func newControlPlane(t *testing.T, p *fake.Provider, desired ...*clusterapi.DesiredJob) *fakeControlPlane {
	t.Helper()
	answer, err := json.Marshal(map[string]any{"experiments": desired})
	if err != nil {
		t.Fatal(err)
	}
	cp := &fakeControlPlane{}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_, _ = w.Write(answer)
			return
		}
		var body struct {
			Reports []pushedReport `json:"reports"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.mu.Lock()
		cp.pushes = append(cp.pushes, callsOf(p))
		cp.reports = append(cp.reports, body.Reports...)
		cp.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(cp.Close)
	return cp
}

func (cp *fakeControlPlane) seen() []vendorCalls {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]vendorCalls(nil), cp.pushes...)
}

func (cp *fakeControlPlane) reported() []pushedReport {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]pushedReport(nil), cp.reports...)
}

// run drives the real loop until done reports true.
func run(t *testing.T, e *Executor, cp *fakeControlPlane, reconcile, status time.Duration, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	agent := &agentloop.Agent{
		ClusterName: "test", ControlPlane: &agentloop.Remote{ClusterName: "test", APIURL: cp.URL, HTTP: cp.Client()}, Executor: e,
		ReconcileInterval: reconcile, StatusInterval: status, Log: func(string, ...any) {},
	}
	stopped := make(chan struct{})
	go func() { agent.Run(ctx); close(stopped) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-stopped
	if !done() {
		t.Fatal("the loop did not get there in time")
	}
}

// rented puts one running, one finished and one booting job on the account.
func rented(t *testing.T, e *Executor, p *fake.Provider) (keep, done, booting *clusterapi.DesiredJob) {
	t.Helper()
	keep, done, booting = job(t, "exp-keep", 0, 1), job(t, "exp-done", 2, 0), on(job(t, "exp-booting", 0, 1), "h100-1")
	mustCreate(t, e, keep)
	mustCreate(t, e, done)
	mustCreate(t, e, booting)
	p.Start("hl-job-t1-exp-keep-a0-h100-0")
	p.Print("hl-job-t1-exp-keep-a0-h100-0", "epoch 1")
	p.Start("hl-job-t1-exp-done-a2-cpu-0")
	p.Exit("hl-job-t1-exp-done-a2-cpu-0", exitline.Format(0))
	return keep, done, booting
}

// One reconcile pass through the real loop: seven capacity reads, a match per desired job, a
// create, the status push before a delete, a destroy and the reap. All of it is answered from
// one List, and the push reads each started instance's output once.
func TestAReconcilePassCostsOneList(t *testing.T) {
	e, p := newTestExecutor(t)
	keep, gone, created := job(t, "exp-keep", 0, 1), job(t, "exp-gone", 1, 0), on(job(t, "exp-new", 0, 1), "h100-1")
	mustCreate(t, e, keep)
	mustCreate(t, e, gone)
	p.Start("hl-job-t1-exp-keep-a0-h100-0")
	p.Start("hl-job-t1-exp-gone-a1-cpu-0")
	cp := newControlPlane(t, p, keep, created)
	before := callsOf(p)
	creates, destroys := p.Calls("Create"), p.Calls("Destroy")
	want := []string{"hl-job-t1-exp-keep-a0-h100-0", "hl-job-t1-exp-new-a0-h100-1"}

	run(t, e, cp, time.Hour, time.Hour, func() bool { return reflect.DeepEqual(p.Names(), want) })

	after := callsOf(p)
	if got := after.list - before.list; got != 1 {
		t.Errorf("List calls in one reconcile pass = %d, want 1", got)
	}
	// The push before the delete reports exp-keep and exp-gone, and exp-new still booting.
	if got := after.logs - before.logs; got != 2 {
		t.Errorf("Logs calls in one reconcile pass = %d, want one per started instance in its one status push (2)", got)
	}
	if p.Calls("Create")-creates != 1 || p.Calls("Destroy")-destroys != 1 || len(cp.seen()) != 1 {
		t.Errorf("creates %d, destroys %d, pushes %d", p.Calls("Create")-creates, p.Calls("Destroy")-destroys, len(cp.seen()))
	}
}

// One status push through the real loop costs one List and one Logs per instance whose command
// has started. A booting instance has no output to read.
func TestAStatusPushCostsOneListAndOneLogsPerStartedInstance(t *testing.T) {
	e, p := newTestExecutor(t)
	keep, done, booting := rented(t, e, p)
	cp := newControlPlane(t, p, keep, done, booting)

	run(t, e, cp, time.Hour, 20*time.Millisecond, func() bool { return len(cp.seen()) >= 4 })

	// The reconcile pass ran once, before the first push. Every push after the first is alone
	// between two arrivals.
	pushes := cp.seen()
	for n := 2; n < len(pushes); n++ {
		if list, logs := pushes[n].list-pushes[n-1].list, pushes[n].logs-pushes[n-1].logs; list != 1 || logs != 2 {
			t.Errorf("push %d cost %d List and %d Logs, want 1 and 2", n, list, logs)
		}
	}
}

// What a pass read is the pass's alone. It follows its own creates and destroys, sees nothing
// that happened outside it, and the next pass reads the account again.
func TestAPassFollowsItsOwnChangesAndIsReadAgainForTheNext(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 0))

	scoped, err := e.ForPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pass := scoped.(*Executor)
	mustCreate(t, pass, job(t, "exp-2", 0, 1))
	if err := pass.CreateWorkload(ctx, on(job(t, "exp-3", 0, 1), "h100-0"), nil); err == nil {
		t.Fatal("a slot rented earlier in the pass was rented again")
	}
	if err := pass.DeleteWorkload(ctx, "exp-1", false); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, pass, on(job(t, "exp-4", 0, 0), "cpu-0"))
	ids, _ := pass.ListManagedJobsForStatus(ctx)
	if !reflect.DeepEqual(sorted(ids), []string{"exp-2", "exp-4"}) || p.Calls("List") != 2 {
		t.Fatalf("pass sees %v after %d List calls", ids, p.Calls("List"))
	}
	if phase, _ := status(t, pass, "exp-2"); phase != clusterapi.JobPhasePending {
		t.Fatalf("an instance created in the pass reads as %v", phase)
	}

	// Outside the pass the vendor takes exp-2's machine back.
	if err := p.Destroy(ctx, p.IDs("hl-job-t1-exp-2-a0-h100-0")[0]); err != nil {
		t.Fatal(err)
	}
	if phase, _ := status(t, pass, "exp-2"); phase != clusterapi.JobPhasePending {
		t.Fatalf("the pass read the account again: %v", phase)
	}
	next, err := e.ForPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if phase, _ := status(t, next.(*Executor), "exp-2"); phase != clusterapi.JobPhaseGone {
		t.Fatalf("the next pass still sees the instance: %v", phase)
	}
	if e.pass != nil {
		t.Fatal("a pass's read was kept on the executor")
	}

	p.Fail = failing("List", "")
	if _, err := e.ForPass(ctx); err == nil {
		t.Fatal("a pass was started on an account that cannot be read")
	}
}

// One exited job whose output the vendor will not return is reported with the rest, as gone, and
// the rest are reported.
func TestOneUnreadableExitedInstanceDoesNotWithholdTheClustersPush(t *testing.T) {
	e, p := newTestExecutor(t)
	keep, done, booting := rented(t, e, p)
	p.Fail = failing("Logs", "hl-job-t1-exp-done-a2-cpu-0")

	var mu sync.Mutex
	reports := map[string]pushedReport{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			answer, _ := json.Marshal(map[string]any{"experiments": []*clusterapi.DesiredJob{keep, done, booting}})
			_, _ = w.Write(answer)
			return
		}
		var body struct {
			Reports []pushedReport `json:"reports"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, report := range body.Reports {
			reports[report.ExperimentID] = report
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	cp := &fakeControlPlane{Server: server}

	run(t, e, cp, time.Hour, 10*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reports) > 0
	})

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 3 || reports["exp-keep"].Phase != clusterapi.JobPhaseRunning.String() || reports["exp-booting"].Phase != clusterapi.JobPhasePending.String() {
		t.Fatalf("pushed %+v", reports)
	}
	unread := reports["exp-done"]
	if unread.Phase != clusterapi.JobPhaseGone.String() || unread.Attempt == nil || *unread.Attempt != 2 {
		t.Fatalf("the unreadable job was pushed as %+v", unread)
	}
	if len(p.Names()) != 3 {
		t.Fatalf("rented %v", p.Names())
	}
}

// Through the real loop: a job preempted from h100-0 is desired again under the same attempt on
// h100-1 while its old instance, stopped for the preemption, has exited with 143. The loop rents
// the job on its new slot and no push ever reports the old exit as the job having failed.
func TestTheLoopNeverReportsAPreemptedRunsExitAsTheReadmittedJobsResult(t *testing.T) {
	e, p := newTestExecutor(t)
	const old, placed = "hl-job-t1-exp-1-a3-h100-0", "hl-job-t1-exp-1-a3-h100-1"
	mustCreate(t, e, job(t, "exp-1", 3, 1))
	p.Start(old)
	p.Exit(old, "Terminated", exitline.Format(143))
	cp := newControlPlane(t, p, on(job(t, "exp-1", 3, 1), "h100-1"))

	run(t, e, cp, 5*time.Millisecond, 5*time.Millisecond, func() bool {
		return reflect.DeepEqual(p.Names(), []string{placed}) && len(cp.reported()) >= 3
	})

	for _, report := range cp.reported() {
		if report.Phase == clusterapi.JobPhaseFailed.String() || report.Phase == clusterapi.JobPhaseSucceeded.String() {
			t.Fatalf("pushed %+v", report)
		}
	}
}
