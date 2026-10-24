package compute

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
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
		_, _, _, scheduled, detailErr := e.PollPhaseDetail(ctx, "exp-1", 0)
		phase, attempt := status(t, e, "exp-1")
		if err != nil || detailErr != nil || !matches || phase != clusterapi.JobPhasePending || attempt != 0 || scheduled != 0 {
			t.Fatalf("pass %d: matches %v, phase %v, attempt %d, scheduled %d, %v %v", pass, matches, phase, attempt, scheduled, err, detailErr)
		}
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
	if available, _, _, _, _ := e.GetLiveAcceleratorCapacitySnapshot(ctx); available[h100] != 1 {
		t.Fatalf("available = %v, want the booting instance's slot taken", available)
	}
	if reason, message, _, _, _ := e.PollPhaseDetail(ctx, "exp-1", 0); reason != clusterapi.PhaseReasonInstanceStarting || message == "" {
		t.Fatalf("a starting instance reads as %q %q, want it to say what the job waits for", reason, message)
	}

	// Past the startup bound it is no longer the workload wanted: the pass destroys it and the
	// next rents another, so nothing rented and not running is kept.
	overdue := *e
	overdue.startup = time.Nanosecond
	if matches, err := overdue.WorkloadMatchesDesired(ctx, exp); err != nil || matches {
		t.Fatalf("an instance starting past the bound: matches %v, %v, want it replaced", matches, err)
	}

	// stuck_pending is an infrastructure fault, so its delete carries no grant.
	if err := e.DeleteWorkload(ctx, "exp-1", false); err != nil || len(p.Names()) != 0 {
		t.Fatalf("delete: %v, rented %v", err, p.Names())
	}
	if available, _, _, _, _ := e.GetLiveAcceleratorCapacitySnapshot(ctx); available[h100] != 2 {
		t.Fatalf("available = %v after the destroy", available)
	}
}

// A host that cannot start a container is on offer again the moment its instance is destroyed,
// and as cheap as before. What the pass that found the instance overdue rents, it rents elsewhere.
func TestWhatAPassRentsAvoidsTheHostsOfTheInstancesItFoundOverdue(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	p.RunsOn("hl-job-t1-exp-1-a0-r0of1-h100-0", "124592", "H100", "AMD EPYC 7B13", 2.25)

	mustCreate(t, e, on(job(t, "exp-2", 0, 1), "h100-1"))
	if avoided := p.Spec("hl-job-t1-exp-2-a0-r0of1-h100-1").AvoidHosts; len(avoided) != 0 {
		t.Fatalf("beside an instance still inside its time to start, a job was rented avoiding %v", avoided)
	}

	// Both are overdue now, and only the first is on a host its vendor names.
	e.startup = time.Nanosecond
	pass, err := e.ForPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := pass.DeleteWorkload(ctx, "exp-1", false); err != nil {
		t.Fatal(err)
	}
	if err := pass.CreateWorkload(ctx, job(t, "exp-3", 0, 0)); err != nil {
		t.Fatal(err)
	}
	if avoided := p.Spec("hl-job-t1-exp-3-a0-r0of1-cpu-0").AvoidHosts; !reflect.DeepEqual(avoided, []string{"124592"}) {
		t.Fatalf("after the pass destroyed an instance that never started, it rented avoiding %v, want that instance's host", avoided)
	}

	// The next pass lists no instance on that host, and nothing else names it.
	if err := e.DeleteWorkload(ctx, "exp-3", false); err != nil {
		t.Fatal(err)
	}
	e.startup = 4 * time.Minute
	mustCreate(t, e, job(t, "exp-3", 1, 0))
	if avoided := p.Spec("hl-job-t1-exp-3-a1-r0of1-cpu-0").AvoidHosts; len(avoided) != 0 {
		t.Fatalf("a later pass rented avoiding %v", avoided)
	}
}

// A host that stops answering its vendor is listed as unknown, hours into a job as well. The
// instance was rented longer ago than the startup bound and is not replaced for it: it stays the
// job's workload, holds its slot, and reads as pending and placed with no output asked for, which
// is what the control plane's check of a job gone silent bounds. The requeue that follows
// destroys it.
func TestAnInstanceTheProviderCannotPlaceIsKeptAndLeftToTheControlPlane(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-job-t1-exp-1-a0-r0of1-h100-0"
	exp := job(t, "exp-1", 0, 1)
	mustCreate(t, e, exp)
	p.Start(name)
	p.Print(name, "epoch 41")
	p.Lose(name)
	e.startup = time.Nanosecond

	for pass := 0; pass < 3; pass++ {
		mustCreate(t, e, exp)
		matches, err := e.WorkloadMatchesDesired(ctx, exp)
		reason, message, _, scheduled, detailErr := e.PollPhaseDetail(ctx, "exp-1", 0)
		phase, attempt := status(t, e, "exp-1")
		if err != nil || detailErr != nil || !matches || phase != clusterapi.JobPhasePending || attempt != 0 || scheduled != 1 || reason != clusterapi.PhaseReasonInstanceUnknown || !strings.Contains(message, "does not say whether the job runs") {
			t.Fatalf("pass %d: matches %v, phase %v, attempt %d, scheduled %d, reason %q, message %q, %v %v", pass, matches, phase, attempt, scheduled, reason, message, err, detailErr)
		}
		if tail, err := e.FetchLogTail(ctx, "exp-1", 10); err != nil || tail != nil {
			t.Fatalf("pass %d: tail %v, %v", pass, tail, err)
		}
		if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": exp}); err != nil {
			t.Fatal(err)
		}
	}
	if p.Calls("Create") != 1 || p.Calls("Destroy") != 0 || p.Calls("Logs") != 0 {
		t.Fatalf("creates %d destroys %d log reads %d, want the instance kept and unread", p.Calls("Create"), p.Calls("Destroy"), p.Calls("Logs"))
	}
	if available, _, _, _, _ := e.GetLiveAcceleratorCapacitySnapshot(ctx); available[h100] != 1 {
		t.Fatalf("available = %v, want its slot taken", available)
	}

	// The host answers again and the job is as it was.
	p.Start(name)
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseRunning {
		t.Fatalf("phase %v after the host answered again", phase)
	}

	// The control plane gives up on a host that stays away: the next attempt is not this instance.
	p.Lose(name)
	if matches, err := e.WorkloadMatchesDesired(ctx, job(t, "exp-1", 1, 1)); err != nil || matches {
		t.Fatalf("after a requeue: matches %v, %v", matches, err)
	}
	// A grant has nothing to reach in an instance no signal is known to arrive at.
	if err := e.DeleteWorkload(ctx, "exp-1", true); err != nil || len(p.Names()) != 0 || p.Calls("Stop") != 0 {
		t.Fatalf("delete: %v, rented %v, stops %d", err, p.Names(), p.Calls("Stop"))
	}
}

// A vendor can take a machine back at any time. The job then reads as gone, with no attempt to
// fence on, and a pass that still desires it rents it again.
func TestAnInstanceThatVanishesMidRunReadsAsGoneAndIsRentedAgain(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 4, 0)
	mustCreate(t, e, exp)
	p.Start("hl-job-t1-exp-1-a4-r0of1-cpu-0")

	if err := p.Destroy(ctx, "fake-0001"); err != nil {
		t.Fatal(err)
	}
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhaseGone || attempt != clusterapi.AttemptUnknown {
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
	if phase, attempt := status(t, e, "exp-1"); phase != clusterapi.JobPhasePending || attempt != 4 {
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
	_, _, _, _, detailErr := e.PollPhaseDetail(ctx, "exp-1", 0)
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
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-h100-0"}) {
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
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseGone || len(p.Names()) != 0 {
		t.Fatalf("phase %v, rented %v", phase, p.Names())
	}

	// With no machine to rent the job waits and says so, and past the bound it is one this
	// cluster cannot run, under the reason the control plane ends it for.
	p.Fail = func(method, _ string) error {
		if method == "Create" {
			return fmt.Errorf("vendor: %w", provider.ErrNoOffer)
		}
		return nil
	}
	admitted := time.Now()
	exp.SubmittedAt = &admitted
	exp = asHanded(t, exp)
	var waiting *agentexec.Waiting
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &waiting) || waiting.Reason != clusterapi.PhaseReasonWaitingForOffer || !strings.Contains(waiting.Why, "h100") {
		t.Fatalf("no offer yet: err = %v, want the job waiting for one for its shape", err)
	}
	admitted = time.Now().Add(-time.Hour)
	exp.SubmittedAt = &admitted
	var unrunnable *agentexec.Unrunnable
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &unrunnable) || unrunnable.Reason != clusterapi.PhaseReasonNoOffer {
		t.Fatalf("no offer past the bound: err = %v, want no_offer", err)
	}

	p.Fail = nil
	mustCreate(t, e, exp)
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-h100-0"}) {
		t.Fatalf("rented %v", names)
	}
}

// One instance the provider will not destroy must not keep the others rented.
func TestOneFailedDestroyStillDestroysTheRest(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, job(t, "exp-1", 0, 1))
	mustCreate(t, e, on(job(t, "exp-2", 0, 1), "h100-1"))
	mustCreate(t, e, job(t, "exp-3", 0, 0))
	for _, name := range p.Names() {
		p.Exit(name, exitLine(0))
	}
	p.Fail = failing("Destroy", "hl-job-t1-exp-1-a0-r0of1-h100-0")

	if err := e.ReapTerminal(ctx, nil); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v, want the failed destroy reported", err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-h100-0"}) {
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
	const name = "hl-job-t1-exp-1-a0-r0of1-cpu-0"
	mustCreate(t, e, job(t, "exp-1", 0, 0))
	p.Start(name)
	p.Fail = failing("Stop", name)
	if err := e.DeleteWorkload(ctx, "exp-1", true); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v", err)
	}
	p.Fail = nil
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseRunning || p.Stops(name) != 0 {
		t.Fatalf("phase %v, stops %d", phase, p.Stops(name))
	}
	if err := e.DeleteWorkload(ctx, "exp-1", true); err != nil || p.Stops(name) != 1 {
		t.Fatalf("next pass: %v, stops %d", err, p.Stops(name))
	}
}

// An exited instance whose output cannot be read has no known outcome. Reporting one would
// finish the job on a guess, and failing the poll would withhold every other job's status with
// it. Reported as still running it would never end: a running report counts as liveness, so the
// control plane never finds the job silent. It is reported gone, which the control plane
// confirms over several snapshots, so a read that fails once costs nothing, and then requeues as
// an infrastructure fault.
func TestAnExitedInstanceWithUnreadableOutputIsReportedGone(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-job-t1-exp-1-a0-r0of1-cpu-0"
	exp := job(t, "exp-1", 0, 0)
	mustCreate(t, e, exp)
	p.Start(name)
	p.Exit(name, exitLine(0))
	p.Fail = failing("Logs", name)

	observed, err := e.PollJobStatus(ctx, "exp-1")
	if err != nil || observed.Phase != clusterapi.JobPhaseGone || observed.Attempt != 0 {
		t.Fatalf("poll = %+v, %v", observed, err)
	}
	if reason, message, _, _, err := e.PollPhaseDetail(ctx, "exp-1", 0); err != nil || reason != "" || message != "" {
		t.Fatalf("detail = %q %q, %v", reason, message, err)
	}
	// While its attempt is desired the instance is kept and not rented again, so a read that
	// works on a later push still finds the outcome.
	mustCreate(t, e, exp)
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": exp}); err != nil || len(p.IDs(name)) != 1 {
		t.Fatalf("reap: %v, rented %v", err, p.Names())
	}
	p.Fail = nil
	if phase, _ := status(t, e, "exp-1"); phase != clusterapi.JobPhaseSucceeded {
		t.Fatalf("phase = %v once the output is readable", phase)
	}
}

// An instance that ended without an exit line is reported gone and kept: renting the attempt
// again is the control plane's decision, made by requeueing it under a new attempt.
func TestAnInstanceThatEndedWithoutAnExitLineIsGoneAndNotRentedAgain(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	const name = "hl-job-t1-exp-1-a0-r0of1-cpu-0"
	exp := job(t, "exp-1", 0, 0)
	desired := map[string]*clusterapi.DesiredJob{"exp-1": exp}
	mustCreate(t, e, exp)
	p.Start(name)
	p.Exit(name, "epoch 4", "Killed")

	for pass := 0; pass < 2; pass++ {
		observed, err := e.PollJobStatus(ctx, "exp-1")
		if err != nil || observed.Phase != clusterapi.JobPhaseGone || observed.Attempt != 0 {
			t.Fatalf("pass %d: poll = %+v, %v", pass, observed, err)
		}
		mustCreate(t, e, exp)
		if err := e.ReapTerminal(ctx, desired); err != nil {
			t.Fatal(err)
		}
	}
	if ids := p.IDs(name); len(ids) != 1 || len(p.Names()) != 1 {
		t.Fatalf("rented %v", p.Names())
	}
	// The requeue the control plane answers with leaves the dead attempt behind.
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": on(job(t, "exp-1", 1, 0), "cpu-0")}); err != nil || len(p.Names()) != 0 {
		t.Fatalf("after the requeue: %v, rented %v", err, p.Names())
	}
}

// Two instances of one attempt: a create the vendor carried out and reported failed, followed by
// the retry. One is kept, the same one whichever pass finds them, and it is the one reported.
func TestDuplicateInstancesOfOneAttemptAreReducedToTheLowestID(t *testing.T) {
	for name, exited := range map[string]bool{"running": false, "exited": true} {
		t.Run(name, func(t *testing.T) {
			e, p := newTestExecutor(t)
			ctx := context.Background()
			exp := job(t, "exp-1", 0, 0)
			const instance = "hl-job-t1-exp-1-a0-r0of1-cpu-0"
			for n := 0; n < 3; n++ {
				if _, err := p.Create(ctx, provider.Spec{Name: instance, Architecture: "amd64"}); err != nil {
					t.Fatal(err)
				}
			}
			p.Start(instance)
			if exited {
				p.Exit(instance, exitLine(0))
			}
			other := on(job(t, "exp-2", 0, 1), "h100-0")
			mustCreate(t, e, other)

			desired := map[string]*clusterapi.DesiredJob{"exp-1": exp, "exp-2": other}
			if err := e.ReapTerminal(ctx, desired); err != nil {
				t.Fatal(err)
			}
			if ids := p.IDs(instance); !reflect.DeepEqual(ids, []string{"fake-0001"}) {
				t.Fatalf("kept %v, want the lowest id alone", ids)
			}
			if len(p.Names()) != 2 {
				t.Fatalf("rented %v", p.Names())
			}
			if observed, err := e.PollJobStatus(ctx, "exp-1"); err != nil || observed.Attempt != 0 || observed.Phase == clusterapi.JobPhaseGone {
				t.Fatalf("poll = %+v, %v", observed, err)
			}
		})
	}
}

// Instances outlive a config edit: a shape can be renamed or its max_instances lowered while
// instances of it run. One whose job is no longer desired is this runtime's to list and delete
// like any other, so it never needs an operator. One whose job is still desired is in no slot and
// billed beside a full account: the pass says so for that job and leaves it running.
func TestAnInstanceInNoConfiguredSlotIsDeletedUnlessItsJobIsDesired(t *testing.T) {
	ctx := context.Background()
	for name, slot := range map[string]string{"hl-job-t1-exp-old-a0-r0of1-a100-0": "a100-0", "hl-job-t1-exp-old-a0-r0of1-h100-5": "h100-5"} {
		e, p := newTestExecutor(t)
		if _, err := p.Create(ctx, provider.Spec{Name: name, Architecture: "amd64"}); err != nil {
			t.Fatal(err)
		}
		p.Start(name)

		if matches, err := e.WorkloadMatchesDesired(ctx, on(job(t, "exp-old", 0, 0), slot)); err == nil || matches || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s while desired: matches %v, err = %v, want an error naming it", name, matches, err)
		}
		if names := p.Names(); !reflect.DeepEqual(names, []string{name}) {
			t.Fatalf("%s while desired: rented %v, want it left running", name, names)
		}

		ids, err := e.ListManagedJobs(ctx)
		if err != nil || !reflect.DeepEqual(ids, []string{"exp-old"}) {
			t.Fatalf("managed = %v, %v", ids, err)
		}
		if err := e.DeleteWorkload(ctx, "exp-old", false); err != nil {
			t.Fatal(err)
		}
		if names := p.Names(); len(names) != 0 {
			t.Fatalf("%s when no longer desired: still rented %v", name, names)
		}
	}
}

// The prefix is this runtime's own, so an instance under it is its to pay for whatever the rest
// of the name says. One that reads as no job can be desired by nobody: it is destroyed by the
// pass, and takes no slot until then.
func TestAnInstanceUnderThePrefixThatNamesNoJobIsDestroyed(t *testing.T) {
	e, p := newTestExecutor(t)
	ctx := context.Background()
	exp := job(t, "exp-1", 0, 0)
	mustCreate(t, e, exp)
	for _, name := range []string{"hl-job-t1-something-unknown", "hl-job-t1-exp-1-ax-cpu-0", "hl-job-t1-"} {
		if _, err := p.Create(ctx, provider.Spec{Name: name, Architecture: "amd64"}); err != nil {
			t.Fatal(err)
		}
		p.Start(name)
	}
	if ids, err := e.ListManagedJobs(ctx); err != nil || !reflect.DeepEqual(ids, []string{"exp-1"}) {
		t.Fatalf("managed = %v, %v", ids, err)
	}

	p.Fail = failing("Destroy", "hl-job-t1-something-unknown")
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": exp}); err == nil {
		t.Fatal("a destroy that failed was not reported")
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-cpu-0", "hl-job-t1-something-unknown"}) {
		t.Fatalf("rented %v, want the job and the one instance whose destroy failed", names)
	}
	p.Fail = nil
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-1": exp}); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-1-a0-r0of1-cpu-0"}) {
		t.Fatalf("rented %v, want the job alone", names)
	}
}

// The agent keeps nothing, so a restarted one finds its jobs in the account: it reports them as
// before, rents none of them again, and can still stop and reap them.
func TestARestartedAgentTakesOverTheInstancesAlreadyRented(t *testing.T) {
	before, p := newTestExecutor(t)
	ctx := context.Background()
	running, finished, booting := job(t, "exp-running", 1, 1), job(t, "exp-finished", 0, 0), on(job(t, "exp-booting", 0, 1), "h100-1")
	for _, exp := range []*clusterapi.DesiredJob{running, finished, booting} {
		mustCreate(t, before, exp)
	}
	p.Start("hl-job-t1-exp-running-a1-r0of1-h100-0")
	p.Start("hl-job-t1-exp-finished-a0-r0of1-cpu-0")
	p.Exit("hl-job-t1-exp-finished-a0-r0of1-cpu-0", exitLine(0))
	rented := p.Names()

	after, err := New(testConfig(p))
	if err != nil {
		t.Fatal(err)
	}
	desired := map[string]*clusterapi.DesiredJob{}
	for _, exp := range []*clusterapi.DesiredJob{running, finished, booting} {
		desired[exp.ID] = exp
		mustCreate(t, after, exp)
	}
	if err := after.ReapTerminal(ctx, desired); err != nil || !reflect.DeepEqual(p.Names(), rented) {
		t.Fatalf("a pass after the restart changed what is rented: %v, %v", p.Names(), err)
	}
	for id, want := range map[string]clusterapi.JobPhase{
		"exp-running": clusterapi.JobPhaseRunning, "exp-finished": clusterapi.JobPhaseSucceeded, "exp-booting": clusterapi.JobPhasePending,
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
	if err := after.DeleteWorkload(ctx, "exp-running", true); err != nil || p.Stops("hl-job-t1-exp-running-a1-r0of1-h100-0") != 2 {
		t.Fatalf("stop after restart: %v, %d stops", err, p.Stops("hl-job-t1-exp-running-a1-r0of1-h100-0"))
	}
	p.Exit("hl-job-t1-exp-running-a1-r0of1-h100-0", exitLine(0))
	delete(desired, "exp-running")
	if err := after.ReapTerminal(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); !reflect.DeepEqual(names, []string{"hl-job-t1-exp-booting-a0-r0of1-h100-1", "hl-job-t1-exp-finished-a0-r0of1-cpu-0"}) {
		t.Fatalf("rented %v", names)
	}
}

// distinctHosts puts an instance on the first host its spec does not avoid, as a vendor that
// names hosts does.
func distinctHosts(spec provider.Spec) string {
	for n := 0; ; n++ {
		if host := fmt.Sprintf("host-%d", n); !slices.Contains(spec.AvoidHosts, host) {
			return host
		}
	}
}

// newGangExecutor is newTestExecutor on a vendor that names the host of every instance.
func newGangExecutor(t *testing.T) (*Executor, *fake.Provider) {
	t.Helper()
	e, p := newTestExecutor(t)
	p.Host = distinctHosts
	return e, p
}

// gangJob is job of two nodes on H100s, admitted to both H100 slots, rank 0 on h100-0.
func gangJob(t *testing.T, id string, attempt int) *clusterapi.DesiredJob {
	t.Helper()
	exp := job(t, id, attempt, 1)
	exp.ResolvedJob.NumNodes = 2
	exp.ResolvedJob.Slots = []string{"h100-0", "h100-1"}
	admitted := time.Now()
	exp.SubmittedAt = &admitted
	return asHanded(t, exp)
}

// rankNames are the instance names of the two ranks of gangJob.
func rankNames(id string, attempt int) (string, string) {
	return fmt.Sprintf("hl-job-t1-%s-a%d-r0of2-h100-0", id, attempt), fmt.Sprintf("hl-job-t1-%s-a%d-r1of2-h100-1", id, attempt)
}

// A job of two nodes is rented as two instances in the place the provider chose for them. Rank 1
// meets rank 0 at the address the provider gave it on the private network there, both open their
// sockets on the network's interface, and the job runs once both do and succeeds once both have.
func TestAJobOfTwoNodesIsRentedOnAPrivateNetworkAndReportedAsOne(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	exp := gangJob(t, "exp-g", 0)
	if !e.SupportsMultiNodeJobs() {
		t.Fatal("a provider with private networks runs no job of several nodes")
	}
	mustCreate(t, e, exp)
	zero, one := rankNames("exp-g", 0)
	if names := p.Names(); !reflect.DeepEqual(names, []string{zero, one}) {
		t.Fatalf("rented %v", names)
	}
	master, _, _ := p.Get(ctx, p.IDs(zero)[0])
	for r, name := range []string{zero, one} {
		spec := p.Spec(name)
		wantMaster := map[int]string{0: masterSelf, 1: master.PrivateAddress}[r]
		for key, want := range map[string]string{
			jobenv.Rank: fmt.Sprint(r), jobenv.TorchRank: fmt.Sprint(r), jobenv.WorldSize: "2", jobenv.TorchLocalRank: "0",
			jobenv.MasterAddr: wantMaster, jobenv.TorchMasterAddr: wantMaster, jobenv.MasterPort: "29500",
			jobenv.NCCLSocketInterface: "eth0", jobenv.GlooSocketInterface: "eth0", "HYPOTHESISLOOP_EXPERIMENT_ID": "exp-g",
		} {
			if spec.Env[key] != want {
				t.Errorf("rank %d: %s = %q, want %q", r, key, spec.Env[key], want)
			}
		}
		if spec.Place != "fake-place" || spec.AcceleratorCount != 1 {
			t.Errorf("rank %d: spec %+v", r, spec)
		}
	}
	if master.PrivateAddress == "" {
		t.Fatal("rank 0 has no address on its network")
	}

	scheduled := func() int32 {
		_, _, _, n, err := e.PollPhaseDetail(ctx, "exp-g", 0)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if phase, attempt := status(t, e, "exp-g"); phase != clusterapi.JobPhasePending || attempt != 0 || scheduled() != 0 {
		t.Fatalf("booting: phase %v, attempt %d, scheduled %d", phase, attempt, scheduled())
	}
	p.Start(zero)
	if phase, _ := status(t, e, "exp-g"); phase != clusterapi.JobPhasePending || scheduled() != 1 {
		t.Fatalf("one rank running: phase %v, scheduled %d", phase, scheduled())
	}
	p.Start(one)
	if phase, _ := status(t, e, "exp-g"); phase != clusterapi.JobPhaseRunning || scheduled() != 2 {
		t.Fatalf("both running: phase %v, scheduled %d", phase, scheduled())
	}
	p.RunsOn(zero, "host-0", "H100", "EPYC", 2.25)
	p.RunsOn(one, "host-1", "H100", "EPYC", 2.5)
	observed, _ := e.PollJobStatus(ctx, "exp-g")
	if observed.UID != zero || observed.Machine.InstanceID != master.ID || observed.Machine.HostID != "host-0" || observed.Machine.PricePerHourUSD != 4.75 {
		t.Fatalf("observed %+v, want rank 0's name and machine at the price of both", observed.Machine)
	}
	if matches, err := e.WorkloadMatchesDesired(ctx, exp); err != nil || !matches {
		t.Fatalf("matches %v, %v", matches, err)
	}
	mustCreate(t, e, exp)
	if names := p.Names(); len(names) != 2 {
		t.Fatalf("a second pass rented again: %v", names)
	}

	p.Exit(one, exitLineAt(0, time.Unix(1000, 0), time.Unix(1500, 0)))
	if phase, _ := status(t, e, "exp-g"); phase != clusterapi.JobPhaseRunning {
		t.Fatalf("one rank done and one running: phase %v, want running", phase)
	}
	p.Exit(zero, exitLineAt(0, time.Unix(900, 0), time.Unix(1400, 0)))
	observed, _ = e.PollJobStatus(ctx, "exp-g")
	if observed.Phase != clusterapi.JobPhaseSucceeded || !observed.StartedAt.Equal(time.Unix(900, 0)) || !observed.EndedAt.Equal(time.Unix(1500, 0)) {
		t.Fatalf("both done: %+v, want succeeded from the first start to the last end", observed)
	}

	if err := e.ReapTerminal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("once no longer desired: rented %v", names)
	}
}

// A job of several nodes is rented whole or not at all. Rank 1 has no machine once rank 0 is
// rented: rank 0 is destroyed in the same call, and the job goes back to the queue at once, so
// rank 0 is rented once in an admission. No machine for rank 0, or no place for every rank, is a
// wait, then a job this cluster cannot run.
func TestAGangWithoutAMachineForEveryRankRentsNothingAndWaits(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	exp := gangJob(t, "exp-g", 0)
	zero, one := rankNames("exp-g", 0)
	p.Fail = func(method, name string) error {
		if method == "Create" && name == one {
			return fmt.Errorf("vendor: %w", provider.ErrNoOffer)
		}
		return nil
	}
	var unrunnable *agentexec.Unrunnable
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &unrunnable) || unrunnable.Reason != clusterapi.PhaseReasonNoOffer || !strings.Contains(unrunnable.Why, "place fake-place") {
		t.Fatalf("rank 1 without an offer: err = %v, want no_offer naming the place", err)
	}
	if names := p.Names(); len(names) != 0 || p.Calls("Create") != 2 {
		t.Fatalf("rented %v after %d creates, want everything undone", names, p.Calls("Create"))
	}
	p.Fail = func(method, name string) error {
		if method == "Create" && name == zero {
			return fmt.Errorf("vendor: %w", provider.ErrNoOffer)
		}
		return nil
	}
	var waiting *agentexec.Waiting
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &waiting) || waiting.Reason != clusterapi.PhaseReasonWaitingForOffer {
		t.Fatalf("rank 0 without an offer: err = %v, want a wait", err)
	}
	admitted := time.Now().Add(-time.Hour)
	exp.SubmittedAt = &admitted
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &unrunnable) || unrunnable.Reason != clusterapi.PhaseReasonNoOffer {
		t.Fatalf("past the bound: err = %v, want no_offer", err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("past the bound: rented %v", names)
	}

	p.Fail = nil
	p.Network = func(provider.Spec) provider.Supply {
		return provider.Supply{Count: 1, Why: "one machine in the cluster"}
	}
	exp.SubmittedAt = &[]time.Time{time.Now()}[0]
	if err := e.CreateWorkload(ctx, exp); !errors.As(err, &waiting) || !strings.Contains(waiting.Why, "no place on the provider's private network") || !strings.Contains(waiting.Why, "2 of h100") {
		t.Fatalf("no place for both: err = %v", err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("rented %v without a place", names)
	}

	// A vendor that fails a rank undoes the rest too, and says why.
	p.Network = nil
	p.Fail = failing("Create", one)
	if err := e.CreateWorkload(ctx, exp); !errors.Is(err, errVendor) || errors.As(err, &waiting) {
		t.Fatalf("a failed rank: err = %v, want the vendor's error", err)
	}
	if names := p.Names(); len(names) != 0 {
		t.Fatalf("after a failed rank: rented %v", names)
	}
}

// A provider that cannot choose a place fails the call, with nothing rented.
func TestAGangWhosePlaceCannotBeChosenRentsNothing(t *testing.T) {
	e, p := newGangExecutor(t)
	p.Fail = failing("Place", "")
	if err := e.CreateWorkload(context.Background(), gangJob(t, "exp-g", 0)); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v, want the vendor's error", err)
	}
	if names := p.Names(); len(names) != 0 || p.Calls("Create") != 0 {
		t.Fatalf("rented %v after %d creates, want nothing", names, p.Calls("Create"))
	}
}

// A rank that fails fails the job, whatever the others do, and explains it.
func TestARankThatFailsFailsItsGang(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	zero, one := rankNames("exp-g", 0)
	p.Start(zero)
	p.Start(one)
	p.Exit(one, "CUDA error: an illegal memory access", exitLine(3))
	if phase, _ := status(t, e, "exp-g"); phase != clusterapi.JobPhaseFailed {
		t.Fatalf("phase %v, want failed", phase)
	}
	reason, message, _, scheduled, err := e.PollPhaseDetail(ctx, "exp-g", 0)
	if err != nil || reason != clusterapi.PhaseReasonOfExit(3) || message != "rank 1: command exited with code 3" || scheduled != 2 {
		t.Fatalf("detail %q %q, scheduled %d, %v", reason, message, scheduled, err)
	}
}

// Each rank's output is a section of the job's tail, rank 0 and then the failed rank last.
func TestTheTailOfAGangHasASectionPerRank(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	zero, one := rankNames("exp-g", 0)
	if tail, err := e.FetchLogTail(ctx, "exp-g", 40); err != nil || len(tail) != 0 {
		t.Fatalf("booting: tail %q, %v", tail, err)
	}
	p.Start(zero)
	p.Start(one)
	p.Print(zero, "waiting in all_reduce")
	p.Exit(one, "Traceback: boom", exitLine(3))
	tail, err := e.FetchLogTail(ctx, "exp-g", 40)
	want := []string{
		"--- rank 0 (instance " + zero + ") ---", "waiting in all_reduce",
		"--- rank 1 failed, exit 3 (instance " + one + ") ---", "Traceback: boom", exitLine(3),
	}
	if err != nil || !reflect.DeepEqual(tail, want) {
		t.Fatalf("tail %q, %v, want %q", tail, err, want)
	}
}

// A rank that vanished leaves the others waiting for it: the job no longer matches, every rank is
// deleted, and the next pass rents the whole job again.
func TestAGangMissingARankIsReplacedWhole(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	exp := gangJob(t, "exp-g", 0)
	mustCreate(t, e, exp)
	zero, one := rankNames("exp-g", 0)
	p.Start(zero)
	p.Start(one)
	if err := p.Destroy(ctx, p.IDs(one)[0]); err != nil {
		t.Fatal(err)
	}
	if phase, _ := status(t, e, "exp-g"); phase != clusterapi.JobPhaseGone {
		t.Fatalf("phase %v, want gone with a rank missing", phase)
	}
	if matches, err := e.WorkloadMatchesDesired(ctx, exp); err != nil || matches {
		t.Fatalf("matches %v, %v, want the gang replaced", matches, err)
	}
	if err := e.DeleteWorkload(ctx, "exp-g", false); err != nil || len(p.Names()) != 0 {
		t.Fatalf("delete: %v, rented %v", err, p.Names())
	}
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-g": exp}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, exp)
	if names := p.Names(); !reflect.DeepEqual(names, []string{zero, one}) {
		t.Fatalf("rented %v, want the whole gang again", names)
	}
}

// A rank that has not started within Startup is replaced with the rest of its gang.
func TestAGangWithAnOverdueRankDoesNotMatch(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	exp := gangJob(t, "exp-g", 0)
	mustCreate(t, e, exp)
	zero, one := rankNames("exp-g", 0)
	p.Start(zero)
	p.Age(one, time.Hour)
	if matches, err := e.WorkloadMatchesDesired(ctx, exp); err != nil || matches {
		t.Fatalf("matches %v, %v, want the gang replaced", matches, err)
	}
}

// A retry is a new attempt: every rank gets a new name.
func TestARetriedGangGetsNewNames(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	zero, one := rankNames("exp-g", 0)
	p.Start(zero)
	p.Start(one)
	p.Exit(one, exitLine(1))
	retry := gangJob(t, "exp-g", 1)
	if matches, err := e.WorkloadMatchesDesired(ctx, retry); err != nil || matches {
		t.Fatalf("the old attempt matches the retry: %v, %v", matches, err)
	}
	if err := e.DeleteWorkload(ctx, "exp-g", false); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, retry)
	if err := e.ReapTerminal(ctx, map[string]*clusterapi.DesiredJob{"exp-g": retry}); err != nil {
		t.Fatal(err)
	}
	zero, one = rankNames("exp-g", 1)
	if names := p.Names(); !reflect.DeepEqual(names, []string{zero, one}) {
		t.Fatalf("rented %v", names)
	}
	if spec := p.Spec(one); spec.Place != "fake-place" || spec.Env[jobenv.Attempt] != "1" {
		t.Fatalf("rank 1 of the retry: place %q, attempt %q", spec.Place, spec.Env[jobenv.Attempt])
	}
}

// A termination granted the checkpoint window is a SIGTERM to every rank, which keep running.
func TestACheckpointGrantStopsEveryRank(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	zero, one := rankNames("exp-g", 0)
	p.Start(zero)
	p.Start(one)
	if err := e.DeleteWorkload(ctx, "exp-g", true); err != nil {
		t.Fatal(err)
	}
	if p.Stops(zero) != 1 || p.Stops(one) != 1 || len(p.Names()) != 2 {
		t.Fatalf("stops %d and %d, rented %v, want both signalled and kept", p.Stops(zero), p.Stops(one), p.Names())
	}
}

// A free slot of a shape a waiting gang wants says how large a gang one private network can
// hold of it now, and why none when it holds none. The gang counts every node on the shape.
func TestAFreeSlotSaysWhatAPrivateNetworkHoldsOfItsShape(t *testing.T) {
	p := fake.New()
	gang := waitingH100("amd64")
	gang.Job.NumNodes = 2
	cfg := testConfig(p)
	cfg.Demand = waitingFor{gang}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var asked []int
	p.Network = func(spec provider.Spec) provider.Supply {
		asked = append(asked, 0)
		if spec.AcceleratorType != h100 {
			t.Errorf("asked about %+v", spec)
		}
		return provider.Supply{Count: 2}
	}
	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if labels["cpu-0"][clusterapi.NodeSlotStartupLabel] != "240" {
		t.Errorf("labels %v, want every slot to say how long its machine may take to start", labels["cpu-0"])
	}
	if err != nil || labels["h100-0"][clusterapi.NodeSlotNetworkLabel] != "2" || labels["h100-1"][clusterapi.NodeSlotNetworkLabel] != "2" || labels["cpu-0"][clusterapi.NodeSlotNetworkLabel] != "" || len(asked) != 1 {
		t.Fatalf("labels %v, asked %d times, %v", labels, len(asked), err)
	}
	if d := e.demanded(ctx, &view{}); d.wanted[marketKey("h100", "amd64")] != 2 || d.gangs[marketKey("h100", "amd64")] != 2 {
		t.Fatalf("demand %+v, want both nodes of the gang on h100", d)
	}
	p.Network = func(provider.Spec) provider.Supply {
		return provider.Supply{Why: "no physical cluster has two H100 machines"}
	}
	_, _, _, labels, _ = e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if labels["h100-0"][clusterapi.NodeSlotNetworkLabel] != "0" || labels["h100-0"][clusterapi.NodeSlotNoNetworkLabel] != "shape h100 on amd64: no physical cluster has two H100 machines" {
		t.Fatalf("labels %v", labels["h100-0"])
	}
}

// Every slot says how near the nodes on one private network are, and a gang that asks for nearer
// is no demand on any shape.
func TestASlotSaysHowNearItsNetworksAreAndAGangAskingNearerIsNoDemand(t *testing.T) {
	p := fake.New()
	p.Near = string(clusterapi.NodesWithinZone)
	gang := waitingH100("amd64")
	gang.Job.NumNodes = 2
	cfg := testConfig(p)
	cfg.Demand = waitingFor{gang}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _, _, labels, err := e.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil || labels["h100-0"][clusterapi.NodeSlotNetworkWithinLabel] != "zone" || labels["cpu-0"][clusterapi.NodeSlotNetworkWithinLabel] != "zone" {
		t.Fatalf("labels %v, %v, want every slot within a zone", labels, err)
	}
	if d := e.demanded(ctx, &view{}); d.wanted[marketKey("h100", "amd64")] != 2 {
		t.Fatalf("demand %+v, want the gang within a zone on h100", d)
	}
	gang.Job.Topology = &clusterapi.TopologySpec{NodesWithin: clusterapi.NodesWithinPlacementGroup}
	e.demand = waitingFor{gang}
	if d := e.demanded(ctx, &view{}); d.wanted[marketKey("h100", "amd64")] != 0 {
		t.Fatalf("demand %+v, want none of a gang within a placement group on networks of zones", d)
	}
	p.Near = "rack"
	if _, err := New(cfg); err == nil {
		t.Fatal("a provider whose places are within no level was taken")
	}
}

// The control plane fences a report on the slot of a job's first node, so a gang without rank 0
// names no node, and no machine with it.
func TestAGangWithoutRankZeroNamesNoNode(t *testing.T) {
	e, p := newGangExecutor(t)
	ctx := context.Background()
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	if _, node, _, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-g"); err != nil || node != "h100-0" {
		t.Fatalf("node %q, %v, want rank 0's slot", node, err)
	}
	zero, _ := rankNames("exp-g", 0)
	if err := p.Destroy(ctx, p.IDs(zero)[0]); err != nil {
		t.Fatal(err)
	}
	if _, node, _, err := e.ResolveAdmittedAcceleratorType(ctx, "exp-g"); err != nil || node != "" {
		t.Fatalf("node %q, %v, want none without rank 0", node, err)
	}
}

// Every rank of a gang is an instance of its own with its own accelerators, which meets
// spread_across_hosts, so a gang that keeps the default runs on a vendor that names no host.
func TestADefaultGangRentsOnAVendorThatNamesNoHost(t *testing.T) {
	e, p := newTestExecutor(t)
	p.Host = func(provider.Spec) string { return "" }
	mustCreate(t, e, gangJob(t, "exp-g", 0))
	if names := p.Names(); len(names) != 2 {
		t.Fatalf("rented %v, want both ranks", names)
	}
}

// leavingVendor rents the instance of the name leave and then reports its Create failed, as the
// contract allows a vendor to.
type leavingVendor struct {
	*fake.Provider
	leave string
}

func (l *leavingVendor) Create(ctx context.Context, spec provider.Spec) (string, error) {
	id, err := l.Provider.Create(ctx, spec)
	if err == nil && spec.Name == l.leave {
		return "", errVendor
	}
	return id, err
}

// A Create that fails may leave its instance behind. Undoing the gang destroys it with the rest.
func TestAGangUndoesAnInstanceAFailedCreateLeftBehind(t *testing.T) {
	_, one := rankNames("exp-g", 0)
	vendor := &leavingVendor{Provider: fake.New(), leave: one}
	e, err := New(testConfig(vendor))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CreateWorkload(context.Background(), gangJob(t, "exp-g", 0)); !errors.Is(err, errVendor) {
		t.Fatalf("err = %v, want the vendor's error", err)
	}
	if names := vendor.Names(); len(names) != 0 || vendor.Calls("Create") != 2 {
		t.Fatalf("rented %v after %d creates, want everything undone", names, vendor.Calls("Create"))
	}
}
