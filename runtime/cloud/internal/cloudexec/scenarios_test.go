package cloudexec

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

var errVendor = errors.New("vendor API unavailable")

func failing(method, name string) func(string, string) error {
	return func(m, n string) error {
		if m == method && (name == "" || n == name) {
			return errVendor
		}
		return nil
	}
}

// A rented machine that never comes up stays pending and unplaced for as long as it is desired,
// which is what the control plane's stuck-pending timeout bounds. It is not rented a second time
// meanwhile, it holds its slot, and the eviction that follows destroys it.
func TestAnInstanceThatNeverBootsWaitsUnplacedUntilItIsNoLongerDesired(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 0, 1)
	for pass := 0; pass < 3; pass++ {
		mustCreate(t, e, exp)
		matches, err := e.WorkloadMatchesDesired(ctx, exp)
		_, _, _, scheduled, _, detailErr := e.PollPhaseDetail(ctx, "exp-1", 0)
		phase, attempt := status(t, e, "exp-1")
		if err != nil || detailErr != nil || !matches || phase != workload.JobPhasePending || attempt != 0 || scheduled != 0 {
			t.Fatalf("pass %d: matches %v, phase %v, attempt %d, scheduled %d, %v %v", pass, matches, phase, attempt, scheduled, err, detailErr)
		}
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a0-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
	if available, _, _, _, _ := e.GetLiveAcceleratorCapacitySnapshot(ctx); available[h100] != 1 {
		t.Fatalf("available = %v, want the booting instance's slot taken", available)
	}

	// stuck_pending is an infrastructure fault, so its delete carries no grant.
	if err := e.DeleteWorkload(ctx, "exp-1", false); err != nil || len(p.Names()) != 0 {
		t.Fatalf("delete: %v, rented %v", err, p.Names())
	}
	if available, _, _, _, _ := e.GetLiveAcceleratorCapacitySnapshot(ctx); available[h100] != 2 {
		t.Fatalf("available = %v after the destroy", available)
	}
}

// A vendor can take a machine back at any time. The job then reads as gone, with no attempt to
// fence on, and a pass that still desires it rents it again.
func TestAnInstanceThatVanishesMidRunReadsAsGoneAndIsRentedAgain(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 4, 0)
	mustCreate(t, e, exp)
	p.Start("hl-exp-1-a4-cpu-0")

	if err := p.Destroy(ctx, "fake-1"); err != nil {
		t.Fatal(err)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhaseGone || attempt != workload.AttemptUnknown {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}
	actual, err := e.ListManagedJobs(ctx)
	reported, statusErr := e.ListManagedJobsForStatus(ctx)
	tail, tailErr := e.FetchLogTail(ctx, "exp-1", 10)
	if err != nil || statusErr != nil || tailErr != nil || len(actual) != 0 || len(reported) != 0 || tail != nil {
		t.Fatalf("actual %v, reported %v, tail %v, %v %v %v", actual, reported, tail, err, statusErr, tailErr)
	}
	if acceleratorType, node, _, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-1"); err != nil || acceleratorType != "" || node != "" {
		t.Fatalf("admitted = %q on %q, %v", acceleratorType, node, err)
	}

	mustCreate(t, e, exp)
	if phase, attempt := status(t, e, "exp-1"); phase != workload.JobPhasePending || attempt != 4 {
		t.Fatalf("after renting again: %v attempt %d", phase, attempt)
	}
}

// Every read goes to the provider, so when the provider cannot be read nothing is answered from
// memory or guessed: the reconcile pass and the status push fail and are tried again.
func TestAnUnreadableProviderFailsEveryReadInsteadOfAnsweringEmpty(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 0, 1)
	mustCreate(t, e, exp)
	p.Fail = failing("List", "")

	_, jobsErr := e.ListManagedJobs(ctx)
	_, reportedErr := e.ListManagedJobsForStatus(ctx)
	_, pollErr := e.PollJobStatus(ctx, "exp-1")
	_, matchErr := e.WorkloadMatchesDesired(ctx, exp)
	_, _, cpuErr := e.GetLiveCPUCapacity(ctx)
	_, _, ramErr := e.GetLiveRAMCapacity(ctx)
	_, _, storageErr := e.GetLiveStorageCapacity(ctx)
	_, nodeErr := e.GetLiveNodeResourceCapacity(ctx)
	_, _, _, _, acceleratorErr := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	_, tailErr := e.FetchLogTail(ctx, "exp-1", 10)
	_, _, _, _, _, detailErr := e.PollPhaseDetail(ctx, "exp-1", 0)
	_, _, _, admittedErr := e.ResolveAdmittedAcceleratorType(ctx, "exp-1")
	for name, err := range map[string]error{
		"ListManagedJobs": jobsErr, "ListManagedJobsForStatus": reportedErr, "PollJobStatus": pollErr,
		"WorkloadMatchesDesired": matchErr, "GetLiveCPUCapacity": cpuErr, "GetLiveRAMCapacity": ramErr,
		"GetLiveStorageCapacity": storageErr, "GetLiveNodeResourceCapacity": nodeErr,
		"GetLiveAcceleratorCapacitySnapshot": acceleratorErr, "FetchLogTail": tailErr, "PollPhaseDetail": detailErr,
		"ResolveAdmittedAcceleratorType": admittedErr,
		"CreateWorkload":                 e.CreateWorkload(ctx, job(t, "exp-2", 0, 1)),
		"DeleteWorkload":                 e.DeleteWorkload(ctx, "exp-1", false),
		"ReapTerminal":                   e.ReapTerminal(ctx, nil),
	} {
		if !errors.Is(err, errVendor) {
			t.Errorf("%s: err = %v, want the provider's error", name, err)
		}
	}

	p.Fail = nil
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a0-h100-0"}) {
		t.Fatalf("a failed pass changed what is rented: %v", names)
	}
}

func TestAFailedCreateRentsNothingAndTheNextPassTriesAgain(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 0, 1)
	p.Fail = failing("Create", "")
	if err := e.CreateWorkload(ctx, exp); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v", err)
	}
	if phase, _ := status(t, e, "exp-1"); phase != workload.JobPhaseGone || len(p.Names()) != 0 {
		t.Fatalf("phase %v, rented %v", phase, p.Names())
	}
	p.Fail = nil
	mustCreate(t, e, exp)
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a0-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
}

// One instance the provider will not destroy must not keep the others rented (important.md #19).
func TestOneFailedDestroyStillDestroysTheRest(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	for _, id := range []string{"exp-1", "exp-2"} {
		mustCreate(t, e, job(t, id, 0, 1))
	}
	mustCreate(t, e, job(t, "exp-3", 0, 0))
	for _, name := range p.Names() {
		p.Exit(name, exitline.Format(0))
	}
	p.Fail = failing("Destroy", "hl-exp-1-a0-h100-0")

	if err := e.ReapTerminal(ctx, nil); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v, want the failed destroy reported", err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-1-a0-h100-0"}) {
		t.Fatalf("rented %v, want only the one that failed", names)
	}
	p.Fail = nil
	if err := e.ReapTerminal(ctx, nil); err != nil || len(p.Names()) != 0 {
		t.Fatalf("next pass: %v, rented %v", err, p.Names())
	}
}

// A stop that fails is reported and leaves the instance running with its window unspent. It is
// never turned into a destroy.
func TestAFailedStopLeavesTheInstanceRunningForTheNextPass(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-exp-1-a0-cpu-0"
	mustCreate(t, e, job(t, "exp-1", 0, 0))
	p.Start(name)
	p.Fail = failing("Stop", name)
	if err := e.DeleteWorkload(ctx, "exp-1", true); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v", err)
	}
	p.Fail = nil
	if phase, _ := status(t, e, "exp-1"); phase != workload.JobPhaseRunning || p.Stops(name) != 0 {
		t.Fatalf("phase %v, stops %d", phase, p.Stops(name))
	}
	if err := e.DeleteWorkload(ctx, "exp-1", true); err != nil || p.Stops(name) != 1 {
		t.Fatalf("next pass: %v, stops %d", err, p.Stops(name))
	}
}

// An exited instance whose output cannot be read has no known outcome. Reporting one would
// finish the job on a guess, so the poll fails and the status push is withheld.
func TestAnExitedInstanceWithUnreadableOutputHasNoPhaseToReport(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-exp-1-a0-cpu-0"
	mustCreate(t, e, job(t, "exp-1", 0, 0))
	p.Start(name)
	p.Exit(name, exitline.Format(0))
	p.Fail = failing("Logs", name)

	if _, err := e.PollJobStatus(ctx, "exp-1"); !errors.Is(err, errVendor) {
		t.Fatalf("poll err = %v", err)
	}
	if _, _, _, _, _, err := e.PollPhaseDetail(ctx, "exp-1", 0); !errors.Is(err, errVendor) {
		t.Fatalf("detail err = %v", err)
	}
	p.Fail = nil
	if phase, _ := status(t, e, "exp-1"); phase != workload.JobPhaseSucceeded {
		t.Fatalf("phase = %v once the output is readable", phase)
	}
}

// Instances outlive a config edit: a shape can be removed or its max_instances lowered while
// instances of it run. They stay this runtime's to report and delete, and they are in no slot, so
// the capacity report stays within its totals.
func TestInstancesInNoConfiguredSlotAreManagedAndCountedNowhere(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	for _, name := range []string{"hl-exp-old-a0-a100-0", "hl-exp-over-a2-h100-5"} {
		if _, err := p.Create(ctx, provider.Spec{Name: name}); err != nil {
			t.Fatal(err)
		}
		p.Start(name)
	}

	cpu, cpuTotal, err := e.GetLiveCPUCapacity(ctx)
	available, total, byNode, labels, acceleratorErr := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	nodes, nodeErr := e.GetLiveNodeResourceCapacity(ctx)
	if err != nil || acceleratorErr != nil || nodeErr != nil || cpu != cpuTotal || available[h100] != total[h100] ||
		len(available) != 1 || len(byNode) != 2 || len(labels) != 3 || len(nodes) != 3 {
		t.Fatalf("cpu %v/%v, accelerators %v/%v, by node %v, labels %v, nodes %v", cpu, cpuTotal, available, total, byNode, labels, nodes)
	}

	ids, err := e.ListManagedJobs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{"exp-old", "exp-over"}) {
		t.Fatalf("managed = %v, %v", ids, err)
	}
	if phase, attempt := status(t, e, "exp-over"); phase != workload.JobPhaseRunning || attempt != 2 {
		t.Fatalf("status = %v attempt %d", phase, attempt)
	}
	acceleratorType, node, _, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-old")
	if err != nil || acceleratorType != "" || node != "a100-0" {
		t.Fatalf("admitted = %q on %q, %v", acceleratorType, node, err)
	}
	for _, id := range ids {
		if err := e.DeleteWorkload(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("still rented: %v", names)
	}
}

// The agent keeps nothing, so a restarted one finds its jobs in the account: it reports them as
// before, rents none of them again, and can still stop and reap them.
func TestARestartedAgentTakesOverTheInstancesAlreadyRented(t *testing.T) {
	before, p := newTestExecutor(t)
	ctx := context.Background()
	running, finished, booting := job(t, "exp-running", 1, 1), job(t, "exp-finished", 0, 0), job(t, "exp-booting", 0, 1)
	for _, exp := range []*domain.Experiment{running, finished, booting} {
		mustCreate(t, before, exp)
	}
	p.Start("hl-exp-running-a1-h100-0")
	p.Start("hl-exp-finished-a0-cpu-0")
	p.Exit("hl-exp-finished-a0-cpu-0", exitline.Format(0))
	rented := p.Names()

	after, err := New(Config{ClusterID: "fake-account-1", APIURL: testAPI, JobImage: "registry.example.invalid/job:1", Shapes: testShapes, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]*domain.Experiment{}
	for _, exp := range []*domain.Experiment{running, finished, booting} {
		desired[exp.ID] = exp
		mustCreate(t, after, exp)
	}
	if err := after.ReapTerminal(ctx, desired); err != nil || !reflect.DeepEqual(p.Names(), rented) {
		t.Fatalf("a pass after the restart changed what is rented: %v, %v", p.Names(), err)
	}
	for id, want := range map[string]workload.JobPhase{
		"exp-running": workload.JobPhaseRunning, "exp-finished": workload.JobPhaseSucceeded, "exp-booting": workload.JobPhasePending,
	} {
		if phase, attempt := status(t, after, id); phase != want || attempt != desired[id].AttemptCount {
			t.Errorf("%s: %v attempt %d", id, phase, attempt)
		}
	}
	if available, _, _, _, err := after.GetLiveAcceleratorCapacitySnapshot(ctx); err != nil || available[h100] != 0 {
		t.Fatalf("available = %v, %v", available, err)
	}

	// The window of a job stopped before the restart is still the wrapper's to keep: the new
	// process stops it again, which the wrapper ignores, and reaps it once it has exited.
	if err := before.DeleteWorkload(ctx, "exp-running", true); err != nil {
		t.Fatal(err)
	}
	if err := after.DeleteWorkload(ctx, "exp-running", true); err != nil || p.Stops("hl-exp-running-a1-h100-0") != 2 {
		t.Fatalf("stop after restart: %v, %d stops", err, p.Stops("hl-exp-running-a1-h100-0"))
	}
	p.Exit("hl-exp-running-a1-h100-0", exitline.Format(0))
	delete(desired, "exp-running")
	if err := after.ReapTerminal(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-exp-booting-a0-h100-1", "hl-exp-finished-a0-cpu-0"}) {
		t.Fatalf("rented %v", names)
	}
}
